// Package gguf is a pure-Go reader for GGUF headers over an io.ReaderAt, so the
// same parser serves a local file and an HTTP Range reader against a remote
// repository. It imports nothing outside the standard library (DESIGN section 1,
// invariant 5; DESIGN section 8.5).
package gguf
