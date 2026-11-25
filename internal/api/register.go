package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"regexp"

	"whatbark/internal/hub"
	"whatbark/internal/store"
)

// P2: self-registration. An org enables it by storing a registration token
// (admin, via POST /api/settings); unauthenticated devices then POST
// /api/register with that token plus their identity, and receive a fresh
// employee token (returned once) that they use for the normal /api/ auth.

// registrationTokenRe is the charset/length rule for the operator-chosen
// registration token: safe characters, 16..128 chars so the public endpoint
// is not trivially brute-forced (rateLimit is a second line of defense).
var registrationTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// registrationTokenKey is the settings key gating POST /api/register. The
// stored value is a SHA-256 hex digest of the plaintext token (never the
// token itself, mirroring employee/admin token storage). Empty/absent = off.
const registrationTokenKey = "registration_token"

// registrationEnabled reports whether self-registration is turned on.
func (s *Server) registrationEnabled() bool {
	return s.Store.GetSettingDefault(registrationTokenKey, "") != ""
}

// handleRegister: POST /api/register (public, P2).
//
// The route chain (set in Handler) is cors→rateLimit→versionGate — no auth:
// the registration token in the body IS the credential.
//
// Body: {"token", "name", "role", "department_id", "kind" (opt, default
// "ai"), "email" (opt, <=254)}.
//   - 403 {"error":"registration.disabled"} when no token is configured
//   - 401 {"error":"registration.token"} on a missing/wrong token
//   - 400 on any field validation failure
//   - 409 {"error":"registration.name_taken"} on a duplicate name
//   - 201 {"id","name","role","kind","department_id","token"} — the employee
//     token is returned exactly once (stored only as a hash, like /api/employees).
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.registrationEnabled() {
		// H-7: fail-soft audit so an admin can see probing of a disabled
		// endpoint (rate limiting is the second line of defense).
		_ = s.Store.AuditAppend("anon", 0, "register.fail", "/api/register", "disabled")
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "registration.disabled"})
		return
	}
	var b struct {
		Token        string `json:"token"`
		Name         string `json:"name"`
		Role         string `json:"role"`
		Kind         string `json:"kind"`
		DepartmentID int    `json:"department_id"`
		Email        string `json:"email"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sum := sha256.Sum256([]byte(b.Token))
	got := hex.EncodeToString(sum[:])
	want := s.Store.GetSettingDefault(registrationTokenKey, "")
	// Constant-time compare: the endpoint is public, keep token checking
	// uniform even though a 256-bit digest makes timing attacks infeasible.
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		_ = s.Store.AuditAppend("anon", 0, "register.fail", "/api/register", "token")
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "registration.token"})
		return
	}
	if b.Kind == "" {
		b.Kind = store.KindAI
	}
	// V-4: optional notification mailbox, at most 254 characters.
	if len(b.Email) > 254 {
		writeErr(w, http.StatusBadRequest, errors.New("email must be at most 254 characters"))
		return
	}
	id, err := s.Store.CreateEmployee(b.Name, b.Role, b.Kind, b.DepartmentID)
	if err != nil {
		// employees.name is UNIQUE (pre-check + index backstop): a duplicate
		// name, including a concurrent insert race, maps to 409.
		if errors.Is(err, store.ErrNameTaken) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "registration.name_taken"})
			return
		}
		// Unexpected store failure: log the detail, return a generic code so
		// internal structure never leaks to an unauthenticated caller.
		slog.Warn("register: create employee failed", "name", b.Name, "err", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "registration.invalid"})
		return
	}
	// S31: auto-join the department's channel, mirroring handleCreateEmployee,
	// so the new device shows up in its department conversation. Legacy
	// departments (no channel) are skipped; "already a member" is a no-op.
	if b.DepartmentID > 0 {
		if dept, derr := s.Store.GetDepartment(b.DepartmentID); derr == nil && dept.GroupID != nil {
			added, addErr := s.Store.AddGroupMembers(*dept.GroupID, []int{id})
			if addErr != nil && !errors.Is(addErr, store.ErrNoNewGroupMembers) {
				writeErr(w, http.StatusInternalServerError, addErr)
				return
			}
			if len(added) > 0 {
				s.Hub.Broadcast(hub.Event{Type: "group.members", Payload: map[string]any{
					"group_id": *dept.GroupID,
					"added":    added,
				}})
			}
		}
	}
	tok, err := newToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	sum2 := sha256.Sum256([]byte(tok))
	if err := s.Store.SetTokenHash(id, hex.EncodeToString(sum2[:])); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if b.Email != "" {
		if err := s.Store.SetEmail(id, b.Email); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	// H-7: fail-soft audit of the self-registration.
	_ = s.Store.AuditAppend("employee", id, "register.create", "/api/register", "name="+b.Name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "name": b.Name, "role": b.Role, "kind": b.Kind,
		"department_id": b.DepartmentID, "token": tok,
	})
}
