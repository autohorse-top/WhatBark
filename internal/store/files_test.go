package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestFilesCRUD covers the S12 files/message_files tables: insert, read back,
// ownership validation, message linking, and deletion cascade.
func TestFilesCRUD(t *testing.T) {
	s := openTestStore(t)
	root, _, leader, worker := seedOrg(t, s)
	_ = root

	// FilesDir is derived from the DB location and pre-created.
	filesDir := s.FilesDir
	if filesDir == "" {
		t.Fatal("FilesDir not set")
	}
	for _, sub := range []string{"avatars", "files"} {
		if st, err := os.Stat(filepath.Join(filesDir, sub)); err != nil || !st.IsDir() {
			t.Fatalf("missing files dir %s: %v", sub, err)
		}
	}

	// Table-driven inserts: boss (0) and a real employee are valid owners;
	// a nonexistent employee is rejected.
	cases := []struct {
		name    string
		ownerID int
		wantErr bool
	}{
		{"boss owner 0", 0, false},
		{"employee owner", leader, false},
		{"ghost owner", 99999, true},
		{"negative owner", -1, true},
	}
	ids := map[int]int{} // test case index -> file id (for the valid ones)
	for i, c := range cases {
		stored := fmt.Sprintf("%016x_case%d.txt", i, i)
		id, err := s.InsertFile(c.ownerID, "a.txt", stored, "text/plain", 2)
		if c.wantErr && err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
		if !c.wantErr {
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			ids[i] = id
		}
	}

	// GetFile round-trip on the boss-owned file.
	bossFileID := ids[0]
	f, err := s.GetFile(bossFileID)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if f.OwnerID != 0 || f.Name != "a.txt" || f.Mime != "text/plain" || f.Size != 2 {
		t.Errorf("GetFile mismatch: %+v", f)
	}

	// GetFileByStoredName round-trip + not-found.
	f2, err := s.GetFileByStoredName("0000000000000000_case0.txt")
	if err != nil || f2.ID != bossFileID {
		t.Errorf("GetFileByStoredName: id=%d err=%v", f2.ID, err)
	}
	if _, err := s.GetFileByStoredName("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetFileByStoredName(nope) err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetFile(424242); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetFile(missing) err = %v, want ErrNotFound", err)
	}

	// ListOwnedFiles: boss sees only their file; a foreign id is rejected.
	own, err := s.ListOwnedFiles(0, []int{bossFileID})
	if err != nil || len(own) != 1 || own[0].ID != bossFileID {
		t.Errorf("ListOwnedFiles(0) = %v, %v", own, err)
	}
	if _, err := s.ListOwnedFiles(0, []int{bossFileID, ids[1]}); !errors.Is(err, ErrNotFound) {
		t.Errorf("ListOwnedFiles mixing owners: err = %v, want ErrNotFound", err)
	}
	// Duplicates are tolerated (deduped).
	dup, err := s.ListOwnedFiles(0, []int{bossFileID, bossFileID})
	if err != nil || len(dup) != 1 {
		t.Errorf("ListOwnedFiles dup = %v, %v", dup, err)
	}

	// Linking to a message.
	from := leader
	msgID, err := s.CreateMessage(worker, MsgReport, "hello", &from, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	empFileID := ids[1]
	if err := s.LinkFilesToMessage(msgID, []int{empFileID, empFileID}); err != nil {
		t.Fatalf("LinkFilesToMessage: %v", err)
	}
	lf, err := s.ListMessageFiles(msgID)
	if err != nil || len(lf) != 1 || lf[0].ID != empFileID {
		t.Errorf("ListMessageFiles = %v, %v (want 1 file id %d)", lf, err, empFileID)
	}
	// A missing message is rejected.
	if err := s.LinkFilesToMessage(99999, []int{empFileID}); err == nil {
		t.Error("LinkFilesToMessage(missing msg): expected error")
	}

	// Deletion cascade: removing the file drops the message_files link.
	if err := s.DeleteFile(empFileID); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	lf, err = s.ListMessageFiles(msgID)
	if err != nil || len(lf) != 0 {
		t.Errorf("after DeleteFile, ListMessageFiles = %v, %v (want empty)", lf, err)
	}
	if err := s.DeleteFile(bossFileID); err != nil {
		t.Fatalf("DeleteFile(boss): %v", err)
	}
	if _, err := s.GetFile(bossFileID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted file still readable: %v", err)
	}
	if err := s.DeleteFile(123456); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteFile(missing) err = %v, want ErrNotFound", err)
	}
}

// TestFilesByOwnerAndAvatar covers ListFilesByOwner and employee avatars.
func TestFilesByOwnerAndAvatar(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)

	_, err := s.InsertFile(worker, "w1.bin", "aaaa_w1.bin", "application/octet-stream", 10)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.InsertFile(worker, "w2.bin", "bbbb_w2.bin", "application/octet-stream", 20)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.InsertFile(0, "boss.bin", "cccc_boss.bin", "application/octet-stream", 30)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.ListFilesByOwner(worker)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListFilesByOwner(worker) = %d, want 2", len(got))
	}
	bossFiles, err := s.ListFilesByOwner(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(bossFiles) != 1 {
		t.Fatalf("ListFilesByOwner(0) = %d, want 1", len(bossFiles))
	}

	// Avatar column: set and read back.
	if err := s.SetAvatar(leader, "emp_1.png"); err != nil {
		t.Fatalf("SetAvatar: %v", err)
	}
	emp, err := s.GetEmployee(leader)
	if err != nil {
		t.Fatal(err)
	}
	if emp.Avatar != "emp_1.png" {
		t.Errorf("Avatar = %q, want emp_1.png", emp.Avatar)
	}
	if err := s.SetAvatar(99999, "x.png"); err == nil {
		t.Error("SetAvatar(missing): expected error")
	}
}
