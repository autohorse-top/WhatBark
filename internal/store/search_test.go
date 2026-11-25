package store

import (
	"testing"
)

// TestSearchMessages covers the S19-C store search: participant restriction
// (1:1 sent/received + own groups), LIKE literal escaping of % and _,
// case-insensitive ASCII matching, group rows carry GroupID with ToID nil,
// newest-first ordering, and the limit cap.
func TestSearchMessages(t *testing.T) {
	s := openTestStore(t)
	alice, bob, carol := seedChatOrg(t, s)

	// Message ids: 1..6 in creation order.
	mA, _ := s.CreateMessage(bob, MsgChat, "alpha marker one", intp(alice), nil, nil, nil) // bob<->alice
	mB, _ := s.CreateMessage(alice, MsgChat, "beta marker two", intp(bob), nil, nil, nil)  // alice<->bob
	mC, _ := s.CreateMessage(bob, MsgDispatch, "gamma marker three", nil, nil, nil, nil)   // boss->bob
	mD, _ := s.CreateMessage(bob, MsgChat, "price 199 dollars", intp(alice), nil, nil, nil)
	gid, err := s.CreateGroup("sg", alice, []int{bob}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	mF, _ := s.CreateGroupMessage(gid, MsgChat, "delta marker four", intp(alice), nil, nil) // group alice+bob
	mG, _ := s.CreateMessage(bob, MsgChat, "Zebra crossing", intp(alice), nil, nil, nil)

	cases := []struct {
		name string
		q    string
		part int
		want []int // ids, newest first
	}{
		{"bob sees his own traffic incl. group", "marker", bob, []int{mF, mC, mB, mA}},
		{"alice sees shared 1:1 + group", "marker", alice, []int{mF, mB, mA}},
		{"carol is a participant of none", "marker", carol, []int{}},
		{"underscore is literal (19_ must not match 199)", "19_", bob, []int{}},
		{"plain digits match", "199", bob, []int{mD}},
		{"percent is literal (price% must not match price)", "price%", bob, []int{}},
		{"case-insensitive ASCII match", "zebra", bob, []int{mG}},
		{"empty result stays empty", "no-such-body", bob, []int{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.SearchMessages(tc.q, tc.part, 20)
			if err != nil {
				t.Fatalf("SearchMessages(%q, %d): %v", tc.q, tc.part, err)
			}
			ids := make([]int, 0, len(got))
			for _, m := range got {
				ids = append(ids, m.ID)
			}
			if !sameInts(ids, tc.want) {
				t.Fatalf("SearchMessages(%q, %d) = %v, want %v", tc.q, tc.part, ids, tc.want)
			}
		})
	}

	// Group rows: GroupID set, ToID nil.
	got, err := s.SearchMessages("delta", bob, 20)
	if err != nil || len(got) != 1 {
		t.Fatalf("search delta: got %d rows err=%v, want 1", len(got), err)
	}
	if got[0].GroupID == nil || *got[0].GroupID != gid {
		t.Fatalf("group row GroupID = %v, want %d", got[0].GroupID, gid)
	}
	if got[0].ToID != nil {
		t.Fatalf("group row ToID = %v, want nil", *got[0].ToID)
	}

	// M1 (S32): the boss (participant < 0) sees every 1:1 message plus group
	// messages of groups the boss is a member of — a hit in a group the boss
	// is not in would 403 on thread open, so it must stay out of results.
	gBoss, err := s.CreateGroup("Boss Crew", 0, []int{alice}, "")
	if err != nil {
		t.Fatalf("CreateGroup Boss Crew: %v", err)
	}
	mBoss, _ := s.CreateGroupMessage(gBoss, MsgChat, "omega boss crew", intp(bob), nil, nil)

	bossIDs := func(q string) ([]int, error) {
		ms, err := s.SearchMessages(q, -1, 50)
		if err != nil {
			return nil, err
		}
		ids := make([]int, 0, len(ms))
		for _, m := range ms {
			ids = append(ids, m.ID)
		}
		return ids, nil
	}
	hasID := func(ids []int, id int) bool {
		for _, x := range ids {
			if x == id {
				return true
			}
		}
		return false
	}

	ids, err := bossIDs("gamma marker three")
	if err != nil {
		t.Fatalf("boss search 1:1: %v", err)
	}
	if !hasID(ids, mC) {
		t.Fatalf("boss must see own 1:1 message %d, got %v", mC, ids)
	}
	ids, err = bossIDs("omega boss crew")
	if err != nil {
		t.Fatalf("boss search own group: %v", err)
	}
	if !hasID(ids, mBoss) {
		t.Fatalf("boss must see group message of own group %d, got %v", mBoss, ids)
	}
	// mF lives in gid (alice+bob) — the boss is NOT a member.
	ids, err = bossIDs("delta marker four")
	if err != nil {
		t.Fatalf("boss search non-member group: %v", err)
	}
	if hasID(ids, mF) {
		t.Fatalf("boss must NOT see non-member group message %d, got %v", mF, ids)
	}

	// Limit: 3 matching messages, limit 2 -> newest 2 only.
	for i := 1; i <= 3; i++ {
		if _, err := s.CreateMessage(bob, MsgChat, "seq row", intp(alice), nil, nil, nil); err != nil {
			t.Fatalf("seq %d: %v", i, err)
		}
	}
	limit2, err := s.SearchMessages("seq row", bob, 2)
	if err != nil {
		t.Fatalf("limit search: %v", err)
	}
	if len(limit2) != 2 {
		t.Fatalf("limit 2 returned %d rows, want 2", len(limit2))
	}
	if !(limit2[0].ID > limit2[1].ID) {
		t.Fatalf("order not newest first: %d then %d", limit2[0].ID, limit2[1].ID)
	}
}

// TestSearchGroupNames covers the S32 group-name search: membership
// restriction (viewer sees only groups they belong to, boss included),
// case-insensitive ASCII matching, LIKE-literal % and _ escaping, kind and
// member_count passthrough, newest-first ordering and the limit cap.
func TestSearchGroupNames(t *testing.T) {
	s := openTestStore(t)
	alice, bob, carol := seedChatOrg(t, s)

	gTeam, err := s.CreateGroup("Alpha Team", alice, []int{bob}, "")
	if err != nil {
		t.Fatalf("CreateGroup Alpha Team: %v", err)
	}
	// Channels are boss-only (S21): creator must be 0.
	gChan, err := s.CreateGroup("alpha channel", 0, []int{alice, bob}, "channel")
	if err != nil {
		t.Fatalf("CreateGroup alpha channel: %v", err)
	}
	wildName := "Unique_%_Name"
	gWild, err := s.CreateGroup(wildName, alice, []int{bob}, "")
	if err != nil {
		t.Fatalf("CreateGroup wildcard group: %v", err)
	}
	// Boss-created group without alice/bob: membership scoping must apply
	// to the boss (viewer 0) as well.
	gBoss, err := s.CreateGroup("Solo Boss Crew", 0, []int{carol}, "")
	if err != nil {
		t.Fatalf("CreateGroup Solo Boss Crew: %v", err)
	}

	cases := []struct {
		name   string
		q      string
		viewer int
		want   []int // group ids, newest first
	}{
		{"member sees both alpha groups (newest first)", "alpha", bob, []int{gChan, gTeam}},
		{"creator is a member too", "alpha", alice, []int{gChan, gTeam}},
		{"non-member sees nothing", "alpha", carol, []int{}},
		{"boss sees only channel he created, not Alpha Team", "alpha", 0, []int{gChan}},
		{"carol is a member of the boss group", "Solo Boss", carol, []int{gBoss}},
		{"bob is not a member of the boss group", "Solo Boss", bob, []int{}},
		{"case-insensitive ASCII match", "ALPHA", bob, []int{gChan, gTeam}},
		{"full name with % and _ matches literally", "Unique_%_Name", bob, []int{gWild}},
		{"% stays literal (no wildcard)", "Unique%Name", bob, []int{}},
		{"_ stays literal (no wildcard)", "UniqueX_Name", bob, []int{}},
		{"no match stays empty", "no-such-group", bob, []int{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.SearchGroupNames(tc.q, tc.viewer, 20)
			if err != nil {
				t.Fatalf("SearchGroupNames(%q, %d): %v", tc.q, tc.viewer, err)
			}
			ids := make([]int, 0, len(got))
			for _, h := range got {
				ids = append(ids, h.GroupID)
			}
			if !sameInts(ids, tc.want) {
				t.Fatalf("SearchGroupNames(%q, %d) = %v, want %v", tc.q, tc.viewer, ids, tc.want)
			}
		})
	}

	// Kind + member_count passthrough.
	got, err := s.SearchGroupNames("alpha", bob, 20)
	if err != nil || len(got) != 2 {
		t.Fatalf("search alpha: got %d rows err=%v, want 2", len(got), err)
	}
	byID := map[int]GroupSearchHit{}
	for _, h := range got {
		byID[h.GroupID] = h
	}
	if byID[gTeam].Kind != "group" || byID[gChan].Kind != "channel" {
		t.Fatalf("kind passthrough: team=%q chan=%q, want group/channel",
			byID[gTeam].Kind, byID[gChan].Kind)
	}
	if byID[gTeam].MemberCount != 2 || byID[gTeam].Name != "Alpha Team" {
		t.Fatalf("team row = %+v, want name=Alpha Team member_count=2", byID[gTeam])
	}

	// Limit: after creating two more, 3 groups match "alpha team"; limit 1
	// returns only the newest.
	for _, extra := range []string{"Alpha Team Two", "Alpha Team Three"} {
		if _, err := s.CreateGroup(extra, alice, []int{bob}, ""); err != nil {
			t.Fatalf("CreateGroup %s: %v", extra, err)
		}
	}
	limited, err := s.SearchGroupNames("alpha team", bob, 1)
	if err != nil {
		t.Fatalf("limit search: %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("limit 1 returned %d rows, want 1", len(limited))
	}
	all, err := s.SearchGroupNames("alpha team", bob, 20)
	if err != nil {
		t.Fatalf("all search: %v", err)
	}
	if len(all) != 3 || limited[0].GroupID != all[0].GroupID {
		t.Fatalf("limit mismatch: limited=%+v all[0]=%+v", limited, all)
	}
}
