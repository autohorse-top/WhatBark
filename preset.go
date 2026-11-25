package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"whatbark/internal/preset"
	"whatbark/internal/store"
)

// cmdPreset manages org-structure presets: list the built-in examples and
// import one (built-in or a custom file) into a database, issuing an
// employee token for every created employee.
//
// Usage:
//
//	whatbark preset list
//	whatbark preset import <name> [-data PATH]
//	whatbark preset import -file <preset.json> [-data PATH]
//
// Importing creates departments (with channels), employees, and department
// leaders, then writes plaintext tokens to <dbPath>-tokens.json (mode 0600).
// It is not a merge: importing a preset whose names collide with an existing
// database fails with the offending name named.
func cmdPreset(args []string) {
	if len(args) == 0 {
		presetUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		presetList()
	case "import":
		cmdPresetImport(args[1:])
	default:
		presetUsage()
		os.Exit(2)
	}
}

func presetUsage() {
	fmt.Fprintln(os.Stderr, `usage: whatbark preset <list|import> [flags]

commands:
  list                      list built-in org presets
  import <name>             import a built-in preset into a database
  import -file <preset.json> import a custom preset file

flags (import):
  -data PATH                database file (default "whatbark.db")
  -file PATH                custom preset file (instead of a built-in name)`)
}

func presetList() {
	names := preset.Names()
	for _, name := range names {
		data, _ := preset.Get(name)
		p, err := preset.Parse(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "preset %s: invalid: %v\n", name, err)
			continue
		}
		employees := 0
		for _, d := range p.Departments {
			employees += len(d.Employees)
		}
		fmt.Printf("%-10s  %-2d depts  %-3d employees  %s\n", name, len(p.Departments), employees, p.Description)
	}
}

func cmdPresetImport(args []string) {
	// The documented forms are `import <name> [-data PATH]` and
	// `import -file X [-data PATH]`; the std flag package stops at the first
	// positional, so split the preset name out before flag parsing.
	var name string
	flagArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-file" || a == "-data":
			// known value flag: keep the flag and its value together
			flagArgs = append(flagArgs, a)
			if i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
		case strings.HasPrefix(a, "-"):
			flagArgs = append(flagArgs, a) // -flag=value or unknown: let flag handle it
		default:
			if name != "" {
				fatalf("unexpected argument %q (usage: preset import <name> [-data PATH])", a)
			}
			name = a
		}
	}
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	dataPath := fs.String("data", "whatbark.db", "SQLite database file path")
	file := fs.String("file", "", "custom preset file (instead of a built-in name)")
	_ = fs.Parse(flagArgs)

	if *file == "" && name == "" {
		presetUsage()
		os.Exit(2)
	}

	var data []byte
	if *file != "" {
		b, err := os.ReadFile(*file)
		if err != nil {
			fatalf("read preset file: %v", err)
		}
		data = b
	} else {
		b, ok := preset.Get(name)
		if !ok {
			fatalf("unknown preset %q; run 'whatbark preset list' for built-in names", name)
		}
		data = b
	}
	p, err := preset.Parse(data)
	if err != nil {
		fatalf("invalid preset: %v", err)
	}

	st, err := store.Open(*dataPath)
	if err != nil {
		fatalf("open %s: %v", *dataPath, err)
	}
	defer func() { _ = st.Close() }()

	rep, err := preset.Import(st, p, *dataPath)
	if err != nil {
		fatalf("import %s: %v", p.Name, err)
	}
	if err := preset.Verify(st, p); err != nil {
		fatalf("verify after import: %v", err)
	}

	for _, d := range rep.Departments {
		fmt.Printf("dept %-18s id=%-3d leader=%s (%d employees)\n", d.Name, d.ID, d.Leader, d.Count)
	}
	fmt.Printf("imported %s: %d departments, %d employees\n", p.Name, len(rep.Departments), rep.Employees)
	// Workflow collaboration graphs (leader orchestration): one line per
	// imported graph so the CLI output mirrors what Verify just checked.
	for _, wf := range p.Workflows {
		fmt.Printf("workflow %-18s id=%-3d %d nodes, %d edges\n", wf.Name, rep.Workflows[wf.Name], len(wf.Nodes), len(wf.Edges))
	}

	// Print tokens sorted so the output is deterministic and copy-pasteable.
	sorted := make([]string, 0, len(rep.Tokens))
	for name := range rep.Tokens {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		fmt.Printf("token %-16s %s\n", name, rep.Tokens[name])
	}
	fmt.Printf("tokens file: %s (keep it private)\n", rep.TokensFile)
}

// fatalf prints "error: msg" to stderr and exits 1.
func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
