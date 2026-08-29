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

// Templates is the embedded set of unit and polkit template files. The files
// committed today are placeholders; the real content — and the render-step
// logic that fills them in — lands with internal/systemd's Controller
// implementations (DESIGN section 5.6).
//
//go:embed templates
var Templates embed.FS
