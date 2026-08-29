// Package instances is the instance service: validating a configuration,
// allocating ports, hashing a configuration so a change can be detected, and
// exposing the desired-state API the supervisor reconciles against. It is also
// the only package in the codebase that renders a llama.cpp command line,
// through exactly two functions — RenderArgv for llama-server and
// RenderBenchArgv for llama-bench — both reading the same model.FlagSet (DESIGN
// section 1, invariant 3; D62).
package instances
