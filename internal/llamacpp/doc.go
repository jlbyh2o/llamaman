// Package llamacpp manages the llama.cpp builds this host has available: the
// version rows that track them, activating one, rolling back to the previous
// one, and garbage-collecting the versions nothing references any more. The
// three ways a version arrives — a GitHub release lookup, a prebuilt tarball,
// or a source build — live in its subpackages (DESIGN section 1).
package llamacpp
