package prebuilt

import "syscall"

// syscallNoFollow is O_NOFOLLOW, the open(2) flag that makes writeFile refuse a
// path whose final component is a symlink.
//
// It is the second half of the two-entry symlink escape defense in extract.go:
// entry one plants `lib -> /etc`, entry two writes `lib/passwd`. The first
// entry is already rejected by checkLinkTarget, and mkdirNoFollow already
// refuses to descend through a link — this flag means that even if both of
// those were wrong, the write itself still fails rather than landing outside
// the destination.
const syscallNoFollow = syscall.O_NOFOLLOW
