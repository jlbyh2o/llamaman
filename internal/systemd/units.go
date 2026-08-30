package systemd

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/packaging"
)

// TemplateVersion is `<N>` in the `# llamaman-units: <N>` stamp every rendered
// unit and polkit file carries as its first line (D95, DESIGN section 5.4).
//
// It is an integer compiled into the binary, and it is what makes a content
// mismatch DECIDABLE. Units are written once at install time and are not
// rewritten by a self-update, while the drift check of section 5.4a renders from
// the RUNNING binary's templates — so on a host that self-updated across a
// release which touched any template the two legitimately differ and nobody
// edited anything. Same stamp with a different hash is a hand-edit (F16); an
// older or absent stamp is `drift: stale`, which blocks nothing.
//
// **Bump this whenever any file under packaging/templates changes.** Forgetting
// turns a template change into a fleet-wide false F16.
const TemplateVersion = 1

// Unit and polkit file names. They are constants because three components have
// to agree on them: the renderer, the polkit rule's name-scoped grant, and the
// drift check.
const (
	UnitDaemon       = "llamaman.service"
	UnitInstance     = "llamaman-instance@.service"
	UnitInstancesTgt = "llamaman-instances.target"
	UnitSelfUpdate   = "llamaman-selfupdate.service"
	UnitUpdateVerify = "llamaman-update-verify.service"

	PolkitRules = "49-llamaman.rules"
	PolkitPKLA  = "49-llamaman.pkla"
)

// Install directories. `install-units` is the only writer of any of them, and
// the daemon never writes any of them at runtime (DESIGN section 5.1).
const (
	DirSystemUnits = "/etc/systemd/system"
	DirUserUnits   = "/etc/systemd/user"
	DirPolkitRules = "/etc/polkit-1/rules.d"
	DirPolkitPKLA  = "/etc/polkit-1/localauthority/50-local.d"
)

// DefaultPrefix is the installation directory when `--prefix` was not given
// (DESIGN section 13). Under `--user-units` the default is
// `~<user>/.local/bin` instead, which only the caller can resolve, so it is not
// a constant here.
const DefaultPrefix = "/usr/local/bin"

// InstanceUnit returns the instance unit name for an instance name, which is the
// only place `%i` is turned into a unit name.
func InstanceUnit(name string) string { return "llamaman-instance@" + name + ".service" }

// Spec is everything the templates need filled in. The same Spec renders the
// files `install-units` writes and the files section 5.4a's drift check compares
// them against — one producer, so the two cannot disagree about what a host
// "should" have.
type Spec struct {
	// Scope selects the whole topology: which ordering directives the units
	// carry, which directory they are installed into, whether a polkit file
	// exists at all, and whether @SYSTEMCTL@ carries --user (DESIGN sections
	// 5.2a and 5.4).
	Scope model.SystemdScope

	// Identity and IdentityGroup render User=/Group= and the polkit subject.
	Identity      string
	IdentityGroup string

	// Prefix is the installation directory, rendered into ExecStart= in every
	// unit that names the binary. Nothing hardcodes it (D15).
	Prefix string

	// Port renders `serve --port N`. Zero renders no flag at all, which is the
	// default: the flag only SEEDS ui.port_desired on a fresh database.
	Port int

	// UnitFilesGrant is false under `install-units --no-autostart-grant`, which
	// renders the polkit manage-unit-files branch as NOT_HANDLED (section 5.2).
	// It has no effect in user scope, where no polkit file is written.
	UnitFilesGrant bool

	// Systemctl is the absolute systemctl path rendered into the two
	// self-update actors' ExecStopPost= lines. Empty resolves it through
	// SystemctlPath(), the deterministic two-candidate probe that is the only
	// producer of a systemctl path in this design (section 12.2) — never a PATH
	// search, so install-units and the drift render agree on any host.
	Systemctl string
}

// File is one rendered file: what to write, where, and with what mode.
type File struct {
	// Name is the base name; Dir is the absolute directory it belongs in.
	Name string
	Dir  string
	Mode fs.FileMode
	// Content is the fully rendered text, stamp line first.
	Content string
	// Unit reports whether this is a systemd unit (as opposed to a polkit
	// file), which is what decides whether install-units' daemon-reload is
	// needed and how the drift check reports it.
	Unit bool
}

// Path is the absolute path this file installs to.
func (f File) Path() string { return path.Join(f.Dir, f.Name) }

// UnitNames returns the units this scope installs, in a stable order.
//
// `llamaman-selfupdate.service` is system-scope only: D12's three-actor design
// exists because an unprivileged daemon cannot overwrite a root-owned binary,
// and in user scope that premise is gone — the daemon performs the swap in
// process and `selfupdate-apply` refuses to run at all (section 5.2a item 2).
// The judge is installed in BOTH topologies.
func UnitNames(scope model.SystemdScope) []string {
	units := []string{UnitDaemon, UnitInstance, UnitInstancesTgt, UnitUpdateVerify}
	if scope != model.ScopeUser {
		units = append(units, UnitSelfUpdate)
	}
	sort.Strings(units)
	return units
}

// UnitDir is where this scope's units are installed. /etc/systemd/user is
// root-writable and is the correct location for admin-installed user units.
func UnitDir(scope model.SystemdScope) string {
	if scope == model.ScopeUser {
		return DirUserUnits
	}
	return DirSystemUnits
}

// PolkitFormat selects which authorization file(s) a host needs. polkit gained
// JavaScript rules in 0.106; older hosts get the local-authority .pkla, and an
// ambiguous detection writes both (DESIGN section 5.2).
type PolkitFormat string

const (
	PolkitFormatRules PolkitFormat = "rules"
	PolkitFormatPKLA  PolkitFormat = "pkla"
	PolkitFormatBoth  PolkitFormat = "both"
	// PolkitFormatNone is user scope: a user manager authorizes its owner
	// unconditionally, so there is no polkit rule in that topology at all.
	PolkitFormatNone PolkitFormat = "none"
)

// Validate reports whether this Spec can be rendered at all.
func (s Spec) Validate() error {
	if !s.Scope.Valid() {
		return fmt.Errorf("systemd: scope must be %q or %q, got %q",
			model.ScopeSystem, model.ScopeUser, s.Scope)
	}
	if s.Identity == "" {
		return fmt.Errorf("systemd: an identity is required (User=/Group= and the polkit subject)")
	}
	if s.Prefix == "" {
		return fmt.Errorf("systemd: a prefix is required (ExecStart= names <prefix>/llamaman)")
	}
	if !strings.HasPrefix(s.Prefix, "/") {
		return fmt.Errorf("systemd: prefix %q must be absolute", s.Prefix)
	}
	if s.Port < 0 || s.Port > 65535 {
		return fmt.Errorf("systemd: port %d out of range", s.Port)
	}
	return nil
}

// resolve fills in the defaults a caller may leave blank.
func (s Spec) resolve() (Spec, error) {
	if s.IdentityGroup == "" {
		s.IdentityGroup = s.Identity
	}
	if s.Systemctl == "" {
		p, err := SystemctlPath()
		if err != nil {
			return s, err
		}
		s.Systemctl = p
	}
	return s, s.Validate()
}

// substitutions is the token table for one template. Every token is written
// `@LIKE_THIS@` so the templates stay readable as near-valid unit files, which
// matters because CI greps the RENDERED user units for the directives that must
// not survive the scope rewrite.
func (s Spec) substitutions() map[string]string {
	user := s.Scope == model.ScopeUser

	// The section 5.2a item (1) rewrite table, applied centrally rather than by
	// three separate templates. A user unit runs INSIDE user@<uid>.service, so
	// it can neither order itself against that unit nor name system targets:
	// network-online.target does not exist in a user manager, and a Wants= on a
	// unit the manager cannot find is a hard start failure.
	after := "network-online.target dbus.service"
	wants := "Wants=network-online.target"
	wantedBy := "multi-user.target"
	if user {
		after = "basic.target dbus.socket"
		wants = "Wants=dbus.socket"
		wantedBy = "default.target"
	}

	scopeFlag := ""
	scopeArg := "--scope system"
	if user {
		scopeFlag = "--scope user"
		scopeArg = "--scope user"
	}

	portFlag := ""
	if s.Port > 0 {
		portFlag = "--port " + strconv.Itoa(s.Port)
	}

	grant := "polkit.Result.NOT_HANDLED"
	pkla := "no"
	if s.UnitFilesGrant {
		grant = "polkit.Result.YES"
		pkla = "yes"
	}

	systemctl := s.Systemctl
	if user {
		systemctl += " --user"
	}

	return map[string]string{
		"@UNITS_VERSION@":    strconv.Itoa(TemplateVersion),
		"@AFTER@":            after,
		"@WANTS_LINE@":       wants,
		"@WANTED_BY@":        wantedBy,
		"@IDENTITY@":         s.Identity,
		"@IDENTITY_GROUP@":   s.IdentityGroup,
		"@PREFIX@":           strings.TrimRight(s.Prefix, "/"),
		"@SCOPE_FLAG@":       scopeFlag,
		"@SCOPE_ARG@":        scopeArg,
		"@PORT_FLAG@":        portFlag,
		"@UNIT_FILES_GRANT@": grant,
		"@UNIT_FILES_PKLA@":  pkla,
		"@SYSTEMCTL@":        systemctl,
	}
}

// instanceSubstitutions is the daemon table with the two ordering tokens the
// instance template rewrites differently: it drops Wants= entirely in user
// scope (there is nothing to want — llama-server binds 127.0.0.1 only and never
// needed a routable address) and orders against basic.target instead.
func (s Spec) instanceSubstitutions() map[string]string {
	subs := s.substitutions()
	if s.Scope == model.ScopeUser {
		subs["@AFTER@"] = "basic.target"
		subs["@WANTS_LINE@"] = ""
	} else {
		subs["@AFTER@"] = "network-online.target"
	}
	return subs
}

// targetSubstitutions is the table for llamaman-instances.target, which orders
// against the same target as the daemon but has no dbus dependency of its own.
func (s Spec) targetSubstitutions() map[string]string {
	subs := s.substitutions()
	if s.Scope == model.ScopeUser {
		subs["@AFTER@"] = "basic.target"
	} else {
		subs["@AFTER@"] = "network-online.target"
	}
	return subs
}

// RenderUnit renders one file by name — a unit or a polkit file.
func (s Spec) RenderUnit(name string) (string, error) {
	spec, err := s.resolve()
	if err != nil {
		return "", err
	}

	var subs map[string]string
	switch name {
	case UnitInstance:
		subs = spec.instanceSubstitutions()
	case UnitInstancesTgt:
		subs = spec.targetSubstitutions()
	case UnitDaemon, UnitSelfUpdate, UnitUpdateVerify, PolkitRules, PolkitPKLA:
		subs = spec.substitutions()
	default:
		return "", fmt.Errorf("systemd: no template for %q", name)
	}

	raw, err := packaging.Templates.ReadFile("templates/" + name + ".tmpl")
	if err != nil {
		return "", fmt.Errorf("systemd: read template %s: %w", name, err)
	}
	return substitute(string(raw), subs)
}

// Files returns every file this Spec installs, in a stable order: the units for
// this scope, then the polkit file(s) the format asks for.
//
// format is ignored in user scope, where there is no polkit rule at all.
func (s Spec) Files(format PolkitFormat) ([]File, error) {
	spec, err := s.resolve()
	if err != nil {
		return nil, err
	}

	var out []File
	dir := UnitDir(spec.Scope)
	for _, name := range UnitNames(spec.Scope) {
		content, err := spec.RenderUnit(name)
		if err != nil {
			return nil, err
		}
		out = append(out, File{Name: name, Dir: dir, Mode: 0o644, Content: content, Unit: true})
	}

	if spec.Scope == model.ScopeUser {
		return out, nil
	}
	for _, p := range polkitFiles(format) {
		content, err := spec.RenderUnit(p.Name)
		if err != nil {
			return nil, err
		}
		p.Content = content
		out = append(out, p)
	}
	return out, nil
}

func polkitFiles(format PolkitFormat) []File {
	rules := File{Name: PolkitRules, Dir: DirPolkitRules, Mode: 0o644}
	pkla := File{Name: PolkitPKLA, Dir: DirPolkitPKLA, Mode: 0o644}
	switch format {
	case PolkitFormatNone:
		return nil
	case PolkitFormatPKLA:
		return []File{pkla}
	case PolkitFormatBoth:
		return []File{rules, pkla}
	default:
		return []File{rules}
	}
}

// unresolved catches a token the substitution table forgot. It deliberately
// requires an uppercase first character so that `llamaman-instance@%i` and
// `llamaman-instance@[a-z0-9]...` in the polkit regex are not mistaken for one.
var unresolved = regexp.MustCompile(`@[A-Z][A-Z0-9_]*@`)

// substitute performs the `@TOKEN@` replacement, line by line.
//
// Two rules beyond plain replacement, both of which exist because a
// substitution can render to nothing:
//
//   - A line that becomes blank is DROPPED. That is how `Wants=` disappears
//     from the user-scope instance unit rather than being left as a bare
//     `Wants=`, which systemd rejects.
//   - A line in which some token rendered EMPTY has its interior runs of spaces
//     collapsed and its trailing space trimmed, so
//     `ExecStart=<prefix>/llamaman serve` does not acquire two dangling spaces
//     when neither the scope flag nor the port flag is rendered. Leading
//     indentation is preserved, because the polkit rules file is indented
//     JavaScript.
func substitute(raw string, subs map[string]string) (string, error) {
	// Longest token first, so no token can be a prefix of another and win by
	// map iteration order.
	keys := make([]string, 0, len(subs))
	for k := range subs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})

	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if !strings.ContainsRune(line, '@') {
			out = append(out, line)
			continue
		}

		rendered := line
		emptied := false
		for _, k := range keys {
			if !strings.Contains(rendered, k) {
				continue
			}
			if subs[k] == "" {
				emptied = true
			}
			rendered = strings.ReplaceAll(rendered, k, subs[k])
		}

		if emptied {
			rendered = collapseInterior(rendered)
			if strings.TrimSpace(rendered) == "" {
				continue
			}
		}
		out = append(out, rendered)
	}

	result := strings.Join(out, "\n")
	if m := unresolved.FindString(result); m != "" {
		return "", fmt.Errorf("systemd: unresolved template token %s", m)
	}
	return result, nil
}

// collapseInterior squeezes runs of spaces inside a line and trims the trailing
// ones, leaving the leading indentation alone.
func collapseInterior(line string) string {
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	body := strings.TrimRight(line[len(indent):], " \t")
	var b strings.Builder
	b.Grow(len(body))
	prevSpace := false
	for _, r := range body {
		if r == ' ' {
			if prevSpace {
				continue
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
		b.WriteRune(r)
	}
	return indent + b.String()
}

// stampRe matches the version stamp in either comment syntax: `#` for unit and
// .pkla files, `//` for the polkit JavaScript rules file.
var stampRe = regexp.MustCompile(`^(?:#|//) llamaman-units: (\d+)$`)

// Stamp reads the `llamaman-units: <N>` version an installed file carries. The
// stamp is required to be the file's FIRST line: reading it anywhere else would
// let a hand-edit that moved it read as a stale install (D95).
func Stamp(content string) (int, bool) {
	line, _, _ := strings.Cut(content, "\n")
	m := stampRe.FindStringSubmatch(strings.TrimRight(line, "\r"))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// Drift is what section 5.4a reports for one installed file.
type Drift string

const (
	// DriftNone: the installed file is byte-identical to what this binary
	// renders.
	DriftNone Drift = "none"
	// DriftStale: the stamp differs from this binary's TemplateVersion, or is
	// absent. This is the ORDINARY state of a host that self-updated across a
	// release which changed a template. It is reported at `info` with the
	// install-units repair line and it BLOCKS NOTHING — no F16, no refused
	// update (D95).
	DriftStale Drift = "stale"
	// DriftEdited: the stamp matches this binary's, so the file was rendered by
	// this exact template set, and the content differs anyway. Somebody edited
	// it. This is F16.
	DriftEdited Drift = "edited"
	// DriftMissing: the file is not installed at all. This is F16.
	DriftMissing Drift = "missing"
)

// Classify compares an installed file against what this binary would render.
// found=false means the file is absent.
func Classify(installed string, found bool, rendered string) Drift {
	if !found {
		return DriftMissing
	}
	if installed == rendered {
		return DriftNone
	}
	if n, ok := Stamp(installed); ok && n == TemplateVersion {
		return DriftEdited
	}
	return DriftStale
}

// Directives parses a unit file into `Key=` → values, in file order, ignoring
// comments and section headers.
//
// It exists because two properties gate every update and both are read as
// DIRECTIVES, never as a hash (D95, section 5.4a): whether llamaman.service
// carries OnFailure=llamaman-update-verify.service, and whether the self-update
// units are present and unmasked. Those are grep-shaped facts about a file's
// content, so they answer the same on a `stale` host as on a fresh one — which
// is exactly what "the drift check reports no drift" could never do.
func Directives(content string) map[string][]string {
	out := make(map[string][]string)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") ||
			strings.HasPrefix(line, "[") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		out[k] = append(out[k], strings.TrimSpace(v))
	}
	return out
}

// HasDirective reports whether content carries `key=value` exactly.
func HasDirective(content, key, value string) bool {
	for _, v := range Directives(content)[key] {
		if v == value {
			return true
		}
	}
	return false
}
