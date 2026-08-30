package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// `llamaman install-units` (DESIGN section 11.3, section 13 step 7).
//
// The binary writes the unit and polkit content, not the shell script: one
// source of truth, testable in Go, and the same command is the F16 repair path.
// This file is the argument surface and the identity lookup; every byte that
// reaches /etc is rendered by internal/systemd, which is also what section
// 5.4a's drift check re-renders — so a host can never be told it has drifted
// from a template nothing would have produced.
//
// It runs as root and creates NOTHING under the state directory (section 11.3).
// Nothing below opens a path under it.

// installUnitsFlags is the flag set of section 11.3's table, and nothing else.
type installUnitsFlags struct {
	identity         string
	prefix           string
	port             int
	userUnits        bool
	noAutostartGrant bool
	repairPolkit     bool
	// root prefixes every path written. It is not in the design's flag table:
	// it exists so the command can be driven against a temp directory by a
	// test, which is the only way to assert what it writes without running the
	// suite as root, and so an image build can stage the files without a live
	// manager. Setting it also skips the root check, since nothing under / is
	// touched.
	root string
	// dryRun prints what would change and writes nothing.
	dryRun bool
}

// installUnitsDeps are the seams a test replaces.
type installUnitsDeps struct {
	geteuid func() int
	lookup  func(name string) (userEntry, error)
	install func(ctx context.Context, opts systemd.InstallOptions) (systemd.InstallResult, error)
}

func defaultInstallUnitsDeps() installUnitsDeps {
	return installUnitsDeps{
		geteuid: os.Geteuid,
		lookup:  func(name string) (userEntry, error) { return lookupUser("/etc/passwd", "/etc/group", name) },
		install: systemd.Install,
	}
}

// InstallUnits renders and installs the systemd unit and polkit files, adds the
// service identity to the systemd-journal group, and reloads the manager.
func InstallUnits(env Env, args []string) error {
	return installUnits(env, args, defaultInstallUnitsDeps())
}

func installUnits(env Env, args []string, deps installUnitsDeps) error {
	var f installUnitsFlags

	fs := flag.NewFlagSet("install-units", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.StringVar(&f.identity, "identity", "", "the service identity the units run as (required)")
	fs.StringVar(&f.prefix, "prefix", "", "installation directory (default /usr/local/bin, or ~<identity>/.local/bin with --user-units)")
	fs.IntVar(&f.port, "port", 0, "render a --port flag into the daemon unit; seeds ui.port_desired on a fresh database")
	fs.BoolVar(&f.userUnits, "user-units", false, "install into /etc/systemd/user and run under the identity's own manager (D2); no polkit rule is written")
	fs.BoolVar(&f.noAutostartGrant, "no-autostart-grant", false, "omit the polkit manage-unit-files branch; autostart becomes read-only in the UI")
	fs.BoolVar(&f.repairPolkit, "repair-polkit", false, "rewrite the polkit files even when they already match (the F9 remediation)")
	fs.BoolVar(&f.dryRun, "dry-run", false, "print what would change and write nothing")
	fs.StringVar(&f.root, "root", "",
		"write into an alternate filesystem root instead of / (the host's service manager is not reloaded)")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: llamaman install-units --identity <user> [flags]\n\n")
		fmt.Fprintf(env.Stderr, "Writes the systemd units and polkit rules from the templates embedded in\n")
		fmt.Fprintf(env.Stderr, "this binary, adds the identity to the %s group, and reloads the\n", systemd.JournalGroup)
		fmt.Fprintf(env.Stderr, "service manager. Root only. It never touches the database or anything else\n")
		fmt.Fprintf(env.Stderr, "under the state directory.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	if f.identity == "" {
		fmt.Fprintf(env.Stderr, "llamaman install-units: --identity is required\n")
		fs.Usage()
		return fmt.Errorf("install-units: --identity is required")
	}

	// Root, and said plainly. Writing /etc/systemd/system, /etc/polkit-1 and a
	// group membership are the only privileged writes in this design besides
	// the self-update oneshot, and every one of them is here.
	if euid := deps.geteuid(); euid != 0 && f.root == "" {
		fmt.Fprintf(env.Stderr, "llamaman install-units: must run as root (euid %d)\n", euid)
		fmt.Fprintf(env.Stderr, "  sudo llamaman install-units --identity %s\n", f.identity)
		return fmt.Errorf("install-units: must run as root")
	}

	user, err := deps.lookup(f.identity)
	if err != nil {
		fmt.Fprintf(env.Stderr, "llamaman install-units: %v\n", err)
		return err
	}

	scope := model.ScopeSystem
	if f.userUnits {
		scope = model.ScopeUser
	}

	prefix := f.prefix
	if prefix == "" {
		prefix = systemd.DefaultPrefix
		if f.userUnits {
			// The D2 topology installs the binary into the identity's own
			// ~/.local/bin, because there the unprivileged daemon performs its
			// own self-update by renaming over that very path, and D15's
			// "never on root's PATH" rationale does not apply.
			if user.Home == "" {
				return fmt.Errorf("install-units: --user-units needs %s to have a home directory; pass --prefix", f.identity)
			}
			prefix = filepath.Join(user.Home, ".local", "bin")
		}
	}

	spec := systemd.Spec{
		Scope:         scope,
		Identity:      f.identity,
		IdentityGroup: user.Group,
		Prefix:        prefix,
		Port:          f.port,
		// The grant is on by default and opted OUT of, because autostart is a
		// feature every install has until an operator decides the deferred
		// escalation of an unscopeable polkit action is not worth it.
		UnitFilesGrant: !f.noAutostartGrant,
	}

	if f.dryRun {
		return printDryRun(env, spec)
	}

	res, err := deps.install(context.Background(), systemd.InstallOptions{
		Spec:         spec,
		Root:         f.root,
		RepairPolkit: f.repairPolkit,
	})
	printInstallResult(env, spec, res)
	if err != nil {
		fmt.Fprintf(env.Stderr, "llamaman install-units: %v\n", err)
		return err
	}
	return nil
}

// printInstallResult is section 13 step 7's "prints what it changed".
func printInstallResult(env Env, spec systemd.Spec, res systemd.InstallResult) {
	fmt.Fprintf(env.Stdout, "llamaman install-units: %s scope, identity %s, prefix %s, template version %d\n",
		spec.Scope, spec.Identity, spec.Prefix, res.TemplateVersion)
	for _, p := range res.Written {
		fmt.Fprintf(env.Stdout, "  wrote      %s\n", p)
	}
	for _, p := range res.Unchanged {
		fmt.Fprintf(env.Stdout, "  unchanged  %s\n", p)
	}
	for _, n := range res.Notes {
		fmt.Fprintf(env.Stdout, "  %s\n", n)
	}
	if !spec.UnitFilesGrant && spec.Scope != model.ScopeUser {
		fmt.Fprintf(env.Stdout,
			"  --no-autostart-grant: autostart is read-only in the UI; enable a unit by hand with\n"+
				"    sudo systemctl enable llamaman-instance@<name>.service\n")
	}
}

// printDryRun renders every file and prints it, writing nothing.
func printDryRun(env Env, spec systemd.Spec) error {
	format := systemd.PolkitFormatRules
	if spec.Scope == model.ScopeUser {
		format = systemd.PolkitFormatNone
	}
	files, err := spec.Files(format)
	if err != nil {
		fmt.Fprintf(env.Stderr, "llamaman install-units: %v\n", err)
		return err
	}
	for _, f := range files {
		fmt.Fprintf(env.Stdout, "----- %s\n%s", f.Path(), f.Content)
	}
	return nil
}

// userEntry is the part of an account this command needs: its primary group
// name for Group=, and its home for the --user-units prefix default.
type userEntry struct {
	Name  string
	UID   int
	GID   int
	Group string
	Home  string
}

// lookupUser reads /etc/passwd and /etc/group directly rather than using
// os/user, whose cgo path is unavailable in this binary (CGO_ENABLED=0 is a hard
// constraint) and whose pure-Go fallback reads the same files anyway — and,
// unlike os/user, this can be pointed at a test fixture.
func lookupUser(passwdPath, groupPath, name string) (userEntry, error) {
	f, err := os.Open(passwdPath)
	if err != nil {
		return userEntry{}, fmt.Errorf("install-units: read %s: %w", passwdPath, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 6 || fields[0] != name {
			continue
		}
		uid, err1 := strconv.Atoi(fields[2])
		gid, err2 := strconv.Atoi(fields[3])
		if err1 != nil || err2 != nil {
			return userEntry{}, fmt.Errorf("install-units: %s has an unparsable uid/gid in %s", name, passwdPath)
		}
		e := userEntry{Name: name, UID: uid, GID: gid, Home: fields[5]}
		e.Group = groupName(groupPath, gid)
		if e.Group == "" {
			// A gid with no name is unusual but not fatal: systemd accepts a
			// numeric Group=, and refusing here would block an install over a
			// cosmetic lookup.
			e.Group = strconv.Itoa(gid)
		}
		return e, nil
	}
	if err := sc.Err(); err != nil {
		return userEntry{}, fmt.Errorf("install-units: read %s: %w", passwdPath, err)
	}
	return userEntry{}, fmt.Errorf("install-units: no such user %q — create it first, or pass the account the daemon should run as", name)
}

// groupName resolves a gid to its name.
func groupName(path string, gid int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	want := strconv.Itoa(gid)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) >= 3 && fields[2] == want {
			return fields[0]
		}
	}
	return ""
}
