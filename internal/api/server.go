// Package api implements the whatbark HTTP API: token auth, REST
// endpoints, the WebSocket event endpoint, and static frontend serving.
package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"whatbark/internal/flow"
	"whatbark/internal/hub"
	"whatbark/internal/store"
	"whatbark/web"
)

// identity is the authenticated caller. EmpID 0 + Admin means an admin token
// holder; AdminID is the admin_tokens row id (V-2: multi-admin, audit records
// the specific admin, not just "admin").
type identity struct {
	EmpID   int
	Admin   bool
	AdminID int
}

type ctxKey int

const idCtxKey ctxKey = 0

var (
	errSelfScope    = errors.New("not allowed to access another employee's resources")
	errNameRequired = errors.New("name is required")
)

// Server wires the store, flow, and hub together. Admin tokens live in the
// admin_tokens table (V-2); the boot token is seeded into it by New.
type Server struct {
	Store *store.Store
	Flow  *flow.Flow
	Hub   *hub.Hub

	mu        sync.Mutex
	online    map[int]time.Time
	offlineAt time.Duration

	limiter *rateLimiter // H-6: per-key /api/ rate limit (token-bucket)

	// P0-1: short-lived HMAC-signed file/avatar URLs (?exp=&sig=) and
	// subprotocol-token WS handshake. signKey is per-process random; signed
	// URLs from a previous process become invalid on restart (by design).
	// signBroken is set when signKey could not be generated; while true,
	// signFileURL mints no signature and fileURLAuth always fails (F1:
	// never fall back to an empty/zero key, which would let anyone forge
	// valid signed URLs).
	signKey    []byte
	signBroken bool
	fileURLTTL time.Duration

	// F7: server process start time, exported on /metrics as
	// whatbark_uptime_seconds.
	startTime time.Time

	// legacyQueryTokenDisabled (F11) cuts the legacy ?token= query-param
	// fallback for file/avatar and /ws auth. Default (false) keeps the v1
	// behavior so existing clients keep working; set the env var
	// WHATBARK_DISABLE_LEGACY_QUERY_TOKEN=1 to force signed-URL /
	// subprotocol-token auth only.
	legacyQueryTokenDisabled bool
}

// fileURLMaxLifetime bounds the acceptance window for signed URLs (clock
// skew / re-requests of stale URLs beyond this are rejected as expired).
// Coupling note (F7): fileURLTTL must stay below this bound — a TTL above
// 24h would mint URLs that fileURLAuth rejects immediately.
const fileURLMaxLifetime = 24 * time.Hour

// effectiveFileURLTTL (F7) clamps the configured signed-URL TTL to
// fileURLMaxLifetime: a TTL beyond 24h would mint URLs that fileURLAuth
// rejects immediately (exp is the acceptance ceiling), silently breaking
// file serving at boot.
func effectiveFileURLTTL(t time.Duration) time.Duration {
	if t <= 0 {
		return time.Hour
	}
	if t > fileURLMaxLifetime {
		slog.Warn("F7: file URL TTL exceeds 24h cap; clamping",
			"requested", t.String(), "applied", fileURLMaxLifetime.String())
		return fileURLMaxLifetime
	}
	return t
}

// legacyQueryTokenDisabled (F11) reports whether the legacy ?token=
// query-param auth fallback is cut. The env var exists so operators can
// flip production to signed-URL / subprotocol-token-only without a code
// change; the default (unset) keeps v1 behavior.
func legacyQueryTokenDisabled() bool {
	switch strings.ToLower(os.Getenv("WHATBARK_DISABLE_LEGACY_QUERY_TOKEN")) {
	case "1", "true":
		return true
	default:
		return false
	}
}

// New builds a Server. admin is the boot (boss) token; New seeds it into
// admin_tokens as name "boss" (idempotent) so every admin token — including
// the boot one — authenticates through the single admin_tokens table and can
// be listed/rotated/deleted via /api/admin/tokens.
func New(st *store.Store, fl *flow.Flow, h *hub.Hub, admin string) *Server {
	s := &Server{
		Store:     st,
		Flow:      fl,
		Hub:       h,
		online:    make(map[int]time.Time),
		offlineAt: 15 * time.Second,
		limiter:   newRateLimiter(rateBurst, ratePerSecond, nil),
		// fileURLTTL is a process-fixed 1h for now; effectiveFileURLTTL's
		// 24h clamp is a defensive guard that only takes effect once the
		// TTL becomes configurable.
		fileURLTTL: effectiveFileURLTTL(time.Hour),
		startTime:  time.Now(),
		// F11: off by default so existing signed-URL + legacy clients keep
		// working; enable WHATBARK_DISABLE_LEGACY_QUERY_TOKEN=1 to cut the
		// legacy ?token= query-param fallback.
		legacyQueryTokenDisabled: legacyQueryTokenDisabled(),
	}
	// P0-1: per-process signing key for short-lived file/avatar URLs.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		// F1: with a nil/zero signKey, HMAC would use an empty key and any
		// client could forge valid signatures, so fail closed: set
		// signBroken, then signFileURL mints no signature (bare paths 401
		// unless a real token authenticates) and fileURLAuth rejects all.
		slog.Error("P0-1: crypto/rand failed; signed file URLs disabled (fail closed)", "err", err)
		s.signBroken = true
	} else {
		s.signKey = key
	}
	if admin != "" {
		if _, err := st.EnsureAdminToken("boss", admin); err != nil {
			slog.Warn("seed boot admin token", "err", err)
		}
	}
	return s
}

// Handler builds the top-level mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /metrics", s.handleMetrics) // P1b
	mux.HandleFunc("GET /ws", s.ws)
	// protocol-v2: public discovery endpoint (no auth, but rate-limited).
	// More specific than the /api/ subtree pattern, so it wins for this path.
	mux.Handle("GET /api/info", s.cors(s.rateLimit(http.HandlerFunc(s.handleAPIInfo))))
	// P2: self-registration. Public (no Bearer auth — the registration token
	// in the body is the credential) but still version-gated and rate-limited.
	// More specific than the /api/ subtree, so it is served without s.auth.
	// limitBody caps the unauthenticated body at the default 1 MiB: a public
	// endpoint must not let an untrusted client allocate unbounded memory.
	mux.Handle("POST /api/register", s.cors(s.rateLimit(s.versionGate(s.limitBody(http.HandlerFunc(s.handleRegister))))))
	// S12: static file/avatar serving lives outside the header-only /api/
	// middleware chain: browser <img>/<a> srcs cannot set Authorization, so
	// these routes authenticate via ?token= inside the handler (same v1
	// trust model as the /ws query token). The more specific patterns win
	// over the /api/ subtree below.
	mux.HandleFunc("GET /api/files/{name}", s.handleGetFile)
	mux.HandleFunc("GET /api/avatars/{name}", s.handleGetAvatar)
	mux.HandleFunc("GET /api/artifacts/{name}", s.handleGetTaskArtifact)
	// S20: deterministic pixel avatars with a server-side default fallback —
	// core.js avatarSpan promises "unknown names fall back to /avatars/default.svg";
	// the plain FileServer below used to 404 any name without a pre-baked SVG
	// (e.g. employees created after the avatar set), flooding the console with
	// load errors.
	mux.Handle("GET /avatars/{name}", noCache(http.HandlerFunc(s.handleStaticAvatar)))
	// noCache: SPA assets must never be served stale — a cached v-N core.js
	// paired with a newer index.html blanks the whole page (S11 incident).
	mux.Handle("/", noCache(http.FileServerFS(web.FS)))

	apiMux := http.NewServeMux()
	s.registerAPI(apiMux)
	// protocol-v2: versionGate sits after rateLimit and before auth so a
	// version mismatch is answered with 426/400, not 401.
	mux.Handle("/api/", s.cors(s.rateLimit(s.versionGate(s.auth(s.limitBody(apiMux)))))) // H-6: cors→rateLimit→versionGate→auth→limitBody
	return mux
}

// Body size caps (S7-Minor7 + S12 + disk management). The default 1 MiB is
// far more than any JSON payload; uploads get larger, path-specific caps.
// The upload caps track the settings-driven per-file cap
// (storage_max_file_bytes) plus multipart framing overhead; at the 10 MiB
// default they equal the previous fixed 12 MiB cap.
const (
	maxBodyBytes        = 1 << 20 // default JSON payload cap
	uploadOverheadBytes = 2 << 20 // multipart framing on top of the file cap
	maxAvatarBodyBytes  = 3 << 20 // /api/me/avatar (2 MiB images + overhead)
)

// limitBody caps request bodies per path. It is a method (not a free
// function) because the upload caps are settings-driven (disk management):
// they must track storage_max_file_bytes at request time.
func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := maxBodyBytes
		p := r.URL.Path
		switch {
		case p == "/api/files",
			// task-duration-artifacts B2: agent artifact uploads (dynamic {id},
			// so a prefix+suffix match instead of an exact path) share the
			// settings-driven file cap.
			strings.HasPrefix(p, "/api/agent/tasks/") && strings.HasSuffix(p, "/artifacts"):
			limit = int(s.loadStorageConfig().MaxFileBytes) + uploadOverheadBytes
		case p == "/api/me/avatar":
			limit = maxAvatarBodyBytes
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, int64(limit))
		}
		next.ServeHTTP(w, r)
	})
}

// noCache forces browsers to revalidate every SPA asset request. The embedded
// frontend is versioned by rebuild only; without this, heuristic caching can
// pair an old core.js with a new index.html and blank the page.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		h.Set("Pragma", "no-cache")
		h.Set("Expires", "0")
		next.ServeHTTP(w, r)
	})
}

// handleStaticAvatar serves an embedded pixel avatar from web/avatars/.
// Unknown names fall back to avatars/default.svg (the contract documented in
// web/core.js avatarSpan), so names created after the avatar set is baked
// still render an image instead of a 404.
func (s *Server) handleStaticAvatar(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// Only the .svg files under web/avatars/ exist; anything else 404s.
	// (Go 1.22 mux wildcards must end the segment, so the suffix is checked
	// here instead of in the pattern.)
	if !strings.HasSuffix(name, ".svg") {
		http.NotFound(w, r)
		return
	}
	base := strings.TrimSuffix(name, ".svg")
	// {name} cannot contain a slash (single-segment wildcard), but keep the
	// guard: the embedded FS must never be probed with traversal-ish names.
	if base == "" || base == "." || base == ".." || strings.ContainsAny(base, `/\`) {
		http.NotFound(w, r)
		return
	}
	p := "avatars/" + base + ".svg"
	if _, err := web.FS.Open(p); err != nil {
		p = "avatars/default.svg"
	}
	f, err := web.FS.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store")
	io.Copy(w, f)
}

// cors allows browser SPAs on other origins (multi-server switching, R8).
// Preflight is answered before auth so OPTIONS needs no token.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) registerAPI(m *http.ServeMux) {
	m.HandleFunc("GET /api/me", s.handleMe)
	m.HandleFunc("POST /api/me/avatar", s.handleUploadAvatar)
	m.HandleFunc("POST /api/files", s.handleUploadFile)
	m.HandleFunc("GET /api/departments", s.handleListDepartments)
	m.HandleFunc("POST /api/departments", s.adminOnly(s.handleCreateDepartment))
	m.HandleFunc("PUT /api/departments/{id}", s.adminOnly(s.handleRenameDepartment)) // S31
	m.HandleFunc("PUT /api/departments/{id}/leader", s.adminOnly(s.handleSetDepartmentLeader))
	m.HandleFunc("DELETE /api/departments/{id}", s.adminOnly(s.handleDeleteDepartment))
	m.HandleFunc("GET /api/employees", s.handleListEmployees)
	m.HandleFunc("POST /api/employees", s.adminOnly(s.handleCreateEmployee))
	m.HandleFunc("PUT /api/employees/{id}/tool", s.adminOnly(s.handleSetEmployeeTool))
	m.HandleFunc("DELETE /api/employees/{id}", s.adminOnly(s.handleDeleteEmployee))
	m.HandleFunc("POST /api/employees/reissue-tokens", s.adminOnly(s.handleReissueEmployeeTokens))
	m.HandleFunc("GET /api/routes", s.handleRoutes)
	m.HandleFunc("GET /api/workflows", s.handleListWorkflows)
	m.HandleFunc("POST /api/decompose", s.handleDecompose) // V-3: lead AI decomposes a task graph
	m.HandleFunc("POST /api/workflows", s.adminOrHuman(s.handleCreateWorkflow))
	m.HandleFunc("GET /api/workflows/{id}", s.handleGetWorkflow)
	m.HandleFunc("DELETE /api/workflows/{id}", s.adminOnly(s.handleDeleteWorkflow))
	m.HandleFunc("POST /api/workflows/{id}/nodes", s.adminOrHuman(s.handleAddWorkflowNode))
	m.HandleFunc("DELETE /api/workflows/{id}/nodes/{nodeId}", s.adminOrHuman(s.handleRemoveWorkflowNode))
	m.HandleFunc("PUT /api/workflows/{id}/nodes/{nodeId}", s.adminOrHuman(s.handleMoveWorkflowNode))
	m.HandleFunc("POST /api/workflows/{id}/edges", s.adminOrHuman(s.handleAddWorkflowEdge))
	m.HandleFunc("DELETE /api/workflows/{id}/edges/{edgeId}", s.adminOrHuman(s.handleRemoveWorkflowEdge))
	m.HandleFunc("GET /api/projects", s.handleListProjects)
	m.HandleFunc("POST /api/projects", s.adminOnly(s.handleCreateProject))
	m.HandleFunc("GET /api/projects/{id}", s.handleGetProject)
	m.HandleFunc("PUT /api/projects/{id}", s.adminOnly(s.handleUpdateProject))
	m.HandleFunc("DELETE /api/projects/{id}", s.adminOnly(s.handleDeleteProject))
	m.HandleFunc("GET /api/schedules", s.handleListSchedules)
	m.HandleFunc("POST /api/schedules", s.adminOrHuman(s.handleCreateSchedule))
	m.HandleFunc("PATCH /api/schedules/{id}", s.adminOrHuman(s.handlePatchSchedule))
	m.HandleFunc("DELETE /api/schedules/{id}", s.adminOrHuman(s.handleDeleteSchedule))
	m.HandleFunc("GET /api/audit", s.adminOnly(s.handleAudit))           // H-7
	m.HandleFunc("GET /api/settings", s.adminOnly(s.handleGetSettings))  // V-4
	m.HandleFunc("POST /api/settings", s.adminOnly(s.handleSetSettings)) // V-4
	// V-2: multi-admin tokens (list / create / rotate-delete). Admin-only;
	// the created plaintext token is returned exactly once.
	m.HandleFunc("GET /api/admin/tokens", s.adminOnly(s.handleListAdminTokens))
	m.HandleFunc("POST /api/admin/tokens", s.adminOnly(s.handleCreateAdminToken))
	m.HandleFunc("DELETE /api/admin/tokens/{id}", s.adminOnly(s.handleDeleteAdminToken))
	s.registerStorageRoutes(m) // disk management: GET /api/admin/storage
	m.HandleFunc("GET /api/tasks", s.handleListTasks)
	m.HandleFunc("GET /api/tasks/{id}", s.handleGetTask)
	m.HandleFunc("POST /api/tasks/{id}/start", s.handleTaskStart)
	m.HandleFunc("POST /api/tasks/{id}/finish", s.handleTaskFinish)
	m.HandleFunc("POST /api/tasks/{id}/fail", s.handleTaskFail)
	m.HandleFunc("POST /api/tasks/{id}/cancel", s.handleTaskCancel)
	m.HandleFunc("POST /api/tasks/{id}/preempt", s.handleTaskPreempt)
	m.HandleFunc("POST /api/tasks/{id}/reassign", s.handleTaskReassign) // S27
	m.HandleFunc("POST /api/agent/tasks/{id}/artifacts", s.handleUploadTaskArtifact)
	m.HandleFunc("GET /api/tasks/{id}/artifacts", s.handleListTaskArtifacts)
	m.HandleFunc("GET /api/messages", s.handleListMessages)
	m.HandleFunc("POST /api/messages", s.handleSendMessage)
	m.HandleFunc("POST /api/groups", s.handleCreateGroup)                            // S14: group chat
	m.HandleFunc("GET /api/groups/{id}", s.handleGetGroup)                           // S17: group settings
	m.HandleFunc("POST /api/groups/{id}/members", s.handleAddGroupMembers)           // S17
	m.HandleFunc("POST /api/groups/{id}/prefs", s.handleSetGroupPrefs)               // S17
	m.HandleFunc("GET /api/groups/{id}/tasks", s.handleGroupTasks)                   // S24: group task board
	m.HandleFunc("PATCH /api/groups/{id}", s.handleRenameGroup)                      // S26: rename
	m.HandleFunc("DELETE /api/groups/{id}/members/{eid}", s.handleRemoveGroupMember) // S26
	m.HandleFunc("DELETE /api/groups/{id}", s.handleDeleteGroup)                     // S26
	m.HandleFunc("POST /api/groups/{id}/policy", s.handleSetGroupPolicy)             // S30
	m.HandleFunc("GET /api/conversations", s.handleConversations)
	m.HandleFunc("POST /api/conversations/{peer_id}/pin", s.handleSetConvPin) // S33: per-viewer pin
	m.HandleFunc("POST /api/messages/read", s.handleMarkRead)
	m.HandleFunc("GET /api/search", s.handleSearch)                // S19-C: unified search
	m.HandleFunc("GET /api/i18n/languages", s.handleI18nLanguages) // i18n T1
	m.HandleFunc("POST /api/agent/attach", s.handleAgentAttach)
	m.HandleFunc("POST /api/agent/output", s.handleAgentOutput)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleI18nLanguages (i18n T1) lists the available UI languages: every
// web/i18n/<lang>.js file embedded at build time, with "en" always present.
// A language without a file is never listed, so the settings page offers no
// switch to it — a mechanism guarantee, not client-side filtering. The route
// sits under /api/, so the auth middleware enforces a valid token (401
// otherwise).
func (s *Server) handleI18nLanguages(w http.ResponseWriter, _ *http.Request) {
	codes := []string{"en"}
	entries, err := web.FS.ReadDir("i18n")
	if err == nil {
		seen := map[string]bool{"en": true}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
				continue
			}
			code := strings.TrimSuffix(e.Name(), ".js")
			if !seen[code] {
				seen[code] = true
				codes = append(codes, code)
			}
		}
	}
	sort.Strings(codes[1:])
	writeJSON(w, http.StatusOK, map[string]any{"languages": codes})
}

// handleMetrics (P1b) exports a minimal Prometheus text-format snapshot.
// Deliberately unauthenticated: it exposes only coarse operational counters
// (uptime, goroutines, online agents, table counts), not task content, file
// bytes, or tokens. Default bind is 127.0.0.1, so this is acceptable; if the
// server is exposed beyond a trusted network, put it behind the reverse
// proxy like /healthz.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP whatbark_uptime_seconds Server process uptime.\n")
	fmt.Fprintf(w, "# TYPE whatbark_uptime_seconds gauge\n")
	fmt.Fprintf(w, "whatbark_uptime_seconds %.6f\n", time.Since(s.startTime).Seconds())
	fmt.Fprintf(w, "# HELP whatbark_goroutines Current Go goroutine count.\n")
	fmt.Fprintf(w, "# TYPE whatbark_goroutines gauge\n")
	fmt.Fprintf(w, "whatbark_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintf(w, "# HELP whatbark_ws_clients Currently connected WebSocket clients.\n")
	fmt.Fprintf(w, "# TYPE whatbark_ws_clients gauge\n")
	fmt.Fprintf(w, "whatbark_ws_clients %d\n", s.Hub.Clients())

	s.mu.Lock()
	online := len(s.online)
	s.mu.Unlock()
	fmt.Fprintf(w, "# HELP whatbark_agents_online Currently attached agents.\n")
	fmt.Fprintf(w, "# TYPE whatbark_agents_online gauge\n")
	fmt.Fprintf(w, "whatbark_agents_online %d\n", online)

	if n, err := s.Store.CountEmployees(); err == nil {
		fmt.Fprintf(w, "# HELP whatbark_employees_total Total employees.\n")
		fmt.Fprintf(w, "# TYPE whatbark_employees_total gauge\n")
		fmt.Fprintf(w, "whatbark_employees_total %d\n", n)
	} else {
		slog.Error("metrics: count employees", "err", err)
		http.Error(w, "metrics error", http.StatusInternalServerError)
		return
	}
	if n, err := s.Store.CountFiles(); err == nil {
		fmt.Fprintf(w, "# HELP whatbark_files_total Total stored files.\n")
		fmt.Fprintf(w, "# TYPE whatbark_files_total gauge\n")
		fmt.Fprintf(w, "whatbark_files_total %d\n", n)
	} else {
		slog.Error("metrics: count files", "err", err)
		http.Error(w, "metrics error", http.StatusInternalServerError)
		return
	}
	counts, err := s.Store.TaskStatusCounts()
	if err != nil {
		slog.Error("metrics: task status counts", "err", err)
		http.Error(w, "metrics error", http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "# HELP whatbark_tasks_total Tasks by status.\n")
	fmt.Fprintf(w, "# TYPE whatbark_tasks_total gauge\n")
	known := []string{store.TaskQueued, store.TaskRunning, store.TaskDone, store.TaskFailed, store.TaskCanceled}
	seen := make(map[string]bool, len(counts))
	for _, st := range known {
		fmt.Fprintf(w, "whatbark_tasks_total{status=%q} %d\n", st, counts[st])
		seen[st] = true
	}
	var unknown []string
	for st := range counts {
		if !seen[st] {
			unknown = append(unknown, st)
		}
	}
	sort.Strings(unknown)
	for _, st := range unknown {
		fmt.Fprintf(w, "whatbark_tasks_total{status=%q} %d\n", st, counts[st])
	}
}

// ws upgrades a WebSocket connection after token auth (P0-1): the token rides
// on Sec-WebSocket-Protocol (browser WebSocket API cannot set custom headers);
// the hybi handshaker echoes the single subprotocol back in the 101 response
// automatically. Legacy ?token= is kept as a fallback for old clients/scripts.
func (s *Server) ws(w http.ResponseWriter, r *http.Request) {
	// protocol-v2: version gate before the auth handshake.
	if !s.apiVersionOK(w, r) {
		return
	}
	tok := ""
	if sp := r.Header.Get("Sec-WebSocket-Protocol"); sp != "" {
		tok = strings.TrimSpace(strings.SplitN(sp, ",", 2)[0])
	}
	if _, ok := s.tokenIdentity(tok); !ok {
		if s.legacyQueryTokenDisabled { // F11: no legacy ?token= fallback
			_ = s.Store.AuditAppend("anon", 0, "auth.fail", "/ws", "ws handshake rejected")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		tok = r.URL.Query().Get("token")
		if _, ok := s.tokenIdentity(tok); !ok {
			_ = s.Store.AuditAppend("anon", 0, "auth.fail", "/ws", "ws handshake rejected") // F10
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	s.Hub.WSHandler().ServeHTTP(w, r)
}

// signFileURL (P0-1) mints a short-lived signed URL for /api/files|/api/avatars
// paths: ?exp=<unix>&sig=HMAC-SHA256(signKey, "<path>|<exp>"). The client
// (browser <img>/<a> srcs) cannot set Authorization headers, so the URL itself
// carries a temporary credential instead of a full token (P0-1: tokens must
// not appear in access logs). With signBroken (no valid key) it returns the
// bare path: a "signed" URL minted with a zero key would be forgeable, and
// the bare path simply 401s unless the caller presents a real token.
func (s *Server) signFileURL(path string) string {
	if s.signBroken {
		return path
	}
	exp := time.Now().Unix() + int64(s.fileURLTTL.Seconds())
	mac := hmac.New(sha256.New, s.signKey)
	fmt.Fprintf(mac, "%s|%d", path, exp)
	return fmt.Sprintf("%s?exp=%d&sig=%s", path, exp, hex.EncodeToString(mac.Sum(nil)))
}

// fileURLAuth reports whether r carries a valid signed URL for its own path.
// Checks: exp present+parseable, not expired, exp not beyond
// fileURLMaxLifetime in the future (rejects replay of very stale URLs and
// clock-skew abuse), and HMAC matches the request path (not just any path).
// F1: with signBroken (no usable signing key) every URL is rejected — a
// zero key would make HMAC deterministic and forgeable.
func (s *Server) fileURLAuth(r *http.Request) bool {
	if s.signBroken {
		return false
	}
	q := r.URL.Query()
	expStr, sig := q.Get("exp"), q.Get("sig")
	if expStr == "" || sig == "" {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return false
	}
	now := time.Now().Unix()
	if exp < now || exp > now+int64(fileURLMaxLifetime.Seconds()) {
		return false
	}
	mac := hmac.New(sha256.New, s.signKey)
	fmt.Fprintf(mac, "%s|%d", r.URL.Path, exp)
	want := mac.Sum(nil)
	got, err := hex.DecodeString(sig)
	if err != nil || len(got) != len(want) {
		return false
	}
	return hmac.Equal(want, got)
}

// auth wraps a handler, requiring a valid admin or employee token. Every
// failed lookup is recorded in the audit log (H-7) as auth.fail.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.tokenIdentity(r.Header.Get("Authorization"))
		if !ok {
			_ = s.Store.AuditAppend("anon", 0, "auth.fail", r.URL.Path, "missing or invalid token")
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing or invalid token"})
			return
		}
		ctx := context.WithValue(r.Context(), idCtxKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// tokenIdentity accepts a raw "Bearer <token>" or bare "<token>" header value.
// V-2: admin auth is a live lookup in admin_tokens (multi-admin); the boot
// token is just the "boss" row seeded by New. Deleted admin tokens stop
// working immediately. Employees keep their sha256-employees lookup.
func (s *Server) tokenIdentity(header string) (identity, bool) {
	tok := strings.TrimPrefix(strings.TrimSpace(header), "Bearer ")
	if tok == "" {
		return identity{}, false
	}
	if adminID, err := s.Store.LookupAdminToken(tok); err == nil {
		return identity{EmpID: 0, Admin: true, AdminID: adminID}, true
	}
	sum := sha256.Sum256([]byte(tok))
	var empID int
	if err := s.Store.DB().QueryRow(`SELECT id FROM employees WHERE token_hash = ?`, hex.EncodeToString(sum[:])).Scan(&empID); err != nil {
		return identity{}, false
	}
	return identity{EmpID: empID}, true
}

func (s *Server) caller(r *http.Request) identity {
	if id, ok := r.Context().Value(idCtxKey).(identity); ok {
		return id
	}
	return identity{}
}

// adminOnly requires an admin token (V-2: any row of admin_tokens, not just
// the boot token). Successful (2xx) requests are recorded in the audit log
// (H-7) with a route-derived action name and the specific admin's id;
// failures are not audited (a denied attempt is not a mutation).
func (s *Server) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := s.caller(r)
		if !id.Admin {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "admin token required"})
			return
		}
		cw := &statusWriter{ResponseWriter: w}
		next(cw, r)
		if cw.status == 0 || (cw.status >= 200 && cw.status < 300) {
			_ = s.Store.AuditAppend("admin", id.AdminID, auditAction(r), r.URL.Path, cw.auditDetail)
		}
	}
}

// adminOrHuman allows admin tokens and human-kind employee tokens through
// (Route B: opens wflow/sched to human employees). AI employees get 403.
// Successful (2xx) requests are also written to the audit log, with actor
// "admin" (adminID) or "employee" (empID).
func (s *Server) adminOrHuman(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := s.caller(r)
		var actorKind string
		var actorID int
		switch {
		case id.Admin:
			actorKind, actorID = "admin", id.AdminID
		default:
			emp, err := s.Store.GetEmployee(id.EmpID)
			if err != nil || emp.Kind != store.KindHuman {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "admin or human token required"})
				return
			}
			actorKind, actorID = "employee", id.EmpID
		}
		cw := &statusWriter{ResponseWriter: w}
		next(cw, r)
		if cw.status == 0 || (cw.status >= 200 && cw.status < 300) {
			_ = s.Store.AuditAppend(actorKind, actorID, auditAction(r), r.URL.Path, cw.auditDetail)
		}
	}
}

// subordinatesOf returns the set of employee ids that are direct or
// transitive subordinates of empID (reverse reporting chain, i.e.
// routes.Graph.Down). Reused from the same graph the chat/dispatch
// routers use, so the set matches what a leader can actually direct.
// Callers always receive a non-nil map: an employee with no subordinates
// gets an empty map, so "m[empID]" lookups fail closed (guardEmpIDs → 403,
// schedule-list filter → zero rows) and /api/me renders [].
func (s *Server) subordinatesOf(empID int) (map[int]bool, error) {
	g, err := flow.NewGraph(s.Store)
	if err != nil {
		return nil, err
	}
	m := g.Down[empID]
	if m == nil {
		m = map[int]bool{}
	}
	return m, nil
}

// statusWriter captures the first WriteHeader code so adminOnly's audit hook
// can distinguish success from failure. Handlers that never call WriteHeader
// (implicit 200) leave status at 0, treated as success. Handlers may set
// auditDetail (via SetAuditDetail) to enrich the audit row (V-2).
type statusWriter struct {
	http.ResponseWriter
	status      int
	auditDetail string
}

// SetAuditDetail records a free-form detail for the adminOnly audit hook
// (e.g. the created token's name). Must be called before WriteHeader.
func (w *statusWriter) SetAuditDetail(detail string) {
	if w.auditDetail == "" {
		w.auditDetail = detail
	}
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// auditAction maps a matched /api/ route (r.Pattern already includes the
// HTTP method, e.g. "POST /api/employees") to a human-readable audit action
// (H-7). Unknown routes fall back to the raw pattern.
func auditAction(r *http.Request) string {
	switch r.Pattern {
	case "POST /api/departments":
		return "department.create"
	case "PUT /api/departments/{id}":
		return "department.rename"
	case "PUT /api/departments/{id}/leader":
		return "department.set_leader"
	case "DELETE /api/departments/{id}":
		return "department.delete"
	case "POST /api/employees":
		return "employee.create"
	case "PUT /api/employees/{id}/tool":
		return "employee.set_tool"
	case "DELETE /api/employees/{id}":
		return "employee.delete"
	case "POST /api/employees/reissue-tokens":
		return "employee.reissue_tokens"
	case "POST /api/workflows":
		return "workflow.create"
	case "DELETE /api/workflows/{id}":
		return "workflow.delete"
	case "POST /api/workflows/{id}/nodes":
		return "workflow.add_node"
	case "PUT /api/workflows/{id}/nodes/{nodeId}":
		return "workflow.move_node"
	case "DELETE /api/workflows/{id}/nodes/{nodeId}":
		return "workflow.remove_node"
	case "POST /api/workflows/{id}/edges":
		return "workflow.add_edge"
	case "DELETE /api/workflows/{id}/edges/{edgeId}":
		return "workflow.remove_edge"
	case "POST /api/projects":
		return "project.create"
	case "PUT /api/projects/{id}":
		return "project.update"
	case "DELETE /api/projects/{id}":
		return "project.delete"
	case "POST /api/schedules":
		return "schedule.create"
	case "PATCH /api/schedules/{id}":
		return "schedule.update"
	case "DELETE /api/schedules/{id}":
		return "schedule.delete"
	case "GET /api/admin/tokens":
		return "admin.token.list"
	case "POST /api/admin/tokens":
		return "admin.token.create"
	case "DELETE /api/admin/tokens/{id}":
		return "admin.token.delete"
	case "GET /api/settings":
		return "settings.read"
	case "POST /api/settings":
		return "settings.update"
	case "GET /api/admin/storage":
		return "admin.storage.read"
	}
	return r.Pattern
}

// MarkOnline records an agent heartbeat and persists last_seen.
func (s *Server) MarkOnline(empID int, ts time.Time) {
	s.mu.Lock()
	_, existed := s.online[empID]
	s.online[empID] = ts
	s.mu.Unlock()
	if err := s.Store.UpdateLastSeen(empID, ts.UTC().Format(time.RFC3339)); err != nil {
		slog.Warn("mark online", "employee", empID, "err", err)
	}
	if !existed {
		s.Hub.Broadcast(hub.Event{Type: "agent.online", Payload: map[string]any{"employee_id": empID}})
	}
}

// RunOfflineReaper marks AI employees offline when heartbeats stop.
func (s *Server) RunOfflineReaper(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reapOffline()
		}
	}
}

func (s *Server) reapOffline() {
	s.mu.Lock()
	stale := make(map[int]time.Time, len(s.online))
	for id, ts := range s.online {
		stale[id] = ts
	}
	s.mu.Unlock()
	now := time.Now()
	for id, ts := range stale {
		if now.Sub(ts) > s.offlineAt {
			s.mu.Lock()
			delete(s.online, id)
			s.mu.Unlock()
			s.Hub.Broadcast(hub.Event{Type: "agent.offline", Payload: map[string]any{"employee_id": id}})
		}
	}
}

// statusOf computes the employee display status.
func (s *Server) statusOf(e store.Employee) string {
	if e.Kind != store.KindAI {
		return "manual"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.online[e.ID]; ok {
		return "online"
	}
	return "offline"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return fmt.Errorf("empty request body")
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// readJSONOptional decodes a body when non-empty (allows empty POST bodies).
func readJSONOptional(r *http.Request, v any) error {
	buf := make([]byte, 0)
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		buf = b
	}
	if len(bytes.TrimSpace(buf)) == 0 {
		return nil
	}
	return json.Unmarshal(buf, v)
}

func intParam(r *http.Request, name string) (int, error) {
	n, err := strconv.Atoi(r.PathValue(name))
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q", name, r.PathValue(name))
	}
	return n, nil
}

// newToken generates a 32-char hex secret.
func newToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
