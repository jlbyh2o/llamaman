// Package auth owns admin authentication: argon2id password hashing and
// verification, session minting and verification against the stored
// sha256(secret), login lockout, CSRF secret derivation, and the setup token
// that gates first-run claim from a non-loopback caller (DESIGN sections 1, 2.2,
// 2.2a and 3.1).
//
// It speaks no HTTP and imports nothing under internal/api. Every method takes
// and returns domain values — a cookie string, an address, a
// model.SessionCredential — and internal/api adapts it to the middleware's
// Authenticator interface, so dependencies point inward (DESIGN section 1,
// invariant 4) and every rule in here is testable without a request.
package auth
