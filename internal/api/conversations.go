package api

import (
	"errors"
	"net/http"
	"sort"
	"strconv"

	"whatbark/internal/hub"
	"whatbark/internal/store"
)

// handleConversations returns the caller's conversation list (S8/S14): one
// entry per peer (boss appears as peer_id 0) that has exchanged messages with
// the caller or holds an open task between the two, plus group conversations
// (S14) the caller is a member of. Each entry carries the peer's identity,
// the latest message (either direction), the unread count and the number of
// open tasks between them. Group entries use peer_id = -(group_id+1) and
// carry is_group=true, kind ("group"|"channel", S21), group_id, member_count.
func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	id := s.caller(r)
	// S33: this viewer's pinned peer set (per-viewer display preference).
	pinnedSet, perr := s.Store.PinnedPeers(id.EmpID)
	if perr != nil {
		writeErr(w, http.StatusInternalServerError, perr)
		return
	}
	cons, err := s.Store.ListConversations(id.EmpID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	groups, err := s.Store.ListGroupConversations(id.EmpID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	empByID := map[int]store.Employee{}
	if emps, err := s.Store.ListEmployees(); err == nil {
		for _, e := range emps {
			empByID[e.ID] = e
		}
	}
	out := make([]map[string]any, 0, len(cons)+len(groups))
	for _, c := range cons {
		name, kind, role, status := "boss", "system", "", ""
		if c.PeerID != 0 {
			e, ok := empByID[c.PeerID]
			if !ok {
				continue // stale peer (defensive; FK normally prevents it)
			}
			name, kind, role, status = e.Name, e.Kind, e.Role, s.statusOf(e)
		}
		// S20: messages beyond the cursor that explicitly @-mention the
		// viewer (unread_mentioned; 0 when none).
		mentioned, merr := s.Store.MentionedUnread(id.EmpID, c.PeerID)
		if merr != nil {
			writeErr(w, http.StatusInternalServerError, merr)
			return
		}
		out = append(out, map[string]any{
			"peer_id":          c.PeerID,
			"name":             name,
			"kind":             kind,
			"role":             role,
			"status":           status,
			"is_group":         false,
			"last_msg_id":      c.LastMsgID,
			"last_msg_at":      c.LastMsgAt,
			"last_msg_kind":    c.LastMsgKind,
			"last_msg_body":    c.LastMsgBody,
			"unread":           c.Unread,
			"unread_mentioned": mentioned,
			"open_tasks":       c.OpenTasks,
			"pinned":           pinnedSet[c.PeerID], // S33
		})
	}
	for _, g := range groups {
		// S20: group messages beyond the cursor that @-mention the viewer.
		mentioned, merr := s.Store.GroupMentionedUnread(id.EmpID, g.GroupID)
		if merr != nil {
			writeErr(w, http.StatusInternalServerError, merr)
			return
		}
		// S22: open (queued+running) tasks dispatched into this group.
		openTasks, terr := s.Store.OpenTasksByGroup(g.GroupID)
		if terr != nil {
			writeErr(w, http.StatusInternalServerError, terr)
			return
		}
		out = append(out, map[string]any{
			"peer_id":          -(g.GroupID + 1),
			"name":             g.Name,
			"kind":             g.Kind, // S21: "group" | "channel" (is_group stays true)
			"role":             "",
			"status":           "",
			"is_group":         true,
			"group_id":         g.GroupID,
			"member_count":     g.MemberCount,
			"muted":            g.Muted,
			"ignored":          g.Ignored,
			"last_msg_id":      g.LastMsgID,
			"last_msg_at":      g.LastMsgAt,
			"last_msg_kind":    g.LastMsgKind,
			"last_msg_body":    g.LastMsgBody,
			"unread":           g.Unread,
			"unread_mentioned": mentioned,
			"open_tasks":       openTasks,
			"pinned":           pinnedSet[-(g.GroupID + 1)], // S33
		})
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := out[i]["last_msg_id"].(int), out[j]["last_msg_id"].(int)
		if li != lj {
			return li > lj
		}
		return out[i]["peer_id"].(int) < out[j]["peer_id"].(int)
	})
	// S33: pinned conversations sort first. Stable partition over the sorted
	// list, so block-internal order (last_msg_id desc, peer_id tiebreak) is
	// untouched; only the pinned / non-pinned split changes.
	var pinnedRows, otherRows []map[string]any
	for _, row := range out {
		if row["pinned"] == true {
			pinnedRows = append(pinnedRows, row)
		} else {
			otherRows = append(otherRows, row)
		}
	}
	out = append(pinnedRows, otherRows...)
	writeJSON(w, http.StatusOK, map[string]any{"conversations": out})
}

// handleSetConvPin: POST /api/conversations/{peer_id}/pin {"pinned": bool}
// (S33). Sets the caller's conversation pin for peer_id. peer_id >= 0 is a 1:1
// peer (0 = the boss side); peer_id <= -1 is a group (-(group_id+1)).
//
// Validation:
//   - 1:1: self-pin -> 400; unknown employee -> 400. peer_id 0 (boss side)
//     is valid for every non-boss caller. No conversation-existence check:
//     a pin is per-viewer display state and the row stays invisible until
//     messages (or an open task) link the two (deliberate, spec on record).
//   - group: unknown group -> 404; non-member -> 403, except boss (emp 0)
//     who is exempt (S33 spec 2258: boss/0 exemption). A boss pin on a non-member
//     group is stored but the row stays invisible until boss actually joins
//     or a message links it (same deliberate design as the 1:1 no-existence
//     check above).
//
// Per-viewer state: no WS event (S17 group-prefs precedent); the caller's
// other tabs pick the change up on the next conversation list load.
func (s *Server) handleSetConvPin(w http.ResponseWriter, r *http.Request) {
	peerID, err := intParam(r, "peer_id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id := s.caller(r)
	if peerID >= 0 {
		if peerID == id.EmpID {
			writeErr(w, http.StatusBadRequest, errSelfScope)
			return
		}
		if peerID > 0 {
			if _, err := s.Store.GetEmployee(peerID); err != nil {
				writeErr(w, http.StatusBadRequest, errors.New("unknown employee"))
				return
			}
		}
	} else {
		gid := -peerID - 1
		if _, err := s.Store.GetGroup(gid); err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		ok, err := s.Store.IsGroupMember(gid, id.EmpID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !ok && id.EmpID != 0 {
			// boss (emp 0) is exempt from the group membership check (S33 spec:
			// boss/0 exemption); the pin row stays invisible until boss joins the
			// group or a message links the conversation.
			writeErr(w, http.StatusForbidden, errSelfScope)
			return
		}
	}
	var b struct {
		Pinned *bool `json:"pinned"`
	}
	if err := readJSON(r, &b); err != nil || b.Pinned == nil {
		writeErr(w, http.StatusBadRequest, errors.New(`"pinned" must be a JSON boolean`))
		return
	}
	if *b.Pinned {
		if err := s.Store.PinConversation(id.EmpID, peerID); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	} else {
		if err := s.Store.UnpinConversation(id.EmpID, peerID); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "peer_id": peerID, "pinned": *b.Pinned})
}

// handleThreadMessages returns the bidirectional message thread between the
// caller and the specified peer (GET /api/messages?with=N).
// (Registered on the existing /api/messages route; see handleListMessages.)

// handleMarkRead: POST /api/messages/read {"with": N} or {"group": G} (S14).
// Advances the caller's read cursor to the latest incoming message in that
// thread. For groups the cursor is stored under peer_id = -(G+1).
func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	id := s.caller(r)
	var b struct {
		Peer  *int `json:"with"`
		Group int  `json:"group"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// with and group are mutually exclusive; group>0 takes the group path,
	// with=0 is the valid 1:1 boss conversation.
	if (b.Group > 0) == (b.Peer != nil) {
		writeErr(w, http.StatusBadRequest, errors.New("exactly one of \"with\" or \"group\" required"))
		return
	}
	if b.Group > 0 {
		ok, err := s.Store.IsGroupMember(b.Group, id.EmpID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeErr(w, http.StatusForbidden, errSelfScope)
			return
		}
		upTo, err := s.Store.LatestGroupMessageID(b.Group)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		// S19-B loop fix: broadcast only when the cursor actually advanced;
		// an unconditional broadcast on every POST fed the client-side
		// refresh -> markRead -> refresh ping-pong loop.
		advanced, err := s.Store.MarkConversationRead(id.EmpID, -(b.Group + 1), upTo)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if advanced {
			s.Hub.Broadcast(hub.Event{Type: "message.read", Payload: map[string]any{
				"reader": id.EmpID, "group": b.Group, "up_to": upTo,
			}})
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "group": b.Group, "up_to": upTo})
		return
	}
	// 1:1 path (with >= 0; with=0 means the boss conversation).
	peer := *b.Peer
	if peer < 0 || peer == id.EmpID {
		writeErr(w, http.StatusBadRequest, errSelfScope)
		return
	}
	upTo, err := s.Store.LatestMessageIDTo(id.EmpID, peer)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// S19-B loop fix: broadcast only when the cursor actually advanced.
	advanced, err := s.Store.MarkConversationRead(id.EmpID, peer, upTo)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if advanced {
		s.Hub.Broadcast(hub.Event{Type: "message.read", Payload: map[string]any{
			"reader": id.EmpID, "peer": peer, "up_to": upTo,
		}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "peer": peer, "up_to": upTo})
}

// parsePeerID parses a positive-or-zero peer id from a query/body string.
func parsePeerID(v string) (int, bool) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
