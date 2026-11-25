// Package flow implements whatbark's business semantics: routed sending,
// project seeding, and automatic replies on task completion
// (report / answer / summary). Route decisions go through routes.Graph,
// which is rebuilt from the store on every decision (orgs are small;
// freshness beats caching). Fail-closed: if the graph cannot be built
// (e.g. a reporting cycle), sends are rejected.
package flow

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"unicode/utf8"

	"whatbark/internal/hub"
	"whatbark/internal/org"
	"whatbark/internal/routes"
	"whatbark/internal/store"
)

// ErrRouteNotAllowed is returned by Send when the routes graph rejects the
// (from, to, kind) combination. Callers should use errors.Is.
var ErrRouteNotAllowed = errors.New("route not allowed")

// Flow bundles the store, a fresh-graph provider, and the event emitter.
type Flow struct {
	Store *store.Store
	Graph func() (*routes.Graph, error)
	Emit  func(hub.Event)
	// V-4 notification hooks (nil = disabled). Wired by serve.go; tests keep
	// them nil so Flow behavior stays deterministic.
	OnTaskTerminal func(taskID int, status string)
	OnMention      func(msgID int)
}

// New wires a Flow. graph and emit may be nil (defaults provided).
func New(st *store.Store, graph func() (*routes.Graph, error), emit func(hub.Event)) *Flow {
	if emit == nil {
		emit = func(hub.Event) {}
	}
	if graph == nil {
		graph = func() (*routes.Graph, error) { return NewGraph(st) }
	}
	return &Flow{Store: st, Graph: graph, Emit: emit}
}

// NewGraph builds a routes.Graph from current store state.
func NewGraph(st *store.Store) (*routes.Graph, error) {
	depts, err := st.ListDepartments()
	if err != nil {
		return nil, err
	}
	emps, err := st.ListEmployees()
	if err != nil {
		return nil, err
	}
	ds := make([]org.Department, 0, len(depts))
	for _, d := range depts {
		ds = append(ds, org.Department{ID: d.ID, ParentID: d.ParentID, LeaderID: d.LeaderID})
	}
	es := make([]org.EmployeeRef, 0, len(emps))
	for _, e := range emps {
		es = append(es, org.EmployeeRef{ID: e.ID, DepartmentID: e.DepartmentID})
	}
	snap := org.NewSnapshot(ds, es)

	up := make(map[int][]int, len(emps))
	isLeader := make(map[int]bool)
	for _, e := range emps {
		chain, err := org.ReportingChain(snap, e.ID)
		if err != nil {
			return nil, err // fail closed on cycle
		}
		ids := make([]int, 0, len(chain))
		for _, n := range chain {
			if !n.IsGap {
				ids = append(ids, n.EmpID)
			}
		}
		up[e.ID] = ids
		if org.IsLeader(snap, e.ID) {
			isLeader[e.ID] = true
		}
	}

	wedges := map[int]map[int]bool{}
	wfs, err := st.ListWorkflows()
	if err != nil {
		return nil, err
	}
	for _, wf := range wfs {
		nodes, err := st.ListWorkflowNodes(wf.ID)
		if err != nil {
			return nil, err
		}
		emp := make(map[int]int, len(nodes))
		for _, n := range nodes {
			emp[n.ID] = n.EmpID
		}
		edges, err := st.ListWorkflowEdges(wf.ID)
		if err != nil {
			return nil, err
		}
		for _, e := range edges {
			f, okF := emp[e.FromNodeID]
			to, okT := emp[e.ToNodeID]
			if !okF || !okT {
				continue
			}
			if wedges[f] == nil {
				wedges[f] = map[int]bool{}
			}
			wedges[f][to] = true
		}
	}
	return routes.New(up, isLeader, wedges), nil
}

// Send performs a routed send.
//   - kind dispatch|ask|scheduled: routes.Allowed check, then create a task
//     for `to` (requested_by=from) and record the message.
//   - any other kind: routes.Allowed check, then record the message only.
//
// from == 0 means the boss (admin token). priority (S18, 0..99) is passed
// through to CreateTask; 0 is the default. fileIDs (optional, S12) are
// pre-uploaded files owned by the sender, linked to the created message.
// replyTo (optional, S16) is the id of the replied-to message, passed
// through to the store as-is (validation is the API layer's job).
// It returns the message id and, for task-creating kinds, the new task id.
func (f *Flow) Send(from, to int, kind, body string, projectID, taskID, replyTo *int, priority int, fileIDs ...int) (msgID, newTaskID int, err error) {
	if from == to {
		return 0, 0, fmt.Errorf("flow: cannot send to self")
	}
	g, err := f.Graph()
	if err != nil {
		return 0, 0, fmt.Errorf("flow: build graph: %w", err)
	}
	if !g.Allowed(from, to, kind) {
		return 0, 0, fmt.Errorf("flow: %w (%d -> %d, kind %s)", ErrRouteNotAllowed, from, to, kind)
	}
	var sender *int
	if from != 0 {
		sender = &from
	}
	msgID, err = f.Store.CreateMessage(to, kind, body, sender, projectID, taskID, replyTo)
	if err != nil {
		return 0, 0, err
	}
	if len(fileIDs) > 0 {
		if err := f.Store.LinkFilesToMessage(msgID, fileIDs); err != nil {
			return 0, 0, err
		}
	}
	f.emit("message.new", map[string]any{"message_id": msgID})
	if f.OnMention != nil {
		f.OnMention(msgID)
	}

	createsTask := kind == store.KindDispatch || kind == store.KindAsk || kind == store.KindScheduled ||
		kind == store.KindSelfReview || kind == store.KindReview
	if createsTask {
		newTaskID, err = f.Store.CreateTask(to, kind, body, projectID, nil, from, priority, nil)
		if err != nil {
			return 0, 0, err
		}
		if err := f.Store.SetMessageTaskID(msgID, newTaskID); err != nil {
			return 0, 0, err
		}
		f.emit("task.state", map[string]any{"task_id": newTaskID, "status": store.TaskQueued})
	}
	return msgID, newTaskID, nil
}

// SendToGroup sends a chat message to a group (S14). The sender must be a
// group member (boss id 0 included); empty body without attachments is
// rejected with a plain error (API maps to 400). Non-members get
// ErrRouteNotAllowed (API maps to 403). replyTo (optional, S16) is the id of
// the replied-to group message, passed through to the store as-is.
func (f *Flow) SendToGroup(from, groupID int, body string, replyTo *int, fileIDs ...int) (int, error) {
	ok, err := f.Store.IsGroupMember(groupID, from)
	if err != nil {
		return 0, fmt.Errorf("flow: send to group: %w", err)
	}
	if !ok {
		return 0, fmt.Errorf("flow: %w (group %d, sender %d)", ErrRouteNotAllowed, groupID, from)
	}
	body = strings.TrimSpace(body)
	if body == "" && len(fileIDs) == 0 {
		return 0, fmt.Errorf("flow: group message needs body or attachments")
	}
	var sender *int
	if from != 0 {
		sender = &from
	}
	msgID, err := f.Store.CreateGroupMessage(groupID, store.MsgChat, body, sender, fileIDs, replyTo)
	if err != nil {
		return 0, err
	}
	f.emit("message.new", map[string]any{"message_id": msgID})
	if f.OnMention != nil {
		f.OnMention(msgID)
	}
	return msgID, nil
}

// DispatchToGroup sends a dispatch message inside a group thread (S22) and
// creates a task for the target member (employee_id=to, requested_by=from,
// group_id=groupID). Permissions (server-enforced, two layers): both from
// and to must be members of the group; to must differ from from; and
// routes.Allowed(from, to, "dispatch") must hold (boss=0 may dispatch
// anyone; leaders only their subordinates; plain employees have no
// dispatch routes). S29: projectID (optional) binds the created task to an
// existing project; an unknown project id fails with a plain error (the API
// maps it to 400) before any message or task is created. priority (S18,
// 0..99) is passed through to CreateTask; S30: nil means "use the group's
// default priority" (read from groups.default_priority; an explicit non-nil
// value always wins).
func (f *Flow) DispatchToGroup(from, groupID, to int, body string, priority *int, replyTo, projectID *int, fileIDs ...int) (msgID, taskID int, err error) {
	if to <= 0 {
		return 0, 0, fmt.Errorf("flow: group dispatch needs a target member")
	}
	if to == from {
		return 0, 0, fmt.Errorf("flow: cannot dispatch to self")
	}
	ok, err := f.Store.IsGroupMember(groupID, from)
	if err != nil {
		return 0, 0, fmt.Errorf("flow: dispatch in group: %w", err)
	}
	if !ok {
		return 0, 0, fmt.Errorf("flow: %w (group %d, sender %d)", ErrRouteNotAllowed, groupID, from)
	}
	ok, err = f.Store.IsGroupMember(groupID, to)
	if err != nil {
		return 0, 0, fmt.Errorf("flow: dispatch in group: %w", err)
	}
	if !ok {
		return 0, 0, fmt.Errorf("flow: group %d: target %d is not a member", groupID, to)
	}
	g, err := f.Graph()
	if err != nil {
		return 0, 0, fmt.Errorf("flow: build graph: %w", err)
	}
	if !g.Allowed(from, to, store.KindDispatch) {
		return 0, 0, fmt.Errorf("flow: %w (%d -> %d, kind %s)", ErrRouteNotAllowed, from, to, store.KindDispatch)
	}
	// S29: validate the project before creating anything, so an unknown
	// project id leaves no orphan dispatch message behind. Deliberately a
	// plain error (not ErrRouteNotAllowed) so the API answers 400.
	if projectID != nil {
		if _, err := f.Store.GetProject(*projectID); err != nil {
			return 0, 0, fmt.Errorf("flow: unknown project %d", *projectID)
		}
	}
	body = strings.TrimSpace(body)
	if body == "" && len(fileIDs) == 0 {
		return 0, 0, fmt.Errorf("flow: group message needs body or attachments")
	}
	var sender *int
	if from != 0 {
		sender = &from
	}
	msgID, err = f.Store.CreateGroupMessage(groupID, store.MsgDispatch, body, sender, fileIDs, replyTo)
	if err != nil {
		return 0, 0, err
	}
	// S30: an explicit priority wins; otherwise the group's default applies.
	// The group row is known to exist (IsGroupMember above), so a failure
	// here is unexpected — fall back to 0 rather than aborting the dispatch.
	// The default is clamped to [0,99] so a hand-edited out-of-range DB value
	// can't make CreateTask fail after the dispatch message is already
	// inserted (orphan message). Explicit caller input stays un-clamped: the
	// API layer validates it, and the store rejects out-of-range values.
	var prio int
	if priority != nil {
		prio = *priority
	} else if grp, gerr := f.Store.GetGroup(groupID); gerr == nil {
		prio = clampPriority(grp.DefaultPriority)
	}
	taskID, err = f.Store.CreateTask(to, store.KindDispatch, body, projectID, nil, from, prio, intp(groupID))
	if err != nil {
		return 0, 0, err
	}
	if err := f.Store.SetMessageTaskID(msgID, taskID); err != nil {
		return 0, 0, err
	}
	f.emit("task.state", map[string]any{"task_id": taskID, "status": store.TaskQueued})
	f.emit("message.new", map[string]any{"message_id": msgID})
	if f.OnMention != nil {
		f.OnMention(msgID)
	}
	return msgID, taskID, nil
}

// SeedProject creates a project. When brief and entry employee are both
// set, it seeds a dispatch task (requested_by=0) plus the dispatch message
// for the entry employee (TASK.md §7.5).
func (f *Flow) SeedProject(name, brief string, workflowID, entryEmpID *int) (int, error) {
	pid, err := f.Store.CreateProject(name, brief, workflowID, entryEmpID)
	if err != nil {
		return 0, err
	}
	if brief != "" && entryEmpID != nil {
		if _, _, err := f.Send(0, *entryEmpID, store.KindDispatch, brief, intp(pid), nil, nil, 0); err != nil {
			return 0, fmt.Errorf("flow: seed project %d: %w", pid, err)
		}
	}
	return pid, nil
}

// FinishTask moves a task to done (ok=true) or failed (ok=false) and emits
// the automatic reply per TASK.md §7.5:
//   - ask: answer back to the requester (else project entry, else none)
//   - selfreview: summary to the first leader in the reporting chain
//   - dispatch/scheduled: report to the requester when > 0, else to the
//     project entry employee (skipped when that is the task owner)
//   - S22: when the task was dispatched in a group (task.group_id set) and
//     the owner is still a member, the report goes to the group thread
//     instead (kind=report, from=owner, task_id linked); if the owner is no
//     longer in the group (or the group vanished, which ON DELETE SET NULL
//     clears), it falls through to the 1:1 path above.
//
// Auto-replies bypass route checks: they follow the stored task topology
// by construction and are system-generated.
func (f *Flow) FinishTask(taskID int, output string, ok bool) error {
	t, err := f.Store.GetTask(taskID)
	if err != nil {
		return err
	}
	status := store.TaskDone
	if !ok {
		status = store.TaskFailed
	}
	if err := f.Store.SetTaskStatus(taskID, status, output); err != nil {
		return err
	}
	f.emit("task.state", map[string]any{"task_id": taskID, "status": status})
	if f.OnTaskTerminal != nil {
		f.OnTaskTerminal(taskID, status)
	}

	// Leader orchestration (S1): a dispatch that just reached done/failed
	// wakes its AI requester with a review task. Fail-soft by contract:
	// the wake must never compromise the already-persisted terminal state
	// or the automatic reply below. The final output is passed explicitly —
	// the t snapshot was read before SetTaskStatus stamped it.
	f.maybeWakeReview(t, status, output)

	kind, target := f.replyTarget(t)
	if kind == "" {
		return nil
	}
	// S22: group-dispatched report tasks reply into the group thread when
	// the owner is still a member (checked before the target==0 fallback,
	// since boss-dispatched group tasks have no 1:1 target by construction).
	if kind == store.MsgReport && t.GroupID != nil {
		okG, err := f.Store.IsGroupMember(*t.GroupID, t.EmpID)
		if err != nil {
			return err
		}
		if okG {
			owner := t.EmpID
			mid, err := f.Store.CreateGroupMessage(*t.GroupID, store.MsgReport, output, &owner, nil, nil)
			if err != nil {
				return err
			}
			if err := f.Store.SetMessageTaskID(mid, taskID); err != nil {
				return err
			}
			f.emit("message.new", map[string]any{"message_id": mid})
			return nil
		}
	}
	if target == 0 {
		return nil
	}
	owner := t.EmpID
	mid, err := f.Store.CreateMessage(target, kind, output, &owner, t.ProjectID, intp(taskID), nil)
	if err != nil {
		return err
	}
	f.emit("message.new", map[string]any{"message_id": mid})
	return nil
}

// replyTarget decides the automatic reply for a finishing task.
// Returns ("", 0) when no reply message is warranted.
func (f *Flow) replyTarget(t store.Task) (string, int) {
	if t.Kind == store.KindReview {
		// Review wakes carry no automatic receipt: the leader closing the
		// review is the end of the chain. Pinned explicitly so a future
		// project-entry fallback cannot mis-route a receipt (F7).
		return "", 0
	}
	if t.Kind == store.KindAsk {
		return store.MsgAnswer, f.resolveTarget(t)
	}
	if t.Kind == store.KindSelfReview {
		return store.MsgSummary, f.firstLeader(t.EmpID)
	}
	// dispatch, scheduled
	return store.MsgReport, f.resolveTarget(t)
}

// resolveTarget maps a reply destination: the requester when > 0, else the
// project entry employee (when it is not the task owner), else 0 (none).
func (f *Flow) resolveTarget(t store.Task) int {
	if t.RequestedBy > 0 {
		return t.RequestedBy
	}
	if t.ProjectID != nil {
		if p, err := f.Store.GetProject(*t.ProjectID); err == nil && p.EntryEmpID != nil && *p.EntryEmpID != t.EmpID {
			return *p.EntryEmpID
		}
	}
	return 0
}

// firstLeader returns the first leader in the reporting chain of empID
// (0 when the chain has no leader — e.g. an org root or a gap chain).
func (f *Flow) firstLeader(empID int) int {
	g, err := f.Graph()
	if err != nil {
		return 0
	}
	if len(g.Up[empID]) > 0 {
		return g.Up[empID][0]
	}
	return 0
}

func intp(v int) *int { return &v }

// clampPriority pins a priority to the valid [0,99] range (S30). The API
// layer already validates dispatch priorities, but the group default
// (groups.default_priority) could be out of range after a manual DB edit;
// clamping keeps CreateTask from rejecting a task whose dispatch message
// was already inserted (orphan message).
func clampPriority(p int) int {
	if p < 0 {
		return 0
	}
	if p > 99 {
		return 99
	}
	return p
}

func (f *Flow) emit(evType string, payload map[string]any) {
	f.Emit(hub.Event{Type: evType, Payload: payload})
}

// Leader orchestration (S1): review wake support.

const (
	// settingReviewMaxPerHour is the settings key holding the review-wake
	// throttle; api/settings.go validates writes to [0,1000].
	settingReviewMaxPerHour = "review_max_per_hour"
	// defaultReviewMaxPerHour applies when the setting is absent or
	// unreadable (default 30 per TASK §3.4).
	defaultReviewMaxPerHour = 30
	// hard bounds for any value read from the DB (S30 lesson: hand-edited
	// settings must be clamped, never trusted — negatives clamp to 0 =
	// disabled, not passed through).
	reviewMaxPerHourMin = 0
	reviewMaxPerHourMax = 1000
	// review body truncation budgets (bytes, UTF-8-safe via tailUTF8).
	reviewInputTailBytes  = 512
	reviewOutputTailBytes = 2048
)

// maybeWakeReview creates a kind="review" task (plus its message) for the
// AI requester of a dispatch that just finished, subject to the per-hour
// throttle (review_max_per_hour, default 30, 0 = disabled).
//
// Conditions (all required, TASK §3.1): the finished task is a dispatch
// (group-dispatched included; the review itself stays 1:1), the terminal
// status is done or failed (canceled never passes through FinishTask), the
// requester is an existing AI employee (human/boss requesters are not
// woken), and the requester is under the hourly review budget.
//
// Whole path is fail-soft (contract): throttling and every error log a
// warning plus an audit row and return — the terminal state and the
// automatic reply are already persisted and must not be jeopardized by
// this derivative feature.
func (f *Flow) maybeWakeReview(t store.Task, status, output string) {
	if t.Kind != store.KindDispatch || t.RequestedBy <= 0 {
		return
	}
	if status != store.TaskDone && status != store.TaskFailed {
		return // canceled never reaches FinishTask; pinned for safety
	}
	req, err := f.Store.GetEmployee(t.RequestedBy)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return // requester vanished: nothing to wake
		}
		f.reviewWakeFailure(t, "review.create_failed", "lookup requester: "+err.Error())
		return
	}
	if req.Kind != store.KindAI {
		return // human/boss requesters review via the normal report thread
	}

	max := f.reviewMaxPerHour()
	if max <= 0 {
		// 0 = disabled by explicit configuration (not an anomaly): skip
		// silently so a busy org does not flood the audit log.
		return
	}
	count, err := f.Store.CountEmployeeReviewsInHour(t.RequestedBy)
	if err != nil {
		f.reviewWakeFailure(t, "review.create_failed", "count recent reviews: "+err.Error())
		return
	}
	if count >= max {
		f.reviewWakeFailure(t, "review.skip.throttled",
			fmt.Sprintf("count=%d limit=%d requester=%d", count, max, t.RequestedBy))
		return
	}

	if _, _, err := f.Send(0, t.RequestedBy, store.KindReview, f.reviewWakeBody(t, status, output), nil, nil, nil, 0); err != nil {
		f.reviewWakeFailure(t, "review.create_failed", err.Error())
	}
}

// reviewWakeFailure records a skipped or failed review wake: one slog.Warn
// line plus one audit row. AuditAppend's error is deliberately ignored —
// the wake path must stay fail-soft all the way down.
func (f *Flow) reviewWakeFailure(t store.Task, action, detail string) {
	slog.Warn("review wake skipped",
		"action", action, "task", t.ID, "requester", t.RequestedBy, "detail", detail)
	_ = f.Store.AuditAppend("system", t.RequestedBy, action,
		fmt.Sprintf("/api/tasks/%d", t.ID), detail)
}

// reviewMaxPerHour reads and clamps the throttle setting. Absent,
// unreadable, or non-numeric values fall back to the default; present
// values are clamped to [0,1000] (S30 lesson: a hand-edited DB value must
// never widen behavior beyond the validated domain).
func (f *Flow) reviewMaxPerHour() int {
	v, err := f.Store.GetSetting(settingReviewMaxPerHour)
	if err != nil {
		return defaultReviewMaxPerHour
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		// Broken DB value: behave as unset rather than disabling wakes.
		return defaultReviewMaxPerHour
	}
	if n < reviewMaxPerHourMin {
		return reviewMaxPerHourMin
	}
	if n > reviewMaxPerHourMax {
		return reviewMaxPerHourMax
	}
	return n
}

// reviewWakeBody renders the review message (TASK §3.2): who finished
// which task in what state, the input tail and output tail (UTF-8-safe
// truncation), and the guidance — accept, follow-up, or re-dispatch at
// most 2 more times before escalating to the boss. output is the final
// terminal output passed by FinishTask (the t snapshot predates the write).
func (f *Flow) reviewWakeBody(t store.Task, status, output string) string {
	name := fmt.Sprintf("Employee #%d", t.EmpID)
	if emp, err := f.Store.GetEmployee(t.EmpID); err == nil {
		name = emp.Name
	}
	return fmt.Sprintf(`Review wake: task #%d by %s is %s.

Input (tail): %s
Output (last 2k chars): %s

Guidance: review the result, then either accept, dispatch follow-up work,
or re-dispatch the same problem to the same employee at most 2 more times;
after that escalate to the boss instead of re-dispatching.`,
		t.ID, name, status, tailUTF8(t.Input, reviewInputTailBytes), tailUTF8(output, reviewOutputTailBytes))
}

// tailUTF8 returns at most the last n bytes of s without splitting a
// multi-byte rune: if the cut point lands mid-rune, the start shifts
// forward to the next rune boundary, so the result is always valid UTF-8
// and never longer than n bytes. n <= 0 yields "".
func tailUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
