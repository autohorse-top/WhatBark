# whatbark

Web-based AI team work-communication platform v1. The server acts as the "track" (inbox, routing arbitration, execution state, scheduling, audit); the employee-side agent runs locally, driving opencode / claudecode / custom tools to execute tasks. Human employees join with `kind=human`, reply via the web inbox, and manually complete/reject tasks.

Single Go binary with three subcommands:

| Subcommand | Role | Description |
| --- | --- | --- |
| `serve` | Server | Web UI + REST + WS + SQLite + scheduler + routing arbitration + token issuance/auth |
| `agent` | Employee | Token attach → poll for tasks → spawn local tool process → report status/output → heartbeat |
| `bark` | CLI | For AI/human employees: send / cancel / history / tasks / status / decompose |

- Database: SQLite (modernc.org/sqlite, pure Go, no CGO, WAL + single-writer connection pool)
- Frontend: vanilla JS + SVG canvas, embedded via `go:embed` into the binary — **no build step**
- Cancellation semantics: SIGTERM process group → 5s grace → SIGKILL (kills entire process tree)

## Repository Layout

```
whatbark/
  main.go serve.go agent.go bark.go   # subcommand dispatch
  internal/store   SQLite schema + typed accessors (fully parameterized)
  internal/org     Role directory + reporting/dispatch chain computation (pure functions)
  internal/routes  Routing arbitration Allowed / CancelAllowed (pure functions)
  internal/flow    Message/task semantics (dispatch/ask → task; done → auto report/answer/summary)
  internal/api     HTTP handlers + token auth + WS event bus
  internal/sched   Second-level tick scheduler (periodic tasks + periodic self-review injection)
  internal/tool    Tool adapters (mock/opencode/claudecode/custom command)
  web/             SPA with seven sections (Org | Workflows | Projects | Status | Inbox | Servers | Schedules)
  scripts/smoke.sh End-to-end acceptance (A1–A15, machine-verified)
```

## Quick Start (3 Steps)

```bash
# 1. Build
make build                      # → bin/whatbark

# 2. Start the server (first run generates admin token: printed once in logs, stored in settings table)
./bin/whatbark serve -listen 127.0.0.1:8080 -data whatbark.db

# 3. Open http://127.0.0.1:8080 in browser
#    Add this server in the "Servers" section (enter admin token) → create departments/employees/workflows
#    Attach an employee agent (use the token returned once at creation):
./bin/whatbark agent -server http://127.0.0.1:8080 -token "$EMP_TOKEN"
```

bark CLI (employee / boss side):

```bash
export BARK_TOKEN=...   # or pass -token each time
./bin/whatbark bark -server http://127.0.0.1:8080 send --to 3 --kind dispatch --body "Fix this"
./bin/whatbark bark cancel <task-id> --reason "went off track"
./bin/whatbark bark tasks --status running
./bin/whatbark bark history 3 --limit 20
```

## Organization Presets (preset)

The binary ships with 4 sample org structures (pure JSON, located in `internal/preset/presets/`). One command imports them into the database, auto-creating departments (with S31 channels), employees, department leaders, the leadership collaboration workflow, and issuing tokens:

```bash
./bin/whatbark preset list                                   # list built-in presets
./bin/whatbark preset import software -data whatbark.db      # import a built-in preset
./bin/whatbark preset import -file custom.json -data whatbark.db   # import a custom structure
```

Built-in presets:

| preset | Size | Description |
| --- | --- | --- |
| `software` | 5 depts / 16 people | Sample software company (Design / Finance / Software Development / Web & App Development / Customer Service), matches the demo seed |
| `finance` | 5 depts / 14 people | Securities/asset-management firm: Investment Research, Wealth Management, Risk & Compliance, Finance & Accounting, Operations Support |
| `law` | 5 depts / 14 people | Law firm practice structure: Corporate / Criminal / Civil & Commercial / IP & Tech + Administration Office |
| `insurance` | 5 depts / 14 people | Insurance brokerage: Life & Health, Property, Underwriting & Reinsurance, Claims, CS & Operations |

Custom preset file format (schema `whatbark-preset/v1`; missing schema defaults to v1):

```json
{
  "schema": "whatbark-preset/v1",
  "name": "Sample Org",
  "departments": [
    {"name": "HQ", "leader": "boss", "employees": [{"name": "boss", "role": "CEO"}]},
    {"name": "Branch", "parent": "HQ", "leader": "lead-a", "employees": [
      {"name": "lead-a", "role": "Lead"},
      {"name": "dev-b", "role": "Developer", "kind": "ai"}
    ]}
  ]
}
```

Constraints:

- `departments` is a flat array; `parent` may only reference a department declared **earlier** (import order = topological order; cycles not allowed)
- Employee names are globally unique, must match `[A-Za-z0-9_-]{1,64}`; `role` is free text; `kind` is `ai`/`human` (default `ai`)
- `leader` must be within the same department's `employees`
- When importing into a non-empty DB, top-level department name or employee name conflicts cause immediate failure with the conflicting item identified (no rollback; use a fresh DB for clean retries)
- Tokens: each employee without a token gets a 64-hex token; plaintext written once to `<db filename minus .db>-tokens.json` (mode 0600, **do not commit to git**); re-importing does not modify existing tokens

Optional `workflows` field (v1-compatible; each preset may declare collaboration graphs imported after the org itself):

```json
"workflows": [
  {
    "name": "Leadership Collab",
    "nodes": ["designer-alex", "finance-david", "dev-grace", "webapp-kate", "cs-nathan"],
    "edges": [["designer-alex", "finance-david"], ["finance-david", "designer-alex"], "…"]
  }
]
```

- `nodes` reference preset-wide employee names (built-in presets declare one "Leadership Collab" graph per org: the 5 department leaders wired K5-complete — every leader can dispatch-workflow every other leader)
- `edges` are directed `[from, to]` pairs; nodes are placed on the workflow canvas as a horizontal row
- Workflow names are unique **per database** (like employee names): importing a second organization into the same DB that reuses a workflow name fails up front with zero partial data. Unknown node names / duplicate nodes / self-loop / duplicate edges are rejected with a locatable error before anything is written
- A preset without `workflows` (or with an empty array) imports exactly like before

## Docker Deployment (2 Steps)

```bash
# 1. Build image (multi-stage: golang static build → alpine runtime, serve only)
make docker                 # = docker build -t whatbark:latest .

# 2. Run (data in volume: SQLite + WAL in /data; host 18080 → container 8080)
make docker-run             # = docker run --rm -p 18080:8080 -v whatbark-data:/data whatbark:latest
```

- Health check: `docker exec <c> wget -qO- http://127.0.0.1:8080/healthz` (HEALTHCHECK built into image)
- Data persistence: docker named volume `whatbark-data` (mounted to container `/data`: whatbark.db / -wal / -shm); container recreation preserves data. For bind-mount to host directory, the host directory must be writable by the container user (uid 10001), otherwise SQLite will fail to open
- Public deployment: the application does **no TLS termination**; you must terminate TLS via Caddy/Nginx reverse proxy before forwarding to port 8080. Run as a dedicated OS user, keep DB files at 0600

## Employee Tool Configuration

Employee `tool` field values:

| tool | Behavior |
| --- | --- |
| `mock` | Built-in deterministic tool (3-line output, `agent -mock-sleep` controls duration); test default |
| `opencode` | Fixed argv `opencode run {prompt}`; requires opencode in employee PATH |
| `claudecode` | Fixed argv `claude -p {prompt}`; requires API key configured on employee side |
| Custom command | `tool` = program name/absolute path, `tool_args` provides argv template |

`tool_args` is a **JSON string** (not a JSON object): `{"argv":[...]}`, where the `{prompt}` placeholder
is substituted as a **single argv element** (no shell expansion, injection-safe). If the template has no `{prompt}`, it is appended at the end.
Note: `opencode`/`claudecode` are built-in names with fixed argv; `tool_args` is ignored.
To add arguments to opencode, set `tool` to the executable file path (uses the custom command branch).

```bash
# Built-in
curl -X PUT http://127.0.0.1:8080/api/employees/5/tool \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"tool":"opencode","tool_args":""}'

# Custom command + argv template
curl -X PUT http://127.0.0.1:8080/api/employees/5/tool \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"tool":"/usr/local/bin/llm-run","tool_args":"{\"argv\":[\"--model\",\"fast\",\"{prompt}\"]}"}'
```

> The agent reads tool config once at attach time. **After changing a tool, the agent process must be restarted** for the change to take effect.
> Missing tool binary → task marked failed, output records a human-readable reason (does not block other tasks).

## Employee Agent Auto-Start via launchd (macOS Example)

`~/Library/LaunchAgents/com.whatbark.agent.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
  <key>Label</key><string>com.whatbark.agent</string>
  <key>ProgramArguments</key>
  <array>
    <string>/path/to/whatbark</string><string>agent</string>
    <string>-server</string><string>http://127.0.0.1:8080</string>
    <string>-token</string><string>EMP_TOKEN_HERE</string>
    <string>-base-dir</string><string>~/whatbark-work</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict><key>PATH</key><string>/usr/local/bin:/usr/bin:/bin</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/tmp/whatbark-agent.out.log</string>
  <key>StandardErrorPath</key><string>/tmp/whatbark-agent.err.log</string>
</dict>
</plist>
```

Load: `launchctl load ~/Library/LaunchAgents/com.whatbark.agent.plist`
(token can also be set via environment variable `BARK_TOKEN`, injected via `EnvironmentVariables` in the plist).

## Context Files & Task Preemption (S18)

- One agent per employee: one `kind=ai` employee should map to one agent process; after agent crash/restart and re-attach,
  the server automatically requeues that employee's orphaned running tasks (no double-execution, no loss).
- Context files: on attach, the agent generates `whatbark/identity.md` (employee identity: name/role/reporting line/available tools) and
  `whatbark/platform.md` (platform quick reference + bark CLI usage) under the employee workdir (`-base-dir`). If a user-written
  `role.md` exists in the same directory, its content is merged into the task prompt (identity files are agent-generated, freely editable).
- Task priority: `bark send` / `POST /api/messages` accept `priority` (default 0);
  when a strictly higher-priority task appears in the queue, the running task is preempted (requeue + kill entire
  process tree: SIGTERM → 5s → SIGKILL). The higher-priority task runs first; the preempted task runs afterward.

## Testing & Acceptance

```bash
make build   # G1 compile
make vet     # G2 go vet
make test    # G3 go test ./... -race
make smoke   # A1–A15 end-to-end (real serve + agent + bark + REST, exit 0 = all pass)
make docker  # A12 image; after docker run, /healthz returns 200
```

## Security Baseline & Known Gaps (v1 Explicit)

- All APIs use Bearer token (256-bit random); all SQL parameterized; server never executes user input
  (task content is data only; tool processes run only on the employee's local machine); employee tokens are
  route-constrained (cross-level → 403); default bind 127.0.0.1; secrets/tokens/.env never in git.
- Rate limiting: /api/ chain uses token-bucket (key = token hash; no token → remote IP),
  burst=500, rate=100/s, excess → 429 + Retry-After; in-memory, resets on restart (v1 single-node positioning).
- v1 known gaps: no IP allowlist/mTLS. (Audit log has been added via H-7: audit_log table +
  adminOnly `GET /api/audit`; admin auth migrated to V-2 `admin_tokens` table with SHA-256
  storage, delete-takes-effect-immediately; boot token still stored in plaintext in the settings table
  for startup re-seeding — known trade-off.)
- P0-1 Security Hardening (completed 2026-11-05, tag `task/whatbark-p01-done`):
  - WS auth token moved to `Sec-WebSocket-Protocol` subprotocol at handshake (browser WebSocket API
    cannot set custom request headers); no credentials → 401, nothing lands in hub before handshake.
  - File/avatar URLs changed to server-issued short-lived HMAC signatures (`?exp=&sig=`, TTL 1h, 24h max,
    cross-path replay/tamper/expiry → 401; key is per-process random, old signed URLs invalidated on restart);
    frontend SPA refreshes signed URLs every 45min and on WS reconnect by re-fetching `/api/me`, `/api/employees`.
  - `?token=` query retained only as legacy fallback for existing clients; new clients no longer put
    full tokens in URL/access logs.
- Security & Observability Hardening Batch (2026-11-05, addressing P0-1 review F6–F11 and P1a/P1b):
  - F9 Unified filename character set: uploaded display/storage names retain only `[A-Za-z0-9._-]` + Greek U+0300–036F
    + CJK U+4E00–9FFF; all other characters → `_`, ≤128 runes; `store.StoredNameRe` is the single source of truth
    (shared by upload generation, `/api/files/{name}` validation, startup migration); at startup
    `migrateLegacyStoredNames` rewrites invalid `stored_name` in old DBs to `<16-hex>.<ext>`
    (disk files renamed in sync, DB rows without files are tolerated, idempotent); frontend `filesHTML`
    applies `WB.esc` to URLs before embedding in HTML attributes, preventing URL structure breakage/DOM injection.
  - F10 Auth failure audit: 401s on `/api/files/{name}`, `/api/avatars/{name}`, `/ws` logged as
    `auth.fail` (actor=anon). Note: these routes' 401s are written to audit_log per request, and these three routes
    are outside the per-token rate-limit chain (design trade-off, see TASK.md §3.4); for external exposure
    (non-127.0.0.1 or high-threat environments), recommend front-end gateway rate limiting.
  - F11 Legacy `?token=` lockdown: set `WHATBARK_DISABLE_LEGACY_QUERY_TOKEN=1` to disable
    query fallback on `/ws` and file/avatar routes (default off, preserves legacy client compatibility).
  - F7 Signed URL TTL decoupled from 24h cap: `effectiveFileURLTTL` clamps TTLs >24h to
    24h and warns, preventing issuance of already-expired URLs.
  - P1a Agent LLM transient failure retry: when a tool process exits with a transient-failure marker
    (529 / overloaded_error / api error / rate limit / connection reset / ECONNRESET / ETIMEDOUT / socket
    hang up / temporarily unavailable / part of the response never arrived; scans last 4KB + error string,
    case-insensitive), exponential backoff retry in place (default 2 retries, initial 2s, cap 30s);
    cancel/preempt/shutdown can interrupt during backoff; start failures and deterministic failures are not retried.
  - P1b `/metrics`: unauthenticated Prometheus text snapshot (uptime / goroutines / agents_online /
    employees_total / files_total / tasks_total{status}), default bind 127.0.0.1, exposes only
    coarse-grained counters.

## v2 Roadmap (concluded 2026-11-05, tag task/whatbark-v2-roadmap-done)

- Persistent sessions (V-1): employee tool sessions reused across tasks, auto-reclaimed on idle/consecutive failures; CLI tools remain one-shot.
- Multi-user/permissions (V-2): admin_tokens multi-admin tokens (SHA-256 storage, live table lookup so delete-instant-invalidate, audit records specific admin id); full RBAC deferred to v2.1.
- Leader AI auto-decomposition of task graphs (V-3): `POST /api/decompose` + `bark decompose`; lead agent produces workflow + initial tasks (single-transaction atomic graph creation; nodes must be dispatch-reachable).
- Leader orchestration — review wakes: when a dispatch reaches done/failed and its requester is an AI employee, the system injects a `kind=review` task (priority 0, requested_by 0, body carries the subordinate's task id/status/output tail plus the ≤2-re-dispatches-then-escalate guidance) into the requester's queue; throttled per requester via the `review_max_per_hour` setting (default 30, 0 disables). Presets ship leadership collaboration graphs so cross-department leader dispatch works out of the box.
- Email/IM notifications (V-4): SMTP (settings-configured, credentials overridable via env `WHATBARK_SMTP_USER/PASS`) + group webhook; async notification on task terminal state / mention; silent skip when unconfigured.
- Multi-server federation (V-5): design finalized (directory + ownership distribution, task ownership transfer, outbox message replication, single-writer conflict policy; see task notes for details); implementation pending product-level decision.
