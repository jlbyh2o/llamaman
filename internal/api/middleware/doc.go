// Package middleware is the http.Handler wrapping the API runs behind: session
// authentication, CSRF double-submit verification, rate limiting, idempotency
// key handling, request logging and panic recovery. It is deliberately small —
// roughly the amount of code a third-party web framework would have replaced
// (DESIGN sections 1 and 3).
package middleware
