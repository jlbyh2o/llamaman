// Package mdrender turns attacker-controlled markdown — Hugging Face model
// cards and upstream release changelogs — into HTML on the server with goldmark,
// then sanitizes it with bluemonday. Rendering happens here and nowhere else:
// unsanitized model-card HTML in the origin that holds the admin session cookie
// is a stored-XSS hole (DESIGN sections 1 and 14, D35).
package mdrender

// Blank imports: keep the section-14 modules in the build graph until the
// render-then-sanitize pipeline lands here. Delete when the real imports
// appear.
import (
	_ "github.com/microcosm-cc/bluemonday"
	_ "github.com/yuin/goldmark"
)
