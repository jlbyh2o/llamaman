package github

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The GitHub Releases shapes this client reads, reduced to the fields DESIGN
// section 2.5's `release_cache` and section 6.2's channel resolution actually
// use. Everything else in the API response is deliberately dropped rather than
// carried: a release payload is ~40 fields per asset and the only ones that
// change behavior here are the name, the size, the URL and the digest.

// Release is one GitHub release.
type Release struct {
	Tag         string    `json:"tag_name"`
	Name        string    `json:"name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	// Body is the changelog markdown. It is rendered to HTML server-side by
	// internal/mdrender (D35) before it reaches a browser; this client never
	// renders and never sanitizes.
	Body   string  `json:"body"`
	Assets []Asset `json:"assets"`
}

// Asset is one file attached to a release.
type Asset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// DownloadURL is `browser_download_url`: a github.com/objects CDN URL, NOT
	// api.github.com. Nothing may send the GitHub token to it (section 6.2).
	DownloadURL string `json:"browser_download_url"`
	ContentType string `json:"content_type"`
	// Digest is GitHub's own `digest` field, `sha256:<hex>` when present and
	// empty when the release predates it. Section 6.4 step 1 compares the
	// hash computed inline during the download against this when it is there.
	Digest    string    `json:"digest"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SHA256 returns the hex digest GitHub published for this asset, and false when
// it published none. An absent digest is not an error — plenty of older
// releases have none — but it is the difference between a verified download and
// a merely completed one, so the caller is made to ask.
func (a Asset) SHA256() (string, bool) {
	const prefix = "sha256:"
	if !strings.HasPrefix(a.Digest, prefix) {
		return "", false
	}
	hex := strings.TrimSpace(a.Digest[len(prefix):])
	if len(hex) != 64 {
		return "", false
	}
	return strings.ToLower(hex), true
}

// Asset finds an asset by exact name.
func (r Release) Asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// nightlyTagRE is section 6.2's nightly filter: `tag ~ ^b\d+$`. A release whose
// tag is anything else — a semver stable tag, a one-off tag someone pushed by
// hand — is not a nightly however it is flagged.
var nightlyTagRE = regexp.MustCompile(`^b(\d+)$`)

// BuildNumber returns the numeric build id of a `b#####` tag.
func BuildNumber(tag string) (int64, bool) {
	m := nightlyTagRE.FindStringSubmatch(tag)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// IsNightlyTag reports whether a tag is a llama.cpp nightly build tag.
func IsNightlyTag(tag string) bool { return nightlyTagRE.MatchString(tag) }

// Nightlies filters and orders a release listing the way section 6.2 specifies:
// keep `prerelease && tag ~ ^b\d+$`, sort by NUMERIC build id descending.
//
// The numeric sort is the whole point. Sorting `b9999` and `b10000` as strings
// puts the older one first, and the UI's "latest nightly" would then be a
// build from three weeks ago — a bug that appears exactly once, at the b10000
// boundary, and is invisible in every test written before it.
//
// Drafts are dropped: a draft release's assets are not downloadable by anyone
// but its author, so offering one is offering a 404.
func Nightlies(in []Release) []Release {
	out := make([]Release, 0, len(in))
	for _, r := range in {
		if r.Draft || !r.Prerelease || !IsNightlyTag(r.Tag) {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := BuildNumber(out[i].Tag)
		b, _ := BuildNumber(out[j].Tag)
		return a > b
	})
	return out
}

// decodeReleases parses a releases listing response body.
func decodeReleases(b []byte) ([]Release, error) {
	var out []Release
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// decodeRelease parses a single-release response body.
func decodeRelease(b []byte) (Release, error) {
	var out Release
	if err := json.Unmarshal(b, &out); err != nil {
		return Release{}, err
	}
	return out, nil
}
