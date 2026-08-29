// Package model is the pure domain layer: entity structs, state enums,
// transition tables, validation rules, the FlagSet that describes a llama.cpp
// invocation, and the closed enum of API error codes. It performs no I/O, reads
// no clock and touches no database, which is what makes it trivially testable,
// and it imports nothing outside the standard library (DESIGN section 1,
// invariant 5).
package model
