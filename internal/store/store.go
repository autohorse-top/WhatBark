// Package store is the SQLite persistence layer for whatbark.
//
// All queries are parameterized; user input is never concatenated into SQL.
// The pure-Go modernc.org/sqlite driver keeps the build CGO-free so the same
// binary runs locally and in the Docker image.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Sentinel errors returned by the accessors.
var (
	// ErrNotFound is returned when a row does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrInUse is returned when a row cannot be removed because it is still referenced.
	ErrInUse = errors.New("store: still in use")
	// ErrInvalidTransition is returned when a task state change is not allowed.
	ErrInvalidTransition = errors.New("store: invalid task transition")
)

// Store wraps *sql.DB with typed accessors. It is safe for concurrent use;
// the connection pool is capped to one writer to keep SQLite happy.
type Store struct {
	db *sql.DB
	// FilesDir is the on-disk directory for user-uploaded binaries
	// (avatars under avatars/, message attachments under files/).
	// It sits next to the database file: <dir(path)>/whatbark-files.
	FilesDir string
}

// schema is applied idempotently on Open.
const schema = `
CREATE TABLE IF NOT EXISTS departments (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	parent_id INTEGER REFERENCES departments(id) ON DELETE SET NULL,
	leader_id INTEGER REFERENCES employees(id) ON DELETE SET NULL,
	-- S31: back-ref to the department's own channel (groups.kind='channel').
	-- Forward FK to groups is legal in SQLite (resolved at DML time); SET NULL
	-- so deleting a channel row never blocks department deletes.
	group_id INTEGER REFERENCES groups(id) ON DELETE SET NULL,
	UNIQUE (parent_id, name)
);
-- SQLite treats NULLs as distinct in UNIQUE constraints, so the constraint
-- above cannot catch duplicate top-level names; this expression index does.
CREATE UNIQUE INDEX IF NOT EXISTS idx_departments_parent_name
	ON departments (COALESCE(parent_id, -1), name);

CREATE TABLE IF NOT EXISTS employees (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	role TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT 'ai' CHECK (kind IN ('ai', 'human')),
	department_id INTEGER NOT NULL REFERENCES departments(id),
	tool TEXT NOT NULL DEFAULT 'mock',
	tool_args TEXT NOT NULL DEFAULT '{}',
	workdir TEXT NOT NULL DEFAULT '',
	token_hash TEXT NOT NULL DEFAULT '',
	last_seen_at TEXT,
	avatar TEXT NOT NULL DEFAULT '',
	email TEXT,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS workflows (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS wflow_nodes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	workflow_id INTEGER NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
	employee_id INTEGER NOT NULL REFERENCES employees(id),
	x REAL NOT NULL DEFAULT 0,
	y REAL NOT NULL DEFAULT 0,
	UNIQUE (workflow_id, employee_id)
);

CREATE TABLE IF NOT EXISTS wflow_edges (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	workflow_id INTEGER NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
	from_node_id INTEGER NOT NULL REFERENCES wflow_nodes(id) ON DELETE CASCADE,
	to_node_id INTEGER NOT NULL REFERENCES wflow_nodes(id) ON DELETE CASCADE,
	UNIQUE (from_node_id, to_node_id)
);
CREATE INDEX IF NOT EXISTS idx_wflow_edges_workflow ON wflow_edges(workflow_id);

CREATE TABLE IF NOT EXISTS projects (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	brief TEXT NOT NULL DEFAULT '',
	workflow_id INTEGER REFERENCES workflows(id) ON DELETE SET NULL,
	entry_employee_id INTEGER REFERENCES employees(id) ON DELETE SET NULL,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS tasks (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
	employee_id INTEGER NOT NULL REFERENCES employees(id),
	workflow_id INTEGER REFERENCES workflows(id) ON DELETE SET NULL,
	group_id INTEGER REFERENCES groups(id) ON DELETE SET NULL,
	requested_by INTEGER NOT NULL DEFAULT 0,
	kind TEXT NOT NULL DEFAULT 'dispatch'
		CHECK (kind IN ('dispatch', 'ask', 'scheduled', 'selfreview', 'review')),
	status TEXT NOT NULL DEFAULT 'queued'
		CHECK (status IN ('queued', 'running', 'done', 'failed', 'canceled')),
	priority INTEGER NOT NULL DEFAULT 0,
	input TEXT NOT NULL DEFAULT '',
	output TEXT NOT NULL DEFAULT '',
	started_at TEXT,
	finished_at TEXT,
	sla_reminded_at TEXT,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_tasks_employee_status ON tasks(employee_id, status);
CREATE INDEX IF NOT EXISTS idx_tasks_project ON tasks(project_id);

CREATE TABLE IF NOT EXISTS schedules (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	employee_id INTEGER NOT NULL REFERENCES employees(id),
	every_seconds INTEGER NOT NULL CHECK (every_seconds > 0),
	prompt TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	last_fired_at TEXT
);

CREATE TABLE IF NOT EXISTS groups (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	created_by INTEGER NOT NULL,
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	kind TEXT NOT NULL DEFAULT 'group',
	default_priority INTEGER NOT NULL DEFAULT 0,
	sla_timeout_seconds INTEGER NOT NULL DEFAULT 0,
	webhook_url TEXT
);

CREATE TABLE IF NOT EXISTS group_members (
	group_id INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
	employee_id INTEGER NOT NULL,
	PRIMARY KEY (group_id, employee_id)
);

-- S17: per-viewer group prefs (mute / ignore). viewer_id 0 = boss (same
-- convention as conv_reads: no FK, 0 has no employees row).
CREATE TABLE IF NOT EXISTS group_prefs (
	group_id INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
	viewer_id INTEGER NOT NULL,
	muted INTEGER NOT NULL DEFAULT 0,
	ignored INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (group_id, viewer_id)
);

CREATE TABLE IF NOT EXISTS messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	from_id INTEGER REFERENCES employees(id),
	to_id INTEGER REFERENCES employees(id),
	kind TEXT NOT NULL CHECK (kind IN ('report', 'dispatch', 'ask', 'answer', 'chat', 'summary', 'system', 'scheduled', 'selfreview', 'review')),
	body TEXT NOT NULL,
	project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
	task_id INTEGER REFERENCES tasks(id) ON DELETE SET NULL,
	group_id INTEGER REFERENCES groups(id) ON DELETE CASCADE,
	-- S16: reply reference (id of the replied-to message; no FK — dangling
	-- references are tolerated and rendered by the client as the original
	-- message).
	reply_to INTEGER,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_messages_to ON messages(to_id, created_at);
CREATE INDEX IF NOT EXISTS idx_messages_project ON messages(project_id);
CREATE INDEX IF NOT EXISTS idx_messages_group ON messages(group_id);

CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

-- Uploaded binaries (S12): message attachments under <FilesDir>/files/
-- (avatars live under <FilesDir>/avatars/ and are indexed by
-- employees.avatar / settings.boss_avatar instead). stored_name is a
-- unique, unguessable on-disk name; the original display name is kept in
-- the name column. owner_id has no FK (like conv_reads): 0 = boss, who
-- has no employees row; cleanup of an employee's files is done by the API
-- layer on employee deletion.
CREATE TABLE IF NOT EXISTS files (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	owner_id INTEGER NOT NULL,
	name TEXT NOT NULL,
	stored_name TEXT NOT NULL UNIQUE,
	mime TEXT NOT NULL,
	size INTEGER NOT NULL,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_files_owner ON files(owner_id);

-- task artifacts (task-duration-artifacts B): files an agent uploaded after a
-- successful task tool run. Bytes live under <FilesDir>/artifacts/<stored_name>;
-- this row is the index. ON DELETE CASCADE keeps the table consistent when a
-- task row is removed.
CREATE TABLE IF NOT EXISTS task_artifacts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	stored_name TEXT NOT NULL UNIQUE,
	mime TEXT NOT NULL,
	size INTEGER NOT NULL,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_task_artifacts_task ON task_artifacts(task_id);

-- message <-> file association (S12); a message may carry several files.
CREATE TABLE IF NOT EXISTS message_files (
	message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
	PRIMARY KEY (message_id, file_id)
);
CREATE INDEX IF NOT EXISTS idx_message_files_file ON message_files(file_id);

-- S20: @mention — mention targets extracted from message bodies.
-- employee_id 0 = boss (no employees row; the boss is a legal mention target
-- via the literal token "boss", see MentionIDsIn). Rows cascade with the
-- message; the composite key keeps AddMessageMentions idempotent.
CREATE TABLE IF NOT EXISTS message_mentions (
	message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	employee_id INTEGER NOT NULL,
	PRIMARY KEY (message_id, employee_id)
);
-- S36: mention-history search (SearchMentions) filters by employee_id, then
-- joins messages by message_id; a (employee_id, message_id) index serves it.
CREATE INDEX IF NOT EXISTS idx_message_mentions_emp ON message_mentions(employee_id, message_id);

-- Per-conversation read cursors for the chat view (S8).
-- viewer_id 0 = boss/admin; peer_id 0 = boss/system (messages with from_id NULL).
-- No FK constraints: 0 is not an employees row. Stale rows for deleted
-- employees are harmless (ListConversations only reports live peers).
CREATE TABLE IF NOT EXISTS conv_reads (
	viewer_id INTEGER NOT NULL,
	peer_id INTEGER NOT NULL,
	last_read_msg_id INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL DEFAULT (datetime('now')),
	PRIMARY KEY (viewer_id, peer_id)
);

-- S33: per-viewer conversation pins (pin to top of the conversation list).
-- peer_id follows the
-- conv_reads convention: >= 0 is 1:1 (0 = boss side), negative = group
-- (-(group_id+1)). No FK (viewer 0 = boss has no employees row; a peer
-- without a conversation yet yields an invisible row until messages exist).
CREATE TABLE IF NOT EXISTS conv_pins (
	viewer_id INTEGER NOT NULL,
	peer_id INTEGER NOT NULL,
	pinned_at TEXT NOT NULL DEFAULT (datetime('now')),
	PRIMARY KEY (viewer_id, peer_id)
);

-- H-7: append-only audit log (admin mutations, task cancel/reassign/preempt,
-- auth failures). Rows are never updated or deleted by the app; id order is
-- chronological, so ORDER BY id DESC = newest first.
CREATE TABLE IF NOT EXISTS audit_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts TEXT NOT NULL DEFAULT (datetime('now')),
	actor_kind TEXT NOT NULL,
	actor_id INTEGER NOT NULL DEFAULT 0,
	action TEXT NOT NULL,
	target TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT ''
);

-- V-2: named admin tokens (multi-admin). Only the sha256 digest is stored;
-- the plaintext is returned exactly once at creation. The boot token
-- (serve --admin-token / settings admin_token) is seeded here at startup
-- (name "boss"), so every admin authenticates through this single table.
CREATE TABLE IF NOT EXISTS admin_tokens (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	token_hash TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
`

// Open opens (or creates) the SQLite database at path, enables WAL, busy
// timeout and foreign keys, and applies the schema.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	// Migrations run before the base schema: the S14 messages rebuild must
	// finish before CREATE INDEX ... messages(group_id) can succeed on old
	// databases. Each step is idempotent.
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate %s: %w", path, err)
	}
	// Uploaded binaries live next to the DB file; create the layout eagerly
	// so upload handlers fail fast on unwritable filesystems.
	filesDir := filepath.Join(filepath.Dir(path), "whatbark-files")
	for _, sub := range []string{"avatars", "files"} {
		if err := os.MkdirAll(filepath.Join(filesDir, sub), 0o755); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: prepare files dir %s: %w", sub, err)
		}
	}
	// F9: pre-F9 stored names may contain URL/HTML-hostile bytes (space, '?',
	// '&', ...); rewrite them to the safe charset once, at startup.
	if err := migrateLegacyStoredNames(db, filesDir); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate legacy stored names %s: %w", path, err)
	}
	return &Store{db: db, FilesDir: filesDir}, nil
}

// migrate applies additive column upgrades for databases created before the
// corresponding schema version. Each step is idempotent (the base schema
// already carries the column for fresh databases). It runs before the base
// schema so table rebuilds complete before new indexes are created.
func migrate(db *sql.DB) error {
	// S12: employees.avatar was added after v1.0; pre-S12 DBs need it.
	if tableExists(db, "employees") {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('employees') WHERE name = 'avatar'`).Scan(&n); err != nil {
			return fmt.Errorf("store: inspect employees columns: %w", err)
		}
		if n == 0 {
			if _, err := db.Exec(
				`ALTER TABLE employees ADD COLUMN avatar TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("store: add employees.avatar column: %w", err)
			}
		}
	}
	// S14: messages.to_id became nullable and messages gained group_id;
	// pre-S14 databases need a table rebuild (SQLite cannot relax a
	// NOT NULL constraint in place).
	if err := migrateMessagesGroup(db); err != nil {
		return err
	}
	// S16: messages.reply_to was added after S14; post-S14 databases (like
	// the current demo.db) need the idempotent column add.
	if err := migrateMessagesReplyTo(db); err != nil {
		return err
	}
	// S18: tasks.priority was added for agent preemption; pre-S18 databases
	// need the idempotent column add.
	if err := migrateTasksPriority(db); err != nil {
		return err
	}
	// S21: groups.kind (S21 project channels). Must run after the S14
	// rebuild: databases rebuilt from pre-S14 layouts get a groups table
	// without the column, exactly like post-S14 databases.
	if err := migrateGroupsKind(db); err != nil {
		return err
	}
	// S22: tasks.group_id (group task dispatch); post-S14/S17 databases
	// need the idempotent column add.
	if err := migrateTasksGroup(db); err != nil {
		return err
	}
	// S30: group-level SLA/priority policy — groups.default_priority,
	// groups.sla_timeout_seconds and tasks.sla_reminded_at.
	if err := migrateGroupsPolicy(db); err != nil {
		return err
	}
	// S31: departments.group_id (back-ref to the department's own channel);
	// post-S30 databases need the idempotent column add.
	if err := migrateDepartmentsGroup(db); err != nil {
		return err
	}
	// V-4: employees.email (nullable) + groups.webhook_url (nullable).
	if err := migrateV4(db); err != nil {
		return err
	}
	// Leader orchestration (S1): the kind CHECK on both tasks and messages
	// gains 'review'. Old databases need a table rebuild (SQLite cannot
	// relax a CHECK in place). Runs last: the full-column INSERT..SELECT
	// copy requires every column-adding migration above (tasks.priority,
	// tasks.group_id, tasks.sla_reminded_at, messages.reply_to, ...) to have
	// already transformed legacy tables into the current 15/10-column shape.
	// Fresh databases skip via the tableExists guards below and get the new
	// CHECK directly from the base schema.
	return migrateTasksKindReview(db)
}

// migrateV4 adds the V-4 notification columns: employees.email (nullable,
// NULL = no mailbox) and groups.webhook_url (nullable, NULL = no webhook).
// Idempotent: fresh databases already carry both columns in the base schema.
func migrateV4(db *sql.DB) error {
	if tableExists(db, "employees") {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('employees') WHERE name = 'email'`).Scan(&n); err != nil {
			return fmt.Errorf("store: inspect employees columns: %w", err)
		}
		if n == 0 {
			if _, err := db.Exec(`ALTER TABLE employees ADD COLUMN email TEXT`); err != nil {
				return fmt.Errorf("store: add employees.email column: %w", err)
			}
		}
	}
	if tableExists(db, "groups") {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('groups') WHERE name = 'webhook_url'`).Scan(&n); err != nil {
			return fmt.Errorf("store: inspect groups columns: %w", err)
		}
		if n == 0 {
			if _, err := db.Exec(`ALTER TABLE groups ADD COLUMN webhook_url TEXT`); err != nil {
				return fmt.Errorf("store: add groups.webhook_url column: %w", err)
			}
		}
	}
	return nil
}

// migrateDepartmentsGroup adds the S31 departments.group_id column to
// databases created before S31. Idempotent: fresh databases already carry
// the column in the base schema.
func migrateDepartmentsGroup(db *sql.DB) error {
	if !tableExists(db, "departments") {
		return nil
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('departments') WHERE name = 'group_id'`).Scan(&n); err != nil {
		return fmt.Errorf("store: inspect departments columns: %w", err)
	}
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE departments ADD COLUMN group_id INTEGER`); err != nil {
			return fmt.Errorf("store: add departments.group_id column: %w", err)
		}
	}
	return nil
}

func tableExists(db *sql.DB, name string) bool {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		return true // fail loudly in the migration step rather than skip it
	}
	return n > 0
}

// migrateMessagesGroup rebuilds the messages table on databases created
// before S14: to_id becomes nullable and group_id is added alongside the new
// groups / group_members tables. Fresh and already-migrated databases are
// left untouched; the base schema recreates the message indexes afterwards.
func migrateMessagesGroup(db *sql.DB) error {
	var hasTable int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'messages'`).Scan(&hasTable); err != nil {
		return fmt.Errorf("store: inspect messages table: %w", err)
	}
	if hasTable == 0 {
		return nil // fresh database: the base schema creates messages with group_id
	}
	var hasGroup int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'group_id'`).Scan(&hasGroup); err != nil {
		return fmt.Errorf("store: inspect messages columns: %w", err)
	}
	if hasGroup > 0 {
		return nil // already migrated
	}

	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("store: rebuild messages: disable foreign keys: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: rebuild messages: %w", err)
	}
	stmts := []string{
		`CREATE TABLE groups (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			created_by INTEGER NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE group_members (
			group_id INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
			employee_id INTEGER NOT NULL,
			PRIMARY KEY (group_id, employee_id)
		)`,
		`CREATE TABLE messages_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			from_id INTEGER REFERENCES employees(id),
			to_id INTEGER REFERENCES employees(id),
			kind TEXT NOT NULL CHECK (kind IN ('report', 'dispatch', 'ask', 'answer', 'chat', 'summary', 'system', 'scheduled', 'selfreview', 'review')),
			body TEXT NOT NULL,
			project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
			task_id INTEGER REFERENCES tasks(id) ON DELETE SET NULL,
			group_id INTEGER REFERENCES groups(id) ON DELETE CASCADE,
			reply_to INTEGER,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`INSERT INTO messages_new (id, from_id, to_id, kind, body, project_id, task_id, created_at)
			SELECT id, from_id, to_id, kind, body, project_id, task_id, created_at FROM messages`,
		`DROP TABLE messages`,
		`ALTER TABLE messages_new RENAME TO messages`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: rebuild messages: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: rebuild messages: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		return fmt.Errorf("store: rebuild messages: re-enable foreign keys: %w", err)
	}
	return nil
}

// migrateMessagesReplyTo adds the S16 reply_to column to databases created
// before S16 (post-S14 layouts have group_id but no reply_to, like the
// current demo.db). Idempotent: the base schema already carries the column
// for fresh databases, and it runs after the S14 rebuild so rebuilt tables
// get it too.
func migrateMessagesReplyTo(db *sql.DB) error {
	if !tableExists(db, "messages") {
		return nil // fresh database: the base schema creates messages with reply_to
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'reply_to'`).Scan(&n); err != nil {
		return fmt.Errorf("store: inspect messages columns: %w", err)
	}
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN reply_to INTEGER`); err != nil {
			return fmt.Errorf("store: add messages.reply_to column: %w", err)
		}
	}
	return nil
}

// migrateTasksPriority adds the S18 tasks.priority column to databases
// created before S18. Idempotent: the base schema already carries the column
// for fresh databases.
func migrateTasksPriority(db *sql.DB) error {
	if !tableExists(db, "tasks") {
		return nil // fresh database: the base schema creates tasks with priority
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name = 'priority'`).Scan(&n); err != nil {
		return fmt.Errorf("store: inspect tasks columns: %w", err)
	}
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN priority INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("store: add tasks.priority column: %w", err)
		}
	}
	return nil
}

// migrateGroupsKind adds the S21 groups.kind column ('group'|'channel') to
// databases created before S21. Idempotent: the base schema already carries
// the column for fresh databases. Runs after migrateMessagesGroup so that
// groups tables rebuilt by the S14 migration get the column too.
func migrateGroupsKind(db *sql.DB) error {
	if !tableExists(db, "groups") {
		return nil // fresh database: the base schema creates groups with kind
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('groups') WHERE name = 'kind'`).Scan(&n); err != nil {
		return fmt.Errorf("store: inspect groups columns: %w", err)
	}
	if n == 0 {
		if _, err := db.Exec(
			`ALTER TABLE groups ADD COLUMN kind TEXT NOT NULL DEFAULT 'group'`); err != nil {
			return fmt.Errorf("store: add groups.kind column: %w", err)
		}
	}
	return nil
}

// migrateTasksGroup adds the S22 tasks.group_id column to databases created
// before S22. Idempotent: the base schema already carries the column for
// fresh databases.
func migrateTasksGroup(db *sql.DB) error {
	if !tableExists(db, "tasks") {
		return nil // fresh database: the base schema creates tasks with group_id
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name = 'group_id'`).Scan(&n); err != nil {
		return fmt.Errorf("store: inspect tasks columns: %w", err)
	}
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN group_id INTEGER`); err != nil {
			return fmt.Errorf("store: add tasks.group_id column: %w", err)
		}
	}
	return nil
}

// migrateGroupsPolicy adds the S30 group-policy columns (groups.
// default_priority, groups.sla_timeout_seconds) and the per-task SLA reminder
// flag (tasks.sla_reminded_at) to databases created before S30. Idempotent:
// the base schema already carries all three columns for fresh databases.
func migrateGroupsPolicy(db *sql.DB) error {
	if tableExists(db, "groups") {
		for _, col := range []struct {
			name  string
			alter string
		}{
			{"default_priority", `ALTER TABLE groups ADD COLUMN default_priority INTEGER NOT NULL DEFAULT 0`},
			{"sla_timeout_seconds", `ALTER TABLE groups ADD COLUMN sla_timeout_seconds INTEGER NOT NULL DEFAULT 0`},
		} {
			var n int
			if err := db.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info('groups') WHERE name = ?`, col.name).Scan(&n); err != nil {
				return fmt.Errorf("store: inspect groups column %s: %w", col.name, err)
			}
			if n == 0 {
				if _, err := db.Exec(col.alter); err != nil {
					return fmt.Errorf("store: add groups.%s column: %w", col.name, err)
				}
			}
		}
	}
	if tableExists(db, "tasks") {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name = 'sla_reminded_at'`).Scan(&n); err != nil {
			return fmt.Errorf("store: inspect tasks column sla_reminded_at: %w", err)
		}
		if n == 0 {
			if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN sla_reminded_at TEXT`); err != nil {
				return fmt.Errorf("store: add tasks.sla_reminded_at column: %w", err)
			}
		}
	}
	return nil
}

// migrateTasksKindReview (leader orchestration, S1) rebuilds tasks and/or
// messages on databases created before the kind CHECK gained 'review'
// (SQLite cannot relax a CHECK constraint in place; only a table rebuild
// can). It mirrors the proven migrateMessagesGroup pattern: PRAGMA
// foreign_keys=OFF around a single transaction of create-new,
// INSERT INTO ... SELECT full-column copy, DROP, RENAME.
//
// Idempotency guard: the DDL text in sqlite_master is inspected for the
// quoted `'review'` literal. Both tables' column sets are fixed and contain
// no review-named column, and the older constraint's 'selfreview' literal
// cannot match '%\'review\'%' (its "review" substring is not preceded by a
// quote), so a hit means exactly the new CHECK is present. tables_new is
// created before messages_new because messages_new.task_id REFERENCES
// tasks; the DROP RENAME order below keeps the pair consistent even when
// only one table is rebuilt (FKs are off for the whole step).
//
// Index safety: Open runs migrate() before db.Exec(schema), and the base
// schema's CREATE INDEX IF NOT EXISTS statements (idx_tasks_employee_status,
// idx_tasks_project, idx_messages_to, ...) execute on every Open, so the
// indexes dropped with the old tables are recreated fresh right after this
// migration returns.
//
// Databases rebuilt by migrateMessagesGroup since this change already carry
// 'review' in their messages DDL, but tasks may predate it — the two
// tables are therefore checked and rebuilt independently.
func migrateTasksKindReview(db *sql.DB) error {
	needTasks, err := ddlLacksReview(db, "tasks")
	if err != nil {
		return err
	}
	needMessages, err := ddlLacksReview(db, "messages")
	if err != nil {
		return err
	}
	if !needTasks && !needMessages {
		return nil // fresh or already migrated: nothing to do
	}

	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("store: rebuild kind review: disable foreign keys: %w", err)
	}
	defer func() {
		// Restore FKs whatever happens: the deferred statement re-enables
		// even when the transaction failed (the in-scope connection stays
		// healthy for the caller).
		if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
			// Nothing useful to return from a defer; surface loudly.
			slog.Error("store: rebuild kind review: re-enable foreign keys", "err", err)
		}
	}()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: rebuild kind review: %w", err)
	}
	stmts := make([]string, 0, 8)
	if needTasks {
		stmts = append(stmts,
			`CREATE TABLE tasks_new (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
				employee_id INTEGER NOT NULL REFERENCES employees(id),
				workflow_id INTEGER REFERENCES workflows(id) ON DELETE SET NULL,
				group_id INTEGER REFERENCES groups(id) ON DELETE SET NULL,
				requested_by INTEGER NOT NULL DEFAULT 0,
				kind TEXT NOT NULL DEFAULT 'dispatch'
					CHECK (kind IN ('dispatch', 'ask', 'scheduled', 'selfreview', 'review')),
				status TEXT NOT NULL DEFAULT 'queued'
					CHECK (status IN ('queued', 'running', 'done', 'failed', 'canceled')),
				priority INTEGER NOT NULL DEFAULT 0,
				input TEXT NOT NULL DEFAULT '',
				output TEXT NOT NULL DEFAULT '',
				started_at TEXT,
				finished_at TEXT,
				sla_reminded_at TEXT,
				created_at TEXT NOT NULL DEFAULT (datetime('now'))
			)`,
			`INSERT INTO tasks_new (id, project_id, employee_id, workflow_id, group_id, requested_by,
				kind, status, priority, input, output, started_at, finished_at, sla_reminded_at, created_at)
				SELECT id, project_id, employee_id, workflow_id, group_id, requested_by,
				kind, status, priority, input, output, started_at, finished_at, sla_reminded_at, created_at
				FROM tasks`)
	}
	if needMessages {
		stmts = append(stmts,
			`CREATE TABLE messages_new (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				from_id INTEGER REFERENCES employees(id),
				to_id INTEGER REFERENCES employees(id),
				kind TEXT NOT NULL CHECK (kind IN ('report', 'dispatch', 'ask', 'answer', 'chat', 'summary', 'system', 'scheduled', 'selfreview', 'review')),
				body TEXT NOT NULL,
				project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
				task_id INTEGER REFERENCES tasks(id) ON DELETE SET NULL,
				group_id INTEGER REFERENCES groups(id) ON DELETE SET NULL,
				reply_to INTEGER,
				created_at TEXT NOT NULL DEFAULT (datetime('now'))
			)`,
			`INSERT INTO messages_new (id, from_id, to_id, kind, body, project_id, task_id, group_id, reply_to, created_at)
				SELECT id, from_id, to_id, kind, body, project_id, task_id, group_id, reply_to, created_at
				FROM messages`)
	}
	if needMessages {
		stmts = append(stmts, `DROP TABLE messages`)
	}
	if needTasks {
		// Child tables dropped before their parents (FKs are off anyway);
		// renames happen only after both drops so the name-swap is atomic
		// within the transaction.
		stmts = append(stmts, `DROP TABLE tasks`, `ALTER TABLE tasks_new RENAME TO tasks`)
	}
	if needMessages {
		stmts = append(stmts, `ALTER TABLE messages_new RENAME TO messages`)
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: rebuild kind review: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: rebuild kind review: %w", err)
	}
	return nil
}

// ddlLacksReview reports whether table name exists in sqlite_master and its
// DDL text does not yet contain the quoted 'review' literal (the idempotency
// guard of migrateTasksKindReview). A missing table (fresh database, guarded
// callers) reads as "no rebuild needed". The pattern %'review'% carries the
// quote bytes themselves (two apostrophes inside a SQL string literal each
// escape to one quote character), so the older check's 'selfreview' cannot
// match — inside it, "review" is preceded by 'f', not by a quote.
func ddlLacksReview(db *sql.DB, name string) (bool, error) {
	if !tableExists(db, name) {
		return false, nil
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ? AND sql LIKE '%''review''%'`,
		name).Scan(&n); err != nil {
		return false, fmt.Errorf("store: inspect %s DDL for review: %w", name, err)
	}
	return n == 0, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw *sql.DB; tests use it to assert pragmas.
func (s *Store) DB() *sql.DB { return s.db }

func nullInt64(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	i := int(v.Int64)
	return &i
}

func nullString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}
