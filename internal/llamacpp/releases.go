package llamacpp

import (
	"context"
	"errors"
	"time"

	"github.com/jlbyh2o/llamaman/internal/llamacpp/github"
	"github.com/jlbyh2o/llamaman/internal/mdrender"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// `GET /api/v1/llamacpp/releases` — DESIGN section 3.5's "cached releases with
// rendered changelog HTML and resolved `nightly_tag`", and section 6.2's
// rate-limit reporting.
//
// Two things about this endpoint are contract rather than convenience:
//
//   - The changelog is rendered and SANITIZED HERE (D35). A release body is
//     markdown from a public repository, and rendering it in the browser that
//     holds the admin session cookie is the stored-XSS hole section 14 calls
//     bluemonday non-negotiable for. internal/mdrender is the only renderer.
//   - The rate limit travels with the answer. Unauthenticated api.github.com
//     allows 60 requests an hour per IP, and a stale nightly list with no
//     explanation is the single most confusing thing this screen can show;
//     section 6.2 says "why is the nightly list stale" must have an answer on
//     screen, and `{"remaining":N,"reset_at":…,"authenticated":false}` is it.

// ReleaseLister is the release half of the GitHub client. *github.Client
// satisfies it, and it is separate from Resolver because a daemon may
// legitimately have one and not the other — a build that was given an explicit
// tag needs no listing at all.
type ReleaseLister interface {
	LatestRelease(ctx context.Context) (github.Release, github.Meta, error)
	Nightlies(ctx context.Context, perPage int) ([]github.Release, github.Meta, error)
	NightlyTag(ctx context.Context, rel github.Release) (string, github.Meta, error)
	RateLimit() github.RateLimit
}

// NightlyListLimit is how many nightlies §6.2 shows: "the last ~30 with dates".
const NightlyListLimit = 30

// ReleaseView is one release as the API returns it.
type ReleaseView struct {
	Tag         string    `json:"tag"`
	Name        string    `json:"name"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	// BodyMarkdown is the changelog verbatim, for a "view source" toggle, and
	// BodyHTML is the rendered, sanitized form the page shows (D35).
	BodyMarkdown string `json:"body_markdown"`
	BodyHTML     string `json:"body_html"`
	// NightlyTag is the `b#####` a stable release pins through its
	// nightly-tag.txt asset — what is actually fetched or built (§6.2). Empty on
	// a nightly, which IS its own build tag.
	NightlyTag string `json:"nightly_tag,omitempty"`
	// AssetName is the prebuilt for THIS host's architecture, empty when the
	// release publishes none — which is the fact that sends §6.3's decision to a
	// source build.
	AssetName string `json:"asset_name,omitempty"`
	AssetSize int64  `json:"asset_size,omitempty"`
	// Installed reports that a version row for this tag already exists on this
	// host, so the list can say "installed" rather than offering a rebuild.
	Installed bool `json:"installed"`
}

// ReleaseList is what `GET /api/v1/llamacpp/releases` answers with.
type ReleaseList struct {
	Channel  model.LlamacppChannel `json:"channel"`
	Releases []ReleaseView         `json:"releases"`
	// Stale reports that the bodies came from the cache because the network or
	// the rate limit would not produce a fresh answer (§6.2's
	// stale-while-revalidate).
	Stale bool `json:"stale"`
	// FetchedAt is when the served answer was last confirmed fresh.
	FetchedAt time.Time `json:"fetched_at"`
	// RateLimit is §6.2's on-screen answer to "why is this list stale".
	RateLimit github.RateLimit `json:"rate_limit"`
}

// Releases lists the releases of one channel (§3.5).
//
// `custom` has no listing at all and says so: a custom build is a git URL and a
// ref, and no GitHub API is involved (§6.2's channel table).
func (s *Service) Releases(ctx context.Context, channel model.LlamacppChannel) (ReleaseList, error) {
	if channel == "" {
		channel = model.ChannelStable
	}
	if !channel.Valid() {
		return ReleaseList{}, errorf(model.CodeBadFlags,
			"channel %q is not stable, nightly or custom", channel)
	}
	if channel == model.ChannelCustom {
		return ReleaseList{}, errorf(model.CodeBadFlags,
			"the custom channel is a git URL and a ref; it has no release listing")
	}
	if s.releases == nil {
		return ReleaseList{}, errorf(CodeResolveFailed,
			"this daemon has no GitHub release client, so it cannot list releases")
	}

	var (
		rels []github.Release
		meta github.Meta
		err  error
	)
	if channel == model.ChannelStable {
		var rel github.Release
		rel, meta, err = s.releases.LatestRelease(ctx)
		if err == nil {
			rels = []github.Release{rel}
		}
	} else {
		rels, meta, err = s.releases.Nightlies(ctx, github.PerPageDefault)
		if len(rels) > NightlyListLimit {
			rels = rels[:NightlyListLimit]
		}
	}
	if err != nil {
		return ReleaseList{}, errorf(CodeResolveFailed,
			"could not list the %s channel: %v", channel, err)
	}

	installed, err := s.installedTags(ctx)
	if err != nil {
		return ReleaseList{}, err
	}

	out := ReleaseList{
		Channel:   channel,
		Releases:  make([]ReleaseView, 0, len(rels)),
		Stale:     meta.Stale,
		FetchedAt: meta.FetchedAt,
		RateLimit: s.releases.RateLimit(),
	}
	for _, r := range rels {
		v := ReleaseView{
			Tag:          r.Tag,
			Name:         r.Name,
			Prerelease:   r.Prerelease,
			PublishedAt:  r.PublishedAt,
			BodyMarkdown: r.Body,
			BodyHTML:     mdrender.Render(r.Body),
		}
		if channel == model.ChannelStable {
			// The semver tag names a BUILD, and the build is what has
			// downloadable binaries. A lookup that fails leaves the field empty
			// rather than failing the listing: the changelog is still worth
			// showing, and the install POST resolves this again anyway.
			if tag, _, terr := s.releases.NightlyTag(ctx, r); terr == nil {
				v.NightlyTag = tag
			} else if !errors.Is(terr, context.Canceled) {
				s.log.Debug("could not resolve the pinned build tag",
					"tag", r.Tag, "error", terr)
			}
		}
		if name, ok := github.AssetName(assetTagOf(v), s.goarch); ok {
			if a, found := r.Asset(name); found {
				v.AssetName, v.AssetSize = a.Name, a.Size
			}
		}
		v.Installed = installed[assetTagOf(v)] || installed[r.Tag]
		out.Releases = append(out.Releases, v)
	}
	return out, nil
}

// assetTagOf is the tag whose asset a release actually offers: the pinned build
// on the stable channel, the release's own tag on the nightly one.
func assetTagOf(v ReleaseView) string {
	if v.NightlyTag != "" {
		return v.NightlyTag
	}
	return v.Tag
}

// installedTags is the set of upstream tags this host already has a version row
// for, in any state. It is what lets the release list say "installed" without
// one request per row.
func (s *Service) installedTags(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		rows, err := s.store.LlamacppVersions(ctx, tx, store.LlamacppVersionFilter{})
		if err != nil {
			return err
		}
		for _, r := range rows {
			out[r.Tag] = true
			if r.BuildTag != nil {
				out[*r.BuildTag] = true
			}
		}
		return nil
	})
	return out, err
}
