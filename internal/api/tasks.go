package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"whatbark/internal/flow"
	"whatbark/internal/hub"
	"whatbark/internal/store"
)

// eventTaskState builds the task.state broadcast payload. H-4: carries the
// task's current owner so clients can distinguish a status-unchanged reassign
// (queued -> queued, owner changed) from a stale echo of their own update.
func eventTaskState(taskID, ownerID int, status string) hub.Event {
	return hub.Event{Type: "task.state", Payload: map[string]any{
		"task_id":     taskID,
		"status":      status,
		"employee_id": ownerID,
	}}
}

// taskStatusErrCode maps a task-mutation store error to an HTTP status
// (S27): missing row -> 404, invalid state transition -> 409, anything
// else -> 500. Used by the start/finish/fail/cancel/reassign handlers so a
// raced-away task no longer masquerades as a conflict.
func taskStatusErrCode(err error) int {
	if errors.Is(err, store.ErrNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, store.ErrInvalidTransition) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// taskRow serializes a task for the API.
func taskRow(s *Server, t store.Task) map[string]any {
	row := map[string]any{
		"id": t.ID, "employee_id": t.EmpID, "kind": t.Kind, "status": t.Status,
		"priority": t.Priority,
		"input":    t.Input, "output": t.Output, "requested_by": t.RequestedBy,
		"created_at": t.CreatedAt, "started_at": t.StartedAt, "finished_at": t.FinishedAt,
	}
	if t.ProjectID != nil {
		row["project_id"] = *t.ProjectID
	}
	if t.GroupID != nil {
		row["group_id"] = *t.GroupID
	}
	if emp, err := s.Store.GetEmployee(t.EmpID); err == nil {
		row["employee_name"] = emp.Name
	}
	// Server-computed duration (task-duration-artifacts A): omitted while the
	// task is still queued (no start reference yet).
	if sec, label, ok := taskDuration(t, time.Now().UTC()); ok {
		row["duration_sec"] = sec
		row["duration"] = label
	}
	// artifact_count (task-duration-artifacts B): cheap indexed COUNT; the
	// full artifact list (with signed URLs) lives on the detail endpoint.
	if n, err := s.Store.CountArtifacts(t.ID); err == nil {
		row["artifact_count"] = n
	}
	return row
}

// fileRows serializes message attachments for the API. P0-1: attachment URLs
// are short-lived signed URLs (browser <img>/<a> cannot set auth headers);
// the client renders f.url as-is.
func fileRows(s *Server, files []store.File) []map[string]any {
	out := make([]map[string]any, 0, len(files))
	for _, f := range files {
		out = append(out, map[string]any{
			"id": f.ID, "name": f.Name, "mime": f.Mime, "size": f.Size,
			"url": s.signFileURL("/api/files/" + f.StoredName),
		})
	}
	return out
}

// messageRow serializes a message for the API.
func messageRow(s *Server, m store.Message) map[string]any {
	row := map[string]any{
		"id": m.ID, "kind": m.Kind, "body": m.Body,
		"created_at": m.CreatedAt,
	}
	if m.ToID != nil {
		row["to_id"] = *m.ToID
	}
	if m.FromID != nil {
		row["from_id"] = *m.FromID
	}
	if m.ProjectID != nil {
		row["project_id"] = *m.ProjectID
	}
	if m.TaskID != nil {
		row["task_id"] = *m.TaskID
	}
	if m.ReplyTo != nil {
		row["reply_to"] = *m.ReplyTo
	}
	files, err := s.Store.ListMessageFiles(m.ID)
	if err != nil {
		files = nil
	}
	row["files"] = fileRows(s, files)
	// S20: real mentions — mentioned employee ids (0 = boss). Always present
	// as an array (empty when none) so the frontend can render @-targets.
	mentions, merr := s.Store.MessageMentions(m.ID)
	if merr != nil {
		mentions = []int{}
	}
	row["mentions"] = mentions
	return row
}

// handleListTasks: GET /api/tasks?employee=N | ?project=N | ?status=S
// A non-admin caller may only list their own tasks.
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	id := s.caller(r)
	q := r.URL.Query()

	var tasks []store.Task
	var err error
	switch {
	case q.Get("employee") != "":
		n, perr := strconv.Atoi(q.Get("employee"))
		if perr != nil || (!id.Admin && n != id.EmpID) {
			writeErr(w, http.StatusBadRequest, errSelfScope)
			return
		}
		// S18: ?status= filter is a backward-compatible extension used by
		// agents to fetch their own queued (or other) tasks.
		status := q.Get("status")
		var sp *string
		if status != "" {
			sp = &status
		}
		tasks, err = s.Store.ListTasksByEmployee(n, sp)
	case q.Get("project") != "":
		n, perr := strconv.Atoi(q.Get("project"))
		if perr != nil {
			writeErr(w, http.StatusBadRequest, errSelfScope)
			return
		}
		tasks, err = s.Store.ListTasksByProject(n)
		if id.EmpID > 0 {
			filtered := make([]store.Task, 0, len(tasks))
			for _, t := range tasks {
				if t.EmpID == id.EmpID {
					filtered = append(filtered, t)
				}
			}
			tasks = filtered
		}
	default:
		status := q.Get("status")
		var sp *string
		if status != "" {
			sp = &status
		}
		tasks, err = s.Store.ListTasks(sp)
		if id.EmpID > 0 {
			filtered := make([]store.Task, 0, len(tasks))
			for _, t := range tasks {
				if t.EmpID == id.EmpID {
					filtered = append(filtered, t)
				}
			}
			tasks = filtered
		}
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, taskRow(s, t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": out})
}

// requireTaskOwner ensures the caller owns the task (or is admin).
func (s *Server) requireTaskOwner(w http.ResponseWriter, r *http.Request, taskID int) (store.Task, bool) {
	t, err := s.Store.GetTask(taskID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return store.Task{}, false
	}
	id := s.caller(r)
	if !id.Admin && id.EmpID != t.EmpID {
		writeErr(w, http.StatusForbidden, errSelfScope)
		return store.Task{}, false
	}
	return t, true
}

// handleGetTask: GET /api/tasks/{id} (owner or admin).
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	t, err := s.Store.GetTask(taskID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	id := s.caller(r)
	if !id.Admin && id.EmpID != t.EmpID {
		writeErr(w, http.StatusForbidden, errSelfScope)
		return
	}
	row := taskRow(s, t)
	// Full artifact list with short-lived signed download URLs (the list
	// endpoint only carries artifact_count to stay cheap).
	arts, err := s.Store.ListArtifactsByTask(taskID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	arows := make([]map[string]any, 0, len(arts))
	for _, a := range arts {
		arows = append(arows, map[string]any{
			"id":         a.ID,
			"name":       a.Name,
			"mime":       a.Mime,
			"size":       a.Size,
			"created_at": a.CreatedAt,
			"url":        s.signFileURL("/api/artifacts/" + a.StoredName),
		})
	}
	row["artifacts"] = arows
	writeJSON(w, http.StatusOK, row)
}

// handleTaskStart: POST /api/tasks/{id}/start (owner or admin).
func (s *Server) handleTaskStart(w http.ResponseWriter, r *http.Request) {
	taskID, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	t, ok := s.requireTaskOwner(w, r, taskID)
	if !ok {
		return
	}
	if err := s.Store.SetTaskStatus(taskID, store.TaskRunning, ""); err != nil {
		writeErr(w, taskStatusErrCode(err), err)
		return
	}
	s.Hub.Broadcast(eventTaskState(taskID, t.EmpID, store.TaskRunning))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": store.TaskRunning})
}

// handleTaskFinish: POST /api/tasks/{id}/finish {output}.
func (s *Server) handleTaskFinish(w http.ResponseWriter, r *http.Request) {
	taskID, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if _, ok := s.requireTaskOwner(w, r, taskID); !ok {
		return
	}
	var b struct {
		Output string `json:"output"`
	}
	if err := readJSONOptional(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Flow.FinishTask(taskID, b.Output, true); err != nil {
		writeErr(w, taskStatusErrCode(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": store.TaskDone})
}

// handleTaskFail: POST /api/tasks/{id}/fail {output}.
func (s *Server) handleTaskFail(w http.ResponseWriter, r *http.Request) {
	taskID, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if _, ok := s.requireTaskOwner(w, r, taskID); !ok {
		return
	}
	var b struct {
		Output string `json:"output"`
	}
	if err := readJSONOptional(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Flow.FinishTask(taskID, b.Output, false); err != nil {
		writeErr(w, taskStatusErrCode(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": store.TaskFailed})
}

// handleTaskCancel: POST /api/tasks/{id}/cancel {reason}.
func (s *Server) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	taskID, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	t, err := s.Store.GetTask(taskID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	id := s.caller(r)
	g, gerr := flow.NewGraph(s.Store)
	if gerr != nil {
		writeErr(w, http.StatusInternalServerError, gerr)
		return
	}
	if !g.CancelAllowed(id.EmpID, t.EmpID, t.RequestedBy) {
		writeErr(w, http.StatusForbidden, errSelfScope)
		return
	}
	var b struct {
		Reason string `json:"reason"`
	}
	if err := readJSONOptional(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.SetTaskStatus(taskID, store.TaskCanceled, b.Reason); err != nil {
		writeErr(w, taskStatusErrCode(err), err)
		return
	}
	s.auditCall(r, "task.cancel", taskID, b.Reason) // H-7
	s.Hub.Broadcast(eventTaskState(taskID, t.EmpID, store.TaskCanceled))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": store.TaskCanceled})
}

// handleTaskPreempt: POST /api/tasks/{id}/preempt {reason?} (S18, owner or
// admin). Returns a running task to the queue so a higher-priority task can
// run now: status=queued, started_at=NULL, output records "preempted:
// {reason}" (audit); the task's priority is preserved. Non-running tasks
// answer 409 (store.ErrInvalidTransition), unknown tasks 404.
func (s *Server) handleTaskPreempt(w http.ResponseWriter, r *http.Request) {
	taskID, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	t, ok := s.requireTaskOwner(w, r, taskID)
	if !ok {
		return
	}
	var b struct {
		Reason string `json:"reason"`
	}
	if err := readJSONOptional(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.PreemptTask(taskID, b.Reason); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		if errors.Is(err, store.ErrInvalidTransition) {
			writeErr(w, http.StatusConflict, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.auditCall(r, "task.preempt", taskID, b.Reason) // H-7
	s.Hub.Broadcast(eventTaskState(taskID, t.EmpID, store.TaskQueued))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": store.TaskQueued})
}

// handleTaskReassign: POST /api/tasks/{id}/reassign {to, reason?} (S27).
// Moves the task to a new owner and resets it to queued. Source states:
// queued/running/canceled/failed; done -> 409. Permission reuses the cancel
// predicate (boss / requester / owner / owner's leader chain). The audit
// note "reassigned: old -> new（reason）" is appended to the task output.
func (s *Server) handleTaskReassign(w http.ResponseWriter, r *http.Request) {
	taskID, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	t, err := s.Store.GetTask(taskID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	id := s.caller(r)
	g, gerr := flow.NewGraph(s.Store)
	if gerr != nil {
		writeErr(w, http.StatusInternalServerError, gerr)
		return
	}
	if !g.CancelAllowed(id.EmpID, t.EmpID, t.RequestedBy) {
		writeErr(w, http.StatusForbidden, errSelfScope)
		return
	}
	var b struct {
		To     int    `json:"to"`
		Reason string `json:"reason"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if b.To <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("reassign target 'to' must be a positive employee id"))
		return
	}
	if b.To == t.EmpID {
		writeErr(w, http.StatusBadRequest, errors.New("reassign to same owner is a no-op"))
		return
	}
	if _, err := s.Store.GetEmployee(b.To); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	oldName := fmt.Sprintf("emp#%d", t.EmpID)
	if emp, e := s.Store.GetEmployee(t.EmpID); e == nil {
		oldName = emp.Name
	}
	newName := fmt.Sprintf("emp#%d", b.To)
	if emp, e := s.Store.GetEmployee(b.To); e == nil {
		newName = emp.Name
	}
	note := fmt.Sprintf("reassigned: %s -> %s", oldName, newName)
	if b.Reason != "" {
		note += "（" + b.Reason + "）"
	}
	// expectedOwner pins the UPDATE to the owner we authorized against
	// (S27 review R2): a concurrent reassign that swapped the owner between
	// the read above and this write now fails with ErrInvalidTransition
	// instead of moving the task a second time.
	if err := s.Store.ReassignTask(taskID, b.To, t.EmpID, note); err != nil {
		writeErr(w, taskStatusErrCode(err), err)
		return
	}
	s.auditCall(r, "task.reassign", taskID, fmt.Sprintf("to=%d reason=%s", b.To, b.Reason)) // H-7
	s.Hub.Broadcast(eventTaskState(taskID, b.To, store.TaskQueued))                         // H-4: new owner goes into the payload
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": store.TaskQueued, "employee_id": b.To})
}

// handleListMessages: GET /api/messages?with=N (bidirectional thread) |
// ?to=N | ?project=N [&limit=N].
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	id := s.caller(r)
	q := r.URL.Query()
	limit := 50
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, errSelfScope)
			return
		}
		limit = n
	}

	var msgs []store.Message
	var err error
	switch {
	case q.Get("group") != "":
		if q.Get("with") != "" || q.Get("to") != "" || q.Get("project") != "" {
			writeErr(w, http.StatusBadRequest, errSelfScope)
			return
		}
		g, perr := strconv.Atoi(q.Get("group"))
		if perr != nil || g <= 0 {
			writeErr(w, http.StatusBadRequest, errSelfScope)
			return
		}
		ok, merr := s.Store.IsGroupMember(g, id.EmpID)
		if merr != nil {
			writeErr(w, http.StatusInternalServerError, merr)
			return
		}
		if !ok {
			writeErr(w, http.StatusForbidden, errSelfScope)
			return
		}
		msgs, err = s.Store.GroupMessages(g, limit)
	case q.Get("with") != "":
		peer, ok := parsePeerID(q.Get("with"))
		if !ok || peer == id.EmpID {
			writeErr(w, http.StatusBadRequest, errSelfScope)
			return
		}
		msgs, err = s.Store.ThreadMessages(id.EmpID, peer, limit)
	case q.Get("to") != "":
		n, perr := strconv.Atoi(q.Get("to"))
		if perr != nil || (!id.Admin && n != id.EmpID) {
			writeErr(w, http.StatusBadRequest, errSelfScope)
			return
		}
		msgs, err = s.Store.ListMessagesTo(n, limit)
	case q.Get("project") != "":
		n, perr := strconv.Atoi(q.Get("project"))
		if perr != nil {
			writeErr(w, http.StatusBadRequest, errSelfScope)
			return
		}
		msgs, err = s.Store.ListMessagesByProject(n)
	default:
		writeErr(w, http.StatusBadRequest, errSelfScope)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if id.EmpID > 0 && q.Get("to") == "" && q.Get("project") != "" {
		filtered := make([]store.Message, 0, len(msgs))
		for _, m := range msgs {
			if (m.ToID != nil && *m.ToID == id.EmpID) || (m.FromID != nil && *m.FromID == id.EmpID) {
				filtered = append(filtered, m)
			}
		}
		msgs = filtered
	}
	out := make([]map[string]any, 0, len(msgs))

	// S19-B: annotate read receipts on viewer-sent messages.
	var oneToOneCursor int
	if q.Get("with") != "" {
		peer, _ := parsePeerID(q.Get("with"))
		pc, cerr := s.Store.ReadCursors(peer)
		if cerr != nil {
			writeErr(w, http.StatusInternalServerError, cerr)
			return
		}
		oneToOneCursor = pc[id.EmpID]
	}
	var groupCursors map[int]int
	if q.Get("group") != "" {
		g, perr := strconv.Atoi(q.Get("group"))
		if perr == nil {
			var gerr error
			groupCursors, gerr = s.Store.GroupReadCursors(g)
			if gerr != nil {
				writeErr(w, http.StatusInternalServerError, gerr)
				return
			}
		}
	}

	// S28: group threads only — annotate every message row carrying a task_id
	// with the task's live status so non-owner members' task chips reflect
	// the real state (task list stays self-scoped). The 1:1 (?with=) and
	// ?project= branches stay unchanged (no task_status key there).
	var taskStatuses map[int]string
	// task-duration-artifacts A: inbox rows (?to=) additionally carry the
	// task duration so the card header can show "· 52s". A message row can
	// reference an arbitrary task_id (Flow.Send passes it through unvalidated),
	// so only tasks the caller may actually read are annotated — the same
	// owner-or-admin rule as GET /api/tasks/:id.
	var taskDurations map[int]string
	if q.Get("group") != "" {
		seen := make(map[int]bool, len(msgs))
		ids := make([]int, 0, len(msgs))
		for _, m := range msgs {
			if m.TaskID != nil && !seen[*m.TaskID] {
				seen[*m.TaskID] = true
				ids = append(ids, *m.TaskID)
			}
		}
		if len(ids) > 0 {
			var terr error
			taskStatuses, terr = s.Store.TaskStatuses(ids)
			if terr != nil {
				writeErr(w, http.StatusInternalServerError, terr)
				return
			}
		}
	}
	if q.Get("to") != "" {
		seen := make(map[int]bool, len(msgs))
		ids := make([]int, 0, len(msgs))
		for _, m := range msgs {
			if m.TaskID != nil && !seen[*m.TaskID] {
				seen[*m.TaskID] = true
				ids = append(ids, *m.TaskID)
			}
		}
		if len(ids) > 0 {
			tasks, terr := s.Store.Tasks(ids)
			if terr != nil {
				writeErr(w, http.StatusInternalServerError, terr)
				return
			}
			now := time.Now().UTC()
			taskDurations = make(map[int]string, len(tasks))
			for tid, tk := range tasks {
				// A message row can reference an arbitrary task_id (Flow.Send
				// passes it through unvalidated): annotate only tasks the
				// caller may actually read (GET /api/tasks/:id is
				// owner-or-admin), otherwise skip.
				if !id.Admin && tk.EmpID != id.EmpID {
					continue
				}
				if _, label, ok := taskDuration(tk, now); ok {
					taskDurations[tid] = label
				}
			}
		}
	}

	for _, m := range msgs {
		row := messageRow(s, m)
		if m.TaskID != nil {
			if st, ok := taskStatuses[*m.TaskID]; ok {
				row["task_status"] = st
			}
			if d, ok := taskDurations[*m.TaskID]; ok {
				row["task_duration"] = d
			}
		}
		if isViewerSent(id, m) {
			switch {
			case q.Get("with") != "":
				row["read"] = oneToOneCursor >= m.ID
			case q.Get("group") != "":
				read := false
				for reader, cur := range groupCursors {
					if reader != id.EmpID && cur >= m.ID {
						read = true
						break
					}
				}
				row["read"] = read
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": out})
}

// isViewerSent reports whether m was sent by the caller (boss: from_id is
// NULL; employee: from_id == their emp id). System messages (e.g. group
// creation events) never count as sent, even with a NULL from_id.
func isViewerSent(id identity, m store.Message) bool {
	if m.Kind == store.MsgSystem {
		return false
	}
	if id.Admin && m.FromID == nil {
		return true
	}
	return m.FromID != nil && *m.FromID == id.EmpID
}

// handleSendMessage: POST /api/messages {to, kind, body, project_id?, task_id?,
// file_ids?, group?, reply_to?, priority?}. Boss (admin) sends with from=0.
// file_ids (S12) must reference files the caller uploaded; body may be empty
// when attachments are present. S14: when group > 0, task_id must be absent
// and the caller must be a group member (403 otherwise). S22: group messages
// accept kind=chat (to absent, unchanged) or kind=dispatch (to = target
// member; creates a task with group_id=G; non-member sender / no dispatch
// route -> 403; target non-member / self / to=0 -> 400; 201 returns
// {message_id, task_id}). S29: group kind=dispatch may carry project_id to
// bind the created task to an existing project (unknown project -> 400);
// kind=chat still rejects project_id, and group messages never accept
// task_id. S16: reply_to is the id of the replied-to message; it must exist
// and live in the same conversation as the new message (1:1 bidirectional
// pair or same group), otherwise 400. S18: priority (int 0..99, default 0)
// is passed through to the created task; out-of-range or non-int values are
// rejected with 400.
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	id := s.caller(r)
	var b struct {
		ToID      int    `json:"to"`
		Group     int    `json:"group"`
		Kind      string `json:"kind"`
		Body      string `json:"body"`
		ProjectID *int   `json:"project_id"`
		TaskID    *int   `json:"task_id"`
		FileIDs   []int  `json:"file_ids"`
		ReplyTo   *int   `json:"reply_to"`
		Priority  *int   `json:"priority"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	priority := 0
	if b.Priority != nil {
		if *b.Priority < 0 || *b.Priority > 99 {
			writeErr(w, http.StatusBadRequest, errors.New("priority must be an integer in [0,99]"))
			return
		}
		priority = *b.Priority
	}
	from := 0
	if !id.Admin {
		from = id.EmpID
	}
	if len(b.FileIDs) > 0 {
		if _, err := s.Store.ListOwnedFiles(from, b.FileIDs); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "file not found or not owned by caller"})
			return
		}
	}
	if b.ReplyTo != nil {
		// S16: the quoted message must exist and be in this conversation.
		// 1:1 -> (to, nil); group -> (nil, group). Missing/cross-conversation
		// targets both answer 400 with the same message.
		var same bool
		var err error
		if b.Group > 0 {
			same, err = s.Store.SameConversation(*b.ReplyTo, from, nil, &b.Group)
		} else {
			same, err = s.Store.SameConversation(*b.ReplyTo, from, &b.ToID, nil)
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if *b.ReplyTo <= 0 || !same {
			writeErr(w, http.StatusBadRequest, errors.New("reply_to does not reference a message in this conversation"))
			return
		}
	}
	if b.Group > 0 {
		if b.TaskID != nil {
			writeErr(w, http.StatusBadRequest, errors.New("group message: task_id is not supported"))
			return
		}
		if b.ProjectID != nil && b.Kind != store.MsgDispatch {
			writeErr(w, http.StatusBadRequest, errors.New("group message: project_id only supported for kind=dispatch"))
			return
		}
		switch b.Kind {
		case store.MsgChat:
			if b.ToID != 0 {
				writeErr(w, http.StatusBadRequest, errors.New("group chat takes no 'to'"))
				return
			}
			msgID, err := s.Flow.SendToGroup(from, b.Group, b.Body, b.ReplyTo, b.FileIDs...)
			if err != nil {
				if errors.Is(err, flow.ErrRouteNotAllowed) {
					writeErr(w, http.StatusForbidden, err)
					return
				}
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"message_id": msgID})
		case store.MsgDispatch:
			// S22: in-group task dispatch — to is the target member; the
			// task gets group_id=G so its finish report returns to this thread.
			// S30: pass b.Priority (nil when absent) so the group's default
			// priority applies instead of the resolved 0.
			msgID, taskID, err := s.Flow.DispatchToGroup(from, b.Group, b.ToID, b.Body, b.Priority, b.ReplyTo, b.ProjectID, b.FileIDs...)
			if err != nil {
				if errors.Is(err, flow.ErrRouteNotAllowed) {
					writeErr(w, http.StatusForbidden, err)
					return
				}
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"message_id": msgID, "task_id": taskID})
		default:
			writeErr(w, http.StatusBadRequest, errors.New("Group messages only support kind=chat or kind=dispatch (dispatch requires to)"))
			return
		}
		return
	}
	msgID, taskID, err := s.Flow.Send(from, b.ToID, b.Kind, b.Body, b.ProjectID, b.TaskID, b.ReplyTo, priority, b.FileIDs...)
	if err != nil {
		if errors.Is(err, flow.ErrRouteNotAllowed) {
			writeErr(w, http.StatusForbidden, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	resp := map[string]any{"message_id": msgID}
	if taskID > 0 {
		resp["task_id"] = taskID
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleAgentAttach: POST /api/agent/attach (employee token only, kind=ai).
// S18: an initial attach (body {"initial": true}) requeues this employee's
// running tasks (agent restart / orphan recovery) and reports the requeued
// count; heartbeats keep the empty body and must not requeue anything.
func (s *Server) handleAgentAttach(w http.ResponseWriter, r *http.Request) {
	id := s.caller(r)
	if id.Admin {
		writeErr(w, http.StatusForbidden, errSelfScope)
		return
	}
	emp, err := s.Store.GetEmployee(id.EmpID)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	if emp.Kind != store.KindAI {
		writeErr(w, http.StatusForbidden, errSelfScope)
		return
	}
	var b struct {
		Initial bool `json:"initial"`
	}
	_ = readJSONOptional(r, &b)
	resp := map[string]any{"ok": true, "employee_id": id.EmpID, "name": emp.Name}
	if b.Initial {
		running, err := s.Store.ListTasksByEmployee(id.EmpID, ptrStatus(store.TaskRunning))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		n, err := s.Store.RequeueRunningTasks(id.EmpID, "agent reattached")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		resp["requeued"] = n
		for _, t := range running {
			s.Hub.Broadcast(eventTaskState(t.ID, t.EmpID, store.TaskQueued))
		}
	}
	s.MarkOnline(id.EmpID, time.Now())
	writeJSON(w, http.StatusOK, resp)
}

func ptrStatus(s string) *string { return &s }

// handleAgentOutput: POST /api/agent/output {task_id, line} (owner only).
// Piggybacks a heartbeat and broadcasts agent.output to the console.
func (s *Server) handleAgentOutput(w http.ResponseWriter, r *http.Request) {
	id := s.caller(r)
	if id.Admin {
		writeErr(w, http.StatusForbidden, errSelfScope)
		return
	}
	var b struct {
		TaskID int    `json:"task_id"`
		Line   string `json:"line"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	t, err := s.Store.GetTask(b.TaskID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if t.EmpID != id.EmpID {
		writeErr(w, http.StatusForbidden, errSelfScope)
		return
	}
	// Reject output for terminal tasks (S7 nit): no post-hoc live feed into
	// a finished/canceled task. Queued is allowed (start may still be
	// in-flight from the agent's side).
	if t.Status == store.TaskDone || t.Status == store.TaskFailed || t.Status == store.TaskCanceled {
		writeErr(w, http.StatusConflict, fmt.Errorf("task %d is %s, no longer accepting output", t.ID, t.Status))
		return
	}
	s.MarkOnline(id.EmpID, time.Now())
	s.Hub.Broadcast(hub.Event{Type: "agent.output", Payload: map[string]any{
		"task_id":     b.TaskID,
		"employee_id": id.EmpID,
		"line":        b.Line,
	}})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
