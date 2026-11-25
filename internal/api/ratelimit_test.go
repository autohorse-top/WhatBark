package api

import (
	"net/http"
	"testing"
	"time"
)

// H-6 unit test: token-bucket semantics with an injectable clock.
// burst=5, rate=10/s. Sequence covers: burst exhaustion, per-key
// independence, partial refill, sub-token refill (still denied), burst cap
// (idle time must not bank beyond the burst), and re-exhaustion.
func TestRateLimiterAllow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newRateLimiter(5, 10, func() time.Time { return now })

	cases := []struct {
		advance time.Duration
		key     string
		want    bool
	}{
		{0, "a", true},                       // 5->4
		{0, "a", true},                       // 4->3
		{0, "a", true},                       // 3->2
		{0, "a", true},                       // 2->1
		{0, "a", true},                       // 1->0
		{0, "a", false},                      // 0: exhausted
		{0, "b", true},                       // independent key: 5->4 (b.last = t0)
		{200 * time.Millisecond, "a", true},  // refill 0.2s*10 = 2 tokens: 2->1
		{0, "a", true},                       // 1->0
		{0, "a", false},                      // 0
		{50 * time.Millisecond, "a", false},  // 0.05s*10 = 0.5 < 1: still denied
		{9750 * time.Millisecond, "a", true}, // t=+10s: 0.5 + 9.75s*10 capped at 5: 5->4
		{0, "a", true},                       // 4->3
		{0, "a", true},                       // 3->2
		{0, "a", true},                       // 2->1
		{0, "a", true},                       // 1->0
		{0, "a", false},                      // 0 again
		{0, "b", true},                       // b: last touched t0, now t=+10s: 4 + 10s*10 capped at 5: 5->4
		{0, "b", true},                       // 4->3
		{0, "b", true},                       // 3->2
		{0, "b", true},                       // 2->1
		{0, "b", true},                       // 1->0
		{0, "b", false},                      // 0
	}
	for i, c := range cases {
		now = now.Add(c.advance)
		got, retryAfter := l.Allow(c.key)
		if got != c.want {
			t.Fatalf("case %d (%s after +%v): got ok=%v want %v", i, c.key, c.advance, got, c.want)
		}
		if !got && (retryAfter < time.Second || retryAfter > 5*time.Second) {
			t.Fatalf("case %d: retryAfter %v outside [1s, 5s]", i, retryAfter)
		}
	}
}

// H-6 integration test: 501 consecutive /api/me requests with one token and a
// frozen clock (no refill) -> first 500 return 200, request 501 returns 429
// with a Retry-After header; a different key (different token) is unaffected.
func TestRateLimitIntegration501(t *testing.T) {
	e := newEnv(t)
	frozen := time.Unix(1_700_000_000, 0)
	e.api.limiter = newRateLimiter(rateBurst, ratePerSecond, func() time.Time { return frozen })

	for i := 0; i < rateBurst; i++ {
		code, _ := req(t, http.MethodGet, e.srv.URL+"/api/me", adminTok, nil)
		if code != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200", i+1, code)
		}
	}

	r, err := http.NewRequest(http.MethodGet, e.srv.URL+"/api/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+adminTok)
	resp, err := e.srv.Client().Do(r)
	if err != nil {
		t.Fatalf("request 501: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("request 501: status %d, want 429", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Fatal("request 501: missing Retry-After header")
	}

	// A different key (different token) must be unaffected.
	code, _ := req(t, http.MethodGet, e.srv.URL+"/api/me", "other-token", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("other token: status %d, want 401 (key isolation)", code)
	}
}
