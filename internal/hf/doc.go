// Package hf is the Hugging Face Hub HTTP client: model search, model metadata,
// the repository file tree, the README behind a model card, resolve URLs, and
// the Range reads a download and a pre-download header peek are built from
// (DESIGN sections 1, 3.6, 7.1, 8.5).
//
// It owns the network half of Hugging Face and nothing else. There is no SQL
// here, no job, no filesystem: internal/hf/cache owns the disk layout,
// internal/hf/download owns the transfer and the rows it writes, and this
// package answers exactly one question — what does the Hub say.
//
// # Three rules that everything else depends on
//
// **True size is `lfs.size`.** A tree entry with an `lfs` object reports its
// real length there; the top-level `size` can be the ~130-byte pointer. Reading
// the wrong one makes a 40 GB model look free, which breaks the fit calculator
// outright and waves a download past the disk guard that exists to stop it. The
// tree call sends `expand=1` for that reason and Tree resolves the field in one
// place (tree.go), so no caller can reach the pointer size by accident.
//
// **The blob name and the HTTP validator are two different strings.** The blob
// name is `x-linked-etag`, de-quoted and `W/`-stripped, equal to the sha256 hex
// for an LFS object; it names `blobs/<etag>` and is never sent in a header. The
// validator is the byte-exact `ETag` of the final response after redirects, and
// it is used for nothing but `If-Range`. Sending the blob name as `If-Range`
// matches no validator any origin will ever compare it against — the server
// answers `200`, the partial is discarded, and resume silently never works while
// every stubbed test passes. FileMeta carries them in two differently named
// fields and OpenParams.ConditionalHeader is the one place the choice between
// them is made (resolve.go).
//
// **The token never crosses a host boundary.** `resolve/` redirects to a CDN
// with its own signed URL; a custom CheckRedirect strips the Authorization
// header on any host change, which is a correctness fix as much as a security
// one — several CDNs reject a request carrying both their signature and an
// Authorization header. The token is read through a function rather than
// captured at construction, so a credential the user revokes stops being sent
// immediately, and MaskToken is the only form of it that ever leaves this
// package.
//
// # The request policy
//
// One shared http.Client with `Timeout: 0` — this client streams multi-gigabyte
// bodies, and a whole-request deadline would kill a healthy 40 GB download;
// per-request contexts and transport-level deadlines bound everything that can
// actually hang. Retries cover 429 and 5xx only, honor `Retry-After`, and use
// jittered exponential backoff over at most five attempts. A two-class limiter
// reserves a slot for interactive work so a bulk metadata refresh cannot starve
// a user-initiated search, and a 30-minute TTL cache serves search, tree and
// metadata.
//
// # Gated repositories
//
// A gate is invisible to metadata: `GET /api/models/{repo}` succeeds while
// `resolve` answers 401 or 403 with `x-error-code: GatedRepo`. errors.go maps
// that to a GatedError carrying the repository and the page a human accepts the
// terms on, because grants are browser-only on the Hub's side and the only
// useful thing this product can do is link out. `RepoNotFound` on an
// existing-looking repository is a different sentence — "sign in", or "this
// token cannot see it" — and PrivateError carries which.
package hf
