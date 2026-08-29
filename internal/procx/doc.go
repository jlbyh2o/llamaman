// Package procx holds the exec helpers every child process in the project goes
// through: context cancellation escalating from SIGTERM to SIGKILL after a
// grace period, and line-oriented streaming of a child's output (DESIGN
// section 1).
package procx
