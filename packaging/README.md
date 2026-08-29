# packaging/

The systemd unit and polkit templates, embedded into the binary and rendered by
`llamaman install-units`. Their contents, their two rendering modes (system
units and `--user-units`), and the properties CI asserts about them are
specified in **DESIGN section 5** — in particular section 5.2 (the three unit
templates), section 5.2a (the system/user rewrite table), and section 5.5 (the
polkit rules that let the service identity manage its own units).

Nothing here yet: the templates land with `internal/systemd`'s renderer, which
is what `llamaman install-units` calls.

## Decision: templates stay in this package, embedded here

Go's `//go:embed` directive can only reach files inside the embedding
package's own directory. `internal/systemd` is not a sibling of `packaging/`,
so it can never `//go:embed packaging/*` directly — the two options were
either move the templates under `internal/systemd` or give `packaging/` its
own tiny Go file holding the `embed.FS`.

This directory keeps the templates and gets `packaging.go`, which exports
`Templates embed.FS` over `packaging/templates/`. `internal/systemd` imports
that FS (today only as a blank import — see the comment in
`internal/systemd/doc.go` — since no renderer reads it yet). Rationale: the
templates are packaging artifacts, not systemd-control-plane logic, so they
read more naturally next to this README and the install-time documentation
than inside `internal/systemd` alongside the D-Bus and exec controllers; and a
dedicated package keeps `go:embed`'s directory rule from ever forcing a choice
between "flatten the package layout" and "duplicate the templates."

`packaging/templates/*.tmpl` today hold placeholder content only, each headed
by a comment naming DESIGN sections 5 and 13; the real unit and polkit rule
content, and the render step that fills them in, land with
`internal/systemd`'s Controller implementations.
