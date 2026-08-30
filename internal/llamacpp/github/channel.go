package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// Channel resolution (DESIGN section 6.2), and the asset-name mapping of
// section 6.3.
//
// | Channel | Resolution                                                        |
// |---------|-------------------------------------------------------------------|
// | stable  | releases/latest → semver tag; then that release's nightly-tag.txt  |
// |         | asset → the pinned b#####, which is what is actually fetched/built |
// | nightly | releases?per_page=50, keep prerelease && ^b\d+$, newest first      |
// | custom  | a git URL and ref. NO GitHub API is involved at all                |

// PerPageDefault is section 6.2's nightly listing size.
const PerPageDefault = 50

// NightlyTagAsset is the asset whose content is a stable release's pinned build
// tag. The indirection exists because a semver release names a *build*, and the
// build is what has downloadable binaries; the UI shows both ("v0.3.0 — build
// b10621").
const NightlyTagAsset = "nightly-tag.txt"

// ErrCustomChannel is returned by Resolve for `channel='custom'`: a custom
// build is a git URL and a ref, validated by `git ls-remote`, and this client
// has nothing to say about it (section 6.2).
var ErrCustomChannel = errors.New("github: the custom channel resolves through git ls-remote, not the GitHub API")

// ErrNoAsset means the release exists but has no prebuilt for this host's
// architecture — the ordinary case for a CUDA request, and the case that sends
// section 6.3's decision to a source build.
var ErrNoAsset = errors.New("github: no prebuilt asset for this architecture")

// AssetArch maps a Go architecture onto the vocabulary UPSTREAM uses in its
// release asset names (section 6.3).
//
// This mapping is pinned in the design because getting it wrong is SILENT:
// `llama-b10621-bin-ubuntu-amd64.tar.gz` simply does not exist, the asset
// lookup misses, and every CPU install falls through to a source build nobody
// asked for. `amd64` is upstream's `x64`; `arm64` and `s390x` happen to agree.
//
// It returns false for an architecture upstream publishes nothing for, which is
// a decision input rather than an error: the plan endpoint reports "no prebuilt
// exists for this architecture" and proposes a source build.
func AssetArch(goarch string) (string, bool) {
	switch goarch {
	case "amd64":
		return "x64", true
	case "arm64":
		return "arm64", true
	case "s390x":
		return "s390x", true
	default:
		return "", false
	}
}

// AssetName builds the release asset file name for a tag and a Go architecture.
// It returns false when AssetArch does, so a caller cannot accidentally
// construct a name with a Go arch string in it.
func AssetName(tag, goarch string) (string, bool) {
	arch, ok := AssetArch(goarch)
	if !ok || tag == "" {
		return "", false
	}
	return fmt.Sprintf("llama-%s-bin-ubuntu-%s.tar.gz", tag, arch), true
}

// ResolveRequest is what the caller knows before resolution.
type ResolveRequest struct {
	// Channel is stable or nightly. Custom returns ErrCustomChannel.
	Channel model.LlamacppChannel
	// Tag pins a specific release. On the nightly channel it is a `b#####` the
	// user picked out of the list; on stable it is ignored, because "stable"
	// means "whatever releases/latest says" by definition.
	Tag string
	// GOARCH is the host architecture (runtime.GOARCH). Empty skips the asset
	// lookup entirely, which is what a CUDA request wants — no Linux CUDA
	// prebuilt exists, so there is nothing to look for.
	GOARCH string
}

// Resolution is a channel resolved to the concrete identity a version row is
// built from: `llamacpp_versions.tag`, `.build_tag`, and the asset the prebuilt
// pipeline would download.
type Resolution struct {
	Channel model.LlamacppChannel
	// Tag is `llamacpp_versions.tag`: `v0.3.0` on stable, `b10621` on nightly.
	Tag string
	// BuildTag is `llamacpp_versions.build_tag`: the `b#####` a stable release
	// pins through nightly-tag.txt. Empty on the nightly channel, where the tag
	// IS the build.
	BuildTag string
	// Release is the release Tag came from — its body is the changelog the UI
	// renders, its published_at the date it shows.
	Release Release
	// Asset is the prebuilt tarball for this host, present only when
	// AssetFound. Its release may differ from Release on the stable channel:
	// the binaries live on the pinned build's release.
	Asset      Asset
	AssetFound bool
	// AssetRelease is the tag whose release actually carries Asset, which is
	// BuildTag when the stable release itself has no binaries attached.
	AssetRelease string
}

// FetchTag is the tag that is actually fetched or built — the pinned build on
// stable, the tag itself on nightly. Section 6.2: "the pinned b#####, which is
// what is actually fetched or built".
func (r Resolution) FetchTag() string {
	if r.BuildTag != "" {
		return r.BuildTag
	}
	return r.Tag
}

// PublishedAt is the release date the UI shows beside a version.
func (r Resolution) PublishedAt() time.Time { return r.Release.PublishedAt }

// LatestRelease is `GET /repos/{repo}/releases/latest`.
func (c *Client) LatestRelease(ctx context.Context) (Release, Meta, error) {
	body, meta, err := c.apiGet(ctx, KeyLatestRelease, "/repos/"+c.repo+"/releases/latest")
	if err != nil {
		return Release{}, meta, err
	}
	rel, err := decodeRelease(body)
	if err != nil {
		return Release{}, meta, fmt.Errorf("github: decoding releases/latest: %w", err)
	}
	return rel, meta, nil
}

// ReleaseByTag is `GET /repos/{repo}/releases/tags/{tag}`.
func (c *Client) ReleaseByTag(ctx context.Context, tag string) (Release, Meta, error) {
	if tag == "" {
		return Release{}, Meta{}, errors.New("github: empty tag")
	}
	path := "/repos/" + c.repo + "/releases/tags/" + url.PathEscape(tag)
	body, meta, err := c.apiGet(ctx, "releases/tags/"+tag, path)
	if err != nil {
		return Release{}, meta, err
	}
	rel, err := decodeRelease(body)
	if err != nil {
		return Release{}, meta, fmt.Errorf("github: decoding release %s: %w", tag, err)
	}
	return rel, meta, nil
}

// ListReleases is `GET /repos/{repo}/releases?per_page=N`, unfiltered and in
// GitHub's own order. `perPage` of zero uses PerPageDefault.
func (c *Client) ListReleases(ctx context.Context, perPage int) ([]Release, Meta, error) {
	if perPage <= 0 {
		perPage = PerPageDefault
	}
	path := fmt.Sprintf("/repos/%s/releases?per_page=%d", c.repo, perPage)
	body, meta, err := c.apiGet(ctx, KeyReleaseList, path)
	if err != nil {
		return nil, meta, err
	}
	rels, err := decodeReleases(body)
	if err != nil {
		return nil, meta, fmt.Errorf("github: decoding the release list: %w", err)
	}
	return rels, meta, nil
}

// Nightlies is the release list reduced to section 6.2's nightly channel:
// prereleases tagged `b#####`, newest build first.
func (c *Client) Nightlies(ctx context.Context, perPage int) ([]Release, Meta, error) {
	rels, meta, err := c.ListReleases(ctx, perPage)
	if err != nil {
		return nil, meta, err
	}
	return Nightlies(rels), meta, nil
}

// NightlyTag reads a stable release's `nightly-tag.txt` asset and returns the
// `b#####` it pins.
//
// The asset is fetched from its `browser_download_url`, which is a CDN host and
// NOT api.github.com — so the request carries no token, by construction rather
// than by care (section 6.2). The content is cached per tag and never expires:
// a published release's asset does not change.
func (c *Client) NightlyTag(ctx context.Context, rel Release) (string, Meta, error) {
	asset, ok := rel.Asset(NightlyTagAsset)
	if !ok {
		return "", Meta{}, fmt.Errorf("%w: release %s has no %s asset", ErrNotFound, rel.Tag, NightlyTagAsset)
	}
	body, meta, err := c.get(ctx, KeyNightlyTag(rel.Tag), asset.DownloadURL, false, maxAssetBody)
	if err != nil {
		return "", meta, err
	}
	tag := strings.TrimSpace(string(body))
	// Trust nothing about a downloaded file's content: a `nightly-tag.txt` that
	// says anything but `b<digits>` would become a directory name, a git ref
	// and part of a version id.
	if !IsNightlyTag(tag) {
		return "", meta, fmt.Errorf("github: %s of release %s is %q, which is not a b#### build tag",
			NightlyTagAsset, rel.Tag, clip(tag, 40))
	}
	return tag, meta, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Resolve turns a channel and an optional pinned tag into the concrete identity
// a `llamacpp_versions` row is built from.
func (c *Client) Resolve(ctx context.Context, req ResolveRequest) (Resolution, Meta, error) {
	switch req.Channel {
	case model.ChannelStable:
		return c.resolveStable(ctx, req)
	case model.ChannelNightly:
		return c.resolveNightly(ctx, req)
	case model.ChannelCustom:
		return Resolution{}, Meta{}, ErrCustomChannel
	default:
		return Resolution{}, Meta{}, fmt.Errorf("github: unknown channel %q", req.Channel)
	}
}

func (c *Client) resolveStable(ctx context.Context, req ResolveRequest) (Resolution, Meta, error) {
	rel, meta, err := c.LatestRelease(ctx)
	if err != nil {
		return Resolution{}, meta, err
	}
	res := Resolution{Channel: model.ChannelStable, Tag: rel.Tag, Release: rel}

	// The nightly-tag.txt indirection. A stable release WITHOUT the asset is
	// not an error: the semver tag is then both the identity and the thing
	// fetched, which is exactly what a release with binaries attached directly
	// means. Refusing here would make the product unusable the first time
	// upstream stops publishing the file.
	if _, ok := rel.Asset(NightlyTagAsset); ok {
		buildTag, tagMeta, err := c.NightlyTag(ctx, rel)
		if err != nil {
			return Resolution{}, tagMeta, fmt.Errorf("resolving the build pinned by %s: %w", rel.Tag, err)
		}
		res.BuildTag = buildTag
		if tagMeta.Stale {
			meta.Stale = true
		}
	}

	assetMeta, err := c.attachAsset(ctx, &res, req.GOARCH)
	if err != nil {
		return Resolution{}, meta, err
	}
	if assetMeta.Stale {
		meta.Stale = true
	}
	return res, meta, nil
}

func (c *Client) resolveNightly(ctx context.Context, req ResolveRequest) (Resolution, Meta, error) {
	var rel Release
	var meta Meta
	var err error

	switch {
	case req.Tag != "":
		if !IsNightlyTag(req.Tag) {
			return Resolution{}, Meta{}, fmt.Errorf("github: %q is not a nightly build tag", clip(req.Tag, 40))
		}
		rel, meta, err = c.ReleaseByTag(ctx, req.Tag)
		if err != nil {
			return Resolution{}, meta, err
		}
	default:
		var list []Release
		list, meta, err = c.Nightlies(ctx, PerPageDefault)
		if err != nil {
			return Resolution{}, meta, err
		}
		if len(list) == 0 {
			return Resolution{}, meta, fmt.Errorf("%w: no nightly releases in the last %d",
				ErrNotFound, PerPageDefault)
		}
		rel = list[0]
	}

	res := Resolution{Channel: model.ChannelNightly, Tag: rel.Tag, Release: rel}
	assetMeta, err := c.attachAsset(ctx, &res, req.GOARCH)
	if err != nil {
		return Resolution{}, meta, err
	}
	if assetMeta.Stale {
		meta.Stale = true
	}
	return res, meta, nil
}

// attachAsset finds the CPU prebuilt for this host, if there is one.
//
// On the stable channel the binaries usually live on the PINNED BUILD's
// release rather than on the semver one, so a miss on the semver release is
// followed by one lookup of the build's release. A miss after that is
// ErrNoAsset's ordinary "there is no prebuilt, build from source" answer, not a
// failure — section 6.3's decision table treats it as an input.
func (c *Client) attachAsset(ctx context.Context, res *Resolution, goarch string) (Meta, error) {
	if goarch == "" {
		return Meta{}, nil
	}
	name, ok := AssetName(res.FetchTag(), goarch)
	if !ok {
		return Meta{}, nil
	}
	if a, found := res.Release.Asset(name); found {
		res.Asset, res.AssetFound, res.AssetRelease = a, true, res.Release.Tag
		return Meta{}, nil
	}
	if res.BuildTag == "" || res.BuildTag == res.Release.Tag {
		return Meta{}, nil
	}

	buildRel, meta, err := c.ReleaseByTag(ctx, res.BuildTag)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// The pinned build has no release of its own. Nothing to download;
			// the plan falls through to a source build.
			return meta, nil
		}
		return meta, err
	}
	if a, found := buildRel.Asset(name); found {
		res.Asset, res.AssetFound, res.AssetRelease = a, true, buildRel.Tag
	}
	return meta, nil
}
