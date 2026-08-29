// Package auth owns admin authentication: argon2id password hashing and
// verification, session minting and verification against the stored
// sha256(secret), login lockout, CSRF secret derivation, and the setup token
// that gates first-run claim from a non-loopback caller (DESIGN sections 1 and
// 3).
package auth

// Blank import: keeps the section-14 module in the build graph until the
// argon2id hasher lands here. Delete when the real import appears.
import (
	_ "golang.org/x/crypto/argon2"
)
