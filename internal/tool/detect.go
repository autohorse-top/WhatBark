package tool

import (
	"context"
	"os/exec"
	"time"
)

// detectCandidates lists local LLM tools in priority order for auto-detection.
var detectCandidates = []struct {
	Name string // tool.Spec.Name
	Bin  string // binary name to look up in PATH
}{
	{Name: "opencode", Bin: "opencode"},
	{Name: "claudecode", Bin: "claude"},
	{Name: "codex", Bin: "codex"},
}

// probeCmd verifies a tool binary is functional. Overridable in tests.
var probeCmd = func(bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

// DetectLocalTool searches PATH for available LLM tools in priority order.
// For each candidate it checks the binary exists and responds to --version.
// Returns the tool spec name (e.g. "opencode") or "" if none found.
func DetectLocalTool() string {
	for _, c := range detectCandidates {
		path, err := exec.LookPath(c.Bin)
		if err != nil {
			continue
		}
		if err := probeCmd(path); err == nil {
			return c.Name
		}
	}
	return ""
}
