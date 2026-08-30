package systemd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// `llamaman install-units` (DESIGN section 11.3, section 13 step 7).
//
// This is the ONE component that decides the topology: it writes the units,
// chooses /etc/systemd/user over /etc/systemd/system, installs or omits the
// polkit rule, and renders `serve --scope user` into the daemon's ExecStart so
// the daemon is TOLD which topology it is in rather than inferring it.
//
// It runs as root and it creates NOTHING under the state directory — not the
// database, not its WAL sidecars, not secret.key, not a directory (section
// 11.3). A root-created llamaman.db is a 0600 file the service identity could
// never write, and the same hazard applies verbatim to a root-created -wal or
// -shm beside one. Nothing in this file opens a path under the state directory
// at all, which is how that guarantee is kept rather than remembered.

// JournalGroup is the group journald ships whose members may read the whole
// journal (D77).
const JournalGroup = "systemd-journal"

// InstallOptions drives one install.
type InstallOptions struct {
	// Spec is what to render.
	Spec Spec

	// Root prefixes every path written. Empty means "/". It exists so the whole
	// install can be driven against a temp directory in a test — the alternative
	// being a suite that either runs as root against the real /etc or does not
	// exist.
	Root string

	// PolkitFormat forces a format. Empty detects one, and writes both when the
	// detection is ambiguous. Ignored in user scope, where there is no polkit
	// rule at all.
	PolkitFormat PolkitFormat

	// RepairPolkit is `--repair-polkit`, the F9 remediation. It writes BOTH
	// polkit formats and rewrites them even when byte-identical: the reason a
	// human is running it is that polkit denied something, and "the file
	// already matches" is exactly the answer that leaves them stuck.
	RepairPolkit bool

	// detectPolkit, grantJournal and reload are injected by tests.
	detectPolkit func(root string) (PolkitFormat, string)
	grantJournal func(root, identity string) (string, error)
	reload       func(ctx context.Context) error
}

// InstallResult is what changed, for the "prints what it changed" half of
// section 13 step 7.
type InstallResult struct {
	// Written are the absolute paths whose content changed (or that did not
	// exist).
	Written []string
	// Unchanged are the paths that already matched, which is the ordinary
	// outcome of re-running the installer for an upgrade.
	Unchanged []string
	// Notes are the human-readable lines the command prints: the journal-group
	// grant, the polkit format chosen and why, and the commands a user-scope
	// install still has to run.
	Notes []string
	// TemplateVersion is the stamp every file was written with.
	TemplateVersion int
}

// Install renders and writes the units and polkit files, then reloads the
// manager.
func Install(ctx context.Context, opts InstallOptions) (InstallResult, error) {
	res := InstallResult{TemplateVersion: TemplateVersion}

	spec, err := opts.Spec.resolve()
	if err != nil {
		return res, err
	}

	format := PolkitFormatNone
	if spec.Scope != model.ScopeUser {
		switch {
		case opts.RepairPolkit:
			format = PolkitFormatBoth
			res.Notes = append(res.Notes,
				"--repair-polkit: writing both the .rules and .pkla forms so the host has one whichever polkit it runs")
		case opts.PolkitFormat != "":
			format = opts.PolkitFormat
		default:
			detect := opts.detectPolkit
			if detect == nil {
				detect = detectPolkitFormat
			}
			var why string
			format, why = detect(opts.Root)
			res.Notes = append(res.Notes, why)
		}
	}

	files, err := spec.Files(format)
	if err != nil {
		return res, err
	}

	for _, f := range files {
		abs := filepath.Join(opts.Root, f.Path())
		force := opts.RepairPolkit && !f.Unit
		changed, err := writeFile(abs, f.Content, f.Mode, force)
		if err != nil {
			return res, err
		}
		if changed {
			res.Written = append(res.Written, abs)
		} else {
			res.Unchanged = append(res.Unchanged, abs)
		}
	}
	sort.Strings(res.Written)
	sort.Strings(res.Unchanged)

	// The journal grant is install-units' job and not install.sh's, precisely
	// so that the F16 repair path re-applies it (D77).
	grant := opts.grantJournal
	if grant == nil {
		grant = grantJournalGroup
	}
	note, err := grant(opts.Root, spec.Identity)
	if note != "" {
		res.Notes = append(res.Notes, note)
	}
	if err != nil {
		res.Notes = append(res.Notes, "journal group: "+err.Error())
	}

	if spec.Scope == model.ScopeUser {
		// Root's own `systemctl --user` talks to ROOT's manager and silently
		// does nothing useful, so this command does not attempt the reload it
		// cannot correctly perform. The canonical sequence lives in section
		// 5.2a item (3) and is printed here for whoever is driving the install.
		res.Notes = append(res.Notes, userScopeCommands(spec.Identity)...)
		return res, nil
	}

	reload := opts.reload
	if reload == nil {
		// An install into an alternate root wrote files the HOST's manager does
		// not have and must not be told about. Reloading it there would be a
		// privileged call about unit directories nothing touched.
		if isAlternateRoot(opts.Root) {
			res.Notes = append(res.Notes,
				"alternate root "+opts.Root+": the host's service manager was not reloaded")
			return res, nil
		}
		reload = systemReload
	}
	if err := reload(ctx); err != nil {
		res.Notes = append(res.Notes, "daemon-reload failed: "+err.Error())
		return res, err
	}
	res.Notes = append(res.Notes, "systemctl daemon-reload: ok")
	return res, nil
}

// isAlternateRoot reports whether Root points somewhere other than the live
// filesystem.
func isAlternateRoot(root string) bool { return root != "" && root != "/" }

// userScopeCommands is the section 5.2a item (3) sequence, verbatim, for a
// topology where a root process cannot reach the target manager.
func userScopeCommands(identity string) []string {
	return []string{
		"user scope: this command cannot reload the target manager — root's own `systemctl --user` addresses root's manager.",
		"run, as root, after this command:",
		"  loginctl enable-linger " + identity,
		"  uid=$(id -u " + identity + ")",
		"  runuser -u " + identity + " -- env XDG_RUNTIME_DIR=/run/user/$uid \\",
		"    DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$uid/bus systemctl --user daemon-reload",
	}
}

// systemReload is the real daemon-reload, through the control channel this
// package already owns.
func systemReload(ctx context.Context) error {
	c, err := NewExecController(ExecOptions{Scope: model.ScopeSystem})
	if err != nil {
		return err
	}
	return c.Reload(ctx)
}

// writeFile writes content atomically when it differs from what is there.
//
// "Rewrites only what changed" is section 13 step 11's promise for a re-run
// installer, and it is also what keeps an upgrade from touching a file's mtime
// for no reason. The write itself is temp-file-plus-rename so a crash cannot
// leave a half-written unit in /etc, which systemd would then refuse to load.
func writeFile(path, content string, mode os.FileMode, force bool) (bool, error) {
	if !force {
		if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
			return false, nil
		}
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("systemd: create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return false, fmt.Errorf("systemd: stage %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return false, fmt.Errorf("systemd: write %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return false, fmt.Errorf("systemd: chmod %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, fmt.Errorf("systemd: sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("systemd: close %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, fmt.Errorf("systemd: install %s: %w", path, err)
	}
	return true, nil
}

// pkactionVersion matches `pkaction version 0.120`.
var pkactionVersion = regexp.MustCompile(`(\d+)\.(\d+)`)

// detectPolkitFormat decides which authorization file this host understands.
//
// polkit gained JavaScript rules in 0.106. Below that only the local-authority
// .pkla works, and .pkla cannot express per-action branching at all — it keys on
// action ids only — so the manage-units stanza it writes is necessarily broader
// than the .rules equivalent. When the version cannot be determined, BOTH are
// written: a host with a rules-capable polkit ignores the .pkla directory
// entirely, so the redundant file costs nothing, while guessing wrong in either
// direction costs the user a working control plane.
func detectPolkitFormat(root string) (PolkitFormat, string) {
	bin, err := exec.LookPath("pkaction")
	if err != nil {
		// No pkaction. Fall back to what is on disk: a rules.d directory is
		// only created by a polkit that reads it.
		if fi, err := os.Stat(filepath.Join(root, DirPolkitRules)); err == nil && fi.IsDir() {
			return PolkitFormatRules, "polkit: pkaction not found; /etc/polkit-1/rules.d exists, writing the .rules form"
		}
		return PolkitFormatBoth, "polkit: version could not be determined; writing both the .rules and .pkla forms"
	}

	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		return PolkitFormatBoth, "polkit: `pkaction --version` failed; writing both the .rules and .pkla forms"
	}
	return polkitFormatFromVersion(string(out))
}

// polkitFormatFromVersion reads `pkaction version 0.120`.
func polkitFormatFromVersion(out string) (PolkitFormat, string) {
	m := pkactionVersion.FindStringSubmatch(out)
	if m == nil {
		return PolkitFormatBoth, "polkit: `pkaction --version` was unparsable; writing both the .rules and .pkla forms"
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major > 0 || minor >= 106 {
		return PolkitFormatRules, fmt.Sprintf("polkit: version %d.%d supports rules.d; writing the .rules form", major, minor)
	}
	return PolkitFormatPKLA, fmt.Sprintf("polkit: version %d.%d predates rules.d; writing the .pkla form", major, minor)
}

// grantJournalGroup adds the service identity to systemd-journal, idempotently.
//
// This is where the grant belongs — a root-only, idempotent change the F16
// repair path must re-apply — rather than in install.sh (D77). It matters most
// on the --dedicated-user topology, where the account is a system account whose
// messages journald keeps in the SYSTEM journal: without the grant every journal
// feature in this design returns an empty stream with exit 0, which is a
// required SPEC section 3.3 feature failing silently on a supported install.
func grantJournalGroup(root, identity string) (string, error) {
	members, found, err := groupMembers(filepath.Join(root, "/etc/group"), JournalGroup)
	if err != nil {
		return "", err
	}
	if !found {
		// journald ships the group; this command creates no group. A host
		// without it is a host whose journald was built differently, and
		// inventing the group would not grant anything.
		return fmt.Sprintf("journal group: %s does not exist on this host; journal reading may be limited", JournalGroup), nil
	}
	for _, m := range members {
		if m == identity {
			return fmt.Sprintf("journal group: %s is already a member of %s", identity, JournalGroup), nil
		}
	}

	usermod, err := exec.LookPath("usermod")
	if err != nil {
		return fmt.Sprintf("journal group: usermod not found; run `sudo usermod -aG %s %s` by hand",
			JournalGroup, identity), nil
	}
	out, err := exec.Command(usermod, "-aG", JournalGroup, identity).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("usermod -aG %s %s: %w: %s",
			JournalGroup, identity, err, trimProse(string(out)))
	}
	return fmt.Sprintf("journal group: added %s to %s", identity, JournalGroup), nil
}

// groupMembers reads one group's supplementary member list from an /etc/group
// file.
//
// It parses the file directly rather than using os/user, whose cgo path is
// unavailable in this binary (CGO_ENABLED=0 is a hard constraint) and whose
// pure-Go fallback reads the same file anyway — and, unlike os/user, this can be
// pointed at a test root.
func groupMembers(path, group string) ([]string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("systemd: read %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 4 || fields[0] != group {
			continue
		}
		var members []string
		for _, m := range strings.Split(fields[3], ",") {
			if m = strings.TrimSpace(m); m != "" {
				members = append(members, m)
			}
		}
		return members, true, nil
	}
	if err := sc.Err(); err != nil {
		return nil, false, fmt.Errorf("systemd: read %s: %w", path, err)
	}
	return nil, false, nil
}
