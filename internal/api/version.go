// Package api implements the whatbark HTTP API: token auth, REST
// endpoints, the WebSocket event endpoint, and static frontend serving.
//
// Protocol versioning (protocol-v2 task): all /api/* requests and /ws
// handshakes may carry X-Whatbark-Api: <int>. Requests without the header
// are legacy v1 clients and are accepted unchanged. A declared version
// outside [APIVersionMin, APIVersionMax] is rejected with 426 (plus the
// supported range) so clients fail fast with a clear, machine-readable
// error instead of an opaque 400/422. See docs/PROTOCOL.md.
package api

import (
	"net/http"
	"strconv"
)

const (
	APIVersionMin  = 1
	APIVersionMax  = 2
	APIVersionCurr = 2
	// APIVersionHeader is the request header carrying the client's declared
	// protocol version. Absent = legacy v1 (always accepted).
	APIVersionHeader = "X-Whatbark-Api"
)

// ServerVersion is the build version reported by GET /api/info. Overridden
// at build time: go build -ldflags "-X whatbark/internal/api.ServerVersion=...".
var ServerVersion = "dev"

// parseAPIVersion reads the version header. present=false means legacy v1.
// err is non-nil only for an unparseable value.
func parseAPIVersion(h http.Header) (version int, present bool, err error) {
	raw := h.Get(APIVersionHeader)
	if raw == "" {
		return 0, false, nil
	}
	v, perr := strconv.Atoi(raw)
	if perr != nil {
		return 0, true, perr
	}
	return v, true, nil
}

// apiVersionOK rejects requests whose declared version is outside the
// supported range [APIVersionMin, APIVersionMax]. It writes the error
// response itself and reports whether the request may proceed.
func (s *Server) apiVersionOK(w http.ResponseWriter, r *http.Request) bool {
	v, present, err := parseAPIVersion(r.Header)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":  "api.version.malformed",
			"header": APIVersionHeader,
		})
		return false
	}
	if present && (v < APIVersionMin || v > APIVersionMax) {
		writeJSON(w, http.StatusUpgradeRequired, map[string]any{
			"error":     "api.version.mismatch",
			"request":   v,
			"supported": []int{APIVersionMin, APIVersionMax},
		})
		return false
	}
	return true
}

// versionGate applies apiVersionOK to the /api/ JSON chain. It sits after
// rate limiting (abusive version probing is throttled) and before auth, so
// a version mismatch is never confused with an auth failure.
func (s *Server) versionGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.apiVersionOK(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleAPIInfo: GET /api/info — public, no auth. Version-discovery endpoint:
// clients call it before first use to learn the supported version range, the
// server's build version, and whether open registration is enabled (P2:
// true when an admin has configured a registration_token via /api/settings).
func (s *Server) handleAPIInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"api_min":        APIVersionMin,
		"api_max":        APIVersionMax,
		"server_version": ServerVersion,
		"registration":   s.registrationEnabled(),
	})
}
