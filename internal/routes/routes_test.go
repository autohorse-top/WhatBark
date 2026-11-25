package routes

import "testing"

// Topology:
//
//	W1 -- L1 -- L2  (chain: W1 up to L1 up to L2)
//	W2 -- L2
//	L1 and L2 are leaders (chat lateral)
//	W1 has no leader above L2.
func testGraph() *Graph {
	up := map[int][]int{
		1:  {11, 12}, // W1 -> L1 -> L2
		2:  {12},     // W2 -> L2
		11: {12},     // L1 -> L2
	}
	isLeader := map[int]bool{11: true, 12: true}
	wedges := map[int]map[int]bool{
		1: {2: true}, // workflow edge W1 -> W2 (lateral, outside org chain)
	}
	return New(up, isLeader, wedges)
}

func TestAllowed(t *testing.T) {
	g := testGraph()
	cases := []struct {
		from, to int
		kind     string
		want     bool
	}{
		// boss can address anyone
		{0, 1, "dispatch", true},
		{0, 12, "report", true},
		// reporting up the chain
		{1, 11, "report", true},
		{1, 12, "report", true}, // two hops up
		{11, 12, "report", true},
		{1, 1, "report", false},  // self
		{2, 11, "report", false}, // not on W2's chain
		{11, 1, "report", false}, // down is not report
		// dispatch down the chain
		{12, 1, "dispatch", true},
		{12, 11, "dispatch", true},
		{1, 12, "dispatch", false}, // up is not dispatch
		// scheduled behaves like dispatch (TASK.md §7.5)
		{0, 1, "scheduled", true},
		{12, 1, "scheduled", true},
		{1, 12, "scheduled", false},
		// workflow edge
		{1, 2, "ask", true},
		{2, 1, "ask", false}, // edges are directed
		// lateral chat between leaders
		{11, 12, "chat", true},
		{12, 11, "chat", true},
		{1, 2, "chat", false}, // workers cannot chat
		// answer: allowed iff reverse ask is legal
		{2, 1, "answer", true},
		{1, 2, "answer", false},
		// unknown kind
		{0, 1, "shout", false},
	}
	for _, c := range cases {
		t.Run("", func(t *testing.T) {
			if got := g.Allowed(c.from, c.to, c.kind); got != c.want {
				t.Fatalf("Allowed(%d,%d,%q) = %v, want %v", c.from, c.to, c.kind, got, c.want)
			}
		})
	}
}

func TestCancelAllowed(t *testing.T) {
	g := testGraph()
	// task on W1 (emp 1), requested by L2 (12)
	cases := []struct {
		canceller int
		want      bool
	}{
		{0, true},    // boss
		{12, true},   // requester
		{1, true},    // owner
		{11, true},   // leader on owner's chain
		{2, false},   // unrelated worker
		{100, false}, // unknown
	}
	for _, c := range cases {
		if got := g.CancelAllowed(c.canceller, 1, 12); got != c.want {
			t.Fatalf("CancelAllowed(%d, task-of=1, requestedBy=12) = %v, want %v", c.canceller, got, c.want)
		}
	}
}
