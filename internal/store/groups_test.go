package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

// sameInts reports whether a and b hold the same values in the same order.
func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCreateGroup covers name validation, member de-duplication, creator
// auto-join, boss=0 as a legal member, member existence checks, and S21 kind
// handling (default "group", boss-only "channel", invalid kind rejected).
func TestCreateGroup(t *testing.T) {
	s := openTestStore(t)
	_, bob, carol := seedChatOrg(t, s)

	longName := strings.Repeat("群", 65) // 65 runes > 64 cap

	cases := []struct {
		name        string
		creator     int
		members     []int
		kind        string // S21: "" = default "group"
		wantMembers []int  // sorted (by employee_id) membership expected on success
	}{
		{"empty name rejected", 0, []int{bob}, "", nil},
		{"whitespace name rejected", bob, []int{carol}, "", nil},
		{"overlong name rejected", bob, []int{carol}, "", nil},
		{"creator only rejected", bob, nil, "", nil},
		{"creator listed twice still one member", bob, []int{bob}, "", nil},
		{"unknown member rejected", bob, []int{999}, "", nil},
		{"boss creator auto-joined", 0, []int{bob}, "", []int{0, bob}},
		{"employee creator auto-joined", bob, []int{carol}, "", []int{bob, carol}},
		{"duplicates de-duplicated", carol, []int{bob, bob, carol}, "", []int{bob, carol}},
		// S21: kind
		{"default kind is group", 0, []int{carol}, "", []int{0, carol}},
		{"boss channel ok", 0, []int{carol}, "channel", []int{0, carol}},
		{"employee channel rejected", bob, []int{carol}, "channel", nil},
		{"invalid kind rejected", bob, []int{carol}, "bogus", nil},
	}
	names := []string{"", "   ", longName, "g1", "g2", "g3", "boss-g", "bob-g", "carol-g",
		"g-default", "chan-1", "chan-rej", "g-bad"}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gid, err := s.CreateGroup(names[i], tc.creator, tc.members, tc.kind)
			if tc.wantMembers == nil {
				if err == nil {
					t.Fatalf("want error, got group %d", gid)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateGroup: %v", err)
			}
			g, err := s.GetGroup(gid)
			if err != nil {
				t.Fatalf("GetGroup: %v", err)
			}
			wantName := strings.TrimSpace(names[i])
			if g.Name != wantName || g.CreatedBy != tc.creator {
				t.Fatalf("group = %+v, want name=%q creator=%d", g, wantName, tc.creator)
			}
			wantKind := tc.kind
			if wantKind == "" {
				wantKind = "group"
			}
			if g.Kind != wantKind {
				t.Fatalf("group kind = %q, want %q", g.Kind, wantKind)
			}
			got, err := s.GroupMemberIDs(gid)
			if err != nil {
				t.Fatalf("GroupMemberIDs: %v", err)
			}
			if !sameInts(got, tc.wantMembers) {
				t.Fatalf("members = %v, want %v", got, tc.wantMembers)
			}
			// IsGroupMember agrees with the membership list.
			for _, id := range tc.wantMembers {
				if ok, _ := s.IsGroupMember(gid, id); !ok {
					t.Errorf("IsGroupMember(%d) = false, want true", id)
				}
			}
			if ok, _ := s.IsGroupMember(gid, 999); ok {
				t.Error("IsGroupMember(999) = true, want false")
			}
		})
	}
}

func TestGetGroupUnknown(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetGroup(999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetGroup(999): %v, want ErrNotFound", err)
	}
}

func TestCreateGroupMessage(t *testing.T) {
	s := openTestStore(t)
	alice, bob, carol := seedChatOrg(t, s)
	gid, err := s.CreateGroup("g", bob, []int{alice, carol}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	// Unknown group / unknown sender are rejected.
	if _, err := s.CreateGroupMessage(999, MsgChat, "x", intp(alice), nil, nil); err == nil {
		t.Fatal("CreateGroupMessage(unknown group) accepted")
	}
	if _, err := s.CreateGroupMessage(gid, MsgChat, "x", intp(999), nil, nil); err == nil {
		t.Fatal("CreateGroupMessage(unknown sender) accepted")
	}

	// Employee sender and boss sender (nil) both work; to_id stays NULL.
	m1, err := s.CreateGroupMessage(gid, MsgChat, "from alice", intp(alice), nil, nil)
	if err != nil {
		t.Fatalf("CreateGroupMessage: %v", err)
	}
	m2, err := s.CreateGroupMessage(gid, MsgChat, "from boss", nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateGroupMessage (boss): %v", err)
	}
	for _, id := range []int{m1, m2} {
		m, err := s.GetMessage(id)
		if err != nil {
			t.Fatalf("GetMessage(%d): %v", id, err)
		}
		if m.ToID != nil {
			t.Fatalf("group message %d to_id = %v, want nil", id, *m.ToID)
		}
		if m.Kind != MsgChat {
			t.Fatalf("group message %d kind = %q, want chat", id, m.Kind)
		}
	}
	if m, _ := s.GetMessage(m2); m.FromID != nil {
		t.Fatalf("boss group message from_id = %v, want nil", *m.FromID)
	}
}

// TestGroupMessages covers ascending order, the limit window, and that group
// traffic never leaks into 1:1 queries (and vice versa).
func TestGroupMessages(t *testing.T) {
	s := openTestStore(t)
	alice, bob, carol := seedChatOrg(t, s)
	gid, err := s.CreateGroup("g", bob, []int{alice, carol}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	// 1:1 noise that must stay out of the group thread.
	dm, err := s.CreateMessage(alice, MsgChat, "one-to-one", intp(bob), nil, nil, nil)
	if err != nil {
		t.Fatalf("1:1 message: %v", err)
	}

	var gmsgs []int
	for i, body := range []string{"g1", "g2", "g3", "g4", "g5"} {
		from := []int{alice, 0, carol, alice, bob}
		var f *int
		if from[i] != 0 {
			f = &from[i]
		}
		id, err := s.CreateGroupMessage(gid, MsgChat, body, f, nil, nil)
		if err != nil {
			t.Fatalf("group msg %d: %v", i, err)
		}
		gmsgs = append(gmsgs, id)
	}

	all, err := s.GroupMessages(gid, 0)
	if err != nil {
		t.Fatalf("GroupMessages: %v", err)
	}
	// S17: the group carries the creation system message as its first row.
	if len(all) != 6 {
		t.Fatalf("GroupMessages = %d msgs, want 6 (1 system + 5 chat)", len(all))
	}
	if all[0].Kind != MsgSystem || all[0].Body != "bob created a group" {
		t.Fatalf("first msg = %+v, want bob's system creation event", all[0])
	}
	if all[0].FromID == nil || *all[0].FromID != bob {
		t.Fatalf("system msg from_id = %v, want bob", all[0].FromID)
	}
	for i, m := range all[1:] {
		if m.ID != gmsgs[i] {
			t.Fatalf("order: msg[%d] = %d, want %d", i+1, m.ID, gmsgs[i])
		}
	}

	tail, err := s.GroupMessages(gid, 2)
	if err != nil {
		t.Fatalf("GroupMessages limit: %v", err)
	}
	if len(tail) != 2 || tail[0].ID != gmsgs[3] || tail[1].ID != gmsgs[4] {
		t.Fatalf("limit window = %+v, want the two newest in ascending order", tail)
	}

	// 1:1 thread between bob and alice: only the direct message.
	thread, err := s.ThreadMessages(bob, alice, 0)
	if err != nil {
		t.Fatalf("ThreadMessages: %v", err)
	}
	if len(thread) != 1 || thread[0].ID != dm {
		t.Fatalf("1:1 thread = %+v, want only the direct message", thread)
	}
	// Inbox listing must not surface group messages either (to_id IS NULL).
	inbox, err := s.ListMessagesTo(alice, 100)
	if err != nil {
		t.Fatalf("ListMessagesTo: %v", err)
	}
	if len(inbox) != 1 || inbox[0].ID != dm {
		t.Fatalf("inbox = %+v, want only the 1:1 message (group msgs excluded)", inbox)
	}
}

// groupConvOf finds one group row by id.
func groupConvOf(t *testing.T, s *Store, viewer, groupID int) (GroupConversation, bool) {
	t.Helper()
	cons, err := s.ListGroupConversations(viewer)
	if err != nil {
		t.Fatalf("ListGroupConversations(%d): %v", viewer, err)
	}
	for _, c := range cons {
		if c.GroupID == groupID {
			return c, true
		}
	}
	return GroupConversation{}, false
}

// TestListGroupConversations checks membership scoping, member_count, last
// message preview, unread counting (own messages excluded), and ordering.
func TestListGroupConversations(t *testing.T) {
	s := openTestStore(t)
	alice, bob, carol := seedChatOrg(t, s)

	g1, err := s.CreateGroup("g1", bob, []int{alice, carol}, "") // {bob, alice, carol}
	if err != nil {
		t.Fatalf("g1: %v", err)
	}
	g2, err := s.CreateGroup("g2", alice, []int{bob}, "") // {alice, bob}
	if err != nil {
		t.Fatalf("g2: %v", err)
	}

	m1, _ := s.CreateGroupMessage(g1, MsgChat, "alice in g1", intp(alice), nil, nil)
	m2, _ := s.CreateGroupMessage(g1, MsgChat, "carol in g1", intp(carol), nil, nil)
	if _, err := s.CreateGroupMessage(g2, MsgChat, "bob in g2", intp(bob), nil, nil); err != nil {
		t.Fatalf("m3: %v", err)
	}

	// g2's last message (m3) is newest overall, so it sorts first.
	cons, err := s.ListGroupConversations(alice)
	if err != nil {
		t.Fatalf("list alice: %v", err)
	}
	if len(cons) != 2 || cons[0].GroupID != g2 || cons[1].GroupID != g1 {
		t.Fatalf("alice order = %+v, want g2 then g1", cons)
	}
	g1c := cons[1]
	if g1c.Name != "g1" || g1c.MemberCount != 3 {
		t.Fatalf("g1 meta = %+v, want name g1 member_count 3", g1c)
	}
	if g1c.LastMsgID != m2 || g1c.LastMsgBody != "carol in g1" {
		t.Fatalf("g1 last msg = %d/%q, want %d/carol in g1", g1c.LastMsgID, g1c.LastMsgBody, m2)
	}
	// Unread for alice: only m2 (m1 is her own).
	if g1c.Unread != 1 {
		t.Fatalf("alice g1 unread = %d, want 1", g1c.Unread)
	}
	if cons[0].MemberCount != 2 || cons[0].Unread != 1 {
		t.Fatalf("alice g2 = %+v, want member_count 2 unread 1 (bob's msg)", cons[0])
	}

	cons, err = s.ListGroupConversations(bob)
	if err != nil {
		t.Fatalf("list bob: %v", err)
	}
	for _, c := range cons {
		switch c.GroupID {
		case g1:
			if c.Unread != 2 {
				t.Fatalf("bob g1 unread = %d, want 2 (alice + carol)", c.Unread)
			}
		case g2:
			if c.Unread != 0 {
				t.Fatalf("bob g2 unread = %d, want 0 (only his own msg)", c.Unread)
			}
		}
	}

	// Boss is in no group: empty list, no error.
	cons, err = s.ListGroupConversations(0)
	if err != nil {
		t.Fatalf("list boss: %v", err)
	}
	if len(cons) != 0 {
		t.Fatalf("boss groups = %+v, want none", cons)
	}

	// S17: a brand-new group carries the creation system message — last-msg
	// fields populated, but zero unread (system never counts).
	g3, err := s.CreateGroup("empty", alice, []int{bob}, "")
	if err != nil {
		t.Fatalf("g3: %v", err)
	}
	cons, _ = s.ListGroupConversations(alice)
	var g3c *GroupConversation
	for i := range cons {
		if cons[i].GroupID == g3 {
			g3c = &cons[i]
		}
	}
	if g3c == nil {
		t.Fatalf("g3 missing from alice's group conversations: %+v", cons)
	}
	if g3c.LastMsgID == 0 || g3c.LastMsgKind != MsgSystem || g3c.LastMsgBody != "alice created a group" ||
		g3c.Unread != 0 || g3c.MemberCount != 2 {
		t.Fatalf("new group entry = %+v, want system last msg + 0 unread", *g3c)
	}
	_ = m1
}

// TestGroupReadCursor: the group cursor lives under peer_id = -(group_id+1)
// in conv_reads, is partial/monotonic like the 1:1 cursors, and never
// touches the 1:1 read state.
func TestGroupReadCursor(t *testing.T) {
	s := openTestStore(t)
	alice, bob, _ := seedChatOrg(t, s)
	gid, err := s.CreateGroup("g", alice, []int{bob}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	m1, _ := s.CreateGroupMessage(gid, MsgChat, "a1", intp(alice), nil, nil)
	m2, _ := s.CreateGroupMessage(gid, MsgChat, "b", intp(bob), nil, nil)
	m3, _ := s.CreateGroupMessage(gid, MsgChat, "a2", intp(alice), nil, nil)

	peer := -(gid + 1)
	if c, ok := groupConvOf(t, s, bob, gid); !ok || c.Unread != 2 {
		t.Fatalf("bob group unread = %+v, want 2 (m1, m3)", c)
	}
	// Read up to m2: m3 stays unread; cursor advanced -> changed=true.
	if advanced, err := s.MarkConversationRead(bob, peer, m2); err != nil {
		t.Fatalf("mark read m2: %v", err)
	} else if !advanced {
		t.Fatal("mark read m2: want changed=true")
	}
	if c, _ := groupConvOf(t, s, bob, gid); c.Unread != 1 {
		t.Fatalf("bob group unread after partial read = %d, want 1", c.Unread)
	}
	// Monotonic: an older cursor never regresses, and no-op reports changed=false.
	if advanced, err := s.MarkConversationRead(bob, peer, m1); err != nil {
		t.Fatalf("mark read m1: %v", err)
	} else if advanced {
		t.Fatal("mark read m1: want changed=false for regressive cursor")
	}
	if c, _ := groupConvOf(t, s, bob, gid); c.Unread != 1 {
		t.Fatalf("bob group unread after regressive read = %d, want 1", c.Unread)
	}
	// Full read clears it.
	if advanced, err := s.MarkConversationRead(bob, peer, m3); err != nil {
		t.Fatalf("mark read m3: %v", err)
	} else if !advanced {
		t.Fatal("mark read m3: want changed=true")
	}
	if c, _ := groupConvOf(t, s, bob, gid); c.Unread != 0 {
		t.Fatalf("bob group unread after full read = %d, want 0", c.Unread)
	}
	// alice's cursor is independent: her own g message aside, m2 is unread.
	if c, _ := groupConvOf(t, s, alice, gid); c.Unread != 1 {
		t.Fatalf("alice group unread = %d, want 1 (bob's m2)", c.Unread)
	}

	// The group cursor must not clear the 1:1 conversation with alice.
	if _, err := s.CreateMessage(bob, MsgChat, "dm", intp(alice), nil, nil, nil); err != nil {
		t.Fatalf("dm: %v", err)
	}
	if cur, ok := convOf(t, s, bob, alice); !ok || cur.Unread != 1 {
		t.Fatalf("bob<->alice 1:1 unread = %+v, want 1 (dm)", cur)
	}
}

// TestDeleteEmployeeCleansGroupMembers: deleting a member drops its
// group_members rows (no ghost members, no FK to block the delete).
func TestDeleteEmployeeCleansGroupMembers(t *testing.T) {
	s := openTestStore(t)
	_, bob, carol := seedChatOrg(t, s)
	gid, err := s.CreateGroup("g", carol, []int{bob}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if ok, _ := s.IsGroupMember(gid, bob); !ok {
		t.Fatal("bob should be a member before delete")
	}
	if err := s.DeleteEmployee(bob); err != nil {
		t.Fatalf("DeleteEmployee: %v", err)
	}
	if ok, _ := s.IsGroupMember(gid, bob); ok {
		t.Fatal("ghost member row survived DeleteEmployee")
	}
	members, err := s.GroupMemberIDs(gid)
	if err != nil {
		t.Fatalf("GroupMemberIDs: %v", err)
	}
	if !sameInts(members, []int{carol}) {
		t.Fatalf("members after delete = %v, want [carol]", members)
	}
}

// TestMigratePreS14Messages builds a legacy DB (messages with NOT NULL
// to_id, no group_id, no groups tables), opens it through Store.Open, and
// verifies the rebuild kept every row, unlocked to_id, created the group
// tables, and is idempotent across re-opens.
func TestMigratePreS14Messages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// Raw legacy schema (what pre-S14 installs had).
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
  to_id INTEGER NOT NULL REFERENCES employees(id),
  kind TEXT NOT NULL CHECK (kind IN ('report', 'dispatch', 'ask', 'answer', 'chat', 'summary', 'system', 'scheduled', 'selfreview')),
  body TEXT NOT NULL,
  project_id INTEGER,
  task_id INTEGER,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
`
	if _, err := db.Exec(legacy); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO employees (id, name, role, kind, department_id)
		VALUES (1, 'legacy-a', 'worker', 'ai', 1), (2, 'legacy-b', 'worker', 'ai', 1)`); err != nil {
		t.Fatalf("seed employees: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO messages (id, from_id, to_id, kind, body, created_at)
		VALUES (1, 1, 2, 'chat', 'legacy hello', '2026-01-01 00:00:00'),
		        (2, 2, 1, 'chat', 'legacy reply', '2026-01-01 00:00:01')`); err != nil {
		t.Fatalf("seed messages: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	// Store.Open must migrate the legacy DB.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// group_id exists; to_id is now nullable.
	var n int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'group_id'`).Scan(&n); err != nil {
		t.Fatalf("inspect messages: %v", err)
	}
	if n != 1 {
		t.Fatalf("group_id column count = %d, want 1", n)
	}
	// S21: groups.kind added by migrateGroupsKind (the S14 rebuild produces a
	// groups table without it, exactly like pre-S14 DBs for messages).
	var nkind int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('groups') WHERE name = 'kind'`).Scan(&nkind); err != nil {
		t.Fatalf("inspect groups: %v", err)
	}
	if nkind != 1 {
		t.Fatalf("groups.kind column count = %d, want 1", nkind)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO messages (from_id, to_id, kind, body, group_id) VALUES (1, NULL, 'chat', 'null to_id ok', NULL)`); err != nil {
		t.Fatalf("nullable to_id rejected after migration: %v", err)
	}

	// Legacy rows intact.
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE group_id IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("1:1 + null-to rows = %d, want 3", n)
	}
	var body string
	if err := s.DB().QueryRow(`SELECT body FROM messages WHERE id = 1`).Scan(&body); err != nil {
		t.Fatalf("read legacy: %v", err)
	}
	if body != "legacy hello" {
		t.Fatalf("legacy body = %q, want %q", body, "legacy hello")
	}

	// The new group surface works on the migrated DB.
	gid, err := s.CreateGroup("migrated", 1, []int{2}, "")
	if err != nil {
		t.Fatalf("CreateGroup on migrated db: %v", err)
	}
	if _, err := s.CreateGroupMessage(gid, MsgChat, "in group", intp(1), nil, nil); err != nil {
		t.Fatalf("CreateGroupMessage on migrated db: %v", err)
	}
	gmsgs, err := s.GroupMessages(gid, 0)
	if err != nil || len(gmsgs) != 2 || gmsgs[0].ToID != nil || gmsgs[1].ToID != nil {
		t.Fatalf("group messages on migrated db = %+v (err %v), want system + chat with nil to_id", gmsgs, err)
	}
	if gmsgs[0].Kind != MsgSystem || gmsgs[0].Body != "legacy-a created a group" {
		t.Fatalf("migrated group first msg = %+v, want creation system message", gmsgs[0])
	}
	// 1:1 queries still see the legacy traffic and nothing else.
	list, err := s.ListMessagesTo(2, 10)
	if err != nil {
		t.Fatalf("ListMessagesTo: %v", err)
	}
	if len(list) != 1 || list[0].ID != 1 {
		t.Fatalf("1:1 list = %+v, want only legacy msg 1", list)
	}

	// Re-open: migration is a no-op and data survives.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if err := s2.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if n != 5 {
		t.Fatalf("messages after re-open = %d, want 5", n)
	}
	if g, err := s2.GetGroup(gid); err != nil || g.Name != "migrated" || g.Kind != "group" {
		t.Fatalf("group after re-open = %+v (err %v), want name=migrated kind=group", g, err)
	}
}

// TestCreateGroupSystemMessage: S17 creation event — exactly one system
// message, exact body "{creator} created a group", boss creator leaves from_id NULL,
// employee creator is the sender; to_id stays NULL (group message).
func TestCreateGroupSystemMessage(t *testing.T) {
	s := openTestStore(t)
	alice, bob, _ := seedChatOrg(t, s)

	cases := []struct {
		name     string
		gname    string
		creator  int
		member   int
		wantFrom *int // nil = NULL (boss creator)
		wantBody string
	}{
		{"boss creator", "sys-boss", 0, alice, nil, "boss created a group"},
		{"employee creator", "sys-emp", bob, alice, intp(bob), "bob created a group"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gid, err := s.CreateGroup(tc.gname, tc.creator, []int{tc.member}, "")
			if err != nil {
				t.Fatalf("CreateGroup: %v", err)
			}
			msgs, err := s.GroupMessages(gid, 0)
			if err != nil {
				t.Fatalf("GroupMessages: %v", err)
			}
			if len(msgs) != 1 {
				t.Fatalf("new group = %d msgs, want exactly 1 (system creation event)", len(msgs))
			}
			m := msgs[0]
			if m.Kind != MsgSystem {
				t.Fatalf("kind = %q, want system", m.Kind)
			}
			if m.Body != tc.wantBody {
				t.Fatalf("body = %q, want %q", m.Body, tc.wantBody)
			}
			if m.ToID != nil {
				t.Fatalf("to_id = %v, want NULL (group message)", *m.ToID)
			}
			if tc.wantFrom == nil {
				if m.FromID != nil {
					t.Fatalf("from_id = %v, want NULL for boss", *m.FromID)
				}
			} else {
				if m.FromID == nil || *m.FromID != *tc.wantFrom {
					t.Fatalf("from_id = %v, want %d", m.FromID, *tc.wantFrom)
				}
			}
		})
	}
}

// TestGroupPrefs: S17 per-viewer mute/ignore prefs — default (false,false),
// single-flag upserts, nil keeps the stored flag, both flags, viewer 0 (boss),
// ErrNotFound for unknown groups.
func TestGroupPrefs(t *testing.T) {
	s := openTestStore(t)
	alice, bob, _ := seedChatOrg(t, s)
	gid, err := s.CreateGroup("prefs", alice, []int{bob}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	if m, i, err := s.GetGroupPrefs(bob, gid); err != nil || m || i {
		t.Fatalf("fresh prefs = (%v, %v, %v), want (false, false, nil)", m, i, err)
	}
	if _, _, err := s.GetGroupPrefs(bob, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetGroupPrefs unknown group: %v, want ErrNotFound", err)
	}

	tr := true
	fl := false
	cases := []struct {
		name      string
		viewer    int
		muted     *bool
		ignored   *bool
		wantMuted bool
		wantIgn   bool
	}{
		{"muted only", bob, &tr, nil, true, false},
		{"ignored only keeps muted", bob, nil, &tr, true, true},
		{"clear muted", bob, &fl, nil, false, true},
		{"boss viewer sets both", 0, &tr, &tr, true, true},
		{"boss viewer clears both", 0, &fl, &fl, false, false},
		{"alice independent from bob", alice, &tr, &tr, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.SetGroupPrefs(tc.viewer, gid, tc.muted, tc.ignored); err != nil {
				t.Fatalf("SetGroupPrefs: %v", err)
			}
			gotM, gotI, err := s.GetGroupPrefs(tc.viewer, gid)
			if err != nil {
				t.Fatalf("GetGroupPrefs: %v", err)
			}
			if gotM != tc.wantMuted || gotI != tc.wantIgn {
				t.Fatalf("viewer %d prefs = (%v,%v), want (%v,%v)",
					tc.viewer, gotM, gotI, tc.wantMuted, tc.wantIgn)
			}
		})
	}

	if err := s.SetGroupPrefs(alice, 999, &tr, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetGroupPrefs unknown group: %v, want ErrNotFound", err)
	}
}

// TestAddGroupMembers: S17 member addition — valid adds (incl. boss 0),
// input-order result, existing members skipped, all-duplicate / empty /
// unknown-employee / unknown-group are errors.
func TestAddGroupMembers(t *testing.T) {
	s := openTestStore(t)
	alice, bob, carol := seedChatOrg(t, s)
	gid, err := s.CreateGroup("am", alice, []int{bob}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	added, err := s.AddGroupMembers(gid, []int{carol, 0, alice})
	if err != nil {
		t.Fatalf("AddGroupMembers: %v", err)
	}
	if !sameInts(added, []int{carol, 0}) {
		t.Fatalf("added = %v, want [carol 0] (alice already in, input order kept)", added)
	}
	members, err := s.GroupMemberIDs(gid)
	if err != nil {
		t.Fatalf("GroupMemberIDs: %v", err)
	}
	if !sameInts(members, []int{0, alice, bob, carol}) {
		t.Fatalf("members = %v, want all four sorted", members)
	}

	if _, err := s.AddGroupMembers(gid, []int{alice, bob, alice}); err == nil {
		t.Fatal("all-duplicate list accepted, want error")
	}
	if _, err := s.AddGroupMembers(gid, nil); err == nil {
		t.Fatal("empty list accepted, want error")
	}
	if _, err := s.AddGroupMembers(gid, []int{999}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown employee: %v, want ErrNotFound", err)
	}
	if _, err := s.AddGroupMembers(999, []int{bob}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown group: %v, want ErrNotFound", err)
	}
}

// TestListGroupMemberDetails: S17 member rows — boss row (name "boss", kind
// "system"), employee kind/role, creator flag, ascending order, unknown group.
func TestListGroupMemberDetails(t *testing.T) {
	s := openTestStore(t)
	alice, bob, _ := seedChatOrg(t, s)
	// seedOrg: alice (leader) created before bob (worker) => alice id < bob id.
	gid, err := s.CreateGroup("details", alice, []int{0, bob}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	dets, err := s.ListGroupMemberDetails(gid)
	if err != nil {
		t.Fatalf("ListGroupMemberDetails: %v", err)
	}
	want := []struct {
		id      int
		name    string
		kind    string
		role    string
		creator bool
	}{
		{0, "boss", "system", "", false},
		{alice, "alice", "ai", "leader", true},
		{bob, "bob", "ai", "worker", false},
	}
	if len(dets) != len(want) {
		t.Fatalf("details = %+v, want %d rows", dets, len(want))
	}
	for i, w := range want {
		d := dets[i]
		if d.EmployeeID != w.id || d.Name != w.name || d.Kind != w.kind ||
			d.Role != w.role || d.IsCreator != w.creator {
			t.Fatalf("row %d = %+v, want %+v", i, d, w)
		}
	}

	if _, err := s.ListGroupMemberDetails(999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown group: %v, want ErrNotFound", err)
	}
}

// TestGroupReadCursors: S19-B — per-member read cursors for a group, keyed by
// the negative peer-id convention; 1:1 cursors never leak into the map.
func TestGroupReadCursors(t *testing.T) {
	s := openTestStore(t)
	alice, bob, _ := seedChatOrg(t, s)

	gid, err := s.CreateGroup("read-cursors", alice, []int{bob}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	if got, err := s.GroupReadCursors(gid); err != nil || len(got) != 0 {
		t.Fatalf("empty group cursors = (%v, %v), want empty", got, err)
	}

	if advanced, err := s.MarkConversationRead(bob, groupPeerID(gid), 5); err != nil {
		t.Fatalf("mark bob: %v", err)
	} else if !advanced {
		t.Fatal("mark bob: fresh cursor wants changed=true")
	}
	if advanced, err := s.MarkConversationRead(alice, groupPeerID(gid), 3); err != nil {
		t.Fatalf("mark alice: %v", err)
	} else if !advanced {
		t.Fatal("mark alice: fresh cursor wants changed=true")
	}
	// A 1:1 cursor must not leak into the group cursors.
	if advanced, err := s.MarkConversationRead(bob, alice, 99); err != nil {
		t.Fatalf("mark 1:1: %v", err)
	} else if !advanced {
		t.Fatal("mark 1:1: fresh cursor wants changed=true")
	}

	got, err := s.GroupReadCursors(gid)
	if err != nil {
		t.Fatalf("GroupReadCursors: %v", err)
	}
	want := map[int]int{bob: 5, alice: 3}
	if len(got) != len(want) {
		t.Fatalf("cursors = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("cursor[%d] = %d, want %d", k, got[k], v)
		}
	}
}

// TestRenameGroup (S26): trims and applies a valid rename; rejects empty,
// whitespace-only and overlong (>64 runes) names; unknown group is
// ErrNotFound.
func TestRenameGroup(t *testing.T) {
	s := openTestStore(t)
	_, bob, carol := seedChatOrg(t, s)
	ctx := context.Background()

	gid, err := s.CreateGroup("old-name", bob, []int{carol}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	if err := s.RenameGroup(ctx, gid, "  new-name  "); err != nil {
		t.Fatalf("RenameGroup: %v", err)
	}
	g, err := s.GetGroup(gid)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if g.Name != "new-name" {
		t.Fatalf("name = %q, want %q (trimmed)", g.Name, "new-name")
	}

	longName := strings.Repeat("群", 65) // 65 runes > 64 cap
	cases := []struct {
		name string
		val  string
	}{
		{"empty name rejected", ""},
		{"whitespace name rejected", "   "},
		{"overlong name rejected", longName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.RenameGroup(ctx, gid, tc.val); err == nil {
				t.Fatalf("RenameGroup(%q) accepted, want error", tc.val)
			}
		})
	}
	if err := s.RenameGroup(ctx, 999, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RenameGroup(unknown group) = %v, want ErrNotFound", err)
	}
	// The name must be unchanged after the rejected renames.
	if g, _ := s.GetGroup(gid); g.Name != "new-name" {
		t.Fatalf("name = %q after rejected renames, want new-name", g.Name)
	}
}

// TestRemoveGroupMember (S26): removes a member; the boss (id 0) cannot be
// removed; a non-member cannot be removed; the group must keep at least 2
// members after removal; unknown group is ErrNotFound.
func TestRemoveGroupMember(t *testing.T) {
	s := openTestStore(t)
	alice, bob, carol := seedChatOrg(t, s)
	ctx := context.Background()

	gid, err := s.CreateGroup("rm", alice, []int{bob, carol}, "") // {alice, bob, carol}
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	if err := s.RemoveGroupMember(ctx, gid, carol); err != nil {
		t.Fatalf("RemoveGroupMember(carol): %v", err)
	}
	got, err := s.GroupMemberIDs(gid)
	if err != nil {
		t.Fatalf("GroupMemberIDs: %v", err)
	}
	if !sameInts(got, []int{alice, bob}) {
		t.Fatalf("members = %v, want %v", got, []int{alice, bob})
	}

	// Boss row is protected.
	if err := s.RemoveGroupMember(ctx, gid, 0); err == nil {
		t.Fatal("RemoveGroupMember(boss) accepted, want error")
	}
	// Non-member cannot be removed.
	if err := s.RemoveGroupMember(ctx, gid, 999); err == nil {
		t.Fatal("RemoveGroupMember(non-member) accepted, want error")
	}
	// Removing bob would leave only alice (<2 members).
	if err := s.RemoveGroupMember(ctx, gid, bob); err == nil {
		t.Fatal("RemoveGroupMember leaving <2 members accepted, want error")
	}
	// The failed removals must not have changed membership.
	if got, _ := s.GroupMemberIDs(gid); !sameInts(got, []int{alice, bob}) {
		t.Fatalf("members = %v after failed removals, want [alice bob]", got)
	}
	if err := s.RemoveGroupMember(ctx, 999, bob); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RemoveGroupMember(unknown group) = %v, want ErrNotFound", err)
	}
}

// TestRemoveGroupMemberConcurrent (S26): concurrent removal of two of three
// members must not break the >=2 member invariant — the count+delete is
// transactional, so at most one of the two removals can succeed.
func TestRemoveGroupMemberConcurrent(t *testing.T) {
	s := openTestStore(t)
	alice, bob, carol := seedChatOrg(t, s)
	ctx := context.Background()

	gid, err := s.CreateGroup("rm-conc", alice, []int{bob, carol}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, eid := range []int{bob, carol} {
		wg.Add(1)
		go func(i, eid int) {
			defer wg.Done()
			errs[i] = s.RemoveGroupMember(ctx, gid, eid)
		}(i, eid)
	}
	wg.Wait()

	succ := 0
	for _, e := range errs {
		if e == nil {
			succ++
		}
	}
	if succ > 1 {
		t.Fatalf("concurrent removals: %d succeeded, want at most 1 (errs %v)", succ, errs)
	}
	got, err := s.GroupMemberIDs(gid)
	if err != nil {
		t.Fatalf("GroupMemberIDs: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("members after concurrent removals = %v, want >= 2 (errs %v)", got, errs)
	}
}

// TestDeleteGroup (S26): deletes the group and cascades to group_members,
// group_prefs and the group's messages; conv_reads cursors for the group are
// cleared; tasks survive with group_id NULL (ON DELETE SET NULL). The
// in-transaction open-task guard rejects deletion while a task is open
// (ErrGroupHasOpenTasks). Unknown group is ErrNotFound.
func TestDeleteGroup(t *testing.T) {
	s := openTestStore(t)
	alice, bob, carol := seedChatOrg(t, s)
	ctx := context.Background()

	gid, err := s.CreateGroup("doomed", alice, []int{bob, carol}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if _, err := s.CreateGroupMessage(gid, MsgChat, "m1", intp(alice), nil, nil); err != nil {
		t.Fatalf("CreateGroupMessage: %v", err)
	}
	if err := s.SetGroupPrefs(bob, gid, nil, nil); err != nil {
		t.Fatalf("SetGroupPrefs: %v", err)
	}
	// A read cursor for the group conversation (peer_id = -(gid+1)).
	if _, err := s.MarkConversationRead(bob, groupPeerID(gid), 1); err != nil {
		t.Fatalf("MarkConversationRead: %v", err)
	}
	// An open task anchored to the group: it must survive deletion with
	// group_id set to NULL.
	tid, err := s.CreateTask(bob, KindDispatch, "open job", nil, nil, alice, 0, intp(gid))
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if err := s.DeleteGroup(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteGroup(unknown) = %v, want ErrNotFound", err)
	}
	// In-transaction guard: an open task blocks deletion.
	var openErr ErrGroupHasOpenTasks
	if err := s.DeleteGroup(ctx, gid); !errors.As(err, &openErr) || openErr.Count != 1 {
		t.Fatalf("DeleteGroup with open task = %v, want ErrGroupHasOpenTasks{Count:1}", err)
	}
	if g, gerr := s.GetGroup(gid); gerr != nil || g.ID != gid {
		t.Fatalf("group %d not intact after refused delete: %v %+v", gid, gerr, g)
	}
	if err := s.SetTaskStatus(tid, TaskCanceled, "S26 guard test"); err != nil {
		t.Fatalf("SetTaskStatus(canceled): %v", err)
	}
	if err := s.DeleteGroup(ctx, gid); err != nil {
		t.Fatalf("DeleteGroup after task canceled: %v", err)
	}

	// Group row is gone.
	if _, err := s.GetGroup(gid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetGroup after delete = %v, want ErrNotFound", err)
	}
	// Members are gone (cascade).
	if got, err := s.GroupMemberIDs(gid); err != nil || len(got) != 0 {
		t.Fatalf("GroupMemberIDs after delete = %v (err %v), want empty", got, err)
	}
	// Prefs are gone (cascade).
	if _, _, err := s.GetGroupPrefs(bob, gid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetGroupPrefs after delete = %v, want ErrNotFound", err)
	}
	// Messages are gone (cascade).
	var msgCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE group_id = ?`, gid).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 0 {
		t.Fatalf("messages after delete = %d, want 0", msgCount)
	}
	// Conv read cursors are cleared.
	var readCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM conv_reads WHERE peer_id = ?`, groupPeerID(gid)).Scan(&readCount); err != nil {
		t.Fatalf("count conv_reads: %v", err)
	}
	if readCount != 0 {
		t.Fatalf("conv_reads after delete = %d, want 0", readCount)
	}
	// The task survives with group_id NULL.
	tk, err := s.GetTask(tid)
	if err != nil {
		t.Fatalf("GetTask after delete: %v", err)
	}
	if tk.GroupID != nil {
		t.Fatalf("task.group_id = %v after delete, want nil", *tk.GroupID)
	}
}
