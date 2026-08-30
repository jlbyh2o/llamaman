package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/api/middleware"
	"github.com/jlbyh2o/llamaman/internal/gguf"
	"github.com/jlbyh2o/llamaman/internal/hf"
	"github.com/jlbyh2o/llamaman/internal/hf/download"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// Handler tests for the remote Hugging Face endpoints of DESIGN section 3.6 and
// the two credential triples beside them.
//
// The Hub client is stubbed. What this layer owns is the wire contract: the
// TRUE `lfs.size` reaching the client rather than the pointer size, the model
// card arriving RENDERED AND SANITIZED (D35), a gate answering `403 hf_gated`
// with the page a human accepts the terms on, and — the one that would be worst
// to get wrong — no endpoint ever returning a token.

type stubHub struct {
	page hf.SearchPage
	info hf.ModelInfo
	tree []hf.TreeEntry
	card string
	err  error

	gotSearch   hf.SearchParams
	gotRepo     string
	gotRevision string
	gotFile     string
}

func (s *stubHub) Search(_ context.Context, p hf.SearchParams) (hf.SearchPage, error) {
	s.gotSearch = p
	return s.page, s.err
}

func (s *stubHub) Model(_ context.Context, repo string) (hf.ModelInfo, error) {
	s.gotRepo = repo
	return s.info, s.err
}

func (s *stubHub) Tree(_ context.Context, repo, revision string) ([]hf.TreeEntry, error) {
	s.gotRepo, s.gotRevision = repo, revision
	return s.tree, s.err
}

func (s *stubHub) Card(_ context.Context, repo, revision string) (string, error) {
	s.gotRepo, s.gotRevision = repo, revision
	return s.card, s.err
}

func (s *stubHub) Peek(_ context.Context, repo, revision, filePath string, _ ...gguf.Option) (
	*gguf.File, error) {
	s.gotRepo, s.gotRevision, s.gotFile = repo, revision, filePath
	return &gguf.File{}, s.err
}

// stubLocal is the local-availability annotation.
type stubLocal struct {
	byPrimary map[string]string
	err       error
	gotRepo   string
}

func (s *stubLocal) LocalModels(_ context.Context, repo string) (map[string]string, error) {
	s.gotRepo = repo
	return s.byPrimary, s.err
}

// stubToken is one credential triple.
type stubToken struct {
	status   TokenStatus
	err      error
	gotToken string
	deleted  bool
}

func (s *stubToken) Status(context.Context) (TokenStatus, error) { return s.status, s.err }

func (s *stubToken) Validate(_ context.Context, token string) (TokenStatus, error) {
	s.gotToken = token
	if s.err != nil {
		return TokenStatus{}, s.err
	}
	return s.status, nil
}

func (s *stubToken) Delete(context.Context) error {
	s.deleted = true
	return s.err
}

func hubAPI(t *testing.T, cfg Config) *API {
	t.Helper()
	cfg.Auth = stubAuth{
		complete: true,
		session:  &middleware.Session{ID: "s-1"},
		csrfOK:   true,
	}
	return newTestAPI(t, cfg)
}

func TestHFSearchReadsItsQuery(t *testing.T) {
	t.Parallel()

	hub := &stubHub{page: hf.SearchPage{
		Items: []hf.SearchResult{{
			ID: "bartowski/Qwen3-8B-GGUF", Author: "bartowski",
			Downloads: 12_000, Likes: 40, Gated: false,
			GGUF: &hf.GGUFSummary{Architecture: "qwen3", ContextLength: 32768},
		}},
		NextCursor: "opaque-cursor",
	}}
	a := hubAPI(t, Config{HF: hub})

	w := do(t, a, http.MethodGet,
		"/api/v1/hf/search?q=qwen&author=bartowski&sort=likes&limit=20&cursor=c0", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	if hub.gotSearch.Query != "qwen" || hub.gotSearch.Author != "bartowski" ||
		hub.gotSearch.Sort != "likes" || hub.gotSearch.Limit != 20 ||
		hub.gotSearch.Cursor != "c0" {
		t.Errorf("the handler passed %+v", hub.gotSearch)
	}

	var got List[HFSearchResultDTO]
	decode(t, w, &got)
	if len(got.Items) != 1 || got.Items[0].ID != "bartowski/Qwen3-8B-GGUF" {
		t.Fatalf("items = %+v", got.Items)
	}
	if got.NextCursor == nil || *got.NextCursor != "opaque-cursor" {
		t.Errorf("next_cursor = %v; the Hub's cursor is passed through verbatim", got.NextCursor)
	}
	if got.Items[0].GGUF == nil || got.Items[0].GGUF.ContextLength != 32768 {
		t.Errorf("the Hub's gguf summary did not survive: %+v", got.Items[0].GGUF)
	}
}

// TestHFTreeCarriesTrueSizesAndLocalIDs is section 7.1's rule where it matters
// most: "True size is always `lfs.size` … for LFS files the plain `size` can be
// the ~130-byte pointer, which would make a 40 GB model look free and break the
// fit calculator outright."
//
// The client already applies that rule; this asserts the DTO does not undo it,
// and that the shard set arrives as ONE group with `local_model_id` beside it.
func TestHFTreeCarriesTrueSizesAndLocalIDs(t *testing.T) {
	t.Parallel()

	hub := &stubHub{tree: []hf.TreeEntry{
		{Path: "Model-Q4_K_M-00001-of-00002.gguf", Size: 12_000_000_000, OID: "aa", LFS: true},
		{Path: "Model-Q4_K_M-00002-of-00002.gguf", Size: 9_000_000_000, OID: "bb", LFS: true},
		{Path: "mmproj-f16.gguf", Size: 600_000_000, OID: "cc", LFS: true},
	}}
	local := &stubLocal{byPrimary: map[string]string{
		"Model-Q4_K_M-00001-of-00002.gguf": "m-local",
	}}
	a := hubAPI(t, Config{HF: hub, LocalModels: local})

	w := do(t, a, http.MethodGet,
		"/api/v1/hf/tree/bartowski/Qwen3-8B-GGUF?revision=main", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	// The multi-segment wildcard delivers the whole repo id, slash included.
	if hub.gotRepo != "bartowski/Qwen3-8B-GGUF" {
		t.Errorf("repo = %q", hub.gotRepo)
	}
	if hub.gotRevision != "main" || local.gotRepo != "bartowski/Qwen3-8B-GGUF" {
		t.Errorf("revision = %q, local lookup = %q", hub.gotRevision, local.gotRepo)
	}

	var got HFTreeDTO
	decode(t, w, &got)
	if len(got.Groups) != 1 {
		t.Fatalf("groups = %+v, want the shard set as ONE group", got.Groups)
	}
	g := got.Groups[0]
	if g.ShardTotal != 2 || len(g.Files) != 2 || !g.Complete {
		t.Errorf("group = %+v", g)
	}
	if g.TotalBytes != 21_000_000_000 {
		t.Errorf("total_bytes = %d, want the sum of the TRUE sizes", g.TotalBytes)
	}
	if g.LocalModelID == nil || *g.LocalModelID != "m-local" {
		t.Errorf("local_model_id = %v", g.LocalModelID)
	}
	if len(got.Mmproj) != 1 || !got.Mmproj[0].Mmproj {
		t.Errorf("mmproj candidates = %+v", got.Mmproj)
	}
}

// TestHFCardIsRenderedAndSanitized is D35: a model card is attacker-controlled
// markdown containing raw HTML, and it is rendered in the origin that holds the
// admin session cookie.
func TestHFCardIsRenderedAndSanitized(t *testing.T) {
	t.Parallel()

	hub := &stubHub{card: "# Qwen3\n\n<script>fetch('/api/v1/auth/sessions')</script>\n\n" +
		"A **good** model.\n"}
	a := hubAPI(t, Config{HF: hub})

	w := do(t, a, http.MethodGet, "/api/v1/hf/card/bartowski/Qwen3-8B-GGUF", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	var got HFCardDTO
	decode(t, w, &got)
	if contains(got.HTML, "<script") || contains(got.HTML, "fetch(") {
		t.Fatalf("THE RENDERED CARD CARRIES A SCRIPT: %s", got.HTML)
	}
	if !contains(got.HTML, "<strong>good</strong>") {
		t.Errorf("the card did not render: %s", got.HTML)
	}
	if got.Markdown == "" {
		t.Error("the raw markdown is missing; \"view source\" has nothing to show")
	}
}

func TestHFPeekRequiresAFile(t *testing.T) {
	t.Parallel()

	a := hubAPI(t, Config{HF: &stubHub{}})
	w := do(t, a, http.MethodGet, "/api/v1/hf/peek/bartowski/Qwen3-8B-GGUF", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 with no ?file=", w.Code)
	}
	if got := errorCode(t, w); got != string(CodeBadRequest) {
		t.Errorf("error.code = %q", got)
	}
}

// TestHubRefusalsCarryTheirCodes is section 3.6's gated-repo UX: "Gated repos
// return `403 hf_gated` with `{"repo","request_url"}`; the UI links out, because
// access grants are browser-only on HF's side."
func TestHubRefusalsCarryTheirCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		err    error
		status int
		code   model.ErrorCode
	}{
		{
			name: "a gated repository",
			err: &hf.GatedError{
				Repo:       "meta-llama/Llama-3-8B",
				RequestURL: "https://huggingface.co/meta-llama/Llama-3-8B",
				Status:     403,
			},
			status: http.StatusForbidden, code: download.CodeHFGated,
		},
		{
			name:   "a private repository with no token",
			err:    &hf.PrivateError{Repo: "someone/private", HaveToken: false, Status: 401},
			status: http.StatusForbidden, code: download.CodeHFPrivate,
		},
		{
			name:   "no such repository",
			err:    hf.ErrNotFound,
			status: http.StatusUnprocessableEntity, code: download.CodeFileNotInRepo,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := hubAPI(t, Config{HF: &stubHub{err: tc.err}})
			w := do(t, a, http.MethodGet, "/api/v1/hf/model/meta-llama/Llama-3-8B", "")
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d; body %s", w.Code, tc.status, w.Body)
			}
			if got := errorCode(t, w); got != string(tc.code) {
				t.Errorf("error.code = %q, want %q", got, tc.code)
			}
		})
	}

	t.Run("the gated body carries the page a human accepts the terms on", func(t *testing.T) {
		t.Parallel()
		a := hubAPI(t, Config{HF: &stubHub{err: &hf.GatedError{
			Repo:       "meta-llama/Llama-3-8B",
			RequestURL: "https://huggingface.co/meta-llama/Llama-3-8B",
		}}})
		w := do(t, a, http.MethodGet, "/api/v1/hf/model/meta-llama/Llama-3-8B", "")
		var env model.ErrorEnvelope
		decode(t, w, &env)
		if env.Error.Details["request_url"] != "https://huggingface.co/meta-llama/Llama-3-8B" {
			t.Errorf("details = %v", env.Error.Details)
		}
	})
}

// TestTokenEndpointsNeverReturnTheToken is the whole point of section 3.6's
// triples: "`{"present","hint","valid","user","scopes"}` — never the token".
func TestTokenEndpointsNeverReturnTheToken(t *testing.T) {
	t.Parallel()

	const secret = "hf_ABCDEFGHIJKLMNOPqrs"
	valid := true

	for _, path := range []string{"hf", "github"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			tok := &stubToken{status: TokenStatus{
				Present: true, Hint: "hf_A…qrs", Valid: &valid,
				User: "someone", Scopes: []string{"read-repos"},
			}}
			cfg := Config{}
			if path == "hf" {
				cfg.HFToken = tok
			} else {
				cfg.GitHubToken = tok
			}
			a := hubAPI(t, cfg)

			w := do(t, a, http.MethodGet, "/api/v1/"+path+"/token", "")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", w.Code, w.Body)
			}
			var got TokenStatusDTO
			decode(t, w, &got)
			if !got.Present || got.Hint != "hf_A…qrs" || got.Valid == nil || !*got.Valid {
				t.Errorf("status = %+v", got)
			}

			w = do(t, a, http.MethodPut, "/api/v1/"+path+"/token",
				`{"token":"`+secret+`"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("PUT status = %d, body %s", w.Code, w.Body)
			}
			if tok.gotToken != secret {
				t.Errorf("the handler validated %q", tok.gotToken)
			}
			if contains(w.Body.String(), secret) {
				t.Fatalf("THE PUT RESPONSE ECHOED THE TOKEN: %s", w.Body)
			}

			w = do(t, a, http.MethodDelete, "/api/v1/"+path+"/token", "")
			if w.Code != http.StatusNoContent {
				t.Fatalf("DELETE status = %d, body %s", w.Code, w.Body)
			}
			if !tok.deleted {
				t.Error("DELETE did not reach the service")
			}
		})
	}
}

// TestRefusedTokenIs422 separates the two failures a PUT can have. A provider
// that REFUSED the credential is a 422 the user can act on; a provider that
// could not be reached must not be reported as "your token is wrong", or the
// user deletes a working credential because the Hub was briefly down.
func TestRefusedTokenIs422(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		path   string
		err    error
		status int
		code   model.ErrorCode
	}{
		{
			name: "the Hub refused it", path: "hf", err: ErrTokenInvalid,
			status: http.StatusUnprocessableEntity, code: CodeHFTokenInvalid,
		},
		{
			name: "GitHub refused it", path: "github", err: ErrTokenInvalid,
			status: http.StatusUnprocessableEntity, code: CodeGitHubTokenInvalid,
		},
		{
			name: "the Hub could not be reached", path: "hf",
			err:    errors.New("dial tcp: i/o timeout"),
			status: http.StatusInternalServerError, code: CodeInternalError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tok := &stubToken{err: tc.err}
			cfg := Config{}
			if tc.path == "hf" {
				cfg.HFToken = tok
			} else {
				cfg.GitHubToken = tok
			}
			a := hubAPI(t, cfg)

			w := do(t, a, http.MethodPut, "/api/v1/"+tc.path+"/token", `{"token":"x"}`)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d; body %s", w.Code, tc.status, w.Body)
			}
			if got := errorCode(t, w); got != string(tc.code) {
				t.Errorf("error.code = %q, want %q", got, tc.code)
			}
		})
	}
}

// TestHubEndpointsWithoutAClient: a build with no Hub client reports the gap
// rather than faking an empty answer.
func TestHubEndpointsWithoutAClient(t *testing.T) {
	t.Parallel()

	a := hubAPI(t, Config{})
	for _, target := range []string{
		"/api/v1/hf/search",
		"/api/v1/hf/model/a/b",
		"/api/v1/hf/tree/a/b",
		"/api/v1/hf/card/a/b",
		"/api/v1/hf/peek/a/b?file=x.gguf",
		"/api/v1/hf/token",
		"/api/v1/github/token",
	} {
		w := do(t, a, http.MethodGet, target, "")
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s = %d, want 503", target, w.Code)
		}
	}
}

// TestMalformedRepoIDIsA400 is what the multi-segment wildcard makes possible:
// `{repo...}` matches any number of segments, so `/hf/model/a/b/c` arrives as a
// three-part "repo id". The client refuses it — a repo id is interpolated into a
// Hub URL and into a cache directory name — but a refusal classified as "the Hub
// could not be reached" sends a user looking at their network instead of at
// their URL.
func TestMalformedRepoIDIsA400(t *testing.T) {
	t.Parallel()

	hub := &stubHub{}
	a := hubAPI(t, Config{HF: hub})

	for _, target := range []string{
		"/api/v1/hf/model/a/b/c",
		"/api/v1/hf/tree/a/b/c",
		"/api/v1/hf/card/",
		"/api/v1/hf/peek/a/b/c?file=x.gguf",
	} {
		w := do(t, a, http.MethodGet, target, "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", target, w.Code)
			continue
		}
		if got := errorCode(t, w); got != string(CodeBadRequest) {
			t.Errorf("%s error.code = %q", target, got)
		}
	}
	if hub.gotRepo != "" {
		t.Errorf("a malformed repo id reached the Hub client as %q", hub.gotRepo)
	}
}
