package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/gguf"
	"github.com/jlbyh2o/llamaman/internal/hf"
	"github.com/jlbyh2o/llamaman/internal/hf/download"
	"github.com/jlbyh2o/llamaman/internal/mdrender"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// The remote Hugging Face surface of DESIGN section 3.6, and the two validating
// credential triples beside it.
//
// # Why the verb precedes the wildcard
//
// A repo id contains a `/` (`bartowski/Qwen3-8B-GGUF`), so it needs Go's
// multi-segment `{repo...}` wildcard — and `net/http.ServeMux` requires such a
// wildcard to be the FINAL element of the pattern. Registering
// `/api/v1/hf/models/{repo...}/tree` does not 404 at request time; it PANICS AT
// REGISTRATION, taking the daemon down at boot. So the verb moves in front:
// `/api/v1/hf/tree/{repo...}`. One segment per concept, the repo id unescaped
// and readable in the URL, and the router still stdlib. Registry.Add refuses the
// other shape, and section 3.6's CI test builds the real ServeMux inside a
// recover() so the two checks cannot both be wrong.
//
// # Secrets are not settings
//
// The two credentials never appear in `GET /api/v1/settings`, because a settings
// value is returned in the clear and these must not be. Each has its own triple
// here returning presence, hint and validity only — never the token — and the
// Settings UI renders them inside the groups they belong to (section 3.4).

// HFService is the remote half of the Hugging Face client this layer needs.
// *hf.Client satisfies it (DESIGN section 1: the consumer owns the interface).
type HFService interface {
	Search(ctx context.Context, p hf.SearchParams) (hf.SearchPage, error)
	Model(ctx context.Context, repo string) (hf.ModelInfo, error)
	Tree(ctx context.Context, repo, revision string) ([]hf.TreeEntry, error)
	Card(ctx context.Context, repo, revision string) (string, error)
	Peek(ctx context.Context, repo, revision, filePath string, opts ...gguf.Option) (*gguf.File, error)
}

// LocalIndex answers "does this host already have this repository", which is the
// "local-availability annotations" of section 3.6's model row and the
// `local_model_id` of its tree row.
//
// It is a separate, OPTIONAL interface rather than a method on HFService because
// it is a database question and HFService is an HTTP client: a daemon can
// legitimately serve the remote endpoints with no catalog behind them, and the
// annotation is then absent rather than wrong.
type LocalIndex interface {
	// LocalModels returns the model ids this host holds for a repository, keyed
	// by the primary file name, so a tree can mark each quantization
	// individually.
	LocalModels(ctx context.Context, repoID string) (map[string]string, error)
}

// TokenService is one credential's validating triple (section 3.6): read the
// status, validate-and-store, delete. The token value never crosses it in the
// read direction.
type TokenService interface {
	// Status reports presence, hint, validity, account and scopes.
	Status(ctx context.Context) (TokenStatus, error)
	// Validate checks the presented token against its provider and stores it
	// sealed only if the provider accepted it. ErrTokenInvalid is the 422.
	Validate(ctx context.Context, token string) (TokenStatus, error)
	// Delete removes the stored token. Deleting one that is not there succeeds.
	Delete(ctx context.Context) error
}

// ErrTokenInvalid is what a TokenService returns when the provider REFUSED the
// presented credential — section 3.6's `422 hf_token_invalid` and
// `422 github_token_invalid`.
//
// It is deliberately distinct from a transport failure. A network error must
// never be reported to a user as "your token is wrong": they would delete a
// working credential because the Hub was briefly unreachable.
var ErrTokenInvalid = errors.New("api: the provider refused this token")

// TokenStatus is what a credential endpoint returns. There is no field for the
// token, deliberately — a struct with one is a struct someone eventually
// marshals.
type TokenStatus struct {
	Present bool
	Hint    string
	Valid   *bool
	User    string
	Scopes  []string
	// RateLimit is the GitHub triple's extra: section 6.2 puts the current
	// api.github.com headroom beside the token, so "why is the nightly list
	// stale" has an answer on screen. It is nil for the Hugging Face token.
	RateLimit *RateLimitDTO
}

// -----------------------------------------------------------------------------
// DTOs
// -----------------------------------------------------------------------------

// HFSearchResultDTO is one row of `GET /api/v1/hf/search`.
type HFSearchResultDTO struct {
	ID        string   `json:"id"`
	Author    string   `json:"author"`
	Downloads int64    `json:"downloads"`
	Likes     int64    `json:"likes"`
	Gated     bool     `json:"gated"`
	Private   bool     `json:"private"`
	UpdatedAt *string  `json:"updated_at"`
	Tags      []string `json:"tags"`
	// GGUF is the Hub's own server-computed summary, null when it computed
	// none. It is a courtesy and never the authority: the fit calculator reads
	// the header this daemon parsed itself (section 8.5).
	GGUF *HFGGUFSummaryDTO `json:"gguf"`
}

// HFGGUFSummaryDTO is the Hub's `gguf` object.
type HFGGUFSummaryDTO struct {
	Architecture  string `json:"architecture"`
	ContextLength int64  `json:"context_length"`
	Total         int64  `json:"total"`
}

// HFModelDTO is `GET /api/v1/hf/model/{repo...}`.
type HFModelDTO struct {
	ID           string            `json:"id"`
	Author       string            `json:"author"`
	SHA          string            `json:"sha"`
	Gated        bool              `json:"gated"`
	Private      bool              `json:"private"`
	Disabled     bool              `json:"disabled"`
	Downloads    int64             `json:"downloads"`
	Likes        int64             `json:"likes"`
	LastModified *string           `json:"last_modified"`
	Tags         []string          `json:"tags"`
	GGUF         *HFGGUFSummaryDTO `json:"gguf"`
	// LocalModelIDs are this host's `models` rows for this repository, keyed by
	// primary file. An empty object means nothing here is downloaded — the
	// local-availability annotation of section 3.6.
	LocalModelIDs map[string]string `json:"local_model_ids"`
}

// HFTreeEntryDTO is one file of a quantization group.
type HFTreeEntryDTO struct {
	Path string `json:"path"`
	// SizeBytes is the TRUE size — `lfs.size` when the entry has an LFS object,
	// never the ~130-byte pointer (section 7.1).
	SizeBytes int64 `json:"size_bytes"`
	// OID is the git blob sha, or the LFS object's sha256 — which for an LFS
	// object IS the cache blob name.
	OID string `json:"oid"`
	LFS bool   `json:"lfs"`
}

// HFTreeGroupDTO is one downloadable unit: a quantization, which is one file or
// a whole shard set (section 7.3).
type HFTreeGroupDTO struct {
	Key        string           `json:"key"`
	QuantLabel string           `json:"quant_label"`
	Files      []HFTreeEntryDTO `json:"files"`
	TotalBytes int64            `json:"total_bytes"`
	ShardTotal int              `json:"shard_total"`
	// Complete reports that every shard the names declare is present. A
	// repository mid-upload can advertise `-00003-of-00005` with two shards
	// missing, and queueing that is a download that can never finish.
	Complete bool `json:"complete"`
	// Mmproj marks a multimodal projector, downloaded as a separate `models`
	// row because it is separately reusable across quantizations.
	Mmproj bool `json:"mmproj"`
	// LocalModelID is this host's row for this group, null when it is not
	// downloaded.
	LocalModelID *string `json:"local_model_id"`
}

// HFTreeDTO is `GET /api/v1/hf/tree/{repo...}`.
type HFTreeDTO struct {
	RepoID   string `json:"repo_id"`
	Revision string `json:"revision"`
	// Groups are the quantizations, and Mmproj the projector candidates section
	// 7.3 pairs against them.
	Groups []HFTreeGroupDTO `json:"groups"`
	Mmproj []HFTreeGroupDTO `json:"mmproj"`
}

// HFCardDTO is `GET /api/v1/hf/card/{repo...}`: the rendered, sanitized HTML
// (D35) plus the raw markdown behind the "view source" toggle.
type HFCardDTO struct {
	RepoID   string `json:"repo_id"`
	Revision string `json:"revision"`
	HTML     string `json:"html"`
	Markdown string `json:"markdown"`
}

// HFPeekDTO is `GET /api/v1/hf/peek/{repo...}?file=` — the GGUF header read over
// HTTP Range BEFORE downloading twenty gigabytes (sections 3.6 and 8.5).
type HFPeekDTO struct {
	RepoID string `json:"repo_id"`
	File   string `json:"file"`
	// Arch is `general.architecture` and the namespace every field below was
	// read from.
	Arch      string `json:"arch"`
	NLayer    int    `json:"n_layer"`
	NEmbd     int    `json:"n_embd"`
	NHead     int    `json:"n_head"`
	NCtxTrain int    `json:"n_ctx_train"`
	NVocab    int    `json:"n_vocab"`
	// NHeadKV is per layer, because section 8.3 sizes the KV cache per layer and
	// an averaged scalar is wrong exactly where the answer matters (D30).
	NHeadKV   []int `json:"n_head_kv"`
	HeadDimK  int   `json:"head_dim_k"`
	HeadDimV  int   `json:"head_dim_v"`
	SWAWindow *int  `json:"swa_window"`
	// SWAPattern is the PERIOD beside the window's WIDTH. Both are null when the
	// keys are absent, which section 8.3 reads as "no SWA at all" — deliberately
	// not zero.
	SWAPattern     *int   `json:"swa_pattern"`
	NExpert        int    `json:"n_expert"`
	NExpertUsed    int    `json:"n_expert_used"`
	TokenizerModel string `json:"tokenizer_model"`
	Quantization   string `json:"quantization"`
	// TensorSummary is section 8.2's bucketing of the tensor index, which is
	// what makes the weight term exact rather than a file-size guess. It is the
	// same shape `models.tensor_summary_json` stores, so the fit panel reads one
	// structure whether the file is on disk or still on the Hub.
	TensorSummary gguf.Sizes `json:"tensor_summary"`
	// Notes records what could not be read and what had to be assumed. It is
	// diagnostic text, never a control signal.
	Notes []string `json:"notes"`
	// SizeBytes is the file's full length, so a fit panel can state the download
	// beside the geometry.
	SizeBytes int64 `json:"size_bytes"`
}

// TokenStatusDTO is what `GET /api/v1/hf/token` and `GET /api/v1/github/token`
// answer with: presence, hint, validity — never the token.
type TokenStatusDTO struct {
	Present bool   `json:"present"`
	Hint    string `json:"hint"`
	// Valid is null when the token has never been validated, which is a
	// different sentence from "it was refused".
	Valid  *bool    `json:"valid"`
	User   string   `json:"user"`
	Scopes []string `json:"scopes"`
	// RateLimit is the GitHub triple's extra (section 6.2); null on the Hugging
	// Face one.
	RateLimit *RateLimitDTO `json:"rate_limit"`
}

// PutTokenRequest is the body of both PUTs.
type PutTokenRequest struct {
	Token string `json:"token"`
}

// -----------------------------------------------------------------------------
// Routes
// -----------------------------------------------------------------------------

func (a *API) hfRoutes() []Route {
	return []Route{
		a.hfSearchRoute(),
		a.hfModelRoute(),
		a.hfTreeRoute(),
		a.hfCardRoute(),
		a.hfPeekRoute(),
	}
}

func (a *API) tokenRoutes() []Route {
	return []Route{
		a.getTokenRoute("hf", "HF", "Hugging Face", "getHFToken"),
		a.putTokenRoute("hf", "HF", "Hugging Face",
			"`GET /api/whoami-v2`", "putHFToken", CodeHFTokenInvalid),
		a.deleteTokenRoute("hf", "HF", "Hugging Face", "deleteHFToken"),

		a.getTokenRoute("github", "GitHub", "GitHub", "getGitHubToken"),
		a.putTokenRoute("github", "GitHub", "GitHub",
			"`GET https://api.github.com/user`", "putGitHubToken", CodeGitHubTokenInvalid),
		a.deleteTokenRoute("github", "GitHub", "GitHub", "deleteGitHubToken"),
	}
}

func (a *API) hfSearchRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/hf/search",
		Auth:        AuthSession,
		OperationID: "searchHF",
		Summary: "Search the Hub's GGUF repositories. `?cursor=` is the Hub's own opaque " +
			"cursor, passed through unmodified.",
		Tag: "hf",
		Query: []QueryParam{
			{Name: "q", Description: "Free text. Empty lists the most-downloaded GGUF repositories."},
			{Name: "author", Description: "Restrict to one namespace."},
			{
				Name:        "sort",
				Description: "Order. Empty means downloads.",
				Enum:        []string{"downloads", "likes", "lastModified", "trendingScore"},
			},
			{Name: "limit", Description: "Page size, up to 100.", Type: "integer"},
			{Name: "cursor", Description: "The previous page's `next_cursor`."},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.hub()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			q := r.URL.Query()
			page, err := svc.Search(r.Context(), hf.SearchParams{
				Query:  q.Get("q"),
				Author: q.Get("author"),
				Sort:   q.Get("sort"),
				Limit:  int(queryInt64(r, "limit", 0)),
				Cursor: q.Get("cursor"),
			})
			if err != nil {
				a.writeHubError(w, r, err)
				return
			}
			items := make([]HFSearchResultDTO, 0, len(page.Items))
			for _, it := range page.Items {
				items = append(items, hfSearchDTO(it))
			}
			var next *string
			if page.NextCursor != "" {
				c := page.NextCursor
				next = &c
			}
			if err := WriteJSON(w, http.StatusOK, NewList(items, len(items), next)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "One page of normalized search results.",
			Body:        List[HFSearchResultDTO]{},
		},
		Errors: hubErrors(),
	}
}

// repoErrors is hubErrors plus the 400 a malformed `{repo...}` produces — and,
// on the peek route, a missing `?file=`.
func repoErrors() []Response {
	return append(hubErrors(), Response{
		Status: http.StatusBadRequest,
		Description: "The path did not name a repository id — `{repo...}` matches any number " +
			"of segments, and only `name` or `org/name` is one — or, on the peek route, " +
			"`?file=` was not given.",
		Codes: []model.ErrorCode{CodeBadRequest},
	})
}

func (a *API) hfModelRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/hf/model/{repo...}",
		Auth:        AuthSession,
		OperationID: "getHFModel",
		Summary: "One repository's metadata, its `gguf` summary, its gated flag and this " +
			"host's local-availability annotations.",
		Tag: "hf",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.hub()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			repo, err := repoParam(r)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			info, err := svc.Model(r.Context(), repo)
			if err != nil {
				a.writeHubError(w, r, err)
				return
			}
			local, err := a.localModels(r.Context(), repo)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, hfModelDTO(info, local)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The repository.",
			Body:        HFModelDTO{},
		},
		Errors: repoErrors(),
	}
}

func (a *API) hfTreeRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/hf/tree/{repo...}",
		Auth:        AuthSession,
		OperationID: "getHFTree",
		Summary: "The file tree grouped by quantization, with TRUE `lfs.size` totals, shard " +
			"groups, mmproj candidates and `local_model_id`.",
		Tag: "hf",
		Query: []QueryParam{{
			Name:        "revision",
			Description: "Branch, tag or commit. Empty means `main`.",
		}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.hub()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			repo, err := repoParam(r)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			revision := r.URL.Query().Get("revision")
			entries, err := svc.Tree(r.Context(), repo, revision)
			if err != nil {
				a.writeHubError(w, r, err)
				return
			}
			local, err := a.localModels(r.Context(), repo)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK,
				hfTreeDTO(repo, revision, entries, local)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The repository's downloadable groups.",
			Body:        HFTreeDTO{},
		},
		Errors: repoErrors(),
	}
}

func (a *API) hfCardRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/hf/card/{repo...}",
		Auth:        AuthSession,
		OperationID: "getHFCard",
		Summary: "The model card, RENDERED AND SANITIZED SERVER-SIDE (D35), plus the raw " +
			"markdown for a \"view source\" toggle.",
		Tag: "hf",
		Query: []QueryParam{{
			Name:        "revision",
			Description: "Branch, tag or commit. Empty means `main`.",
		}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.hub()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			repo, err := repoParam(r)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			revision := r.URL.Query().Get("revision")
			md, err := svc.Card(r.Context(), repo, revision)
			if err != nil {
				a.writeHubError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, HFCardDTO{
				RepoID: repo, Revision: revision,
				// The ONE renderer (D35). A card is attacker-controlled markdown
				// containing raw HTML, and this origin holds the admin session
				// cookie.
				HTML:     mdrender.Render(md),
				Markdown: md,
			}); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status: http.StatusOK,
			Description: "The card. A repository with no README answers with empty strings, " +
				"not a 404: plenty of good repositories have none.",
			Body: HFCardDTO{},
		},
		Errors: repoErrors(),
	}
}

func (a *API) hfPeekRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/hf/peek/{repo...}",
		Auth:        AuthSession,
		OperationID: "peekHFFile",
		Summary: "The GGUF header read over HTTP Range BEFORE downloading: architecture, " +
			"layers, KV heads, trained context, vocabulary, SWA window and the tensor " +
			"summary (sections 3.6, 8.5).",
		Tag: "hf",
		Query: []QueryParam{
			{
				Name: "file",
				Description: "The file to peek, inside the repository. Only shard 1 of a " +
					"sharded set carries the geometry.",
				Required: true,
			},
			{
				Name:        "revision",
				Description: "Branch, tag or commit. Empty means `main`.",
			},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.hub()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			repo, err := repoParam(r)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			file := strings.TrimSpace(r.URL.Query().Get("file"))
			if file == "" {
				WriteError(w, r, a.log, BadRequest("?file= names the file to peek"))
				return
			}
			f, err := svc.Peek(r.Context(), repo, r.URL.Query().Get("revision"), file)
			if err != nil {
				a.writeHubError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, hfPeekDTO(repo, file, f)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The header's geometry.",
			Body:        HFPeekDTO{},
		},
		// The 400 repoErrors() already documents covers this route's second
		// cause too — a missing `?file=` — so the description names both rather
		// than the status being listed twice.
		Errors: repoErrors(),
	}
}

// getTokenRoute, putTokenRoute and deleteTokenRoute build one credential's
// triple. They are shared because the two triples differ in exactly three
// things — the path, the provider's name and the validation endpoint — and two
// hand-written copies would be two places for the "never return the token" rule
// to be got right.
func (a *API) getTokenRoute(path, short, provider, op string) Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/" + path + "/token",
		Auth:        AuthSession,
		OperationID: op,
		Summary: "Whether a " + provider + " token is stored, its masked hint, and what the " +
			"last validation said. Never the token.",
		Tag: "hf",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.token(path)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			st, err := svc.Status(r.Context())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, tokenStatusDTO(st)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The " + short + " token's status.",
			Body:        TokenStatusDTO{},
		},
	}
}

func (a *API) putTokenRoute(path, short, provider, endpoint, op string,
	invalid model.ErrorCode) Route {

	return Route{
		Method:      http.MethodPut,
		Pattern:     BasePath + "/" + path + "/token",
		Auth:        AuthSession,
		OperationID: op,
		Summary: "Validate a " + provider + " token against " + endpoint + " and store it " +
			"sealed only if it is accepted.",
		Tag:         "hf",
		RequestBody: PutTokenRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.token(path)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body PutTokenRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			st, err := svc.Validate(r.Context(), body.Token)
			switch {
			case errors.Is(err, ErrTokenInvalid):
				// The provider REFUSED it. A transport failure lands in the
				// branch below instead, because telling a user their working
				// token is wrong makes them delete it.
				WriteError(w, r, a.log, Errorf(http.StatusUnprocessableEntity, invalid,
					"%s did not accept this token", provider))
				return
			case err != nil:
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, tokenStatusDTO(st)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The token was accepted and stored sealed.",
			Body:        TokenStatusDTO{},
		},
		Errors: []Response{{
			Status:      http.StatusUnprocessableEntity,
			Description: provider + " refused this token.",
			Codes:       []model.ErrorCode{invalid},
		}},
	}
}

func (a *API) deleteTokenRoute(path, short, provider, op string) Route {
	return Route{
		Method:      http.MethodDelete,
		Pattern:     BasePath + "/" + path + "/token",
		Auth:        AuthSession,
		OperationID: op,
		Summary: "Forget the stored " + provider + " token. The client that used it reverts " +
			"to anonymous.",
		Tag: "hf",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.token(path)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := svc.Delete(r.Context()); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			WriteNoContent(w)
		}),
		Success: Response{
			Status:      http.StatusNoContent,
			Description: "The " + short + " token is gone.",
		},
	}
}

// -----------------------------------------------------------------------------
// Plumbing
// -----------------------------------------------------------------------------

// repoParam reads and validates the `{repo...}` wildcard.
//
// The check happens HERE, before the value reaches the client, for one reason:
// the multi-segment wildcard matches any number of path segments, so
// `/api/v1/hf/model/a/b/c` arrives as a three-part "repo id". The client refuses
// it — a repo id is interpolated into a Hub URL and into a cache directory name,
// so a `..` segment would escape both — but it refuses it with an error this
// layer would otherwise classify as "the Hub could not be reached", which sends
// a user looking at their network instead of at their URL.
func repoParam(r *http.Request) (string, error) {
	repo := strings.TrimSuffix(r.PathValue("repo"), "/")
	if err := hf.ValidateRepo(repo); err != nil {
		return "", BadRequest("%q is not a repository id (expected `name` or `org/name`)", repo)
	}
	return repo, nil
}

// hub returns the Hub client, or the 503 a build without one answers with.
func (a *API) hub() (HFService, error) {
	if a.cfg.HF == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without a Hugging Face client")
	}
	return a.cfg.HF, nil
}

func (a *API) token(path string) (TokenService, error) {
	var svc TokenService
	switch path {
	case "hf":
		svc = a.cfg.HFToken
	case "github":
		svc = a.cfg.GitHubToken
	}
	if svc == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without a credential store")
	}
	return svc, nil
}

// localModels asks the catalog what this host already holds for a repository. A
// daemon with no catalog wired answers an empty map rather than failing: the
// remote endpoints are useful without the annotation, and an annotation that is
// absent is honest where one that is wrong is not.
func (a *API) localModels(ctx context.Context, repo string) (map[string]string, error) {
	if a.cfg.LocalModels == nil {
		return map[string]string{}, nil
	}
	out, err := a.cfg.LocalModels.LocalModels(ctx, repo)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]string{}
	}
	return out, nil
}

// writeHubError maps internal/hf's error vocabulary onto the statuses section
// 3.6 pairs it with, above all the `403 hf_gated` the UI links out from.
//
// It reuses the SAME classification and the SAME status table `POST /downloads`
// uses (internal/hf/download's hubError and downloadStatus). One vocabulary: a
// gated repository must not read as `hf_gated` on the download button and as
// something else on the search result the user clicked to get there.
func (a *API) writeHubError(w http.ResponseWriter, r *http.Request, err error) {
	mapped := download.ClassifyHubError(err)
	var me model.Error
	if errors.As(mapped, &me) {
		if status, ok := downloadStatus[me.Code]; ok {
			WriteError(w, r, a.log, &Error{
				Status: status, Code: me.Code, Message: me.Message,
				Details: me.Details, Err: err,
			})
			return
		}
	}
	WriteError(w, r, a.log, mapped)
}

// hubErrors are the responses every remote endpoint shares.
func hubErrors() []Response {
	return []Response{
		{
			Status: http.StatusForbidden,
			Description: "The repository is gated, or private and unreachable with this " +
				"host's credentials. A gated body carries `repo` and `request_url`; the UI " +
				"links out, because access grants are browser-only on the Hub's side.",
			Codes: []model.ErrorCode{download.CodeHFGated, download.CodeHFPrivate},
		},
		{
			Status:      http.StatusUnprocessableEntity,
			Description: "No such repository, revision or file.",
			Codes:       []model.ErrorCode{download.CodeFileNotInRepo},
		},
		{
			Status:      http.StatusBadGateway,
			Description: "The Hub could not be reached, or answered something unusable.",
			Codes:       []model.ErrorCode{download.CodeHFUnreachable},
		},
	}
}

// -----------------------------------------------------------------------------
// Projections
// -----------------------------------------------------------------------------

// tokenStatusDTO projects a credential's status onto the wire. There is no
// branch here that could ever reach the token: TokenStatus does not carry one.
func tokenStatusDTO(st TokenStatus) TokenStatusDTO {
	out := TokenStatusDTO{
		Present: st.Present, Hint: st.Hint, Valid: st.Valid,
		User: st.User, Scopes: st.Scopes, RateLimit: st.RateLimit,
	}
	if out.Scopes == nil {
		out.Scopes = []string{}
	}
	return out
}

func hfSearchDTO(r hf.SearchResult) HFSearchResultDTO {
	out := HFSearchResultDTO{
		ID: r.ID, Author: r.Author, Downloads: r.Downloads, Likes: r.Likes,
		Gated: r.Gated, Private: r.Private, UpdatedAt: r.UpdatedAt, Tags: r.Tags,
		GGUF: hfSummaryDTO(r.GGUF),
	}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	return out
}

func hfSummaryDTO(g *hf.GGUFSummary) *HFGGUFSummaryDTO {
	if g == nil {
		return nil
	}
	return &HFGGUFSummaryDTO{
		Architecture:  g.Architecture,
		ContextLength: g.ContextLength,
		Total:         g.Total,
	}
}

func hfModelDTO(m hf.ModelInfo, local map[string]string) HFModelDTO {
	out := HFModelDTO{
		ID: m.ID, Author: m.Author, SHA: m.SHA, Gated: m.Gated, Private: m.Private,
		Disabled: m.Disabled, Downloads: m.Downloads, Likes: m.Likes,
		LastModified: m.LastModified, Tags: m.Tags, GGUF: hfSummaryDTO(m.GGUF),
		LocalModelIDs: local,
	}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	if out.LocalModelIDs == nil {
		out.LocalModelIDs = map[string]string{}
	}
	return out
}

func hfTreeDTO(repo, revision string, entries []hf.TreeEntry,
	local map[string]string) HFTreeDTO {

	out := HFTreeDTO{
		RepoID: repo, Revision: revision,
		Groups: []HFTreeGroupDTO{}, Mmproj: []HFTreeGroupDTO{},
	}
	for _, g := range hf.GroupTree(entries) {
		dto := HFTreeGroupDTO{
			Key: g.Key, QuantLabel: g.QuantLabel, TotalBytes: g.TotalBytes,
			ShardTotal: g.ShardTotal, Complete: g.Complete, Mmproj: g.Mmproj,
			Files: make([]HFTreeEntryDTO, 0, len(g.Files)),
		}
		for _, f := range g.Files {
			dto.Files = append(dto.Files, HFTreeEntryDTO{
				Path: f.Path, SizeBytes: f.Size, OID: f.OID, LFS: f.LFS,
			})
		}
		// The annotation is keyed by the group's PRIMARY file, which is shard 1
		// of a set and the file itself otherwise — the same key
		// `models.primary_file` holds (section 2.6).
		if len(g.Files) > 0 {
			if id, ok := local[g.Files[0].Path]; ok {
				dto.LocalModelID = &id
			}
		}
		if g.Mmproj {
			out.Mmproj = append(out.Mmproj, dto)
			continue
		}
		out.Groups = append(out.Groups, dto)
	}
	return out
}

func hfPeekDTO(repo, file string, f *gguf.File) HFPeekDTO {
	sh := f.Shape()
	out := HFPeekDTO{
		RepoID: repo, File: file,
		Arch:      sh.Architecture,
		NLayer:    sh.BlockCount,
		NEmbd:     sh.EmbeddingLength,
		NHead:     sh.HeadCount,
		NCtxTrain: sh.ContextLength,
		NVocab:    sh.VocabSize,
		NHeadKV:   sh.HeadCountKV,
		HeadDimK:  sh.KeyLength,
		HeadDimV:  sh.ValueLength,

		SWAWindow:      sh.SlidingWindow,
		SWAPattern:     sh.SlidingWindowPattern,
		NExpert:        sh.ExpertCount,
		NExpertUsed:    sh.ExpertUsedCount,
		TokenizerModel: sh.TokenizerModel,
		Quantization:   sh.Quantization,
		TensorSummary:  f.Sizes(),
		Notes:          sh.Notes,
		SizeBytes:      f.FileSize,
	}
	if out.NHeadKV == nil {
		out.NHeadKV = []int{}
	}
	if out.Notes == nil {
		out.Notes = []string{}
	}
	return out
}
