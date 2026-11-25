package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// contextDirName is the subdirectory (inside the employee workdir) where the
// agent writes its self-description files (S18). User files such as
// role.md live in the workdir root and are never touched by the agent.
const contextDirName = "whatbark"

// writeContextFiles (re)writes whatbark/identity.md and whatbark/platform.md
// inside the employee workdir. It runs on every initial attach, so files are
// idempotent: overwritten with the same layout, never appended. Errors are
// returned for the caller to log; a failure here never stops the agent loop.
func (a *Agent) writeContextFiles(ctx context.Context, emp *Employee) error {
	workdir := a.resolveWorkdir(emp.Workdir)
	if workdir == "" {
		return nil // no workdir configured: nothing to write
	}
	dir := filepath.Join(workdir, contextDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("agent: mkdir %s: %w", dir, err)
	}

	identity, err := a.identityContent(ctx, emp)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.md"), []byte(identity), 0o644); err != nil {
		return fmt.Errorf("agent: write identity.md: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "platform.md"), []byte(platformDoc), 0o644); err != nil {
		return fmt.Errorf("agent: write platform.md: %w", err)
	}
	return nil
}

// identityContent builds whatbark/identity.md: who this employee is, who they
// report to, and where to find the platform guide and optional role file.
func (a *Agent) identityContent(ctx context.Context, emp *Employee) (string, error) {
	// Up-chain leader ids from /api/routes; names resolved via /api/employees.
	var routes struct {
		Reporting []int `json:"reporting"`
	}
	if err := a.do(ctx, "GET", fmt.Sprintf("/api/routes?employee_id=%d", emp.ID), nil, &routes); err != nil {
		return "", fmt.Errorf("agent: routes: %w", err)
	}
	var emps struct {
		Employees []Employee `json:"employees"`
	}
	if err := a.do(ctx, "GET", "/api/employees", nil, &emps); err != nil {
		return "", fmt.Errorf("agent: employee directory: %w", err)
	}
	names := make(map[int]string, len(emps.Employees))
	for _, e := range emps.Employees {
		names[e.ID] = e.Name
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Employee identity\n\n")
	fmt.Fprintf(&b, "- Name: %s\n", emp.Name)
	fmt.Fprintf(&b, "- Employee ID: %d\n", emp.ID)
	fmt.Fprintf(&b, "- Kind: %s\n", emp.Kind)
	fmt.Fprintf(&b, "- Role: %s\n", emp.Role)
	if len(routes.Reporting) > 0 {
		b.WriteString("- Reports to: ")
		up := make([]string, 0, len(routes.Reporting))
		for _, id := range routes.Reporting {
			if n, ok := names[id]; ok {
				up = append(up, n)
			} else {
				up = append(up, fmt.Sprintf("#%d", id))
			}
		}
		b.WriteString(strings.Join(up, ", ") + "\n")
	} else {
		b.WriteString("- Reports to: (none, top of organization)\n")
	}
	b.WriteString("\n")
	b.WriteString("See whatbark/platform.md for platform usage.\n")
	if _, err := os.Stat(filepath.Join(a.resolveWorkdir(emp.Workdir), "role.md")); err == nil {
		b.WriteString("See ../role.md for your role description.\n")
	}
	return b.String(), nil
}

// bootstrapPrompt prefixes every task body with a fixed short header telling
// the tool who it is and where to read the context files. Details live in the
// files themselves; the header only carries identity + file pointers + the
// output convention (S18).
func (a *Agent) bootstrapPrompt(emp *Employee, body string) string {
	header := fmt.Sprintf(
		"You are an AI employee of the whatbark platform, %s (id %d). Before starting, read whatbark/identity.md and whatbark/platform.md in your working directory (also read role.md if it exists). After finishing the task, end your output with a concise conclusion (it will be used as the task report).\n---\n",
		emp.Name, emp.ID,
	)
	return header + body
}

// toolEnv returns the environment for spawned tool processes: the agent's own
// environment plus BARK_TOKEN / BARK_SERVER, so a tool (e.g. a shell that
// calls the bark CLI) can talk to the platform as this employee. Same-named
// entries in the inherited environment are overridden.
func (a *Agent) toolEnv() []string {
	out := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "BARK_TOKEN=") || strings.HasPrefix(kv, "BARK_SERVER=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "BARK_TOKEN="+a.cfg.Token, "BARK_SERVER="+a.cfg.ServerURL)
}

// platformDoc is the static whatbark/platform.md content (S18).
const platformDoc = `# whatbark platform guide

You are an AI employee of the whatbark platform. The platform is a "mailbox + routing" system:

- Messages: employees communicate via messages (kind: dispatch/ask/answer/report/chat/summary, etc.).
- Routing: you can only send messages to targets allowed by the organization routes (reporting to your leader, subordinates/peers allowed by the same department/workflow); otherwise the server returns 403.
- Tasks: dispatch/ask/scheduled/selfreview messages automatically create tasks for you; tasks have queued/running/done/failed/canceled states.
- State & audit: tasks and messages are all persisted in the server SQLite database and can be inspected in the console.

## bark command cheat-sheet (credentials are injected by the client as env vars BARK_TOKEN / BARK_SERVER, just call them)

- Send a message (creates a task per kind):
  whatbark bark send --to <employee ID> --kind <dispatch|ask|scheduled> --body "..." [--project <project ID>] [--priority <0-99>]
- Cancel a task: whatbark bark cancel <task ID>
- Message history: whatbark bark history --to <employee ID>
- Tasks: whatbark bark tasks [--employee <employee ID>]
- Server status: whatbark bark status
- Decompose a task graph (kind=ai employees only): whatbark bark decompose --data '{"name":"<name>","nodes":[{"employee_id":N}],"edges":[{"from":N,"to":M}],"tasks":[{"employee_id":N,"input":"<instruction>"}]}' (2~32 nodes, all on your dispatch routes; edges define dependency order)

## Output convention

- The tail of a tool's stdout is sent back to the requester/leader as the task report.
- After finishing a task, end your output with a short, explicit conclusion (what was done, the result, whether blocked).

## Priority

- Task priority range is 0..99; higher is more urgent.
- The platform picks tasks by priority, highest first; ties go by creation order (smaller id first).
- When a higher-priority task arrives, the current task is preempted (re-queued, keeping its original priority).
`
