package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/llamacpp/github"
	"github.com/jlbyh2o/llamaman/internal/mdrender"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The release feed (DESIGN section 12.1 step 1 and section 3.14).
//
// `POST /update/apply` "refreshes `release_cache` for source `llamaman`; the UI
// shows the rendered changelog and the version delta". The client is the same
// one section 6.2 uses for llama.cpp, pointed at this project's own repository:
// one request policy, one conditional-request implementation, one rate-limit
// accounting, one cross-host header strip — because a second GitHub client would
// be a second place for the token rule to be got wrong.
//
// The changelog is rendered to HTML **server-side, once, on the way into the
// cache** (D35). A release body is markdown written upstream and rendered into
// the origin that holds the admin session cookie; internal/mdrender is the only
// place in this binary that turns markdown into HTML, and a caller cannot get
// half of it.

// ReleaseSource is the release feed this package reads. *github.Client satisfies
// it; the interface is declared here because the consumer owns it (DESIGN
// section 1) and because a test needs a feed with no network behind it.
type ReleaseSource interface {
	ListReleases(ctx context.Context, perPage int) ([]github.Release, github.Meta, error)
	ReleaseByTag(ctx context.Context, tag string) (github.Release, github.Meta, error)
}

// ReleaseSourceName is `release_cache.source` for this project's own releases.
// The column's CHECK admits exactly two values, and the other one is llama.cpp's.
const ReleaseSourceName = "llamaman"

// Repo is this project's GitHub repository, the one the release feed and
// `install.sh` both name.
const Repo = "jlbyh2o/llamaman"

// Release is one row of `GET /api/v1/update/releases`.
type Release struct {
	Tag  string
	Name string
	// PublishedAt is Unix milliseconds, or nil for a release GitHub did not date.
	PublishedAt *int64
	// BodyHTML is the changelog, rendered server-side and sanitized (D35).
	BodyHTML string
	// Prerelease is GitHub's flag. The listing hides prereleases: an update is
	// applied by tag and a prerelease is not something a one-click button offers.
	Prerelease bool
	// FetchedAt is when this row was last confirmed against api.github.com, in
	// Unix milliseconds. It is what `GET /update/status` reports as the last
	// check — NOT `published_at`, which is a fact about the release rather than
	// about this host's knowledge of it.
	FetchedAt int64
	// HasAsset reports whether this release carries a tarball for this host's
	// architecture. A release that does not is listed and is not applicable, which
	// is more useful than hiding it — an arm64 host should be able to see that
	// v1.2.0 exists and shipped amd64 only.
	HasAsset bool
	// Newer, Same and Older place the release against the running version, which
	// is what the update dialog needs in order to say "downgrade" and to print
	// section 12.4's five commands.
	Newer, Same, Older bool
}

// RefreshReleases fetches the listing, renders each changelog once and upserts
// `release_cache` for source `llamaman`.
func (s *Service) RefreshReleases(ctx context.Context) ([]Release, error) {
	if s.cfg.Releases == nil {
		return nil, fmt.Errorf("selfupdate: this daemon was built without a release feed")
	}
	list, _, err := s.cfg.Releases.ListReleases(ctx, releasePageSize)
	if err != nil {
		return nil, err
	}

	now := s.now()
	err = s.cfg.Store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		for _, r := range list {
			if r.Draft {
				// A draft release's assets are not downloadable by anyone but its
				// author, so caching one is caching a 404.
				continue
			}
			if err := s.cfg.Store.PutReleaseCache(ctx, tx, cacheEntry(r, now)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Releases(ctx)
}

// releasePageSize bounds one listing request. A project with more than a hundred
// releases would page; this one lists the newest hundred, which is every release
// a host could plausibly be updating between.
const releasePageSize = 100

// Releases reads the cached listing — the answer `GET /update/releases` renders,
// with the changelog already HTML.
func (s *Service) Releases(ctx context.Context) ([]Release, error) {
	var rows []store.ReleaseCacheEntry
	err := s.cfg.Store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		rows, err = s.cfg.Store.ReleaseCache(ctx, tx, ReleaseSourceName, releasePageSize)
		return err
	})
	if err != nil {
		return nil, err
	}

	out := make([]Release, 0, len(rows))
	for _, row := range rows {
		if row.Prerelease {
			continue
		}
		r := Release{
			Tag:         row.Tag,
			PublishedAt: row.PublishedAt,
			Prerelease:  row.Prerelease,
			FetchedAt:   row.FetchedAt,
			HasAsset:    assetNamed(row.AssetsJSON, TarballName(row.Tag, s.goarch())),
		}
		if row.Name != nil {
			r.Name = *row.Name
		}
		if row.BodyHTML != nil {
			r.BodyHTML = *row.BodyHTML
		}
		switch CompareVersions(row.Tag, s.cfg.Version) {
		case 1:
			r.Newer = true
		case 0:
			r.Same = true
		default:
			r.Older = true
		}
		out = append(out, r)
	}
	return out, nil
}

// cacheEntry turns a GitHub release into the row `release_cache` holds, with the
// changelog rendered once on the way in.
func cacheEntry(r github.Release, now time.Time) store.ReleaseCacheEntry {
	e := store.ReleaseCacheEntry{
		ID:         ReleaseSourceName + ":" + r.Tag,
		Source:     ReleaseSourceName,
		Tag:        r.Tag,
		Prerelease: r.Prerelease,
		FetchedAt:  now.UnixMilli(),
	}
	if r.Name != "" {
		name := r.Name
		e.Name = &name
	}
	if !r.PublishedAt.IsZero() {
		at := r.PublishedAt.UnixMilli()
		e.PublishedAt = &at
	}
	if r.Body != "" {
		body := r.Body
		html := mdrender.Render(r.Body)
		e.BodyMD, e.BodyHTML = &body, &html
	}
	if b, err := json.Marshal(r.Assets); err == nil {
		s := string(b)
		e.AssetsJSON = &s
	}
	return e
}

// assetNamed reports whether a cached `assets_json` carries an asset with this
// name — the tarball for this host's architecture.
func assetNamed(assetsJSON *string, name string) bool {
	if assetsJSON == nil {
		return false
	}
	var assets []github.Asset
	if err := json.Unmarshal([]byte(*assetsJSON), &assets); err != nil {
		return false
	}
	for _, a := range assets {
		if a.Name == name {
			return true
		}
	}
	return false
}

// CompareVersions orders two release tags: -1 when a is older than b, 0 when
// they are the same release, 1 when a is newer.
//
// It is deliberately small. The only questions this design asks of it are "is
// there an update available" and "is this tag older than the one running", and
// the tags it is ever asked about are the `vX.Y.Z` this project's release job
// produces. A tag with a suffix — `v1.2.0-rc.1` — sorts BELOW the same numbers
// without one, which is the ordinary semver rule and the only part of semver
// that matters here.
func CompareVersions(a, b string) int {
	an, asuffix := splitVersion(a)
	bn, bsuffix := splitVersion(b)

	for i := 0; i < len(an) || i < len(bn); i++ {
		x, y := 0, 0
		if i < len(an) {
			x = an[i]
		}
		if i < len(bn) {
			y = bn[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case asuffix == bsuffix:
		return 0
	case asuffix == "":
		return 1 // a release outranks its own prereleases
	case bsuffix == "":
		return -1
	case asuffix < bsuffix:
		return -1
	default:
		return 1
	}
}

// splitVersion turns `v1.2.0-rc.1` into ([1 2 0], "rc.1"). A component that is
// not a number contributes zero and everything from it on becomes the suffix, so
// a tag this function does not understand sorts below one it does rather than
// panicking or claiming equality.
func splitVersion(v string) ([]int, string) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	core, suffix, _ := strings.Cut(v, "-")
	var nums []int
	for _, part := range strings.Split(core, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			break
		}
		nums = append(nums, n)
	}
	return nums, suffix
}
