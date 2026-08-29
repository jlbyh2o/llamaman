// Package github is the llama.cpp Releases client: it resolves a channel to a
// concrete tag, picks the right asset for this host, and caches responses by
// ETag so the polling that keeps the version list fresh costs almost nothing
// (DESIGN section 1).
package github
