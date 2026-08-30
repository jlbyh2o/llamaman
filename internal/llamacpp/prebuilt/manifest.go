package prebuilt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// `versions/<id>/manifest.json` for a PREBUILT install (DESIGN section 6.1's
// layout, section 6.4 step 4).
//
// The database is the source of truth for everything the API serves — section
// 2.5 has columns for the state, the binaries, the sizes and the parsed help
// flags. What the manifest adds is that the version DIRECTORY describes itself:
// after a restore from backup, or on a state directory carried to another
// machine, it is the only record of where these binaries came from, which
// release asset they are, and what its published checksum was.
//
// Its keys are deliberately the SAME keys a source build's manifest uses for
// the same concepts — `manifest_version`, `version_id`, `tag`, `acquisition`,
// `backend`, `built_at`, `binaries`, `size_bytes`, `server_help`, `help_flags`,
// `supports_fit`, `devices_output`, `version_output` — so one reader decodes
// both and `GET /api/v1/llamacpp/active` does not need a branch. The fields
// below that a source build has no equivalent for describe the download: the
// asset, its URL, its digest, and the glibc the host had when it was accepted.

// ManifestVersion is the format version of the file this package writes, so a
// reader can tell a v1 manifest from whatever replaces it without guessing.
const ManifestVersion = 1

// ManifestName is the file name inside a version directory.
const ManifestName = "manifest.json"

// Manifest is `versions/<id>/manifest.json` for a prebuilt install.
type Manifest struct {
	ManifestVersion int    `json:"manifest_version"`
	VersionID       string `json:"version_id"`
	Tag             string `json:"tag,omitempty"`
	// BuildTag is the `b#####` a stable release pinned through nightly-tag.txt
	// (section 6.2), empty on the nightly channel where the tag IS the build.
	BuildTag    string `json:"build_tag,omitempty"`
	Channel     string `json:"channel,omitempty"`
	Acquisition string `json:"acquisition"`
	Backend     string `json:"backend"`

	// The download this tree came from.
	AssetName string `json:"asset_name,omitempty"`
	AssetURL  string `json:"asset_url,omitempty"`
	// AssetSHA256 is the digest of the bytes that were actually extracted —
	// computed inline during the download, not read back afterwards.
	AssetSHA256 string `json:"asset_sha256,omitempty"`
	// PublishedSHA256 is the digest GitHub published, when it published one.
	// Recording both makes "the mirror served something else" answerable months
	// later rather than only at the moment it happens.
	PublishedSHA256 string `json:"published_sha256,omitempty"`
	AssetReleaseTag string `json:"asset_release_tag,omitempty"`

	BuiltAt time.Time `json:"built_at"`
	BuiltBy string    `json:"built_by,omitempty"` // the llamaman version that installed it
	// HostGlibc is what this host had when the binary passed D18's acceptance
	// test. A state directory moved to an older distribution has a manifest
	// that says why the binaries stopped running.
	HostGlibc string `json:"host_glibc,omitempty"`
	// TopLevel is the archive directory that was stripped at extraction.
	TopLevel string `json:"top_level,omitempty"`

	Binaries  []string `json:"binaries"`
	SizeBytes int64    `json:"size_bytes"`

	// ServerHelp is the capture section 2.5 calls for verbatim; HelpFlags is
	// the projection that becomes the column. SupportsFit is derived from it
	// and is what RenderArgv reads (through the column, never this file).
	ServerHelp    string   `json:"server_help,omitempty"`
	HelpFlags     []string `json:"help_flags,omitempty"`
	SupportsFit   bool     `json:"supports_fit"`
	DevicesOutput string   `json:"devices_output,omitempty"`
	VersionOutput string   `json:"version_output,omitempty"`
}

// ManifestPath is the manifest inside a version or staging directory.
func ManifestPath(dir string) string { return filepath.Join(dir, ManifestName) }

// WriteManifest writes the manifest into a version or staging directory.
//
// It is written to the STAGING tree, before the rename that publishes it, so a
// version directory never exists in a state where the binaries are present and
// the manifest is not.
func WriteManifest(dir string, m Manifest) error {
	if m.ManifestVersion == 0 {
		m.ManifestVersion = ManifestVersion
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("prebuilt: encoding the manifest: %w", err)
	}
	b = append(b, '\n')
	path := ManifestPath(dir)
	if err := os.WriteFile(path, b, 0o640); err != nil {
		return fmt.Errorf("prebuilt: writing %s: %w", path, err)
	}
	return nil
}

// ReadManifest reads a version directory's manifest.
func ReadManifest(dir string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(ManifestPath(dir))
	if err != nil {
		return m, fmt.Errorf("prebuilt: reading the manifest: %w", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("prebuilt: parsing the manifest: %w", err)
	}
	return m, nil
}

// DirSize sums the regular files under a directory, which is section 2.5's
// `size_bytes`. Symlinks are counted once, at their target's size, only when
// the target is inside the tree — the `libggml.so -> libggml.so.0` pairs a
// llama.cpp tree is full of would otherwise double the number.
func DirSize(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("prebuilt: measuring %s: %w", dir, err)
	}
	return total, nil
}
