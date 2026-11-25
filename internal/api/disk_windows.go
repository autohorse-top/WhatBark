//go:build windows

package api

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// diskFree reports the free bytes on the filesystem holding dir.
//
// GetDiskFreeSpaceEx's first size is the bytes available to the calling user
// (matches the Unix Bavail semantics used on other platforms). The call fails
// rather than panic; callers treat a failure as "unknown" and skip the
// free-space guard (fail-open).
func diskFree(dir string) (int64, error) {
	var free uint64
	dirs, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, fmt.Errorf("convert %s to utf16: %w", dir, err)
	}
	if err := windows.GetDiskFreeSpaceEx(dirs, &free, nil, nil); err != nil {
		return 0, fmt.Errorf("GetDiskFreeSpaceEx %s: %w", dir, err)
	}
	return int64(free), nil
}
