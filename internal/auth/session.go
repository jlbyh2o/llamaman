package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Session secrets and the CSRF double-submit (DESIGN section 3).
//
// The cookie is `<session_id>.<secret>` and only `sha256(secret)` is stored, so
// a database read — a `db-backups/` entry, a `VACUUM INTO` snapshot, a
// diagnostics bundle — cannot mint a usable cookie. The id half is stored in the
// clear precisely so the row can be FOUND without the secret: the lookup is by
// primary key and the comparison is constant time, rather than a lookup by hash
// that would have to be an index on a secret-derived value.
//
// The CSRF token is an HMAC of the session's own `csrf_secret`, keyed by that
// secret and computed over the session id. Two properties follow, and both are
// requirements rather than conveniences: it needs no process-wide key, so a
// daemon restart does not invalidate the `lm_csrf` cookies of sessions that
// survived in the database (§3.1's sessions outlive a restart); and it is bound
// to one session, so a token minted for one login cannot authorize a request
// carrying another session's cookie.

// secretLen is the size of the session secret and of a csrf_secret, in bytes.
// Thirty-two bytes is DESIGN §2.2's "sha256 of the 32-byte secret half".
const secretLen = 32

// cookieSeparator splits the two halves of `lm_session`. It is a character that
// cannot occur in base64url, so the split is unambiguous.
const cookieSeparator = "."

// ErrMalformedCookie is returned when a session cookie is not `<id>.<secret>`.
// Callers turn every session failure — this one included — into the same 401, so
// that the endpoint is not an oracle for which half was wrong.
var ErrMalformedCookie = errors.New("auth: malformed session cookie")

// randomSecret returns n cryptographically random bytes encoded base64url
// without padding, which is safe in a cookie value and in a header.
func randomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashSecret is the one-way function behind `sessions.token_hash` and
// `setup_claim.token_hash`: hex-encoded sha256, which is what §2.2's TEXT columns
// hold.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// equalHash compares two hex hashes in constant time. The values are not secret
// — they are hashes — but the comparison is still constant time so that no timing
// path in the login flow depends on how much of a credential was correct.
func equalHash(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// composeCookie builds the `lm_session` value from its two halves.
func composeCookie(id, secret string) string { return id + cookieSeparator + secret }

// splitCookie parses `<id>.<secret>`.
func splitCookie(v string) (id, secret string, err error) {
	id, secret, ok := strings.Cut(v, cookieSeparator)
	if !ok || id == "" || secret == "" {
		return "", "", ErrMalformedCookie
	}
	return id, secret, nil
}

// CSRFToken derives the double-submit token for a session: the value of the
// non-HttpOnly `lm_csrf` cookie AND of the `X-CSRF-Token` header that must echo
// it (§3).
//
// It is deterministic, so it can be recomputed on any later request — including
// after a daemon restart — from the session row alone.
func CSRFToken(sessionID, csrfSecret string) string {
	mac := hmac.New(sha256.New, []byte(csrfSecret))
	mac.Write([]byte(sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifyCSRF is the double-submit check §3 defines: the cookie and the header
// must be equal to each other AND to the token this session's secret derives.
//
// Both comparisons are needed. Comparing only cookie against header would accept
// any pair an attacker can set on both sides — a subdomain that can write the
// cookie, say — while comparing only the header against the derived token would
// drop the "double submit" half that makes a stolen header useless without the
// cookie.
func verifyCSRF(sessionID, csrfSecret, cookie, header string) bool {
	if cookie == "" || header == "" {
		return false
	}
	want := CSRFToken(sessionID, csrfSecret)
	return subtle.ConstantTimeCompare([]byte(cookie), []byte(want)) == 1 &&
		subtle.ConstantTimeCompare([]byte(header), []byte(want)) == 1
}
