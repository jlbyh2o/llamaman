package middleware

import (
	"net/http"
	"net/url"
	"strings"
)

// safeMethods are the ones the CSRF double-submit does not apply to. DESIGN
// section 3 says "every non-GET must echo it"; HEAD and OPTIONS join GET
// because neither can carry a state change and OPTIONS is what a preflight
// sends before it is allowed to carry anything.
func safeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// CSRF is section 3's double-submit check, plus the two fetch-metadata checks
// it says are made "when present".
//
// It is mounted on `session` routes. A `setup` route has no session and
// therefore no `csrf_secret` to double-submit, so it gets FetchMetadata below
// instead — the half of this check that needs no session.
//
// The three checks are ordered cheapest-first and each one is sufficient on its
// own to reject:
//
//  1. Sec-Fetch-Site — sent by every current browser, unforgeable by page
//     script. `cross-site` is rejected outright.
//  2. Origin — compared against the Host the request was actually served on.
//     A same-origin non-GET always carries it.
//  3. The double-submit pair: the non-HttpOnly `lm_csrf` cookie must be
//     present and must verify against the X-CSRF-Token header for this
//     session. Only internal/auth holds the HMAC key, so the comparison is
//     Authenticator.VerifyCSRF's, in constant time, rather than a == here.
func CSRF(a Authenticator) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if safeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}

			if !sameSite(w, r) {
				return
			}

			sess, ok := SessionFrom(r.Context())
			if !ok {
				// Unreachable through the standard chain — SessionGate runs
				// first on every `session` route and returns 401 without a
				// session. Answering csrf_failed rather than falling through
				// keeps a mis-ordered chain from becoming an authentication
				// bypass.
				WriteError(w, http.StatusForbidden, CodeCSRFFailed,
					"no session to verify a CSRF token against", nil)
				return
			}

			var cookie string
			if c, err := r.Cookie(CookieCSRF); err == nil {
				cookie = c.Value
			}
			header := r.Header.Get(HeaderCSRF)
			if cookie == "" || header == "" || a == nil || !a.VerifyCSRF(r.Context(), sess, cookie, header) {
				WriteError(w, http.StatusForbidden, CodeCSRFFailed,
					"missing or invalid "+HeaderCSRF, nil)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// FetchMetadata is the two fetch-metadata checks above, plus one requirement on
// the body, mounted on `setup` routes — the half of the CSRF check that needs
// no session to key on.
//
// It exists because D38's loopback-or-token rule cannot tell the operator's own
// browser from a hostile page driving it. A page loaded from anywhere can issue
// a `no-cors` POST to `http://127.0.0.1:<port>/api/v1/setup/password`; the
// request arrives from 127.0.0.1, `AuthorizeSetup` waves it through, and the
// host is claimed with the attacker's password before the operator ever opens
// the wizard. Section 3's `Origin`/`Sec-Fetch-Site` checks are exactly what
// distinguishes the two, and "when present" is not a weakness here: every
// browser that can be driven this way sends both, and a caller that sends
// neither is `curl` on the host itself, which already has the state directory.
//
// The Content-Type requirement closes the same hole from the other side. A
// `no-cors` POST is limited to CORS-safelisted content types, and a body sent
// as a bare Blob carries NO Content-Type at all — which is precisely how such a
// request avoids a preflight. Requiring `application/json` on the claim body
// means the only cross-origin request that could reach the handler is one the
// browser preflighted, and this API answers no preflight.
func FetchMetadata() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if safeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			if !sameSite(w, r) {
				return
			}
			if !jsonBody(w, r) {
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// sameSite is checks 1 and 2, shared by both middlewares above. It writes the
// 403 itself and reports whether the request may proceed.
func sameSite(w http.ResponseWriter, r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
		WriteError(w, http.StatusForbidden, CodeCSRFFailed,
			"cross-site requests are not accepted", nil)
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
		u, err := url.Parse(origin)
		if err != nil || !sameHost(u.Host, r.Host) {
			WriteError(w, http.StatusForbidden, CodeCSRFFailed,
				"the Origin header does not match this host", nil)
			return false
		}
	}
	return true
}

// jsonBody requires the one media type this API accepts. The handler's decoder
// rejects a WRONG type already; what this adds is rejecting a MISSING one,
// which is the shape a request built to avoid a preflight has.
func jsonBody(w http.ResponseWriter, r *http.Request) bool {
	mt := strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0])
	if strings.EqualFold(mt, "application/json") {
		return true
	}
	WriteError(w, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType,
		"this request must carry Content-Type: application/json", nil)
	return false
}

// sameHost compares an Origin's host against the request's Host. Both may or
// may not carry a port, and the comparison is case-insensitive because a host
// name is.
func sameHost(origin, host string) bool {
	return origin != "" && strings.EqualFold(origin, host)
}
