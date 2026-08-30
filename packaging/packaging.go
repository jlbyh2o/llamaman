// Package packaging holds the systemd unit and polkit templates that
// `llamaman install-units` renders (DESIGN sections 5 and 13). They live in
// their own package, embedded here, rather than inside internal/systemd:
// Go's //go:embed directive cannot reach outside the embedding package's own
// directory, so internal/systemd — which is not this directory's sibling —
// could never embed packaging/*.tmpl directly. Giving the templates their own
// tiny package with its own embed.FS, and having internal/systemd import that
// FS, solves the boundary once instead of forcing the templates to move under
// internal/systemd (see packaging/README.md for the decision record).
package packaging

import "embed"

// Templates is the embedded set of unit and polkit template files, rooted at
// "templates/". Every substitution in them is written `@LIKE_THIS@` and is
// filled in by internal/systemd's renderer, which is also the only reader of
// this FS: the render step is what DESIGN section 5.4a's drift check re-runs
// against an installed file, so there is exactly one producer of unit content
// in the whole design.
//
//go:embed templates
var Templates embed.FS
