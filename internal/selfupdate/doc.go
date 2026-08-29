// Package selfupdate handles updating the llamaman binary itself: checking for a
// release, downloading it, verifying it against the embedded ed25519 public key
// and its sha256 checksum, and staging the swap that the root oneshot performs.
// Verification must work offline, so the trust root is the compiled-in key
// (DESIGN sections 1 and 12).
package selfupdate
