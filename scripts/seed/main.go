// Command seed imports the built-in "software" org preset into a demo
// database: departments + channels, employees, leaders, one bearer token
// per employee (plaintext in <dbPath>-tokens.json), then verifies the
// reporting chain. Thin wrapper over internal/preset so the CLI and this
// demo tool share a single import path.
//
// Usage: go run ./scripts/seed [dbPath]   (default: demo.db)
//
// Idempotency: if the database already has departments, the import is
// skipped; tokenization still ensures every employee has a token, and the
// verification always runs against the "software" preset structure.
package main

import (
	"fmt"
	"log"
	"os"

	"whatbark/internal/preset"
	"whatbark/internal/store"
)

func main() {
	dbPath := "demo.db"
	if len(os.Args) > 1 {
		dbPath = os.Args[1]
	}

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open %s: %v", dbPath, err)
	}
	defer func() { _ = st.Close() }()

	data, ok := preset.Get("software")
	if !ok {
		log.Fatalf("embedded preset %q not found", "software")
	}
	p, err := preset.Parse(data)
	if err != nil {
		log.Fatalf("parse preset: %v", err)
	}

	existing, err := st.ListDepartments()
	if err != nil {
		log.Fatalf("list departments: %v", err)
	}
	if len(existing) > 0 {
		fmt.Printf("%s already has %d departments — skipping import\n", dbPath, len(existing))
	} else {
		rep, err := preset.Import(st, p, dbPath)
		if err != nil {
			log.Fatalf("import %s: %v", p.Name, err)
		}
		for _, d := range rep.Departments {
			fmt.Printf("dept %-14s id=%d leader=%s (%d employees)\n", d.Name, d.ID, d.Leader, d.Count)
		}
		fmt.Printf("imported %d departments, %d employees\n", len(rep.Departments), rep.Employees)
	}

	if file, err := preset.EnsureTokens(st, dbPath); err != nil {
		log.Fatalf("tokens: %v", err)
	} else {
		fmt.Printf("tokens file: %s\n", file)
	}
	if err := preset.Verify(st, p); err != nil {
		log.Fatalf("verify: %v", err)
	}
	total := 0
	for _, d := range p.Departments {
		total += len(d.Employees)
	}
	fmt.Printf("verify OK: %d departments (all with channel+leader), %d employees, chains correct\n", len(p.Departments), total)
}
