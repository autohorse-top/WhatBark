package store

import (
	"testing"
)

// TestSearchMentions covers S36 (v1.2.x item 13): mention-history search.
//
// Setup:
//   - employees: alice (leader), bob (worker), carol (worker) via seedChatOrg
//   - group g1 = {alice, bob}; group g2 = {boss(0), bob, carol}
//   - a spread of 1:1 and group messages, some mentioning the viewer, some not
//
// Spec case 5b (1:1 employee -> boss "@boss") cannot occur in this product:
// routes.Allowed rejects to<=0 and CreateMessage requires a valid toID, so no
// 1:1 message can ever carry a mention row for employee_id 0. The boss
// 1:1 visibility clause is still verified against a row inserted directly,
// to prove the SQL behaves per spec even though the product never writes it.
func TestSearchMentions(t *testing.T) {
	s := openTestStore(t)
	alice, bob, carol := seedChatOrg(t, s)

	g1, err := s.CreateGroup("s36g1", alice, []int{bob}, "")
	if err != nil {
		t.Fatalf("create g1: %v", err)
	}
	g2, err := s.CreateGroup("s36g2", 0, []int{bob, carol}, "")
	if err != nil {
		t.Fatalf("create g2: %v", err)
	}

	one2one := func(to int, body string, from *int) int {
		t.Helper()
		id, err := s.CreateMessage(to, MsgChat, body, from, nil, nil, nil)
		if err != nil {
			t.Fatalf("create 1:1 %q: %v", body, err)
		}
		return id
	}
	inGroup := func(gid int, body string, from *int) int {
		t.Helper()
		id, err := s.CreateGroupMessage(gid, MsgChat, body, from, nil, nil)
		if err != nil {
			t.Fatalf("create group msg %q: %v", body, err)
		}
		return id
	}

	mAlice1 := one2one(alice, "@alice s36m1", intp(bob))         // mentions alice
	mAlice2 := one2one(alice, "s36m2 plain", intp(bob))          // no mention
	mCarol := one2one(carol, "@carol s36m3", intp(bob))          // mentions carol
	mSelfOther := one2one(bob, "@bob s36sentother", intp(alice)) // alice sent, mentions bob
	mG1 := inGroup(g1, "@alice s36g1", intp(bob))                // group, mentions alice
	mG2Boss := inGroup(g2, "@boss s36boss", intp(bob))           // group, mentions boss
	mSelf := inGroup(g1, "@bob s36self", intp(bob))              // self-mention -> no row
	mPctA := one2one(alice, "@alice s36pctA 100%", intp(bob))    // literal % in body
	mPctB := one2one(alice, "@alice s36pctB 100100", intp(bob))
	mBoss1 := one2one(bob, "@bob s36b1", nil) // boss 1:1, mentions bob
	limA := one2one(alice, "@alice s36limA", intp(bob))
	limB := one2one(alice, "@alice s36limB", intp(bob))
	limC := one2one(alice, "@alice s36limC", intp(bob))

	// Spec case 5b + system-kind exclusion: rows the product cannot create,
	// inserted directly to exercise the SQL branches.
	var rawBoss11, rawSys int64
	res, err := s.DB().Exec(
		`INSERT INTO messages (to_id, kind, body) VALUES (?, 'chat', '@boss s36raw')`, bob)
	if err != nil {
		t.Fatalf("raw boss 1:1 insert: %v", err)
	}
	rawBoss11, _ = res.LastInsertId()
	if _, err := s.DB().Exec(
		`INSERT INTO message_mentions (message_id, employee_id) VALUES (?, 0)`, rawBoss11,
	); err != nil {
		t.Fatalf("raw mention insert: %v", err)
	}
	res, err = s.DB().Exec(
		`INSERT INTO messages (to_id, kind, body) VALUES (?, 'system', '@boss s36sys')`, bob)
	if err != nil {
		t.Fatalf("raw system insert: %v", err)
	}
	rawSys, _ = res.LastInsertId()
	if _, err := s.DB().Exec(
		`INSERT INTO message_mentions (message_id, employee_id) VALUES (?, 0)`, rawSys,
	); err != nil {
		t.Fatalf("raw system mention insert: %v", err)
	}

	// Sanity: mention rows exist as the S20 rules dictate (otherwise the
	// search cases below would pass trivially with zero hits).
	checkMentions := func(msgID int, want ...int) {
		t.Helper()
		got, err := s.MessageMentions(msgID)
		if err != nil {
			t.Fatalf("MessageMentions(%d): %v", msgID, err)
		}
		if !sameInts(got, want) {
			t.Fatalf("MessageMentions(%d) = %v, want %v", msgID, got, want)
		}
	}
	checkMentions(mAlice1, alice)
	checkMentions(mG2Boss, 0)
	checkMentions(mSelf)   // self-mention excluded
	checkMentions(mAlice2) // no mention at all
	checkMentions(mSelfOther, bob)

	cases := []struct {
		name   string
		q      string
		viewer int
		limit  int
		want   []int // expected ids newest-first; nil = empty result
	}{
		{"1:1 mentioned hit", "s36m1", alice, 0, []int{mAlice1}},
		{"1:1 body match without mention", "s36m2", alice, 0, nil},
		{"1:1 mentioning a third employee", "s36m3", alice, 0, nil},
		{"third employee sees own 1:1 mention", "s36m3", carol, 0, []int{mCarol}},
		{"group member mentioned hit", "s36g1", alice, 0, []int{mG1}},
		{"group msg hidden from non-member", "s36g1", carol, 0, nil},
		{"boss @boss in own group hit", "s36boss", 0, 0, []int{mG2Boss}},
		{"@boss msg hidden from unmentioned member", "s36boss", carol, 0, nil},
		{"sent msg mentioning other not in own mentions", "s36selfother", alice, 0, nil},
		{"self mention has no row", "s36self", bob, 0, nil},
		{"LIKE escape: % is literal", "100%", alice, 0, []int{mPctA}},
		{"LIKE escape: plain match unaffected", "s36pctB", alice, 0, []int{mPctB}},
		{"boss 1:1 @employee hit", "s36b1", bob, 0, []int{mBoss1}},
		{"limit=2 returns two newest", "s36lim", alice, 2, []int{limC, limB}},
		{"limit default keeps all three", "s36lim", alice, 0, []int{limC, limB, limA}},
		{"no match -> empty slice", "zzzqqq", alice, 0, nil},
		{"spec 5b: raw boss->employee 1:1 @boss row (from_id NULL) visible to boss", "s36raw", 0, 0, []int{int(rawBoss11)}},
		{"system kind excluded even with mention row", "s36sys", 0, 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.SearchMentions(c.q, c.viewer, c.limit)
			if err != nil {
				t.Fatalf("SearchMentions(%q, viewer=%d, limit=%d): %v", c.q, c.viewer, c.limit, err)
			}
			gotIDs := make([]int, 0, len(got))
			for _, m := range got {
				gotIDs = append(gotIDs, m.ID)
			}
			if !sameInts(gotIDs, c.want) {
				t.Fatalf("SearchMentions(%q, viewer=%d, limit=%d) = %v, want %v",
					c.q, c.viewer, c.limit, gotIDs, c.want)
			}
		})
	}

	// The boss 1:1 row must carry no sender (from_id NULL) per the data model.
	got, err := s.SearchMentions("s36raw", 0, 10)
	if err != nil {
		t.Fatalf("re-query s36raw: %v", err)
	}
	if len(got) != 1 || got[0].FromID != nil {
		t.Fatalf("boss 1:1 row = %+v, want FromID nil", got)
	}
}
