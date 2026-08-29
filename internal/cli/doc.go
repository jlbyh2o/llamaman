// Package cli implements every subcommand except the dispatch itself: status,
// doctor, diagnostics, reset-password, restore-db, install-units,
// instance-exec, selfupdate-apply, update-verify and verify-release, plus the
// serve entry point that hands off to internal/app. cmd/llamaman does argument
// dispatch and nothing else, so each command's behavior is testable without a
// process (DESIGN sections 1, 11.2 and 12).
//
// Every function here is a stub in this skeleton: it prints "not implemented"
// and returns ErrNotImplemented, which cmd/llamaman turns into a non-zero exit.
package cli
