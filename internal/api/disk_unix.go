//go:build !windows

package api

import (
	"fmt"
	"syscall"
)

// diskFree reports the free bytes on the filesystem holding dir.
//
// Unix statfs exposes Bavail (blocks available to an unprivileged user) and
// Bsize (block size); the product is the space a new file could actually use.
// The call fails (e.g. exotic filesystems) rather than panic; callers treat
// a failure as "unknown" and skip the free-space guard (fail-open).
func diskFree(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
