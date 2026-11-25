package flow

import (
	"context"
	"strings"
	"testing"
)

// TestDispatchToGroupPriority (S30): an explicit priority always wins; a nil
// priority inherits the group's default_priority (0 when unset).
func TestDispatchToGroupPriority(t *testing.T) {
	fx := newFixtures(t)
	grp, err := fx.st.CreateGroup("prio-group", fx.L2, []int{fx.W1}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	cases := []struct {
		name       string
		defPrio    *int
		explicit   *int
		wantPrio   int
		wantErrMsg string
	}{
		{name: "explicit wins over group default", defPrio: intp(42), explicit: intp(5), wantPrio: 5},
		{name: "nil inherits group default", defPrio: intp(42), explicit: nil, wantPrio: 42},
		{name: "nil with unset default stays 0", defPrio: intp(0), explicit: nil, wantPrio: 0},
		{name: "out-of-range explicit rejected", defPrio: intp(42), explicit: intp(100), wantErrMsg: "priority"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := fx.st.SetGroupPolicy(context.Background(), grp, tc.defPrio, nil); err != nil {
				t.Fatalf("SetGroupPolicy: %v", err)
			}
			_, taskID, err := fx.flow.DispatchToGroup(fx.L2, grp, fx.W1, "prio task", tc.explicit, nil, nil)
			if tc.wantErrMsg != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrMsg) {
					t.Fatalf("DispatchToGroup = %v, want error containing %q", err, tc.wantErrMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("DispatchToGroup: %v", err)
			}
			task, err := fx.st.GetTask(taskID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if task.Priority != tc.wantPrio {
				t.Errorf("task.Priority = %d, want %d", task.Priority, tc.wantPrio)
			}
		})
	}
}

// TestDispatchToGroupPriorityClampsDefault (S30, review Minor-3): a
// hand-edited out-of-range groups.default_priority (bypassing the API
// validation) must be clamped into [0,99] instead of failing CreateTask
// after the dispatch message was already inserted (orphan message).
func TestDispatchToGroupPriorityClampsDefault(t *testing.T) {
	fx := newFixtures(t)
	grp, err := fx.st.CreateGroup("clamp-group", fx.L2, []int{fx.W1}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	for _, c := range []struct {
		set  int
		want int
	}{
		{150, 99},
		{-5, 0},
	} {
		if _, err := fx.st.DB().Exec(
			`UPDATE groups SET default_priority = ? WHERE id = ?`, c.set, grp); err != nil {
			t.Fatalf("UPDATE default_priority=%d: %v", c.set, err)
		}
		_, taskID, err := fx.flow.DispatchToGroup(fx.L2, grp, fx.W1, "clamp task", nil, nil, nil)
		if err != nil {
			t.Fatalf("DispatchToGroup (default=%d): %v", c.set, err)
		}
		task, err := fx.st.GetTask(taskID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if task.Priority != c.want {
			t.Errorf("default_priority=%d: task.Priority = %d, want %d (clamped)", c.set, task.Priority, c.want)
		}
	}
}
