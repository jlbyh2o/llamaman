package cache_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// The six-rule chain of DESIGN section 7.2, as a table over environments
// (section 15). The environment is DATA here rather than the process's own, so
// a case can name a variable another case must not see.

// mkhub creates a directory and, when repos is true, one `models--*` inside it —
// the test rule 7.2 applies before registering a NON-primary root.
func mkhub(t *testing.T, path string, repos bool) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if repos {
		if err := os.MkdirAll(filepath.Join(path, "models--org--name", "snapshots"), 0o755); err != nil {
			t.Fatalf("mkdir repo: %v", err)
		}
	}
	return path
}

func TestDetectChain(t *testing.T) {
	root := t.TempDir()

	var (
		hubCache  = filepath.Join(root, "explicit-hub")
		legacy    = filepath.Join(root, "legacy-hub")
		hfHome    = filepath.Join(root, "hfhome")
		xdg       = filepath.Join(root, "xdg")
		home      = filepath.Join(root, "home")
		stateDir  = filepath.Join(root, "state")
		dedicated = filepath.Join(stateDir, "hf-cache", "hub")
	)

	cases := []struct {
		name string
		env  map[string]string
		// exists are directories to create before the chain runs; repos names
		// the subset that also gets a `models--*` entry.
		exists   []string
		repos    []string
		stateDir string

		wantPath   string
		wantFrom   model.CacheRootDetectedFrom
		wantOthers []string
	}{
		{
			// Rule 1. The value is taken VERBATIM: no `/hub` is appended, which
			// is the whole reason nothing in this package may assume the suffix.
			name:     "HF_HUB_CACHE wins and is verbatim",
			env:      map[string]string{"HF_HUB_CACHE": hubCache, "HOME": home},
			exists:   []string{hubCache},
			stateDir: stateDir,
			wantPath: hubCache,
			wantFrom: model.DetectedFromHFHubCache,
		},
		{
			// Rule 2, and the reason `Others` exists: a user who once used
			// HF_HOME and later moved to HF_HUB_CACHE must see BOTH libraries.
			name: "the legacy variables are honored and the loser is kept",
			env: map[string]string{
				"HUGGINGFACE_HUB_CACHE": legacy,
				"HF_HOME":               hfHome,
				"HOME":                  home,
			},
			exists:     []string{legacy, filepath.Join(hfHome, "hub")},
			repos:      []string{filepath.Join(hfHome, "hub")},
			stateDir:   stateDir,
			wantPath:   legacy,
			wantFrom:   model.DetectedFromLegacyEnv,
			wantOthers: []string{filepath.Join(hfHome, "hub")},
		},
		{
			// Rule 3 appends the suffix, because HF_HOME names the home rather
			// than the hub.
			name:     "HF_HOME gains the /hub suffix",
			env:      map[string]string{"HF_HOME": hfHome, "HOME": home},
			exists:   []string{filepath.Join(hfHome, "hub")},
			stateDir: stateDir,
			wantPath: filepath.Join(hfHome, "hub"),
			wantFrom: model.DetectedFromHFHome,
		},
		{
			// Rule 4: the --dedicated-user topology, detected from `$HOME ==
			// state_dir` rather than from a variable SPEC section 3.9 forbids
			// requiring. Without it rules 5-6 would bury GGUFs under
			// <state_dir>/.cache/huggingface, which section 6.1 says never
			// holds one.
			name:     "$HOME == state_dir resolves to <state_dir>/hf-cache/hub",
			env:      map[string]string{"HOME": stateDir},
			exists:   []string{dedicated},
			stateDir: stateDir,
			wantPath: dedicated,
			wantFrom: model.DetectedFromDedicated,
		},
		{
			name:     "XDG_CACHE_HOME",
			env:      map[string]string{"XDG_CACHE_HOME": xdg, "HOME": home},
			exists:   []string{filepath.Join(xdg, "huggingface", "hub")},
			stateDir: stateDir,
			wantPath: filepath.Join(xdg, "huggingface", "hub"),
			wantFrom: model.DetectedFromXDGCacheHome,
		},
		{
			name:     "the default, with nothing exported",
			env:      map[string]string{"HOME": home},
			exists:   []string{filepath.Join(home, ".cache", "huggingface", "hub")},
			stateDir: stateDir,
			wantPath: filepath.Join(home, ".cache", "huggingface", "hub"),
			wantFrom: model.DetectedFromDefault,
		},
		{
			// "or (when none exists) the first entry at all": on a fresh host
			// nothing is on disk and the chain still has to name a winner.
			name:     "nothing exists — the first candidate still wins",
			env:      map[string]string{"HF_HUB_CACHE": hubCache, "HOME": home},
			stateDir: stateDir,
			wantPath: hubCache,
			wantFrom: model.DetectedFromHFHubCache,
		},
		{
			// A directory that exists but holds no repository is a variable
			// somebody exported once, not a library — it must not become a root.
			name: "an empty non-winner is not registered",
			env: map[string]string{
				"HF_HUB_CACHE": hubCache,
				"HF_HOME":      hfHome,
				"HOME":         home,
			},
			exists:   []string{hubCache, filepath.Join(hfHome, "hub")},
			stateDir: stateDir,
			wantPath: hubCache,
			wantFrom: model.DetectedFromHFHubCache,
		},
		{
			// A relative value cannot be a root: this daemon would resolve it
			// differently on the next boot.
			name:     "a relative value is skipped",
			env:      map[string]string{"HF_HUB_CACHE": "relative/hub", "HOME": home},
			exists:   []string{filepath.Join(home, ".cache", "huggingface", "hub")},
			stateDir: stateDir,
			wantPath: filepath.Join(home, ".cache", "huggingface", "hub"),
			wantFrom: model.DetectedFromDefault,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repos := map[string]bool{}
			for _, p := range tc.repos {
				repos[p] = true
			}
			for _, p := range tc.exists {
				mkhub(t, p, repos[p])
			}
			t.Cleanup(func() {
				for _, p := range tc.exists {
					os.RemoveAll(p)
				}
			})

			got := cache.Detect(cache.Env{
				Getenv:   func(k string) string { return tc.env[k] },
				StateDir: tc.stateDir,
			})

			if got.Primary.Path != tc.wantPath {
				t.Fatalf("primary = %q, want %q", got.Primary.Path, tc.wantPath)
			}
			if got.Primary.From != tc.wantFrom {
				t.Fatalf("detected_from = %q, want %q", got.Primary.From, tc.wantFrom)
			}

			var others []string
			for _, o := range got.Others {
				others = append(others, o.Path)
			}
			if len(others) != len(tc.wantOthers) {
				t.Fatalf("non-primary roots = %v, want %v", others, tc.wantOthers)
			}
			for i := range others {
				if others[i] != tc.wantOthers[i] {
					t.Fatalf("non-primary roots = %v, want %v", others, tc.wantOthers)
				}
			}
		})
	}
}

// TestDetectDeduplicatesByPath: two variables pointing at one directory is one
// root, and the rule that found it first is the one whose name explains what the
// UI is looking at.
func TestDetectDeduplicatesByPath(t *testing.T) {
	dir := mkhub(t, filepath.Join(t.TempDir(), "shared"), true)

	got := cache.Detect(cache.Env{
		Getenv: func(k string) string {
			switch k {
			case "HF_HUB_CACHE", "HUGGINGFACE_HUB_CACHE", "TRANSFORMERS_CACHE":
				return dir
			}
			return ""
		},
	})
	if len(got.Candidates) != 1 {
		t.Fatalf("candidates = %d, want 1 — the same directory named three times is one root", len(got.Candidates))
	}
	if got.Primary.From != model.DetectedFromHFHubCache {
		t.Fatalf("detected_from = %q, want the FIRST rule that named it", got.Primary.From)
	}
	if len(got.Others) != 0 {
		t.Fatalf("non-primary roots = %d, want 0", len(got.Others))
	}
}

// TestDetectWithNoEnvironmentAtAll: no HOME, nothing exported. The chain names
// nothing and says so, rather than guessing a path.
func TestDetectWithNoEnvironmentAtAll(t *testing.T) {
	got := cache.Detect(cache.Env{Getenv: func(string) string { return "" }})
	if got.Primary.Path != "" || len(got.Candidates) != 0 {
		t.Fatalf("Detect with an empty environment = %+v, want nothing", got)
	}
}

func TestHasRepos(t *testing.T) {
	dir := t.TempDir()
	if cache.HasRepos(dir) {
		t.Fatal("an empty directory reports repositories")
	}
	if err := os.MkdirAll(filepath.Join(dir, "datasets--squad"), 0o755); err != nil {
		t.Fatal(err)
	}
	if cache.HasRepos(dir) {
		t.Fatal("a datasets directory counted as a model repository")
	}
	if err := os.MkdirAll(filepath.Join(dir, "models--org--name"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !cache.HasRepos(dir) {
		t.Fatal("a models-- directory was not recognized")
	}
}
