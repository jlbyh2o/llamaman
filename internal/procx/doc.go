// Package procx holds the exec helpers every child process in the project goes
// through: context cancellation escalating from SIGTERM to SIGKILL after a
// grace period, and line-oriented streaming of a child's output (DESIGN
// section 1).
//
// # The group, not the process
//
// Every child is started in its own process group and every signal is sent to
// that group. A source build is `cmake` → `ninja` → N compilers (DESIGN section
// 6.5), and a cancellation that signals only the leader leaves the compilers
// running on a host the user believes is idle, still holding the pipes that
// keep the parent's Wait from returning. Section 6.5's cancellation rule —
// "SIGTERM the process group → SIGKILL after 10 s" — is therefore the whole
// contract of Run, and DefaultGrace is that ten seconds.
//
// # Merged output, in arrival order
//
// stdout and stderr are read concurrently and delivered to one serialized
// callback, which is what section 6.5 means by "merges stdout and stderr line
// by line": cmake writes progress to stdout and errors to stderr, and a log
// that interleaves them the way a terminal would is the one a person can read.
// Lines are capped rather than unbounded, and a capped line is MARKED rather
// than silently shortened, because a build log that quietly loses output is
// worse than one that says it lost some.
//
// # What is deliberately not here
//
// No PTY (DESIGN section 14 rules out creack/pty: nothing this daemon runs
// needs a terminal), no shell (every command is an argv, so nothing this
// package runs can be word-split or glob-expanded by accident), and no
// retry/backoff policy — how many times a compile is worth re-running is D20's
// question and belongs to the build pipeline that knows why it failed.
package procx
