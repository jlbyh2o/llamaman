# packaging/

The systemd unit and polkit templates, embedded into the binary and rendered by
`llamaman install-units`. Their contents, their two rendering modes (system
units and `--user-units`), and the properties CI asserts about them are
specified in **DESIGN section 5** — in particular section 5.2 (scope, identity
and the polkit rule), section 5.2a (the system/user rewrite table), sections 5.4
and 5.5 (the daemon unit, the instance template and the target) and section 12.2
(the two self-update actors).

## What is here

| File | Installed as | Scope |
|---|---|---|
| `llamaman.service.tmpl` | `llamaman.service` | both |
| `llamaman-instance@.service.tmpl` | `llamaman-instance@.service` | both |
| `llamaman-instances.target.tmpl` | `llamaman-instances.target` | both |
| `llamaman-update-verify.service.tmpl` | `llamaman-update-verify.service` | both |
| `llamaman-selfupdate.service.tmpl` | `llamaman-selfupdate.service` | **system only** |
| `49-llamaman.rules.tmpl` | `/etc/polkit-1/rules.d/49-llamaman.rules` | system only, polkit >= 0.106 |
| `49-llamaman.pkla.tmpl` | `/etc/polkit-1/localauthority/50-local.d/49-llamaman.pkla` | system only, polkit < 0.106 |

Units go to `/etc/systemd/system` in the default topology and to
`/etc/systemd/user` under `--user-units`, where there is no polkit file at all —
a user manager authorizes its owner unconditionally.

`llamaman-selfupdate.service` exists only in system scope because the privilege
boundary it crosses exists only there: in user scope the daemon performs the
swap in process and `selfupdate-apply` refuses to run (section 5.2a item 2).

## Substitutions

Every placeholder is written `@LIKE_THIS@` so the templates stay readable as
near-valid unit files. `internal/systemd` fills them in and is the only reader
of this directory. Two rules beyond plain replacement, both because a
substitution can render to nothing:

- a line that becomes blank is **dropped**, which is how `Wants=` disappears
  from the user-scope instance unit rather than surviving as a bare `Wants=`;
- a line in which some token rendered empty has its interior spaces collapsed,
  so `ExecStart=<prefix>/llamaman serve` does not acquire dangling spaces when
  neither the scope flag nor the port flag is rendered.

An unresolved `@TOKEN@` fails the render rather than reaching a unit file.

## The version stamp

Every rendered file's **first line** is `# llamaman-units: <N>` (`//` in the
polkit rules file), where `<N>` is `systemd.TemplateVersion` compiled into the
binary that wrote it (D95). It is what makes a content mismatch decidable: the
same stamp with a different hash is a hand-edit and is F16, while an older or
absent stamp is `drift: stale` — the ordinary state of a host that self-updated
across a release which changed a template — and blocks nothing.

**Changing any file in `templates/` means bumping `TemplateVersion` in
`internal/systemd/units.go`.** Forgetting turns a template change into a
fleet-wide false F16. Golden files for both scopes live in
`internal/systemd/testdata/units/`; regenerate them with
`go test ./internal/systemd -update`.

## Decision: templates stay in this package, embedded here

Go's `//go:embed` directive can only reach files inside the embedding package's
own directory. `internal/systemd` is not a sibling of `packaging/`, so it can
never `//go:embed packaging/*` directly — the two options were either move the
templates under `internal/systemd` or give `packaging/` its own tiny Go file
holding the `embed.FS`.

This directory keeps the templates and gets `packaging.go`, which exports
`Templates embed.FS` over `packaging/templates/`. `internal/systemd` imports
that FS. Rationale: the templates are packaging artifacts, not
systemd-control-plane logic, so they read more naturally next to this README and
the install-time documentation than inside `internal/systemd` alongside the
D-Bus and exec controllers; and a dedicated package keeps `go:embed`'s directory
rule from ever forcing a choice between "flatten the package layout" and
"duplicate the templates."
