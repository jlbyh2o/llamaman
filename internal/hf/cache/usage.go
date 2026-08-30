package cache

import (
	"io/fs"
	"syscall"
)

// allocatedBytes is what a file actually occupies, rather than what it claims:
// `st_blocks × 512`, the unit POSIX fixes regardless of the filesystem's own
// block size.
//
// The two differ in both directions and both matter here. A resumed download's
// `.incomplete` file and a sparse copy occupy less than their length; a small
// file occupies a whole block and so occupies more. `models.bytes_on_disk` and
// `models.total_bytes` are separate columns in section 2.6 for exactly this
// reason — the first is what deleting the model frees, the second is what the
// fit calculator reads — and conflating them would make the delete preview's
// "will free N GB" a number the disk does not agree with.
//
// A platform whose FileInfo carries no Stat_t falls back to the logical size,
// which is the only honest answer available there.
func allocatedBytes(info fs.FileInfo, size int64) int64 {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return size
	}
	return st.Blocks * 512
}

// AllocatedBytes is the exported form, for the callers outside this package that
// have a stat result and must arrive at the SAME number the scan and the delete
// preview did. internal/models' verify pass is the one: a re-stat that reported
// a file's logical size where the scan had reported its allocation would make
// `bytes_on_disk` flip on every verify, and the delete preview's "will free N
// GB" would move with it.
func AllocatedBytes(info fs.FileInfo) int64 {
	return allocatedBytes(info, info.Size())
}
