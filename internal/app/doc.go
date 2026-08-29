// Package app is the composition root. It opens the database, runs migrations
// and the boot integrity check, constructs every service and its dependencies,
// starts the background workers, wires the HTTP mux, and owns graceful
// shutdown. It is the only package allowed to know how the whole program fits
// together; everything it builds is reachable through interfaces owned by the
// consumer (DESIGN section 1).
package app
