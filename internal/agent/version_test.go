package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"whatbark/internal/store"
)

// TestAgentSendsAPIVersion (protocol-v2 A3): every /api request carries the
// X-Whatbark-Api version header, and the real server (with the version gate
// active) accepts the agent's declared version.
func TestAgentSendsAPIVersion(t *testing.T) {
	// 1) Header capture: the header must be present on /api requests.
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(apiVersionHeader)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"admin":false,"employee_id":1,"employee":{"id":1,"name":"probe","kind":"ai","role":"probe"},"subordinates":[]}`)
	}))
	defer srv.Close()

	a := New(Config{ServerURL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := a.getMe(ctx); err != nil {
		t.Fatalf("getMe: %v", err)
	}
	if got != apiVersion {
		t.Fatalf("X-Whatbark-Api = %q, want %q", got, apiVersion)
	}

	// 2) End-to-end: the real server accepts the agent's version header.
	e := newEnv(t)
	_, tok := makeEmployee(t, e, "PV", store.KindAI)
	a2 := New(Config{ServerURL: e.srv.URL, Token: tok})
	emp, err := a2.getMe(ctx)
	if err != nil {
		t.Fatalf("getMe against real server: %v", err)
	}
	if emp.Name != "PV" {
		t.Fatalf("employee name = %q, want %q", emp.Name, "PV")
	}
}
