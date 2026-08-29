// Package download is the resumable file downloader and its queue worker: range
// resumption across restarts, progress and speed reporting, and verification of
// the finished blob against the digest the hub advertised (DESIGN sections 1 and
// 7).
package download

// Blank imports: keep the section-14 modules in the build graph until the
// download pool lands here. Delete when the real imports appear.
import (
	_ "golang.org/x/sync/errgroup"
	_ "golang.org/x/sync/semaphore"
)
