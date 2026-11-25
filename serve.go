package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"whatbark/internal/api"
	"whatbark/internal/flow"
	"whatbark/internal/hub"
	"whatbark/internal/notify"
	"whatbark/internal/sched"
	"whatbark/internal/store"
)

// cmdServe runs the platform server: SQLite + HTTP API + WebSocket events +
// scheduler (periodic schedules, self-review injection, offline reaping).
func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dataPath := fs.String("data", "whatbark.db", "SQLite database file path")
	listen := fs.String("listen", "127.0.0.1:8080", "listen address")
	adminToken := fs.String("admin-token", "", "boss token (generated on first start if empty)")
	tick := fs.Duration("tick", time.Second, "scheduler tick interval")
	_ = fs.Parse(args)

	st, err := store.Open(*dataPath)
	if err != nil {
		slog.Error("open database", "err", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	// Disk management: the storage_path setting overrides the files
	// directory. Applied at boot only; existing files under the previous
	// directory are NOT migrated (operator moves them manually).
	if p := st.GetSettingDefault("storage_path", ""); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			slog.Error("resolve storage_path", "path", p, "err", err)
			os.Exit(1)
		}
		// Compare absolute-to-absolute: FilesDir may still be relative when
		// -data is relative (review finding #4), which would otherwise fire a
		// spurious "files were NOT moved" warning for the default location.
		absCurrent, err := filepath.Abs(st.FilesDir)
		if err != nil {
			slog.Error("resolve current files dir", "dir", st.FilesDir, "err", err)
			os.Exit(1)
		}
		if abs != absCurrent {
			for _, sub := range []string{"avatars", "files", "artifacts"} {
				if err := os.MkdirAll(filepath.Join(abs, sub), 0o755); err != nil {
					slog.Error("prepare storage_path", "dir", filepath.Join(abs, sub), "err", err)
					os.Exit(1)
				}
			}
			slog.Warn("storage_path override applied; existing files were NOT moved", "old", st.FilesDir, "new", abs)
			st.FilesDir = abs
		}
	}

	admin := *adminToken
	if admin == "" {
		if v, err := st.GetSetting("admin_token"); err == nil && v != "" {
			admin = v
		} else {
			admin, err = generateToken()
			if err != nil {
				slog.Error("generate admin token", "err", err)
				os.Exit(1)
			}
			if err := st.SetSetting("admin_token", admin); err != nil {
				slog.Error("store admin token", "err", err)
				os.Exit(1)
			}
			slog.Warn("generated admin token — shown once, store it safely", "token", admin)
		}
	}

	h := hub.New()
	fl := flow.New(st, nil, h.Broadcast)
	ntf := notify.New(st)
	fl.OnTaskTerminal = ntf.OnTaskTerminal
	fl.OnMention = ntf.OnMention
	srv := api.New(st, fl, h, admin)
	sc := sched.New(st, fl)
	sc.Tick = *tick

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() { sc.Run(ctx) }()
	go srv.RunOfflineReaper(ctx)

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, // S7 nit: slow header attacks
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	slog.Info("whatbark serve listening", "addr", *listen, "data", *dataPath)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("http server", "err", err)
		os.Exit(1)
	}
}

// generateToken returns a 32-char hex secret.
func generateToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
