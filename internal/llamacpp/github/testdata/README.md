# internal/llamacpp/github/testdata

Recorded api.github.com responses, served by the `httptest` fake in
`github_test.go`. No test in this package ever touches the network: unit tests
use these fixtures, and the only live call this design allows is
`.github/workflows/nightly.yml`'s re-resolution of the current nightly tag
(DESIGN section 6.3).

| file | what it is |
|---|---|
| `releases_latest.json` | `GET /repos/ggml-org/llama.cpp/releases/latest` — a semver stable release carrying a `nightly-tag.txt` asset and NO binaries, which is the shape section 6.2's indirection exists for |
| `nightly-tag.txt` | that release's `nightly-tag.txt` asset: the pinned build, `b10621` |
| `release_b10621.json` | `GET /releases/tags/b10621` — the pinned build's own release, carrying the ubuntu x64 and arm64 tarballs plus a Windows CUDA zip the asset picker must not choose |
| `releases_list.json` | `GET /releases?per_page=50`, containing every case the nightly filter has to get right: a draft (`b10622`), a non-prerelease semver tag, two ordinary nightlies, a hand-pushed prerelease whose tag is not `b####`, and `b9999` — which sorts ABOVE `b10621` as a string and below it as a number |
| `user.json` | `GET /user`, for token validation |
| `rate_limited.json` | the 403 body api.github.com returns when the hourly budget is spent |

The asset `browser_download_url` values are the real `https://github.com/...`
CDN URLs they were recorded with. The test rewrites that host to its own second
`httptest` server, which is how the "the token never reaches the asset host"
assertion has two distinct hosts to distinguish.

The shapes and field names are api.github.com's; the ids, digests, sizes and
dates are synthetic. Nothing here contains a personal identifier: the only
accounts named are `ggml-org`, `github-actions[bot]` and `octocat`, and the
tokens in the tests are obviously fake (`ghp_TESTTOKEN…`).
