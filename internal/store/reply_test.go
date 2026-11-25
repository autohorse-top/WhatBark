package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMessageReplyToRoundTrip (S16): reply_to round-trips through
// CreateMessage / GetMessage / ListMessagesTo / ThreadMessages /
// GroupMessages; messages without a reply keep ReplyTo == nil.
func TestMessageReplyToRoundTrip(t *testing.T) {
	s := openTestStore(t)
	_, _, alice, bob := seedOrg(t, s)

	m1, err := s.CreateMessage(bob, MsgChat, "original", intp(alice), nil, nil, nil)
	if err != nil {
		t.Fatalf("m1: %v", err)
	}
	m2, err := s.CreateMessage(bob, MsgChat, "> original\nmy reply", intp(alice), nil, nil, intp(m1))
	if err != nil {
		t.Fatalf("m2: %v", err)
	}

	got, err := s.GetMessage(m2)
	if err != nil {
		t.Fatalf("get m2: %v", err)
	}
	if got.ReplyTo == nil || *got.ReplyTo != m1 {
		t.Fatalf("GetMessage(m2).ReplyTo = %v, want %d", got.ReplyTo, m1)
	}
	g1, err := s.GetMessage(m1)
	if err != nil {
		t.Fatalf("get m1: %v", err)
	}
	if g1.ReplyTo != nil {
		t.Fatalf("GetMessage(m1).ReplyTo = %v, want nil", g1.ReplyTo)
	}

	ls, err := s.ListMessagesTo(bob, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := false
	for _, m := range ls {
		if m.ID == m2 {
			seen = true
			if m.ReplyTo == nil || *m.ReplyTo != m1 {
				t.Fatalf("ListMessagesTo(m2).ReplyTo = %v, want %d", m.ReplyTo, m1)
			}
		}
	}
	if !seen {
		t.Fatalf("m2 missing from ListMessagesTo(bob)")
	}

	th, err := s.ThreadMessages(alice, bob, 0)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	for _, m := range th {
		if m.ID == m2 && (m.ReplyTo == nil || *m.ReplyTo != m1) {
			t.Fatalf("ThreadMessages(m2).ReplyTo = %v, want %d", m.ReplyTo, m1)
		}
	}

	gid, err := s.CreateGroup("g", alice, []int{bob}, "")
	if err != nil {
		t.Fatalf("group: %v", err)
	}
	gm1, err := s.CreateGroupMessage(gid, MsgChat, "group original", intp(alice), nil, nil)
	if err != nil {
		t.Fatalf("gm1: %v", err)
	}
	gm2, err := s.CreateGroupMessage(gid, MsgChat, "> group original\nreply", intp(bob), nil, intp(gm1))
	if err != nil {
		t.Fatalf("gm2: %v", err)
	}
	gs, err := s.GroupMessages(gid, 0)
	if err != nil {
		t.Fatalf("group messages: %v", err)
	}
	for _, m := range gs {
		if m.ID == gm2 && (m.ReplyTo == nil || *m.ReplyTo != gm1) {
			t.Fatalf("GroupMessages(gm2).ReplyTo = %v, want %d", m.ReplyTo, gm1)
		}
	}
}

// TestSameConversation (S16): table-driven coverage of the 1:1 pair match
// (both directions), the boss thread, group membership, cross-conversation
// rejection, missing targets, and the malformed-call error.
func TestSameConversation(t *testing.T) {
	s := openTestStore(t)
	root, _, alice, bob := seedOrg(t, s)
	carol, err := s.CreateEmployee("carol", "worker", KindAI, root)
	if err != nil {
		t.Fatalf("create carol: %v", err)
	}

	g1, err := s.CreateGroup("g1", alice, []int{bob}, "")
	if err != nil {
		t.Fatalf("group g1: %v", err)
	}
	g2, err := s.CreateGroup("g2", bob, []int{carol}, "")
	if err != nil {
		t.Fatalf("group g2: %v", err)
	}

	dmAB, _ := s.CreateMessage(bob, MsgChat, "dm alice->bob", intp(alice), nil, nil, nil)
	dmAC, _ := s.CreateMessage(carol, MsgChat, "dm alice->carol", intp(alice), nil, nil, nil)
	bossMsg, _ := s.CreateMessage(bob, MsgDispatch, "boss->bob", nil, nil, nil, nil)
	gm1, _ := s.CreateGroupMessage(g1, MsgChat, "g1 alice", intp(alice), nil, nil)
	gm2, _ := s.CreateGroupMessage(g2, MsgChat, "g2 bob", intp(bob), nil, nil)

	cases := []struct {
		name    string
		reply   int
		from    int
		toID    *int
		group   *int
		want    bool
		wantErr bool
	}{
		{"reverse direction of the 1:1 pair", dmAB, bob, intp(alice), nil, true, false},
		{"forward direction of the 1:1 pair", dmAB, alice, intp(bob), nil, true, false},
		{"different peer is not the same thread", dmAB, bob, intp(carol), nil, false, false},
		{"third party is not in the pair", dmAB, carol, intp(alice), nil, false, false},
		{"other thread of the same sender", dmAC, carol, intp(alice), nil, true, false},
		{"boss message answered by the employee", bossMsg, bob, intp(0), nil, true, false},
		{"boss message answered by the boss", bossMsg, 0, intp(bob), nil, true, false},
		{"group message quoted in the same group", gm1, alice, nil, intp(g1), true, false},
		{"group message quoted by another member", gm1, bob, nil, intp(g1), true, false},
		{"group message quoted in another group", gm1, bob, nil, intp(g2), false, false},
		{"1:1 message quoted from a group", dmAB, alice, nil, intp(g1), false, false},
		{"group message quoted in a 1:1 thread", gm2, bob, intp(carol), nil, false, false},
		{"missing target is not an error", 999999, bob, intp(alice), nil, false, false},
		{"neither to_id nor group given", dmAB, bob, nil, nil, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.SameConversation(tc.reply, tc.from, tc.toID, tc.group)
			if (err != nil) != tc.wantErr {
				t.Fatalf("SameConversation = (%v, %v), wantErr %v", got, err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Fatalf("SameConversation = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMigratePreS16Messages builds a post-S14 database (messages already has
// group_id, no reply_to, like the current demo.db), opens it through
// Store.Open, and verifies the column add kept every row and is idempotent
// across re-opens.
func TestMigratePreS16Messages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s15.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	legacy := `
CREATE TABLE employees (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  role TEXT NOT NULL,
  kind TEXT NOT NULL DEFAULT 'ai' CHECK (kind IN ('ai', 'human')),
  department_id INTEGER NOT NULL,
  tool TEXT NOT NULL DEFAULT 'mock',
  tool_args TEXT NOT NULL DEFAULT '{}',
  workdir TEXT NOT NULL DEFAULT '',
  token_hash TEXT NOT NULL DEFAULT '',
  last_seen_at TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  from_id INTEGER REFERENCES employees(id),
  to_id INTEGER REFERENCES employees(id),
  kind TEXT NOT NULL CHECK (kind IN ('report', 'dispatch', 'ask', 'answer', 'chat', 'summary', 'system', 'scheduled', 'selfreview')),
  body TEXT NOT NULL,
  project_id INTEGER,
  task_id INTEGER,
  group_id INTEGER,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
`
	if _, err := db.Exec(legacy); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO employees (id, name, role, kind, department_id)
		VALUES (1, 's15-a', 'worker', 'ai', 1), (2, 's15-b', 'worker', 'ai', 1)`); err != nil {
		t.Fatalf("seed employees: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO messages (id, from_id, to_id, kind, body, created_at)
		VALUES (1, 1, 2, 'chat', 'pre-s16 hello', '2026-01-01 00:00:00'),
		        (2, 2, NULL, 'chat', 'pre-s16 group message', '2026-01-01 00:00:01')`); err != nil {
		t.Fatalf("seed messages: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var n int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'reply_to'`).Scan(&n); err != nil {
		t.Fatalf("inspect messages: %v", err)
	}
	if n != 1 {
		t.Fatalf("reply_to column count = %d, want 1", n)
	}
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE reply_to IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count legacy rows: %v", err)
	}
	if n != 2 {
		t.Fatalf("legacy rows with NULL reply_to = %d, want 2", n)
	}

	// New writes can use the migrated column.
	mid, err := s.CreateMessage(2, MsgChat, "post-migration reply", intp(1), nil, nil, intp(1))
	if err != nil {
		t.Fatalf("create with reply_to: %v", err)
	}
	got, err := s.GetMessage(mid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ReplyTo == nil || *got.ReplyTo != 1 {
		t.Fatalf("ReplyTo = %v, want 1", got.ReplyTo)
	}

	// Idempotent: a second open re-runs the migration without error or
	// duplicate columns.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if err := s2.DB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'reply_to'`).Scan(&n); err != nil {
		t.Fatalf("re-inspect: %v", err)
	}
	if n != 1 {
		t.Fatalf("reply_to count after re-open = %d, want 1", n)
	}
}
