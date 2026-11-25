package store

import (
	"reflect"
	"slices"
	"testing"
)

// TestEmployeeIDByName: exact, case-sensitive name lookup.
func TestEmployeeIDByName(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)

	cases := []struct {
		name   string
		wantID int
		wantOK bool
	}{
		{"alice", leader, true},
		{"bob", worker, true},
		{"Alice", 0, false}, // case-sensitive (BINARY collation)
		{"bob2", 0, false},  // no prefix matching
		{"nobody", 0, false},
	}
	for _, c := range cases {
		id, ok, err := s.EmployeeIDByName(c.name)
		if err != nil {
			t.Fatalf("EmployeeIDByName(%q): %v", c.name, err)
		}
		if id != c.wantID || ok != c.wantOK {
			t.Fatalf("EmployeeIDByName(%q) = (%d, %v), want (%d, %v)", c.name, id, ok, c.wantID, c.wantOK)
		}
	}
}

// TestMentionIDsIn: table-driven extraction rules (S20).
func TestMentionIDsIn(t *testing.T) {
	s := openTestStore(t)
	root, _, leader, worker := seedOrg(t, s)
	carol, err := s.CreateEmployee("carol", "worker", KindAI, root)
	if err != nil {
		t.Fatalf("create carol: %v", err)
	}

	cases := []struct {
		name   string
		body   string
		parts  []int
		sender int
		want   []int // nil means empty
	}{
		{"empty body", "", []int{leader, worker}, leader, nil},
		{"no mention token", "hello world", []int{leader, worker}, leader, nil},
		{"participant mention", "@bob 请看一下", []int{leader, worker}, leader, []int{worker}},
		{"self mention ignored", "@bob 在吗", []int{leader, worker}, worker, nil},
		{"non participant dropped", "@carol 在吗", []int{leader, worker}, leader, nil},
		{"unknown token dropped", "@nobody 在吗", []int{leader, worker}, leader, nil},
		{"case sensitive names", "@Bob 在吗", []int{leader, worker}, leader, nil},
		{"partial token not a name", "@bob2 在吗", []int{leader, worker}, leader, nil},
		{"multiple deduped and sorted", "@carol hi @bob @carol", []int{leader, worker, carol}, leader, []int{worker, carol}},
		{"boss token with boss participant", "@boss 在吗", []int{0, leader}, leader, []int{0}},
		{"boss token without boss participant", "@boss 在吗", []int{leader, worker}, leader, nil},
		{"Boss is not the boss token", "@Boss 在吗", []int{0, leader}, leader, nil},
		{"boss self-mention ignored", "@boss 在吗", []int{0, worker}, 0, nil},
		{"boss sender mentioning member", "@bob 在吗", []int{0, worker}, 0, []int{worker}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.MentionIDsIn(c.body, c.parts, c.sender)
			if err != nil {
				t.Fatalf("MentionIDsIn: %v", err)
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("MentionIDsIn(%q, %v, sender=%d) = %v, want %v", c.body, c.parts, c.sender, got, c.want)
			}
		})
	}
}

// TestMessageMentionsRoundTrip: insert, read back, idempotent re-add,
// FK guard, cascade delete with the message.
func TestMessageMentionsRoundTrip(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)

	if ids, err := s.MessageMentions(9999); err != nil || len(ids) != 0 {
		t.Fatalf("MessageMentions(unknown) = %v, %v; want empty, nil", ids, err)
	}
	if err := s.AddMessageMentions(9999, []int{leader}); err == nil {
		t.Fatal("AddMessageMentions on missing message accepted, want FK error")
	}

	mid, err := s.CreateMessage(worker, MsgChat, "@bob hi", intp(leader), nil, nil, nil)
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	ids, err := s.MessageMentions(mid)
	if err != nil {
		t.Fatalf("MessageMentions: %v", err)
	}
	if !reflect.DeepEqual(ids, []int{worker}) {
		t.Fatalf("mentions = %v, want [%d]", ids, worker)
	}
	// Idempotent re-add (composite PK, INSERT OR IGNORE).
	if err := s.AddMessageMentions(mid, []int{worker}); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if ids, _ = s.MessageMentions(mid); len(ids) != 1 {
		t.Fatalf("mentions after re-add = %v, want 1 row", ids)
	}

	// Cascade: deleting the message removes its mentions.
	if _, err := s.DB().Exec(`DELETE FROM messages WHERE id = ?`, mid); err != nil {
		t.Fatalf("delete message: %v", err)
	}
	if ids, err := s.MessageMentions(mid); err != nil || len(ids) != 0 {
		t.Fatalf("mentions after cascade delete = %v, %v; want empty, nil", ids, err)
	}
}

// TestMentionedUnread: 1:1 mention-unread count vs. read cursor.
func TestMentionedUnread(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)

	m1, err := s.CreateMessage(worker, MsgChat, "@bob 在吗", intp(leader), nil, nil, nil)
	if err != nil {
		t.Fatalf("create m1: %v", err)
	}
	if _, err := s.CreateMessage(worker, MsgChat, "no mention", intp(leader), nil, nil, nil); err != nil {
		t.Fatalf("create m2: %v", err)
	}
	n, err := s.MentionedUnread(worker, leader)
	if err != nil {
		t.Fatalf("MentionedUnread: %v", err)
	}
	if n != 1 {
		t.Fatalf("mentioned unread = %d, want 1 (only the @bob message)", n)
	}

	// Cursor boundary: reading up to m1 excludes it (id > cursor).
	if _, err := s.MarkConversationRead(worker, leader, m1); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if n, _ = s.MentionedUnread(worker, leader); n != 0 {
		t.Fatalf("mentioned unread after read = %d, want 0", n)
	}

	// New mention beyond the cursor counts again.
	if _, err := s.CreateMessage(worker, MsgChat, "@bob again", intp(leader), nil, nil, nil); err != nil {
		t.Fatalf("create m3: %v", err)
	}
	if n, _ = s.MentionedUnread(worker, leader); n != 1 {
		t.Fatalf("mentioned unread after new mention = %d, want 1", n)
	}

	// Boss (from_id NULL) mentions: separate peer-0 conversation.
	if _, err := s.CreateMessage(worker, MsgChat, "@bob 任务来了", nil, nil, nil, nil); err != nil {
		t.Fatalf("boss message: %v", err)
	}
	if n, err := s.MentionedUnread(worker, 0); err != nil || n != 1 {
		t.Fatalf("boss mentioned unread = %d, %v; want 1", n, err)
	}
}

// TestGroupMentionedUnread: group mention-unread rules (member guard,
// system-message exclusion, self-sent exclusion, cursor boundary).
func TestGroupMentionedUnread(t *testing.T) {
	s := openTestStore(t)
	root, _, leader, worker := seedOrg(t, s)
	carol, err := s.CreateEmployee("carol", "worker", KindAI, root)
	if err != nil {
		t.Fatalf("create carol: %v", err)
	}
	gid, err := s.CreateGroup("g", leader, []int{worker, 0}, "")
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	m1, err := s.CreateGroupMessage(gid, MsgChat, "@bob 在吗 @boss", intp(leader), nil, nil)
	if err != nil {
		t.Fatalf("group message: %v", err)
	}
	if ids, _ := s.MessageMentions(m1); !reflect.DeepEqual(ids, []int{0, worker}) {
		t.Fatalf("m1 mentions = %v, want [0 %d]", ids, worker)
	}
	if n, _ := s.GroupMentionedUnread(worker, gid); n != 1 {
		t.Fatalf("worker group mentioned unread = %d, want 1", n)
	}
	if n, _ := s.GroupMentionedUnread(0, gid); n != 1 {
		t.Fatalf("boss group mentioned unread = %d, want 1", n)
	}
	if n, _ := s.GroupMentionedUnread(leader, gid); n != 0 {
		t.Fatalf("sender's own mention counted = %d, want 0", n)
	}
	// Non-member sees 0 regardless of content.
	if n, _ := s.GroupMentionedUnread(carol, gid); n != 0 {
		t.Fatalf("non-member mentioned unread = %d, want 0", n)
	}

	// Worker's self-mention creates no rows.
	if m2, err := s.CreateGroupMessage(gid, MsgChat, "@bob 自己", intp(worker), nil, nil); err != nil {
		t.Fatalf("self-mention message: %v", err)
	} else if ids, _ := s.MessageMentions(m2); len(ids) != 0 {
		t.Fatalf("self-mention rows = %v, want none", ids)
	}

	// Worker mentioning the leader counts for the leader.
	if _, err := s.CreateGroupMessage(gid, MsgChat, "@alice 看一下", intp(worker), nil, nil); err != nil {
		t.Fatalf("mention leader: %v", err)
	}
	if n, _ := s.GroupMentionedUnread(leader, gid); n != 1 {
		t.Fatalf("leader group mentioned unread = %d, want 1", n)
	}

	// A system message carrying a mention row must not count (kind filter).
	res, err := s.DB().Exec(
		`INSERT INTO messages (from_id, to_id, kind, body, group_id) VALUES (NULL, NULL, 'system', '@bob 系统消息', ?)`, gid)
	if err != nil {
		t.Fatalf("insert system message: %v", err)
	}
	sysID, _ := res.LastInsertId()
	if err := s.AddMessageMentions(int(sysID), []int{worker}); err != nil {
		t.Fatalf("add system mention: %v", err)
	}
	if n, _ := s.GroupMentionedUnread(worker, gid); n != 1 {
		t.Fatalf("mentioned unread with system mention = %d, want 1 (system excluded)", n)
	}

	// Cursor boundary: reading up to m1 hides all later non-mentions for worker.
	if _, err := s.MarkConversationRead(worker, groupPeerID(gid), m1); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if n, _ := s.GroupMentionedUnread(worker, gid); n != 0 {
		t.Fatalf("mentioned unread after read = %d, want 0", n)
	}
}
