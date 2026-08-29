// Package web serves the built single-page UI out of the binary: a go:embed of
// the Vite output, immutable caching for the content-hashed assets, no-store for
// index.html, and an SPA fallback that answers an unknown HTML path with
// index.html so client-side routes survive a page reload. Unknown /api paths are
// not this handler's business and are answered by the API mux (DESIGN sections 1
// and 4).
package web
