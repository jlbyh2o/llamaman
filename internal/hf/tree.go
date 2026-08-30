package hf

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/hf/cache"
)

// The repository file tree (DESIGN sections 7.1, 7.3, 3.6).
//
//	GET {endpoint}/api/models/{repo}/tree/{rev}?recursive=1&expand=1
//
// **True size is always `lfs.size` when the entry has an `lfs` object, never the
// top-level `size`.** For an LFS file the plain `size` can be the ~130-byte
// pointer, which would make a 40 GB model look free, break the fit calculator
// outright and make the disk guard of section 7.4 wave through a download that
// cannot possibly fit. SPEC section 3.2 calls this out by name, and it is the
// single most consequential line in this file.
//
// `expand=1` is what makes the `lfs` object appear at all. Without it the Hub
// returns the pointer size and nothing else, and every number downstream is
// wrong by four orders of magnitude with no symptom until the disk fills.

// TreeEntry is one file in a repository tree.
type TreeEntry struct {
	// Path is the file's path inside the repository, slash-separated. It is
	// `model_files.filename` and the `<path>` half of a resolve URL.
	Path string
	// Size is the TRUE size: `lfs.size` for an LFS object, the plain size
	// otherwise. Nothing outside this file ever sees the pointer size.
	Size int64
	// OID is the git blob sha for a regular file and the LFS object's sha256 for
	// an LFS one. For an LFS object it IS the blob name — `blobs/<etag>` — which
	// is why a download can be planned without a single HEAD request.
	OID string
	// LFS reports whether this entry is an LFS object, which is what makes OID
	// a sha256 rather than a git hash. Every GGUF of consequence is one.
	LFS bool
}

// Tree lists a repository's files at one revision.
//
// revision may be a branch, a tag or a commit sha; empty means `main`. The
// entries come back sorted by path so two calls produce the same order and a
// diff of two trees is readable.
func (c *Client) Tree(ctx context.Context, repo, revision string) ([]TreeEntry, error) {
	if err := validateRepo(repo); err != nil {
		return nil, err
	}
	rev := revision
	if rev == "" {
		rev = "main"
	}
	if err := validateRevision(rev); err != nil {
		return nil, err
	}

	raw := c.endpoint + "/api/models/" + repo + "/tree/" + url.PathEscape(rev) +
		"?recursive=1&expand=1"

	body, err := c.getJSON(ctx, "tree:"+repo+"@"+rev, raw, repo)
	if err != nil {
		return nil, err
	}

	var rows []struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Size int64  `json:"size"`
		OID  string `json:"oid"`
		LFS  *struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"lfs"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("hf: decoding the tree response: %w", err)
	}

	out := make([]TreeEntry, 0, len(rows))
	for _, r := range rows {
		if r.Type != "" && r.Type != "file" {
			continue
		}
		e := TreeEntry{Path: r.Path, Size: r.Size, OID: r.OID}
		if r.LFS != nil {
			// The one rule this file exists for.
			e.Size = r.LFS.Size
			e.OID = r.LFS.OID
			e.LFS = true
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// FileGroup is one downloadable unit: a quantization of a model, which is one
// file or a whole shard set (section 7.3).
//
// The grouping is what makes `POST /api/v1/downloads` safe to drive from a
// click. A user picks a quant; the API expands it to every shard, and "the queue
// refuses a partial shard set" is only enforceable because the set is named
// here, in one place, from two independent signals.
type FileGroup struct {
	// Key identifies the group within its repository: the shard base name for a
	// sharded set, the file path otherwise. It is what a client sends back in
	// `POST /downloads {"files":[…]}` — or rather, the client sends the file
	// names, and this key is what groups them in the UI.
	Key string
	// QuantLabel is the short name a user recognizes — `Q4_K_M`, `IQ3_XXS` —
	// read from the file name, which is all a remote tree offers. The header's
	// own answer replaces it once the file is on disk (internal/hf/cache's
	// QuantLabel), and the two agree for every file that follows the convention.
	QuantLabel string
	// Files are the group's entries in shard order.
	Files []TreeEntry
	// TotalBytes is the sum of the true sizes. It is exact from the first byte,
	// which is what section 7.3 means by "total bytes and ETA are exact".
	TotalBytes int64
	// ShardTotal is the declared set size, 1 for an unsharded file.
	ShardTotal int
	// Complete reports that every shard the names declare is actually present.
	// A repository mid-upload can advertise `-00003-of-00005` with two shards
	// missing, and queueing that is a download that can never finish.
	Complete bool
	// Mmproj marks a multimodal projector, which section 7.3 downloads as a
	// SEPARATE models row of `kind='mmproj'` linked by `mmproj_model_id`,
	// because it is separately reusable across quantizations.
	Mmproj bool
}

// GroupTree splits a tree's GGUF files into downloadable groups.
//
// Non-GGUF files are ignored outright: this product downloads GGUFs, and a
// repository's `README.md`, `.gitattributes` and tokenizer JSON are not part of
// any model llama.cpp loads. That is a deliberate divergence from a general
// mirror tool, and it is what keeps a 40 GB download from also fetching a 2 GB
// safetensors copy of the same weights.
func GroupTree(entries []TreeEntry) []FileGroup {
	type acc struct {
		files map[int]TreeEntry
		plain []TreeEntry
		total int
	}
	groups := map[string]*acc{}
	order := []string{}

	for _, e := range entries {
		name := path.Base(e.Path)
		if !cache.IsGGUF(name) {
			continue
		}
		key := e.Path
		shard, sharded := cache.ParseShardName(name)
		if sharded {
			// The key keeps the directory, so two shard sets with the same base
			// name in different subdirectories stay apart.
			key = path.Join(path.Dir(e.Path), shard.Base)
			if key == "." {
				key = shard.Base
			}
		}
		a := groups[key]
		if a == nil {
			a = &acc{files: map[int]TreeEntry{}}
			groups[key] = a
			order = append(order, key)
		}
		if sharded {
			a.files[shard.Index] = e
			if shard.Total > a.total {
				a.total = shard.Total
			}
		} else {
			a.plain = append(a.plain, e)
		}
	}

	out := make([]FileGroup, 0, len(order))
	for _, key := range order {
		a := groups[key]
		g := FileGroup{Key: key}
		switch {
		case a.total > 0:
			g.ShardTotal = a.total
			g.Complete = true
			for i := 1; i <= a.total; i++ {
				e, ok := a.files[i]
				if !ok {
					g.Complete = false
					continue
				}
				g.Files = append(g.Files, e)
				g.TotalBytes += e.Size
			}
			// A file whose name declares a set of five but which arrives with
			// six is not a set this code understands; report what is there and
			// let Complete carry the doubt.
			if len(g.Files) != a.total {
				g.Complete = false
			}
		default:
			g.ShardTotal = 1
			g.Complete = true
			g.Files = append(g.Files, a.plain...)
			for _, e := range a.plain {
				g.TotalBytes += e.Size
			}
		}
		if len(g.Files) == 0 {
			continue
		}
		first := path.Base(g.Files[0].Path)
		g.Mmproj = cache.LooksLikeMmproj(first)
		g.QuantLabel = quantFromName(first)
		out = append(out, g)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// quantFromName reads the quantization out of a file name — the only signal a
// REMOTE tree offers, since the header is what would answer authoritatively and
// reading it costs a Range request per file.
//
// It is the same convention internal/hf/cache reads as its last resort, and it
// is stated as a convention rather than a fact: a repository free to name its
// files anything may produce an empty label here, and the UI then shows the file
// name, which is never wrong.
func quantFromName(name string) string {
	base := strings.TrimSuffix(name, ".gguf")
	if shard, ok := cache.ParseShardName(name); ok {
		base = strings.TrimSuffix(shard.Base, ".gguf")
	}
	parts := strings.Split(base, "-")
	for i := len(parts) - 1; i >= 0; i-- {
		p := strings.ToUpper(parts[i])
		if looksLikeQuant(p) {
			return p
		}
	}
	return ""
}

// looksLikeQuant recognizes the llama.cpp quantization vocabulary: `Q4_K_M`,
// `IQ3_XXS`, `F16`, `BF16`, `Q8_0`, `MXFP4`.
func looksLikeQuant(s string) bool {
	switch s {
	case "F16", "F32", "BF16", "MXFP4":
		return true
	}
	if strings.HasPrefix(s, "IQ") && len(s) > 2 && s[2] >= '1' && s[2] <= '8' {
		return true
	}
	if strings.HasPrefix(s, "Q") && len(s) > 1 && s[1] >= '1' && s[1] <= '8' {
		return true
	}
	return false
}
