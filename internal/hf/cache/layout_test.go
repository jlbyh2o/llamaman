package cache_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/hf/cache"
)

// fixtureHub is the checked-in `huggingface_hub` tree of testdata/README.md. It
// is what makes the path assertions below mean something: the expectations come
// from a directory nobody generated with the code under test.
const fixtureHub = "testdata/hub"

func TestRepoFolderName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		repoID string
		folder string
	}{
		{"org and name", "bartowski/Qwen3-8B-GGUF", "models--bartowski--Qwen3-8B-GGUF"},
		{"no organization", "gpt2", "models--gpt2"},
		{"digits and hyphens survive", "unsloth/gemma-3-4b-it-GGUF", "models--unsloth--gemma-3-4b-it-GGUF"},
		{"dots are not escaped", "org/model.v2", "models--org--model.v2"},
		{"leading slash is ignored", "/gpt2", "models--gpt2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := cache.RepoFolderName(tc.repoID); got != tc.folder {
				t.Errorf("RepoFolderName(%q) = %q, want %q", tc.repoID, got, tc.folder)
			}
			back, ok := cache.RepoIDFromFolder(tc.folder)
			if !ok {
				t.Fatalf("RepoIDFromFolder(%q) reported not a model repo", tc.folder)
			}
			// The round trip normalizes the leading slash away, which is the
			// point of the last case: the folder name is the canonical form.
			want := cache.RepoFolderName(back)
			if want != tc.folder {
				t.Errorf("round trip: %q -> %q -> %q", tc.repoID, back, want)
			}
		})
	}
}

func TestRepoIDFromFolderRejectsNonModelDirs(t *testing.T) {
	t.Parallel()

	// A hub directory holds more than model repositories, and the walk must not
	// invent a repo id for a datasets directory another tool created.
	for _, dir := range []string{
		"datasets--squad", "spaces--org--demo", ".locks", "models", "models--", "version.txt",
	} {
		if id, ok := cache.RepoIDFromFolder(dir); ok {
			t.Errorf("RepoIDFromFolder(%q) = %q, want not-a-model-repo", dir, id)
		}
	}
}

// TestLockPathAgainstFixtureTree is D27's test, and the reason testdata/hub is
// committed: the lock path is built by exactly one function, and its expectation
// has to come from a tree that function did not write. Every path it constructs
// must name a `.lock` file that is really there.
func TestLockPathAgainstFixtureTree(t *testing.T) {
	t.Parallel()

	cases := []struct {
		repoID string
		etag   string
		want   string
	}{
		{
			repoID: "bartowski/Qwen3-8B-GGUF",
			etag:   "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
			want: "testdata/hub/.locks/models--bartowski--Qwen3-8B-GGUF/" +
				"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08.lock",
		},
		{
			// A repo id with no organization: one `--`, not two.
			repoID: "gpt2",
			etag:   "a94a8fe5ccb19ba61c4c0873d391e987982fbbd3",
			want:   "testdata/hub/.locks/models--gpt2/a94a8fe5ccb19ba61c4c0873d391e987982fbbd3.lock",
		},
		{
			repoID: "unsloth/gemma-3-4b-it-GGUF",
			etag:   "60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752",
			want: "testdata/hub/.locks/models--unsloth--gemma-3-4b-it-GGUF/" +
				"60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752.lock",
		},
	}

	for _, tc := range cases {
		t.Run(tc.repoID, func(t *testing.T) {
			t.Parallel()
			got := cache.LockPath(fixtureHub, tc.repoID, tc.etag)
			if got != tc.want {
				t.Fatalf("LockPath(%q, %q, %q)\n got %q\nwant %q",
					fixtureHub, tc.repoID, tc.etag, got, tc.want)
			}
			// The equality above is a string comparison against a literal; this
			// is the half that proves the literal describes the real layout.
			if _, err := os.Stat(got); err != nil {
				t.Fatalf("the constructed lock path does not exist in the fixture tree: %v", err)
			}
			// And the method form must agree with the package function, because
			// D27 says the path is built by exactly ONE function.
			if m := cache.NewLayout(fixtureHub).LockPath(tc.repoID, tc.etag); m != got {
				t.Errorf("Layout.LockPath = %q, LockPath = %q — two answers to one question", m, got)
			}
		})
	}
}

// TestLockPathIsNotTheIncompleteFile pins the mistake D27 exists to prevent.
// Locking the `.incomplete` file, or a `.lock` inside the repo directory,
// interlocks with nothing.
func TestLockPathIsNotTheIncompleteFile(t *testing.T) {
	t.Parallel()

	l := cache.NewLayout("/hub")
	lock := l.LockPath("org/name", "deadbeef")

	if lock == l.IncompletePath("org/name", "deadbeef") {
		t.Fatal("the lock path is the .incomplete file — huggingface_hub would not see it")
	}
	if filepath.Dir(lock) == l.RepoDir("org/name") {
		t.Fatal("the lock lives inside the repo directory — huggingface_hub locks under <hub>/.locks")
	}
	if want := "/hub/.locks/models--org--name/deadbeef.lock"; lock != want {
		t.Fatalf("lock path = %q, want %q", lock, want)
	}
}

func TestLayoutPaths(t *testing.T) {
	t.Parallel()

	l := cache.NewLayout("/srv/models")
	const repo = "org/name"

	cases := []struct{ name, got, want string }{
		{"repo dir", l.RepoDir(repo), "/srv/models/models--org--name"},
		{"blobs", l.BlobsDir(repo), "/srv/models/models--org--name/blobs"},
		{"blob", l.BlobPath(repo, "e1"), "/srv/models/models--org--name/blobs/e1"},
		{"incomplete", l.IncompletePath(repo, "e1"), "/srv/models/models--org--name/blobs/e1.incomplete"},
		{"snapshots", l.SnapshotsDir(repo), "/srv/models/models--org--name/snapshots"},
		{"snapshot", l.SnapshotDir(repo, "abc"), "/srv/models/models--org--name/snapshots/abc"},
		{"snapshot file", l.SnapshotFile(repo, "abc", "sub/m.gguf"),
			"/srv/models/models--org--name/snapshots/abc/sub/m.gguf"},
		{"refs", l.RefsDir(repo), "/srv/models/models--org--name/refs"},
		{"ref", l.RefPath(repo, "refs/pr/3"), "/srv/models/models--org--name/refs/refs/pr/3"},
		{"no_exist", l.NoExistDir(repo, "abc"), "/srv/models/models--org--name/.no_exist/abc"},
		{"locks", l.LocksDir(), "/srv/models/.locks"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestLinkTargetIsRelative pins the symlink body: relative is what keeps the
// cache movable, and it is what huggingface_hub writes. The fixture tree's own
// link is the reference.
func TestLinkTargetIsRelative(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, path, etag, want string }{
		{"snapshot root", "Qwen3-8B-Q4_K_M.gguf", "abc", "../../blobs/abc"},
		{"one directory deep", "sub/model.gguf", "abc", "../../../blobs/abc"},
		{"two directories deep", "a/b/model.gguf", "abc", "../../../../blobs/abc"},
	}
	for _, tc := range cases {
		if got := cache.LinkTarget(tc.path, tc.etag); got != tc.want {
			t.Errorf("%s: LinkTarget(%q, %q) = %q, want %q", tc.name, tc.path, tc.etag, got, tc.want)
		}
	}

	link := filepath.Join(fixtureHub, "models--bartowski--Qwen3-8B-GGUF",
		"snapshots", "1f2e3d4c5b6a7988990a1b2c3d4e5f6071829304", "Qwen3-8B-Q4_K_M.gguf")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink the fixture snapshot entry: %v", err)
	}
	want := cache.LinkTarget("Qwen3-8B-Q4_K_M.gguf",
		"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08")
	if target != want {
		t.Fatalf("the fixture link body is %q; LinkTarget builds %q", target, want)
	}
}

func TestHFHome(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		hub  string
		want string
		ok   bool
	}{
		{"ordinary layout", "/home/u/.cache/huggingface/hub", "/home/u/.cache/huggingface", true},
		{"trailing slash", "/srv/hf/hub/", "/srv/hf", true},
		// Rule 1 of section 7.2: HF_HUB_CACHE names a hub directory with no
		// /hub suffix at all, and there is then no HF_HOME to project.
		{"no suffix", "/mnt/models", "", false},
		{"root", "/", "", false},
		{"bare hub", "hub", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := cache.HFHome(tc.hub)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("HFHome(%q) = %q, %v; want %q, %v", tc.hub, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestParseShardName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		filename     string
		ok           bool
		base         string
		index, total int
	}{
		{"first shard", "Model-Q4_K_M-00001-of-00003.gguf", true, "Model-Q4_K_M", 1, 3},
		{"middle shard", "Model-Q4_K_M-00002-of-00003.gguf", true, "Model-Q4_K_M", 2, 3},
		{"unsharded", "Model-Q4_K_M.gguf", false, "", 0, 0},
		{"not a gguf", "Model-00001-of-00002.bin", false, "", 0, 0},
		{"non-standard width", "M-1-of-2.gguf", true, "M", 1, 2},
		{"index past total", "M-00004-of-00003.gguf", false, "", 0, 0},
		{"zero index", "M-00000-of-00003.gguf", false, "", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sh, ok := cache.ParseShardName(tc.filename)
			if ok != tc.ok {
				t.Fatalf("ParseShardName(%q) ok = %v, want %v", tc.filename, ok, tc.ok)
			}
			if !ok {
				return
			}
			if sh.Base != tc.base || sh.Index != tc.index || sh.Total != tc.total {
				t.Fatalf("ParseShardName(%q) = %+v, want base %q index %d total %d",
					tc.filename, sh, tc.base, tc.index, tc.total)
			}
			if name := cache.ShardName(sh.Base, sh.Index, sh.Total); tc.name == "first shard" && name != tc.filename {
				t.Fatalf("ShardName round trip = %q, want %q", name, tc.filename)
			}
		})
	}
}
