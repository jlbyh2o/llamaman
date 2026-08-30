// Package cache owns the Hugging Face hub cache on disk (DESIGN sections 7.2,
// 7.2a; D26, D27, D28).
//
// It is deliberately the whole filesystem half and nothing else: no database, no
// HTTP client, no service state. What lives here is the layout — the path
// scheme, the interop lock, the blobs and the relative snapshot symlinks that
// point at them — plus the three operations that read or write it: detecting a
// hub directory, walking one, and deleting from one safely. internal/models is
// what turns any of that into catalog rows.
//
// # The layout is huggingface_hub's, and that is the whole point
//
// SPEC section 3.2 promises one shared cache: a model Llama Man downloads is one
// `hf download` would find, and the reverse. A promise about a directory layout
// is only as true as the strings that build it, so every path in this product is
// built by layout.go and nowhere else, and the checked-in tree under
// `testdata/hub` — written by hand, not by this code — is what pins them.
//
// # Two halves of one contract
//
// D27 names the lock PATH: `<hub>/.locks/<repo_folder>/<etag>.lock`. The other
// half is the PRIMITIVE, and it is `flock(2)` — the syscall `huggingface_hub`'s
// own `filelock` backend takes. POSIX record locks and BSD file locks are
// independent mechanisms in the kernel, so a correct path taken with `fcntl`
// would interlock with nothing at all and would still pass every path test. Both
// halves are asserted; see Acquire.
//
// # Deleting is a refcount, not a walk
//
// D28: blobs are refcounted across EVERY snapshot in a repository before
// anything is removed, because `blobs/` is shared by every revision and a
// tokenizer two snapshots both link to must survive the deletion of either. The
// plan is computed from the filesystem rather than from the catalog, and the
// same value that is shown as a preview is the one that is executed.
package cache
