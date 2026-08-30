package instances

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// `config_hash` (D52, DESIGN sections 5.7 and 2.8).
//
// The hash is sha256 over three inputs: the rendered argv WITH THE LISTENER
// IDENTITY ELIDED, the resolved model paths, and the active version id. The
// elision is the load-bearing part. The internal port is an allocation detail
// the supervisor may reassign after an exit 78 (F5) with no user action, and a
// hash that moved with it would raise `restart_required` on an instance whose
// configuration nobody touched — and would break the golden argv test's
// stability claim. Everything a user can actually change is in the hash.
//
// The other two inputs are why D69 exists: neither is user-edited, both move
// under the row (a version flip, a download completing), and a stored hash that
// is not recomputed when they do makes `applied_config_hash` stop carrying
// information.

// ConfigHash hashes an already-rendered argv together with the resolved model
// paths and the active version id.
//
// The canonical form is deliberately unambiguous rather than compact: one field
// per line, each tagged, so that no combination of a path containing a space and
// a flag value containing a newline can produce the same digest as a different
// configuration.
func ConfigHash(argv []string, primary, mmproj, draft *ModelFile, versionID string) string {
	var b strings.Builder
	for _, tok := range elideListener(argv) {
		b.WriteString("argv\x00")
		b.WriteString(tok)
		b.WriteByte('\n')
	}
	for _, m := range []struct {
		tag  string
		file *ModelFile
	}{{"model", primary}, {"mmproj", mmproj}, {"draft", draft}} {
		b.WriteString(m.tag)
		b.WriteString("\x00")
		if m.file != nil {
			b.WriteString(m.file.Path)
		}
		b.WriteByte('\n')
	}
	b.WriteString("version\x00")
	b.WriteString(versionID)
	b.WriteByte('\n')

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// ConfigHashFor renders and hashes in one call — what `POST`/`PATCH` and
// RecomputeConfigHash (D69) both use, so the three writers of the column cannot
// disagree about what they are hashing.
func ConfigHashFor(
	inst model.Instance,
	flags model.FlagSet,
	primary, mmproj, draft *ModelFile,
	version Runtime,
) (string, error) {
	argv, err := RenderArgv(inst, flags, primary, mmproj, draft, version)
	if err != nil {
		return "", err
	}
	return ConfigHash(argv, primary, mmproj, draft, version.ID), nil
}

// elideListener drops `--host` with its value and the VALUE of `--port`,
// keeping `--port` itself as a marker that the instance has a listener at all.
//
// Reading D52 the other way — eliding the whole `--port` pair — would hash
// identically for a configuration that had no listener, which is not a
// configuration this design can produce but is a distinction the canonical form
// costs nothing to keep.
func elideListener(argv []string) []string {
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--host":
			i++ // and its value
		case "--port":
			out = append(out, "--port")
			i++ // its value only
		default:
			out = append(out, argv[i])
		}
	}
	return out
}
