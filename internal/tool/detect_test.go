package tool

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// prepFakeBinaries creates fake executable files in tmpDir and prepends it to PATH.
// Returns a cleanup function.
func prepFakeBinaries(t *testing.T, bins ...string) func() {
	t.Helper()
	tmpDir := t.TempDir()
	for _, bin := range bins {
		f, err := os.Create(tmpDir + "/" + bin)
		if err != nil {
			t.Fatalf("create fake %s: %v", bin, err)
		}
		f.Close()
		if err := os.Chmod(tmpDir+"/"+bin, 0o755); err != nil {
			t.Fatalf("chmod %s: %v", bin, err)
		}
	}
	origPath := os.Getenv("PATH")
	// PATH entries are separated by ':' on Unix, ';' on Windows.
	sep := ":"
	if os.PathSeparator == '\\' {
		sep = ";"
	}
	os.Setenv("PATH", tmpDir+sep+origPath)
	return func() { os.Setenv("PATH", origPath) }
}

func TestDetectLocalToolPriority(t *testing.T) {
	// All probes succeed; verify the first candidate (opencode) wins.
	origProbe := probeCmd
	probeCmd = func(bin string) error { return nil }
	defer func() { probeCmd = origProbe }()

	cleanup := prepFakeBinaries(t, "opencode", "claude", "codex")
	defer cleanup()

	if got := DetectLocalTool(); got != "opencode" {
		t.Fatalf("DetectLocalTool() = %q, want opencode", got)
	}
}

func TestDetectLocalToolSkipsFailedProbe(t *testing.T) {
	// First candidate fails probe, second succeeds.
	origProbe := probeCmd
	probeCmd = func(bin string) error {
		if strings.HasSuffix(bin, "/fakefail") || bin == "fakefail" {
			return errors.New("probe failed")
		}
		return nil
	}
	defer func() { probeCmd = origProbe }()

	cleanup := prepFakeBinaries(t, "fakefail", "oktool")
	defer cleanup()

	origCandidates := detectCandidates
	detectCandidates = []struct {
		Name string
		Bin  string
	}{
		{Name: "fail-tool", Bin: "fakefail"},
		{Name: "ok-tool", Bin: "oktool"},
	}
	defer func() { detectCandidates = origCandidates }()

	if got := DetectLocalTool(); got != "ok-tool" {
		t.Fatalf("DetectLocalTool() = %q, want ok-tool", got)
	}
}

func TestDetectLocalToolNoneAvailable(t *testing.T) {
	// Point PATH to an empty temp dir so no candidate binary is found.
	origProbe := probeCmd
	probeCmd = func(bin string) error { return nil }
	defer func() { probeCmd = origProbe }()

	tmpDir := t.TempDir()
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir)
	defer os.Setenv("PATH", origPath)

	if got := DetectLocalTool(); got != "" {
		t.Fatalf("DetectLocalTool() = %q, want empty", got)
	}
}
