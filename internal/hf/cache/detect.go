package cache

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The hub-directory detection chain (DESIGN section 7.2).
//
// It runs ONCE, at first boot, and the answer is then persisted in
// `hf_cache_roots` and edited in the UI. Environment variables are honored for
// DETECTION only and are never required — SPEC section 3.9 forbids requiring
// one, and this is what makes honoring them consistent with that: reading a
// variable to find a library someone else created is not configuration, it is
// discovery.
//
// The chain resolves a HUB DIRECTORY, not an `HF_HOME`, because
// `huggingface_hub` lets the two come apart — and the case where they do is
// exactly the case this product exists for, a multi-terabyte model disk.

// The environment variables the chain reads, in the order it reads them.
const (
	EnvHFHubCache            = "HF_HUB_CACHE"
	EnvHuggingfaceHubCache   = "HUGGINGFACE_HUB_CACHE"
	EnvTransformersCache     = "TRANSFORMERS_CACHE"
	EnvHFHome                = "HF_HOME"
	EnvXDGCacheHome          = "XDG_CACHE_HOME"
	EnvHome                  = "HOME"
	dedicatedUserCacheSubdir = "hf-cache"
)

// Env is what the chain needs to know about this host. Every field is data
// rather than a call into the process environment, so the six rules can be
// exercised as a table (DESIGN section 15) without mutating a global.
type Env struct {
	// Getenv reads an environment variable. Nil uses os.Getenv.
	Getenv func(string) string
	// StateDir is the RESOLVED state directory (D72), which rule 4 compares
	// against `$HOME` to recognize the `--dedicated-user` topology. Empty
	// disables rule 4 rather than making it match an empty `$HOME`.
	StateDir string
	// IsDir reports whether a path is an existing directory. Nil uses a stat.
	IsDir func(string) bool
	// HasRepos reports whether a directory contains at least one `models--*`
	// entry — the test rule 7.2 applies before registering a NON-primary root,
	// so an empty directory named by a stale variable does not become a root
	// nobody asked for. Nil uses a directory read.
	IsDirWithRepos func(string) bool
}

// Candidate is one hub directory the chain named, and which rule named it.
type Candidate struct {
	// Path is the hub directory itself, cleaned. It need NOT end in `/hub`:
	// rule 1 produces one that does not, and section 7.2 forbids assuming the
	// suffix anywhere.
	Path string
	// From is `hf_cache_roots.detected_from`.
	From model.CacheRootDetectedFrom
	// Exists records whether the directory was there when the chain ran. The
	// winner may not exist — on a fresh host none of them do, and the first
	// entry wins anyway — but a non-primary root always does.
	Exists bool
}

// Detection is the chain's whole answer.
type Detection struct {
	// Primary is the hub directory that becomes the `is_primary=1` row: the
	// first candidate that names an existing directory, or, when none exists,
	// the first candidate at all.
	Primary Candidate
	// Others are the candidates that named an existing directory containing at
	// least one `models--*` and did not win. They are registered as
	// scan-and-serve roots, so a user who once used `HF_HOME` and later moved
	// to `HF_HUB_CACHE` sees BOTH libraries on the first boot rather than half
	// of one.
	Others []Candidate
	// Candidates is every rule's answer, in chain order, including the ones
	// that named nothing on disk. It is what the wizard's cache step shows when
	// it asks the user to confirm.
	Candidates []Candidate
}

// Detect runs the six-rule chain, top to bottom, exactly once.
func Detect(env Env) Detection {
	getenv := env.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	isDir := env.IsDir
	if isDir == nil {
		isDir = func(p string) bool {
			st, err := os.Stat(p)
			return err == nil && st.IsDir()
		}
	}
	hasRepos := env.IsDirWithRepos
	if hasRepos == nil {
		hasRepos = HasRepos
	}

	// The chain, in section 7.2's order. A rule that names nothing contributes
	// nothing; a rule whose value is relative is skipped, because a hub
	// directory that depends on a working directory is a root this daemon could
	// resolve differently on the next boot.
	var raw []Candidate
	add := func(path string, from model.CacheRootDetectedFrom) {
		if path == "" {
			return
		}
		clean := filepath.Clean(path)
		if !filepath.IsAbs(clean) {
			return
		}
		raw = append(raw, Candidate{Path: clean, From: from})
	}

	// 1. $HF_HUB_CACHE — the value VERBATIM. It overrides <HF_HOME>/hub
	//    outright, with no `/hub` appended, which is the rule that makes the
	//    "nothing may assume the suffix" warning load-bearing rather than
	//    defensive.
	add(getenv(EnvHFHubCache), model.DetectedFromHFHubCache)

	// 2. The legacy variables, same rule, in the order huggingface_hub itself
	//    deprecated them.
	add(getenv(EnvHuggingfaceHubCache), model.DetectedFromLegacyEnv)
	add(getenv(EnvTransformersCache), model.DetectedFromLegacyEnv)

	// 3. $HF_HOME — and HERE the suffix is appended, because this variable
	//    names the home, not the hub.
	if v := getenv(EnvHFHome); v != "" {
		add(filepath.Join(v, hubDirNameSuffix), model.DetectedFromHFHome)
	}

	// 4. The `--dedicated-user` topology: the service identity's home IS the
	//    state directory. Detected from a fact the daemon can observe rather
	//    than from a variable SPEC section 3.9 forbids requiring. Without this
	//    rule, rules 5-6 would resolve to <state_dir>/.cache/huggingface/hub —
	//    a different directory from the /var/lib/llamaman/hf-cache that section
	//    5.2 advertises, and one that buries GGUFs in a path section 6.1 says
	//    never holds them.
	home := filepath.Clean(getenv(EnvHome))
	if env.StateDir != "" && home != "." && home == filepath.Clean(env.StateDir) {
		add(filepath.Join(env.StateDir, dedicatedUserCacheSubdir, hubDirNameSuffix),
			model.DetectedFromDedicated)
	}

	// 5. $XDG_CACHE_HOME.
	if v := getenv(EnvXDGCacheHome); v != "" {
		add(filepath.Join(v, huggingfaceSubdir, hubDirNameSuffix), model.DetectedFromXDGCacheHome)
	}

	// 6. The default.
	if h := getenv(EnvHome); h != "" {
		add(filepath.Join(h, ".cache", huggingfaceSubdir, hubDirNameSuffix), model.DetectedFromDefault)
	}

	// De-duplicate by path, keeping the FIRST rule that named it: two variables
	// pointing at one directory is one root, and the rule that found it first
	// is the one whose name explains what the UI is looking at.
	seen := make(map[string]struct{}, len(raw))
	candidates := make([]Candidate, 0, len(raw))
	for _, c := range raw {
		if _, dup := seen[c.Path]; dup {
			continue
		}
		seen[c.Path] = struct{}{}
		c.Exists = isDir(c.Path)
		candidates = append(candidates, c)
	}

	out := Detection{Candidates: candidates}
	if len(candidates) == 0 {
		return out
	}

	// "The first entry that names an existing directory, or (when none exists)
	// the first entry at all, wins."
	winner := 0
	for i, c := range candidates {
		if c.Exists {
			winner = i
			break
		}
	}
	out.Primary = candidates[winner]

	for i, c := range candidates {
		if i == winner || !c.Exists {
			continue
		}
		if hasRepos(c.Path) {
			out.Others = append(out.Others, c)
		}
	}
	return out
}

// HasRepos reports whether dir contains at least one `models--*` entry. It is
// the test section 7.2 applies before registering a non-primary root: a
// directory that exists but holds no repository is a variable someone exported
// once, not a library.
func HasRepos(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), RepoFolderPrefix+RepoIDSeparator) {
			return true
		}
	}
	return false
}
