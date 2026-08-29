// Package prebuilt installs a llama.cpp release tarball: fetch, hardened
// extraction that refuses path traversal and unexpected entry types, an
// ELF/glibc compatibility check against this host, and verification of the
// result before it becomes an installable version (DESIGN section 1).
package prebuilt
