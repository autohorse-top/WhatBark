package store

import (
	"testing"
)

// seedChatOrg creates three AI employees (leader + two workers) and returns
// their IDs (alice=leader, bob=worker, carol=worker).
func seedChatOrg(t *testing.T, s *Store) (alice, bob, carol int) {
	t.Helper()
	root, _, leader, worker := seedOrg(t, s)
	alice = leader
	bob = worker
	var err error
	if carol, err = s.CreateEmployee("carol", "worker", KindAI, root); err != nil {
		t.Fatalf("create carol: %v", err)
	}
	return alice, bob, carol
}

func convOf(t *testing.T, s *Store, viewer, wantPeer int) (Conversation, bool) {
	t.Helper()
	cons, err := s.ListConversations(viewer)
	if err != nil {
		t.Fatalf("ListConversations(%d): %v", viewer, err)
	}
	for _, c := range cons {
		if c.PeerID == wantPeer {
			return c, true
		}
	}
	return Conversation{}, false
}

func TestListConversations(t *testing.T) {
	s := openTestStore(t)
	alice, bob, carol := seedChatOrg(t, s)

	// Order matters for the "most recent first" assertion: carol first, then
	// the alice<->bob exchange, then the boss dispatch (newest overall).
	m4, err := s.CreateMessage(carol, MsgAsk, "status?", intp(alice), nil, nil, nil)
	if err != nil {
		t.Fatalf("m4: %v", err)
	}
	if _, err := s.CreateMessage(bob, MsgChat, "hi from alice", intp(alice), nil, nil, nil); err != nil {
		t.Fatalf("m1: %v", err)
	}
	m2, err := s.CreateMessage(alice, MsgChat, "hi back", intp(bob), nil, nil, nil)
	if err != nil {
		t.Fatalf("m2: %v", err)
	}
	m3, err := s.CreateMessage(bob, MsgDispatch, "do the thing", nil, nil, nil, nil) // boss -> bob
	if err != nil {
		t.Fatalf("m3: %v", err)
	}
	if _, err := s.CreateTask(bob, KindDispatch, "open work", nil, nil, alice, 0, nil); err != nil {
		t.Fatalf("task: %v", err)
	}

	cases := []struct {
		name       string
		viewer     int
		peer       int
		wantLastID int
		wantUnread int
		wantOpen   int
	}{
		// Unread counts incoming messages from the peer beyond the (zero)
		// cursor; open tasks count queued/running work linking the two.
		{"alice sees bob: unread reply + open task", alice, bob, m2, 1, 1},
		{"alice sees carol: sent-only, no unread", alice, carol, m4, 0, 0},
		{"bob sees alice: unread + her open task", bob, alice, m2, 1, 1},
		{"bob sees boss: dispatch unread, no open tasks", bob, 0, m3, 1, 0},
		{"boss sees bob: outgoing only, nothing to boss", 0, bob, m3, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := convOf(t, s, tc.viewer, tc.peer)
			if !ok {
				t.Fatalf("peer %d not in viewer %d conversations", tc.peer, tc.viewer)
			}
			if c.LastMsgID != tc.wantLastID {
				t.Errorf("last_msg_id = %d, want %d", c.LastMsgID, tc.wantLastID)
			}
			if c.Unread != tc.wantUnread {
				t.Errorf("unread = %d, want %d", c.Unread, tc.wantUnread)
			}
			if c.OpenTasks != tc.wantOpen {
				t.Errorf("open_tasks = %d, want %d", c.OpenTasks, tc.wantOpen)
			}
		})
	}

	// Most-recent first: bob's thread (m2) is newer than carol's (m4).
	cons, err := s.ListConversations(alice)
	if err != nil {
		t.Fatalf("list alice: %v", err)
	}
	if len(cons) != 2 || cons[0].PeerID != bob {
		t.Fatalf("alice conversation order = %+v, want bob first", cons)
	}
}

func TestConversationsEmpty(t *testing.T) {
	s := openTestStore(t)
	seedChatOrg(t, s)
	cons, err := s.ListConversations(0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(cons) != 0 {
		t.Fatalf("expected no conversations, got %+v", cons)
	}
}

func TestConversationReadCursor(t *testing.T) {
	s := openTestStore(t)
	alice, bob, _ := seedChatOrg(t, s)

	m1, err := s.CreateMessage(bob, MsgDispatch, "one", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("m1: %v", err)
	}
	m2, err := s.CreateMessage(bob, MsgDispatch, "two", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("m2: %v", err)
	}

	// Read up to m1: m2 stays unread; cursor advanced -> reports changed.
	advanced, err := s.MarkConversationRead(bob, 0, m1)
	if err != nil {
		t.Fatalf("mark read m1: %v", err)
	}
	if !advanced {
		t.Fatal("mark read m1: want changed=true")
	}
	if cur, ok := convOf(t, s, bob, 0); !ok || cur.Unread != 1 {
		t.Fatalf("unread after partial read = %+v, want 1", cur)
	}
	// Monotonic: a smaller id never moves the cursor back, and a no-op mark
	// reports changed=false so callers can skip emitting message.read.
	if advanced, err = s.MarkConversationRead(bob, 0, 0); err != nil {
		t.Fatalf("mark read smaller: %v", err)
	} else if advanced {
		t.Fatal("mark read smaller: want changed=false for regressive cursor")
	}
	if cur, _ := convOf(t, s, bob, 0); cur.Unread != 1 {
		t.Fatalf("unread after regressive mark = %d, want 1", cur.Unread)
	}
	// Read up to m2: all clear.
	if advanced, err = s.MarkConversationRead(bob, 0, m2); err != nil {
		t.Fatalf("mark read m2: %v", err)
	} else if !advanced {
		t.Fatal("mark read m2: want changed=true")
	}
	if cur, _ := convOf(t, s, bob, 0); cur.Unread != 0 {
		t.Fatalf("unread after full read = %d, want 0", cur.Unread)
	}

	// Employee-side cursor is independent; upsert path for a fresh viewer.
	if _, err := s.CreateMessage(alice, MsgChat, "ping", intp(bob), nil, nil, nil); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if advanced, err = s.MarkConversationRead(alice, bob, 0); err != nil {
		t.Fatalf("mark read alice: %v", err)
	} else if !advanced {
		t.Fatal("mark read alice: fresh cursor wants changed=true")
	}
}

func TestThreadMessages(t *testing.T) {
	s := openTestStore(t)
	alice, bob, carol := seedChatOrg(t, s)

	a1, _ := s.CreateMessage(bob, MsgChat, "a1", intp(alice), nil, nil, nil)
	b1, _ := s.CreateMessage(alice, MsgChat, "b1", intp(bob), nil, nil, nil)
	a2, _ := s.CreateMessage(bob, MsgChat, "a2", intp(alice), nil, nil, nil)
	// Outside the alice<->bob thread:
	cross, _ := s.CreateMessage(carol, MsgAsk, "x", intp(alice), nil, nil, nil)
	bossMsg, _ := s.CreateMessage(bob, MsgDispatch, "boss", nil, nil, nil, nil)

	cases := []struct {
		name    string
		viewer  int
		peer    int
		limit   int
		wantIDs []int
	}{
		{"full alice<->bob thread", alice, bob, 0, []int{a1, b1, a2}},
		{"limit 2 keeps newest, still ascending", alice, bob, 2, []int{b1, a2}},
		{"thread is symmetric", bob, alice, 0, []int{a1, b1, a2}},
		{"boss thread sees only boss messages", bob, 0, 0, []int{bossMsg}},
		{"no cross-thread leakage", alice, carol, 0, []int{cross}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ThreadMessages(tc.viewer, tc.peer, tc.limit)
			if err != nil {
				t.Fatalf("ThreadMessages: %v", err)
			}
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("got %d messages, want %d: %+v", len(got), len(tc.wantIDs), got)
			}
			for i, m := range got {
				if m.ID != tc.wantIDs[i] {
					t.Fatalf("msg[%d] = %d, want %d", i, m.ID, tc.wantIDs[i])
				}
			}
		})
	}

	// Boss view of the same thread: boss can only see what it sent.
	got, err := s.ThreadMessages(0, bob, 0)
	if err != nil {
		t.Fatalf("boss thread: %v", err)
	}
	if len(got) != 1 || got[0].ID != bossMsg {
		t.Fatalf("boss thread = %+v, want only bossMsg", got)
	}
}

func TestLatestMessageIDTo(t *testing.T) {
	s := openTestStore(t)
	alice, bob, _ := seedChatOrg(t, s)
	m1, _ := s.CreateMessage(bob, MsgChat, "one", intp(alice), nil, nil, nil)
	m2, _ := s.CreateMessage(alice, MsgChat, "two", intp(bob), nil, nil, nil)

	if id, err := s.LatestMessageIDTo(bob, alice); err != nil || id != m1 {
		t.Fatalf("bob<-alice = (%d, %v), want (%d, nil)", id, err, m1)
	}
	if id, err := s.LatestMessageIDTo(alice, bob); err != nil || id != m2 {
		t.Fatalf("alice<-bob = (%d, %v), want (%d, nil)", id, err, m2)
	}
	if id, err := s.LatestMessageIDTo(alice, 999); err != nil || id != 0 {
		t.Fatalf("empty thread = (%d, %v), want (0, nil)", id, err)
	}
}
