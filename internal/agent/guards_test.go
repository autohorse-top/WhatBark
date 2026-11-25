package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"whatbark/internal/store"
)

func TestAgentRefusesHuman(t *testing.T) {
	e := newEnv(t)
	_, tok := makeEmployee(t, e, "H1", store.KindHuman)
	ag := New(Config{
		ServerURL:    e.srv.URL,
		Token:        tok,
		PollInterval: 10 * time.Millisecond,
		BaseDir:      t.TempDir(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := ag.Run(ctx)
	if err == nil {
		t.Fatal("agent should reject non-ai employee")
	}
	if !strings.Contains(err.Error(), "human") {
		t.Errorf("error = %v", err)
	}
}

func TestAgentBadToken(t *testing.T) {
	e := newEnv(t)
	ag := New(Config{
		ServerURL:    e.srv.URL,
		Token:        "no-such-token",
		PollInterval: 10 * time.Millisecond,
		BaseDir:      t.TempDir(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ag.Run(ctx); err == nil {
		t.Fatal("Run with invalid token should fail")
	}
}
