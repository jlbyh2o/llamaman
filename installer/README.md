# installer/

`install.sh` — the one-liner that installs Llama Man on a fresh host. Its
argument surface, its ordered steps, its uninstall path, and the guarantee that
`--uninstall` leaves the state directory intact are specified in **DESIGN
section 13**; the CI matrix that exercises it on a real `ubuntu-24.04` runner
(default, `--prefix`, `--dedicated-user`) is **DESIGN section 16.2**, job
`install-system`.

The script itself is not written yet: it installs units and a binary that do not
exist. It lands with DESIGN section 13.
