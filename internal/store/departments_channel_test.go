package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// TestMigrateDepartmentsGroup (S31): a pre-S31 database (departments without
// group_id) gains the column on Open, keeps legacy rows with group_id NULL,
// and re-opening is a no-op.
func TestMigrateDepartmentsGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-s31.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	legacy := `
CREATE TABLE employees (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  role TEXT NOT NULL,
  kind TEXT NOT NULL DEFAULT 'ai' CHECK (kind IN ('ai', 'human')),
  department_id INTEGER NOT NULL REFERENCES departments(id),
  tool TEXT NOT NULL DEFAULT 'mock',
  tool_args TEXT NOT NULL DEFAULT '{}',
  workdir TEXT NOT NULL DEFAULT '',
  token_hash TEXT NOT NULL DEFAULT '',
  last_seen_at TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE departments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  parent_id INTEGER REFERENCES departments(id) ON DELETE SET NULL,
  leader_id INTEGER REFERENCES employees(id) ON DELETE SET NULL,
  UNIQUE (parent_id, name)
);
`
	if _, err := db.Exec(legacy); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO departments (id, name, parent_id)
		VALUES (1, 'legacy-top', NULL), (2, 'legacy-child', 1)`); err != nil {
		t.Fatalf("seed departments: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var n int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('departments') WHERE name = 'group_id'`).Scan(&n); err != nil {
		t.Fatalf("inspect departments: %v", err)
	}
	if n != 1 {
		t.Fatalf("departments.group_id column count = %d, want 1", n)
	}
	depts, err := s.ListDepartments()
	if err != nil {
		t.Fatalf("ListDepartments: %v", err)
	}
	if len(depts) != 2 {
		t.Fatalf("legacy departments = %d, want 2", len(depts))
	}
	for _, d := range depts {
		if d.GroupID != nil {
			t.Fatalf("legacy department %d group_id = %v, want nil", d.ID, *d.GroupID)
		}
	}

	// The new channel surface works on the migrated DB.
	deptID, gid, err := s.CreateDepartmentWithChannel("migrated-dept", nil)
	if err != nil {
		t.Fatalf("CreateDepartmentWithChannel on migrated db: %v", err)
	}
	d, err := s.GetDepartment(deptID)
	if err != nil || d.GroupID == nil || *d.GroupID != gid {
		t.Fatalf("migrated dept = %+v (err %v), want group_id=%d", d, err, gid)
	}

	// M-1 regression (S31 review): the migrated departments table has no FK
	// on group_id (SQLite forbids adding FKs in ALTER ADD COLUMN), so
	// deleting the channel directly via DeleteGroup must clear the reference
	// itself, matching the fresh-DB ON DELETE SET NULL behavior.
	if err := s.DeleteGroup(context.Background(), gid); err != nil {
		t.Fatalf("DeleteGroup on migrated db: %v", err)
	}
	dm, err := s.GetDepartment(deptID)
	if err != nil {
		t.Fatalf("department must survive channel delete: %v", err)
	}
	if dm.GroupID != nil {
		t.Fatalf("migrated department.group_id = %v, want NULL after channel delete", *dm.GroupID)
	}

	// Re-open: migration is a no-op and data survives.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	depts2, err := s2.ListDepartments()
	if err != nil || len(depts2) != 3 {
		t.Fatalf("departments after re-open = %d (err %v), want 3", len(depts2), err)
	}
}

// TestCreateDepartmentWithChannel (S31): success path — department + channel
// (kind=channel, created_by=0, name = department name), boss as sole member,
// exactly one system message with the S31 body, departments.group_id written
// back, name trimmed.
func TestCreateDepartmentWithChannel(t *testing.T) {
	s := openTestStore(t)

	deptID, gid, err := s.CreateDepartmentWithChannel("  研发部 ", nil)
	if err != nil {
		t.Fatalf("CreateDepartmentWithChannel: %v", err)
	}
	d, err := s.GetDepartment(deptID)
	if err != nil {
		t.Fatalf("GetDepartment: %v", err)
	}
	if d.Name != "研发部" || d.GroupID == nil || *d.GroupID != gid {
		t.Fatalf("department = %+v, want name=研发部 group_id=%d", d, gid)
	}
	g, err := s.GetGroup(gid)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if g.Name != "研发部" || g.Kind != "channel" || g.CreatedBy != 0 {
		t.Fatalf("group = %+v, want name=研发部 kind=channel created_by=0", g)
	}
	members, err := s.GroupMemberIDs(gid)
	if err != nil || len(members) != 1 || members[0] != 0 {
		t.Fatalf("members = %v (err %v), want [0]", members, err)
	}
	msgs, err := s.GroupMessages(gid, 0)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("group messages = %+v (err %v), want exactly 1", msgs, err)
	}
	if msgs[0].Kind != MsgSystem || msgs[0].FromID != nil ||
		msgs[0].Body != `boss created a department channel "研发部"` {
		t.Fatalf("system message = %+v, want from_id NULL body=部门频道创建文案", msgs[0])
	}

	// Child department under the parent also gets its own channel.
	dept2, gid2, err := s.CreateDepartmentWithChannel("前端组", intp(deptID))
	if err != nil {
		t.Fatalf("child CreateDepartmentWithChannel: %v", err)
	}
	d2, _ := s.GetDepartment(dept2)
	g2, _ := s.GetGroup(gid2)
	if d2.GroupID == nil || *d2.GroupID != gid2 || g2.Name != "前端组" || g2.Kind != "channel" {
		t.Fatalf("child dept/group = %+v %+v", d2, g2)
	}
}

// TestCreateDepartmentWithChannelValidation (S31): table-driven validation;
// every failed create must leave zero residual rows (dept or channel).
func TestCreateDepartmentWithChannelValidation(t *testing.T) {
	s := openTestStore(t)
	if _, _, err := s.CreateDepartmentWithChannel("A部", nil); err != nil {
		t.Fatalf("seed A部: %v", err)
	}
	parent, _, err := s.CreateDepartmentWithChannel("父部", nil)
	if err != nil {
		t.Fatalf("seed 父部: %v", err)
	}
	if _, _, err := s.CreateDepartmentWithChannel("C组", intp(parent)); err != nil {
		t.Fatalf("seed C组: %v", err)
	}
	baseDepts, baseGroups := 3, 3

	long65 := strings.Repeat("部", 65)
	cases := []struct {
		name     string
		deptName string
		parentID *int
	}{
		{"empty name", "", nil},
		{"whitespace name", "   ", nil},
		{"65 runes", long65, nil},
		{"duplicate top-level", "A部", nil},
		{"duplicate under same parent", "C组", intp(parent)},
		{"missing parent", "孤儿部", intp(999)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := s.CreateDepartmentWithChannel(c.deptName, c.parentID); err == nil {
				t.Fatalf("want error, got success")
			}
			var depts, groups int
			if err := s.DB().QueryRow(`SELECT COUNT(*) FROM departments`).Scan(&depts); err != nil {
				t.Fatalf("count depts: %v", err)
			}
			if err := s.DB().QueryRow(`SELECT COUNT(*) FROM groups`).Scan(&groups); err != nil {
				t.Fatalf("count groups: %v", err)
			}
			if depts != baseDepts || groups != baseGroups {
				t.Fatalf("residual rows: depts=%d groups=%d, want %d/%d (rolled back)",
					depts, groups, baseDepts, baseGroups)
			}
		})
	}
	// Same name at a different level is allowed (parent dimension).
	if _, _, err := s.CreateDepartmentWithChannel("C组", nil); err != nil {
		t.Fatalf("top-level C组 should be allowed (different level): %v", err)
	}
}

// TestCreateDepartmentTightened (S31): the legacy CreateDepartment path
// adopts the same 1..64-rune + TrimSpace rule, and still creates no channel
// (group_id stays NULL).
func TestCreateDepartmentTightened(t *testing.T) {
	s := openTestStore(t)

	deptID, err := s.CreateDepartment("  padded ", nil)
	if err != nil {
		t.Fatalf("padded name: %v", err)
	}
	d, err := s.GetDepartment(deptID)
	if err != nil {
		t.Fatalf("GetDepartment: %v", err)
	}
	if d.Name != "padded" {
		t.Fatalf("name = %q, want trimmed %q", d.Name, "padded")
	}
	if d.GroupID != nil {
		t.Fatalf("legacy department group_id = %v, want nil", *d.GroupID)
	}

	for _, name := range []string{"", "   ", strings.Repeat("部", 65)} {
		if _, err := s.CreateDepartment(name, nil); err == nil {
			t.Fatalf("CreateDepartment(%q): want error, got success", name)
		}
	}
}

// TestRenameDepartment (S31): renames with 1..64-rune + same-level-unique
// (excluding self) semantics; returns the updated department including
// GroupID for the API layer's channel sync.
func TestRenameDepartment(t *testing.T) {
	s := openTestStore(t)
	deptID, gid, err := s.CreateDepartmentWithChannel("甲部", nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := s.CreateDepartmentWithChannel("乙部", nil); err != nil {
		t.Fatalf("seed sibling: %v", err)
	}

	// Success: echoes the group_id so the caller can sync the channel.
	d, err := s.RenameDepartment(deptID, "  丙部 ")
	if err != nil {
		t.Fatalf("RenameDepartment: %v", err)
	}
	if d.Name != "丙部" || d.GroupID == nil || *d.GroupID != gid {
		t.Fatalf("renamed dept = %+v, want name=丙部 group_id=%d", d, gid)
	}
	if got, _ := s.GetDepartment(deptID); got.Name != "丙部" {
		t.Fatalf("persisted name = %q, want 丙部", got.Name)
	}
	// H-3: the channel row is renamed in the same transaction as the
	// department row — both must carry the new name.
	g, err := s.GetGroup(gid)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if g.Name != "丙部" {
		t.Fatalf("channel name = %q, want 丙部 (H-3: renamed atomically with the department)", g.Name)
	}

	// Renaming to the current name is a no-op success (no self-dup error).
	if _, err := s.RenameDepartment(deptID, "丙部"); err != nil {
		t.Fatalf("rename to own name: %v", err)
	}
	// Same-level duplicate (other department) is rejected.
	if _, err := s.RenameDepartment(deptID, "乙部"); err == nil {
		t.Fatal("same-level duplicate name accepted, want error")
	}
	// Unknown department.
	if _, err := s.RenameDepartment(999, "X"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename unknown = %v, want ErrNotFound", err)
	}
	// Validation.
	for _, name := range []string{"", "   ", strings.Repeat("部", 65)} {
		if _, err := s.RenameDepartment(deptID, name); err == nil {
			t.Fatalf("RenameDepartment(%q): want error", name)
		}
	}

	// Different levels may share a name: renaming the child to the name of
	// an existing top-level department is legal (cross-level uniqueness is
	// not enforced).
	parent, _, err := s.CreateDepartmentWithChannel("父部2", nil)
	if err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	child, _, err := s.CreateDepartmentWithChannel("丁组", intp(parent))
	if err != nil {
		t.Fatalf("seed child: %v", err)
	}
	if _, err := s.RenameDepartment(child, "乙部"); err != nil {
		t.Fatalf("cross-level same name should be allowed: %v", err)
	}
}

// TestRenameDepartmentChannelVanished (H-3): if the channel row is gone by the
// time the rename's transaction runs, the whole rename must roll back
// (wrapped ErrNotFound, department name unchanged). The live race is
// unrepresentable because departments.group_id has ON DELETE SET NULL (the FK
// clears the stale reference the moment the group row is deleted), so the
// test forces an inconsistent reference directly with foreign_keys=OFF.
func TestRenameDepartmentChannelVanished(t *testing.T) {
	s := openTestStore(t)
	deptID, _, err := s.CreateDepartmentWithChannel("甲部", nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatalf("disable fk: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE departments SET group_id = 999999 WHERE id = ?`, deptID); err != nil {
		t.Fatalf("force stale group_id: %v", err)
	}
	if _, err := s.db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatalf("re-enable fk: %v", err)
	}

	if _, err := s.RenameDepartment(deptID, "乙部2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename with vanished channel = %v, want ErrNotFound", err)
	}
	got, err := s.GetDepartment(deptID)
	if err != nil {
		t.Fatalf("GetDepartment: %v", err)
	}
	if got.Name != "甲部" {
		t.Fatalf("department name after rollback = %q, want 甲部 (unchanged)", got.Name)
	}
}

// TestDeleteDepartmentAndChannel (S31): unknown -> ErrNotFound; child dept or
// employees -> ErrInUse; open channel task -> ErrGroupHasOpenTasks with the
// count, everything intact; after the task is canceled the delete succeeds
// (department + channel gone, task survives with group_id NULL, conv_reads
// cleared). Legacy departments without a channel delete plainly (nil gid).
func TestDeleteDepartmentAndChannel(t *testing.T) {
	// Unknown department.
	s0 := openTestStore(t)
	if _, err := s0.DeleteDepartmentAndChannel(999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown dept = %v, want ErrNotFound", err)
	}

	s := openTestStore(t)
	_, _, _, worker := seedOrg(t, s) // worker belongs to "child", not the target dept
	deptID, gid, err := s.CreateDepartmentWithChannel("目标部", nil)
	if err != nil {
		t.Fatalf("CreateDepartmentWithChannel: %v", err)
	}

	// Child department blocks.
	child, _, err := s.CreateDepartmentWithChannel("子部", intp(deptID))
	if err != nil {
		t.Fatalf("child dept: %v", err)
	}
	if _, err := s.DeleteDepartmentAndChannel(deptID); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete with child dept = %v, want ErrInUse", err)
	}
	if _, err := s.DeleteDepartmentAndChannel(child); err != nil {
		t.Fatalf("delete child first: %v", err)
	}

	// Employees block.
	empID, err := s.CreateEmployee("dept-emp", "worker", KindAI, deptID)
	if err != nil {
		t.Fatalf("CreateEmployee: %v", err)
	}
	if _, err := s.DeleteDepartmentAndChannel(deptID); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete with employees = %v, want ErrInUse", err)
	}
	// Clear the blocker so the open-task scenario below can run.
	if err := s.DeleteEmployee(empID); err != nil {
		t.Fatalf("DeleteEmployee: %v", err)
	}

	// Open task anchored to the channel blocks; the task's owner may live in
	// a different department.
	tid, err := s.CreateTask(worker, KindDispatch, "open job", nil, nil, 0, 0, intp(gid))
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	var openErr ErrGroupHasOpenTasks
	if _, err := s.DeleteDepartmentAndChannel(deptID); !errors.As(err, &openErr) || openErr.Count != 1 {
		t.Fatalf("delete with open task = %v, want ErrGroupHasOpenTasks{Count:1}", err)
	}
	// Everything intact after the refused delete.
	if _, err := s.GetDepartment(deptID); err != nil {
		t.Fatalf("department gone after refused delete: %v", err)
	}
	if _, err := s.GetGroup(gid); err != nil {
		t.Fatalf("channel gone after refused delete: %v", err)
	}

	// Cancel the task, then delete succeeds.
	if err := s.SetTaskStatus(tid, TaskCanceled, "S31 test"); err != nil {
		t.Fatalf("cancel task: %v", err)
	}
	gotGID, err := s.DeleteDepartmentAndChannel(deptID)
	if err != nil {
		t.Fatalf("DeleteDepartmentAndChannel after cancel: %v", err)
	}
	if gotGID == nil || *gotGID != gid {
		t.Fatalf("deleted channel id = %v, want %d", gotGID, gid)
	}
	if _, err := s.GetDepartment(deptID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("department after delete = %v, want ErrNotFound", err)
	}
	if _, err := s.GetGroup(gid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("channel after delete = %v, want ErrNotFound", err)
	}
	task, err := s.GetTask(tid)
	if err != nil {
		t.Fatalf("task should survive: %v", err)
	}
	if task.GroupID != nil {
		t.Fatalf("task.group_id = %v, want NULL (SET NULL)", *task.GroupID)
	}
	var cursors int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM conv_reads WHERE peer_id = ?`, groupPeerID(gid)).Scan(&cursors); err != nil {
		t.Fatalf("count conv_reads: %v", err)
	}
	if cursors != 0 {
		t.Fatalf("conv_reads for deleted channel = %d, want 0", cursors)
	}

	// Legacy department (no channel) deletes plainly and reports nil gid.
	legacy, err := s.CreateDepartment("存量部", nil)
	if err != nil {
		t.Fatalf("legacy dept: %v", err)
	}
	gidOut, err := s.DeleteDepartmentAndChannel(legacy)
	if err != nil {
		t.Fatalf("legacy delete: %v", err)
	}
	if gidOut != nil {
		t.Fatalf("legacy delete returned gid %v, want nil", *gidOut)
	}
	if _, err := s.GetDepartment(legacy); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy dept after delete = %v, want ErrNotFound", err)
	}
}

// TestDepartmentChannelFKSetNull (S31): deleting a channel group directly
// (via DeleteGroup, not the department endpoint) leaves the department row
// with group_id NULL (FK ON DELETE SET NULL on fresh databases).
func TestDepartmentChannelFKSetNull(t *testing.T) {
	s := openTestStore(t)
	deptID, gid, err := s.CreateDepartmentWithChannel("fk部", nil)
	if err != nil {
		t.Fatalf("CreateDepartmentWithChannel: %v", err)
	}
	if err := s.DeleteGroup(context.Background(), gid); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}
	d, err := s.GetDepartment(deptID)
	if err != nil {
		t.Fatalf("department must survive direct channel delete: %v", err)
	}
	if d.GroupID != nil {
		t.Fatalf("department.group_id = %v, want NULL after channel delete", *d.GroupID)
	}
}

// TestRemoveGroupMemberChannelFloor (S31): channels may shrink to a single
// remaining member (the boss, who is always protected); ordinary groups keep
// the >=2 floor.
func TestRemoveGroupMemberChannelFloor(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, _, leader, worker := seedOrg(t, s)
	deptID, gid, err := s.CreateDepartmentWithChannel("频道部", nil)
	if err != nil {
		t.Fatalf("CreateDepartmentWithChannel: %v", err)
	}
	// Two employees join the channel from other departments.
	other, err := s.CreateEmployee("chan-guest", "worker", KindAI, deptID)
	if err != nil {
		t.Fatalf("CreateEmployee: %v", err)
	}
	if _, err := s.AddGroupMembers(gid, []int{worker, other}); err != nil {
		t.Fatalf("AddGroupMembers: %v", err)
	}
	if err := s.RemoveGroupMember(ctx, gid, other); err != nil {
		t.Fatalf("remove to 2 members (3->2): %v", err)
	}
	// Channel 2 -> 1 is allowed: only the boss remains.
	if err := s.RemoveGroupMember(ctx, gid, worker); err != nil {
		t.Fatalf("remove to 1 member (channel floor): %v", err)
	}
	members, err := s.GroupMemberIDs(gid)
	if err != nil || len(members) != 1 || members[0] != 0 {
		t.Fatalf("channel members = %v (err %v), want [0]", members, err)
	}
	// The boss can never be removed.
	if err := s.RemoveGroupMember(ctx, gid, 0); err == nil {
		t.Fatal("boss removal from channel accepted, want error")
	}
	// Groups keep the >=2 floor: a 2-member group cannot lose its last
	// non-creator member.
	gidPlain, err := s.CreateGroup("普通群", leader, []int{worker}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if err := s.RemoveGroupMember(ctx, gidPlain, worker); err == nil {
		t.Fatal("group 2->1 member removal accepted, want error (floor 2)")
	}
	// Removing a non-member is an error; unknown group is ErrNotFound.
	if err := s.RemoveGroupMember(ctx, gid, worker); err == nil {
		t.Fatal("remove non-member accepted, want error")
	}
	if err := s.RemoveGroupMember(ctx, 999, worker); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove from unknown group = %v, want ErrNotFound", err)
	}
}
