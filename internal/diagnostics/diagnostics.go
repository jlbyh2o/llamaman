// Package diagnostics builds the redacted support bundle DESIGN section 11.3
// promises for `llamaman diagnostics --out FILE` (D50): doctor output, a
// sanitized settings dump, recent journal excerpts, unit render and drift
// status, the schema version and per-table row counts, job and instance state
// summaries, and a versions manifest.
//
// Three things this package NEVER puts in a bundle, because §11.3 states them
// as absolutes rather than aspirations:
//
//   - the database itself, in any form — no copy, no VACUUM INTO;
//   - a plaintext secret — every value Redact is handed is scrubbed from
//     every file's content, and the settings and secrets sections carry only
//     hints to begin with;
//   - anything the design calls out separately: a session id, or a path
//     outside the cache root.
//
// Every section degrades rather than failing the whole bundle when its input
// is unavailable — no database yet, no systemd, no journal access, no
// installed llama.cpp version — because a bug report from a half-broken host
// is the ordinary case this command exists for, not the exceptional one. A
// section that could not run says so in its own file instead of being
// silently absent, which is the same rule internal/cli/doctor.go states for
// its own rows.
package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/secrets"
	"github.com/jlbyh2o/llamaman/internal/settings"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// File is one member of the bundle, addressed by its slash-separated path
// inside the archive.
type File struct {
	Name    string
	Content []byte
}

// SecretsService is the subset of *secrets.Service this package needs: the
// plaintext of a stored secret, so Redact can scrub it from every other
// section, and never anything else. The plaintext this returns is used ONLY
// as a scrub target — it is never itself written to a File.
type SecretsService interface {
	Get(ctx context.Context, name model.SecretName) (string, error)
	Info(ctx context.Context, name model.SecretName) (secrets.Info, error)
}

// Options is everything Build needs. Every field beyond Now is optional: a
// fresh host with no database, no systemd, and no journal access still gets a
// bundle, with each missing section saying so.
type Options struct {
	Now time.Time

	// DoctorJSON is the already-rendered `llamaman doctor --format json`
	// report. internal/cli owns runDoctor; this package does not import
	// internal/cli (that would cycle back to here), so the caller renders the
	// report and hands us the bytes.
	DoctorJSON []byte

	// DB is a read-only store, or nil when there is no database yet — every
	// DB-backed section degrades to "skipped: no database", exactly as
	// `doctor` does for the same input.
	DB *store.Store

	// Secrets, when non-nil, is used only to build the presence/hint/validity
	// summary and to gather the plaintext scrub targets Redact needs. Its
	// plaintext values never reach a File's Content directly.
	Secrets SecretsService

	// Registry is the settings registry. Nil skips the settings section
	// entirely (it never happens outside a test that means to exercise that
	// path — cli.Diagnostics always builds one).
	Registry *settings.Registry

	// Unit rendering (DESIGN section 5.4a): Scope, Identity, IdentityGroup,
	// Prefix and Port are what a Spec needs to render the files this host's
	// binary would install, compared against what is actually on disk.
	// Identity empty means "unresolved" — the render is skipped and only the
	// installed content and stamp are reported.
	Scope          model.SystemdScope
	Identity       string
	IdentityGroup  string
	Prefix         string
	Port           int
	UnitFilesGrant bool
	Systemctl      string
	// UnitRoot prefixes every unit/polkit directory this package reads,
	// exactly like systemd.InstallOptions.Root — empty means the real
	// filesystem, and a test points it at a temp directory.
	UnitRoot string

	// JournalTail reads a bounded slice of one unit's journal. Nil (or an
	// environment with no journalctl) degrades the journal section to
	// "unavailable" rather than failing the bundle. cli.Diagnostics wires
	// this to systemd.Tail.
	JournalTail func(ctx context.Context, opts systemd.JournalOptions) ([]systemd.Entry, error)
	// JournalUnits lists which units to capture. Empty uses
	// systemd.UnitNames(Scope).
	JournalUnits []string
	// JournalLines caps how many lines are captured per unit. Zero uses
	// DefaultJournalLines.
	JournalLines int

	// BuildLogDir is `<state_dir>/logs/build`; every `*.log` file there is
	// included, tail-capped at BuildLogMaxBytes. Empty skips the section (no
	// state directory resolved).
	BuildLogDir      string
	BuildLogMaxBytes int64

	DaemonVersion string
	DaemonCommit  string
	DaemonChannel string
}

// DefaultJournalLines is how many lines of each unit's journal are captured
// when Options.JournalLines is zero — enough to catch the failure that
// prompted the bundle without turning it into the whole boot's log.
const DefaultJournalLines = 500

// DefaultBuildLogMaxBytes caps each captured build log to its tail — a failed
// CUDA build can log for pages, and the point of the bundle is the failure,
// which is at the end.
const DefaultBuildLogMaxBytes = 256 * 1024

// RedactionNote is printed on completion and stored as the bundle's own
// README (DESIGN section 11.3): what this bundle deliberately does not carry.
const RedactionNote = `This bundle is redacted per DESIGN section 11.3:
  - no copy of the database, in any form
  - no plaintext secret — the Hugging Face and GitHub tokens (when stored) are
    represented only by their masked hint, and every stored secret value is
    scrubbed from every other file before the bundle is written
  - no session id
  - no path outside the cache root
Every section that could not be produced on this host says so in its own
file rather than being silently absent.`

// Build assembles the bundle's files. It never returns a partial success as
// an error: a section it cannot produce is a file that says so, not a reason
// to fail the whole command — the one exception being a failure while
// gathering the scrub targets Redact needs, since writing a bundle it could
// not attempt to redact would defeat the point of the command.
func Build(ctx context.Context, opt Options) ([]File, error) {
	if opt.Now.IsZero() {
		opt.Now = time.Now().UTC()
	}
	if opt.JournalLines <= 0 {
		opt.JournalLines = DefaultJournalLines
	}
	if opt.BuildLogMaxBytes <= 0 {
		opt.BuildLogMaxBytes = DefaultBuildLogMaxBytes
	}

	var files []File
	add := func(name string, v any) {
		files = append(files, jsonFile(name, v))
	}

	add("manifest.json", buildManifest(opt))
	files = append(files, File{Name: "REDACTION.txt", Content: []byte(RedactionNote + "\n")})

	if len(opt.DoctorJSON) > 0 {
		files = append(files, File{Name: "doctor.json", Content: prettyJSON(opt.DoctorJSON)})
	} else {
		add("doctor.json", map[string]string{"skipped": "no doctor report was supplied"})
	}

	add("settings.json", settingsDump(ctx, opt))
	add("secrets.json", secretsDump(ctx, opt))

	unitFiles, unitDrift := unitSection(opt)
	files = append(files, unitFiles...)
	add("units/drift.json", unitDrift)

	files = append(files, journalSection(ctx, opt)...)
	files = append(files, buildLogSection(opt)...)

	add("versions.json", versionsManifest(ctx, opt))
	add("schema.json", schemaSection(ctx, opt))
	add("jobs_summary.json", jobsSummary(ctx, opt))
	add("instances_summary.json", instancesSummary(ctx, opt))

	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	scrub, err := scrubTargets(ctx, opt)
	if err != nil {
		return nil, fmt.Errorf("diagnostics: gather redaction targets: %w", err)
	}
	return Redact(files, scrub), nil
}

func jsonFile(name string, v any) File {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		b = []byte(fmt.Sprintf(`{"marshal_error":%q}`, err.Error()))
	}
	return File{Name: name, Content: append(b, '\n')}
}

// prettyJSON re-indents an already-marshaled JSON document for a human
// reading the bundle by hand; a document that does not parse is stored
// verbatim rather than dropped.
func prettyJSON(raw []byte) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return raw
	}
	return append(b, '\n')
}

func buildManifest(opt Options) map[string]any {
	return map[string]any{
		"generated_at":   opt.Now.UTC().Format(time.RFC3339),
		"daemon_version": orDev(opt.DaemonVersion),
		"daemon_commit":  orUnknownStr(opt.DaemonCommit),
		"daemon_channel": orUnknownStr(opt.DaemonChannel),
		"go_version":     runtime.Version(),
		"os":             runtime.GOOS,
		"arch":           runtime.GOARCH,
		"redaction":      "see REDACTION.txt",
	}
}

func orDev(s string) string {
	if s == "" {
		return "dev"
	}
	return s
}

func orUnknownStr(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// secretsDump reports presence, hint and validity for every secret this
// design stores — never the value (secrets.Info never carries one, and this
// function does not call Get).
func secretsDump(ctx context.Context, opt Options) []map[string]any {
	out := make([]map[string]any, 0, len(model.SecretNameValues()))
	for _, name := range model.SecretNameValues() {
		row := map[string]any{"name": string(name), "present": false}
		if opt.Secrets == nil {
			row["note"] = "no secrets service was available on this host"
			out = append(out, row)
			continue
		}
		info, err := opt.Secrets.Info(ctx, name)
		if err != nil {
			row["error"] = err.Error()
			out = append(out, row)
			continue
		}
		row["present"] = info.Present
		if info.Present {
			row["hint"] = info.Hint
			row["valid"] = info.Valid
			row["updated_at"] = info.UpdatedAt
			row["last_used_at"] = info.LastUsedAt
		}
		out = append(out, row)
	}
	return out
}

// scrubTargets gathers the plaintext of every stored secret, which is the
// input Redact scrubs from every other section. The plaintext lives only in
// this slice and in the bytes Redact compares against — it is never a File's
// Content.
func scrubTargets(ctx context.Context, opt Options) ([]string, error) {
	if opt.Secrets == nil {
		return nil, nil
	}
	var out []string
	for _, name := range model.SecretNameValues() {
		v, err := opt.Secrets.Get(ctx, name)
		switch {
		case err == nil && strings.TrimSpace(v) != "":
			out = append(out, v)
		case err == nil, errors.Is(err, secrets.ErrNotStored):
			// Nothing stored under this name — not a failure.
		default:
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	return out, nil
}

// settingsDump renders the settings registry against this database's
// overrides. Every value here is a setting the registry itself defines
// (DESIGN section 2.1) — no secret has ever lived in this table (D46 keeps
// those in `secrets`), so nothing here needs masking on its own account, but
// Build's global scrub still runs over it as defense against a future key
// that stores something it should not.
func settingsDump(ctx context.Context, opt Options) []map[string]any {
	if opt.Registry == nil {
		return nil
	}
	overrides := map[string]model.Setting{}
	if opt.DB != nil {
		_ = opt.DB.Read(ctx, func(ctx context.Context, tx store.Tx) error {
			rows, err := opt.DB.Settings(ctx, tx)
			if err != nil {
				return err
			}
			for _, r := range rows {
				overrides[r.Key] = r
			}
			return nil
		})
	}

	defs := opt.Registry.Definitions()
	out := make([]map[string]any, 0, len(defs))
	for _, d := range defs {
		row := map[string]any{
			"key": d.Key, "kind": string(d.Kind), "group": d.Group,
			"restart_required": d.RestartRequired, "default": d.Default,
			"overridden": false, "value": d.Default,
		}
		if o, ok := overrides[d.Key]; ok {
			row["overridden"] = true
			row["updated_at"] = o.UpdatedAt
			row["updated_by"] = string(o.UpdatedBy)
			var v any
			if err := json.Unmarshal([]byte(o.Value), &v); err == nil {
				row["value"] = v
			} else {
				row["value"] = o.Value
			}
		}
		out = append(out, row)
	}
	return out
}

// schemaSection is D50's "the schema plus row counts": the migration version
// this database is at, and how many rows every table holds — never the rows
// themselves.
func schemaSection(ctx context.Context, opt Options) map[string]any {
	if opt.DB == nil {
		return map[string]any{"skipped": "no database"}
	}
	var version int
	counts := map[string]int64{}
	err := opt.DB.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		v, err := opt.DB.SchemaVersion(ctx, tx)
		if err != nil {
			return err
		}
		version = v

		rows, err := tx.QueryContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
		if err != nil {
			return err
		}
		var tables []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			tables = append(tables, name)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		for _, name := range tables {
			var n int64
			// name came from sqlite_master, never from a caller, so quoting it
			// as an identifier is safe.
			q := fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, strings.ReplaceAll(name, `"`, `""`))
			if err := tx.QueryRowContext(ctx, q).Scan(&n); err != nil {
				return err
			}
			counts[name] = n
		}
		return nil
	})
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return map[string]any{"schema_version": version, "row_counts": counts}
}

// jobsSummary counts live and historical jobs by kind and state — never the
// job rows themselves, which can carry a model or repo id but never a secret;
// the summary is deliberately coarser than that anyway.
func jobsSummary(ctx context.Context, opt Options) map[string]any {
	if opt.DB == nil {
		return map[string]any{"skipped": "no database"}
	}
	type row struct {
		Kind  string
		State string
		N     int64
	}
	var rows []row
	err := opt.DB.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		r, err := tx.QueryContext(ctx, `SELECT kind, state, COUNT(*) FROM jobs GROUP BY kind, state ORDER BY kind, state`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var v row
			if err := r.Scan(&v.Kind, &v.State, &v.N); err != nil {
				return err
			}
			rows = append(rows, v)
		}
		return r.Err()
	})
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	by := map[string]map[string]int64{}
	var total int64
	for _, r := range rows {
		if by[r.Kind] == nil {
			by[r.Kind] = map[string]int64{}
		}
		by[r.Kind][r.State] = r.N
		total += r.N
	}
	return map[string]any{"total": total, "by_kind_state": by}
}

// instancesSummary counts live instances by desired and observed state.
// Instance NAMES are included — they are operator-chosen labels, not
// secrets — but nothing about a model path, a token or a session is.
func instancesSummary(ctx context.Context, opt Options) map[string]any {
	if opt.DB == nil {
		return map[string]any{"skipped": "no database"}
	}
	type row struct {
		ID, Name, Desired string
		State             *string
	}
	var rows []row
	err := opt.DB.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		r, err := tx.QueryContext(ctx, `
			SELECT i.id, i.name, i.desired_state, s.state
			FROM instances i
			LEFT JOIN instance_status s ON s.instance_id = i.id
			WHERE i.deleted_at IS NULL
			ORDER BY i.name`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var v row
			if err := r.Scan(&v.ID, &v.Name, &v.Desired, &v.State); err != nil {
				return err
			}
			rows = append(rows, v)
		}
		return r.Err()
	})
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	byDesired := map[string]int64{}
	byState := map[string]int64{}
	list := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		byDesired[r.Desired]++
		state := "unknown"
		if r.State != nil {
			state = *r.State
		}
		byState[state]++
		list = append(list, map[string]any{
			"id": r.ID, "name": r.Name, "desired_state": r.Desired, "state": state,
		})
	}
	return map[string]any{
		"total": len(rows), "by_desired_state": byDesired, "by_state": byState, "instances": list,
	}
}

// versionsManifest is D50's "versions manifest": what this daemon is, what it
// is running on, and which llama.cpp versions are installed. No secret has
// ever lived in `llamacpp_versions`.
func versionsManifest(ctx context.Context, opt Options) map[string]any {
	out := map[string]any{
		"daemon_version": orDev(opt.DaemonVersion),
		"daemon_commit":  orUnknownStr(opt.DaemonCommit),
		"daemon_channel": orUnknownStr(opt.DaemonChannel),
		"go_version":     runtime.Version(),
		"os":             runtime.GOOS,
		"arch":           runtime.GOARCH,
	}
	if opt.DB == nil {
		out["llamacpp"] = "skipped: no database"
		return out
	}

	var versions []map[string]any
	err := opt.DB.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		vs, err := opt.DB.LlamacppVersions(ctx, tx, store.LlamacppVersionFilter{IncludeDeleted: true})
		if err != nil {
			return err
		}
		for _, v := range vs {
			versions = append(versions, map[string]any{
				"id": v.ID, "channel": string(v.Channel), "tag": v.Tag,
				"acquisition": string(v.Acquisition), "backend": string(v.Backend),
				"state": string(v.State), "is_active": v.IsActive,
				"previous_active": v.PreviousActive, "size_bytes": v.SizeBytes,
				"created_at": v.CreatedAt,
			})
		}
		return nil
	})
	if err != nil {
		out["llamacpp_error"] = err.Error()
		return out
	}
	out["llamacpp"] = versions
	return out
}

// buildLogSection includes every `*.log` under BuildLogDir, tail-capped, so a
// failed build's own output rides along with the doctor report that names it.
func buildLogSection(opt Options) []File {
	if opt.BuildLogDir == "" {
		return []File{{Name: "build-logs/README.txt", Content: []byte("no build log directory was resolved on this host\n")}}
	}
	entries, err := os.ReadDir(opt.BuildLogDir)
	if err != nil {
		return []File{{Name: "build-logs/README.txt", Content: []byte("could not list " + opt.BuildLogDir + ": " + err.Error() + "\n")}}
	}
	var out []File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		content, err := tailFile(filepath.Join(opt.BuildLogDir, e.Name()), opt.BuildLogMaxBytes)
		if err != nil {
			content = []byte("could not read this log: " + err.Error() + "\n")
		}
		out = append(out, File{Name: "build-logs/" + e.Name(), Content: content})
	}
	if len(out) == 0 {
		out = append(out, File{Name: "build-logs/README.txt", Content: []byte("no build logs on this host\n")})
	}
	return out
}

func tailFile(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size <= max {
		return os.ReadFile(path)
	}
	if _, err := f.Seek(size-max, 0); err != nil {
		return nil, err
	}
	b := make([]byte, max)
	n, err := f.Read(b)
	if err != nil && n == 0 {
		return nil, err
	}
	return append([]byte(fmt.Sprintf("… truncated to the last %d bytes of %s …\n", max, filepath.Base(path))), b[:n]...), nil
}
