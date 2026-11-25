// Package routes enforces who may talk to whom and who may cancel what.
// Decisions are pure functions over a precomputed Graph.
package routes

// Graph is the precomputed topology used for route enforcement.
type Graph struct {
	// Up maps an employee to the leader chain above it (nearest first),
	// with gap segments already dropped.
	Up map[int][]int
	// Down maps an employee to every employee that reports to it
	// through any number of chain hops (reverse of Up).
	Down map[int]map[int]bool
	// IsLeader marks department leaders (lateral chat).
	IsLeader map[int]bool
	// WEdges holds workflow edges merged across workflows: from -> {to}.
	WEdges map[int]map[int]bool
}

// New builds a Graph.
//
//	up:      employee -> leader chain (nearest first, gaps dropped)
//	isLeader: employee -> is a department leader
//	wEdges:  workflow edges from -> {to} (merged across workflows)
func New(up map[int][]int, isLeader map[int]bool, wEdges map[int]map[int]bool) *Graph {
	down := make(map[int]map[int]bool, len(up))
	for emp, chain := range up {
		for _, leader := range chain {
			if down[leader] == nil {
				down[leader] = make(map[int]bool)
			}
			down[leader][emp] = true
		}
	}
	return &Graph{Up: up, Down: down, IsLeader: isLeader, WEdges: wEdges}
}

// Allowed reports whether "from" may send a message of "kind" to "to".
// from == 0 means the boss (admin token), who may address anyone for a
// valid kind; an unknown kind is always rejected.
func (g *Graph) Allowed(from, to int, kind string) bool {
	if to <= 0 || from == to {
		return false
	}
	switch kind {
	case "report", "summary":
		if from == 0 {
			return true
		}
		for _, l := range g.Up[from] {
			if l == to {
				return true
			}
		}
		return false
	case "dispatch", "ask", "scheduled":
		if from == 0 {
			return true
		}
		if g.Down[from] != nil && g.Down[from][to] {
			return true
		}
		return g.WEdges[from][to]
	case "chat":
		if from == 0 {
			return true
		}
		return g.IsLeader[from] && g.IsLeader[to]
	case "selfreview":
		// Only the scheduler (boss/system, from==0) injects self-reviews.
		return from == 0
	case "review":
		// System-only (leader orchestration): review wakes are injected by
		// FinishTask (from==0, the boss/system form); only the admin may
		// send one by hand. Employees can never forge a review task.
		return from == 0
	case "answer":
		if from == 0 {
			return true
		}
		return g.Allowed(to, from, "ask")
	default:
		return false
	}
}

// CancelAllowed reports whether "canceller" may cancel a task owned by
// "taskEmployee" that was requested by "requestedBy" (0 = boss).
// Allowed: the boss, the requester, the owner, or any leader on the
// owner's reporting chain.
func (g *Graph) CancelAllowed(canceller, taskEmployee, requestedBy int) bool {
	if canceller == 0 {
		return true
	}
	if canceller == requestedBy || canceller == taskEmployee {
		return true
	}
	for _, l := range g.Up[taskEmployee] {
		if l == canceller {
			return true
		}
	}
	return false
}
