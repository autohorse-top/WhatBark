package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"whatbark/internal/flow"
	"whatbark/internal/routes"
	"whatbark/internal/store"
)

// handleSearch: GET /api/search?q=<query>&limit=N (S19-C unified search).
// Returns matching contacts (employees whose name contains q and are
// reachable by the caller), matching messages (body contains q, scoped to
// conversations the caller participates in; the boss sees all 1:1 messages
// plus group messages of groups the boss is a member of — S32/M1), matching
// group/channel names the caller is a member of (S32), and S36's mentions
// facet: messages that @ the caller (message_mentions) and also match q,
// with the same visibility scoping. The mentions facet always appears as an
// array (empty when no hits).
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	id := s.caller(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if utf8.RuneCountInString(q) < 2 {
		writeErr(w, http.StatusBadRequest, errors.New("q must be at least 2 characters"))
		return
	}
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 50 {
			writeErr(w, http.StatusBadRequest, errors.New("limit must be an integer in [1,50]"))
			return
		}
		limit = n
	}

	// --- contacts ---
	emps, err := s.Store.ListEmployees()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	g, gerr := flow.NewGraph(s.Store)
	if gerr != nil {
		writeErr(w, http.StatusInternalServerError, gerr)
		return
	}

	from := 0
	self := 0
	if !id.Admin {
		from = id.EmpID
		self = id.EmpID
	}

	lq := strings.ToLower(q)
	contacts := make([]map[string]any, 0, len(emps))
	for _, e := range emps {
		if e.ID == self {
			continue
		}
		if !strings.Contains(strings.ToLower(e.Name), lq) {
			continue
		}
		if !searchReachable(g, from, e.ID) {
			continue
		}
		contacts = append(contacts, map[string]any{
			"employee_id": e.ID,
			"name":        e.Name,
			"role":        e.Role,
			"kind":        e.Kind,
			"status":      s.statusOf(e),
		})
	}

	// --- messages ---
	participant := 0
	if !id.Admin {
		participant = id.EmpID
	} else {
		participant = -1 // boss sees all
	}
	msgs, err := s.Store.SearchMessages(q, participant, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	rows := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		row := messageRow(s, m)
		if m.GroupID != nil {
			row["group_id"] = *m.GroupID
		}
		rows = append(rows, row)
	}

	// --- groups (S32): group/channel names the caller is a member of ---
	gnames, err := s.Store.SearchGroupNames(q, id.EmpID, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	grows := make([]map[string]any, 0, len(gnames))
	for _, h := range gnames {
		grows = append(grows, map[string]any{
			"group_id":     h.GroupID,
			"name":         h.Name,
			"kind":         h.Kind,
			"member_count": h.MemberCount,
		})
	}

	// --- mentions (S36): messages that @ the caller and match q. Boss is
	// viewerID 0 (identity: EmpID 0 + Admin); employees pass their id.
	viewerID := id.EmpID
	if id.Admin {
		viewerID = 0
	}
	mentions, err := s.Store.SearchMentions(q, viewerID, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	mrows := make([]map[string]any, 0, len(mentions))
	for _, m := range mentions {
		row := messageRow(s, m)
		if m.GroupID != nil {
			row["group_id"] = *m.GroupID
		}
		mrows = append(mrows, row)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"q":        q,
		"contacts": contacts,
		"messages": rows,
		"groups":   grows,
		"mentions": mrows,
	})
}

// searchReachable reports whether `from` can initiate at least one kind of
// message to `to` under the routing graph.
func searchReachable(g *routes.Graph, from, to int) bool {
	if from == 0 {
		return true // boss can reach anyone
	}
	kinds := []string{
		store.MsgReport, store.MsgSummary, store.MsgDispatch,
		store.MsgAsk, store.MsgScheduled, store.MsgChat, store.MsgAnswer,
	}
	for _, k := range kinds {
		if g.Allowed(from, to, k) {
			return true
		}
	}
	return false
}
