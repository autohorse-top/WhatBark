package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"whatbark/internal/agent"
)

// cmdAgent runs the local executor: attach to the server with an employee
// token, poll queued tasks, run the local tool, stream output back.
func cmdAgent(args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	serverURL := fs.String("server", "http://127.0.0.1:8080", "server URL")
	token := fs.String("token", "", "employee bearer token (or set BARK_TOKEN)")
	poll := fs.Duration("poll", 3*time.Second, "task poll interval")
	heartbeat := fs.Duration("heartbeat", 15*time.Second, "heartbeat interval")
	cancelPoll := fs.Duration("cancel-poll", time.Second, "cancel poll interval")
	baseDir := fs.String("base-dir", "", "base directory for employee workdirs (default: cwd)")
	mockSleep := fs.Duration("mock-sleep", 100*time.Millisecond, "built-in mock tool sleep duration")
	fs.Parse(args)

	tok := *token
	if tok == "" {
		tok = os.Getenv("BARK_TOKEN")
	}
	if tok == "" {
		slog.Error("no token: pass -token or set BARK_TOKEN")
		os.Exit(2)
	}

	ag := agent.New(agent.Config{
		ServerURL:    *serverURL,
		Token:        tok,
		PollInterval: *poll,
		Heartbeat:    *heartbeat,
		CancelPoll:   *cancelPoll,
		BaseDir:      *baseDir,
		MockSleep:    *mockSleep,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := ag.Run(ctx); err != nil {
		slog.Error("agent exited", "err", err)
		os.Exit(1)
	}
}
