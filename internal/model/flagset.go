package model

// FlagSet is the typed form of an instance's `flags_json` column (D41): one
// struct field per llama.cpp flag we expose, so a new upstream flag is a field
// and a golden argv test rather than a migration. A nil field means "do not pass
// the flag", which is distinct from passing its zero value.
//
// The struct is deliberately empty in this skeleton. Its fields are added
// alongside the argv renderers in internal/instances (DESIGN sections 6 and
// 10.1), which are the only place in the codebase permitted to turn a FlagSet
// into a command line (DESIGN section 1, invariant 3).
type FlagSet struct{}
