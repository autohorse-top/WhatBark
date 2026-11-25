package api

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// H-6: per-key token-bucket rate limit on the /api/ chain. The key is the
// hashed bearer token (remote IP when no token is sent), so one abusive
// client cannot starve the others. In-memory by design: v1 is single-node
// and the state resets on restart (documented in README).
const (
	rateBurst     = 500 // tokens a key may hold: absorbs UI bursts + agent output floods
	ratePerSecond = 100 // steady refill per key per second: plenty for agents, caps runaway loops
)

type tokenBucket struct {
	tokens float64
	last   time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	burst   int
	rate    float64 // tokens per second
	now     func() time.Time
	buckets map[string]*tokenBucket
}

// newRateLimiter builds a limiter; now is injectable for tests (nil = time.Now).
func newRateLimiter(burst int, rate float64, now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{burst: burst, rate: rate, now: now, buckets: make(map[string]*tokenBucket)}
}

// Allow consumes one token for key. When the bucket is empty it returns
// ok=false with a conservative Retry-After estimate (>= 1s).
func (l *rateLimiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, exists := l.buckets[key]
	if !exists {
		b = &tokenBucket{tokens: float64(l.burst), last: now}
		l.buckets[key] = b
	} else if dt := now.Sub(b.last).Seconds(); dt > 0 {
		b.tokens += dt * l.rate
		if b.tokens > float64(l.burst) {
			b.tokens = float64(l.burst) // idle time must not bank beyond the burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		wait := (1 - b.tokens) / l.rate                                    // seconds
		retryAfter = time.Duration(math.Ceil(wait * float64(time.Second))) // ns
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		return false, retryAfter
	}
	b.tokens--
	return true, 0
}

// rateLimit sits between cors and auth on the /api/ chain: unauthenticated
// floods are limited by IP before any token lookup runs.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, retryAfter := s.limiter.Allow(rateKey(r))
		if !ok {
			w.Header().Set("Retry-After", strconv.FormatInt(int64(retryAfter/time.Second), 10))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "rate limit exceeded"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rateKey is the hashed token when present, else the remote IP: raw secrets
// never live in the limiter map, and token-less requests are still rate
// limited (by address).
func rateKey(r *http.Request) string {
	if tok := strings.TrimPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer "); tok != "" {
		sum := sha256.Sum256([]byte(tok))
		return "tok:" + hex.EncodeToString(sum[:])
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	return "ip:" + ip
}
