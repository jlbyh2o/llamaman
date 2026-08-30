package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/secrets"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/tokens"
)

// The API-token endpoints of DESIGN section 3.12, and the denial counters that
// go with them.
//
// The one rule that shapes every DTO in this file: `POST /api/v1/tokens` is the
// ONLY response in this whole API that ever contains a secret. Nothing else can
// — the database holds a sha256 and a nine-character prefix, and neither is
// reversible — but stating it here is what stops a well-meaning future change
// from adding a `secret` field to the listing DTO and finding it always empty.

// APITokenService is everything this layer needs from internal/tokens. The
// consumer owns the interface (DESIGN section 1); *tokens.Service satisfies it.
type APITokenService interface {
	List(ctx context.Context) ([]tokens.Token, error)
	Get(ctx context.Context, id string) (tokens.Token, error)
	Mint(ctx context.Context, p tokens.MintParams) (tokens.Minted, error)
	Patch(ctx context.Context, id string, p tokens.PatchParams) (tokens.Token, error)
	Revoke(ctx context.Context, id string) error
	Usage(ctx context.Context, id string, rng store.UsageRange) ([]store.TokenUsageRow, error)
}

// GatewayService is what `GET /api/v1/gateway/denials` needs. *gateway.Gateway
// satisfies it.
type GatewayService interface {
	Denials(ctx context.Context, instanceID string, rng store.UsageRange) ([]store.DenialRow, error)
}

// APITokenDTO is one row of `GET /api/v1/tokens`. It never carries a secret.
type APITokenDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Prefix is `lm_` plus the first six characters, stored in the clear for
	// display and log correlation (section 2.9).
	Prefix string `json:"prefix"`
	// Hint is the masked display form. For a stored row it can only be built
	// from the prefix — the rest of the secret does not exist anywhere — so it
	// is `lm_abc123…` rather than internal/secrets' head-and-tail mask, which
	// needs the whole value and is used on the mint response alone.
	Hint  string `json:"hint"`
	Scope string `json:"scope"`
	State string `json:"state"`
	// InstanceIDs is empty for a `global` token, which reaches everything.
	InstanceIDs  []string `json:"instance_ids"`
	RateLimitRPM *int64   `json:"rate_limit_rpm"`
	ExpiresAt    *string  `json:"expires_at"`

	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	RevokedAt *string `json:"revoked_at"`

	LastUsedAt   *string `json:"last_used_at"`
	LastUsedIP   *string `json:"last_used_ip"`
	RequestCount int64   `json:"request_count"`
}

// CreateAPITokenRequest is the body of `POST /api/v1/tokens`.
type CreateAPITokenRequest struct {
	Name string `json:"name"`
	// Scope is `global` (the default) or `instances`, in which case
	// `instance_ids` must name at least one.
	Scope        *string  `json:"scope,omitempty"`
	InstanceIDs  []string `json:"instance_ids,omitempty"`
	RateLimitRPM *int64   `json:"rate_limit_rpm,omitempty"`
	// ExpiresAt is an RFC 3339 instant. Omitted means the token never expires.
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// CreateAPITokenResponse is the 201 of `POST /api/v1/tokens` — the only
// response in this API that carries a secret (section 3.12).
type CreateAPITokenResponse struct {
	Token APITokenDTO `json:"token"`
	// Secret is shown ONCE. It is not stored in any form anyone can reverse, so
	// a client that loses it must mint a new token.
	Secret string `json:"secret"`
	// Hint is the masked form of that secret, so the UI can go on showing
	// something recognizable after the value is cleared from the screen.
	Hint string `json:"hint"`
}

// PatchAPITokenRequest is the body of `PATCH /api/v1/tokens/{id}`.
//
// An omitted field is left alone. `instance_ids` is the exception that carries
// the scope: a non-empty list makes the token `instances`-scoped, and an
// explicitly EMPTY list makes it `global`. There is deliberately no separate
// `scope` field, because a body could then contradict itself.
type PatchAPITokenRequest struct {
	Name  *string `json:"name,omitempty"`
	State *string `json:"state,omitempty"`
	// InstanceIDs distinguishes absent (leave the scope alone) from `[]` (make
	// it global) by nil-ness, which is what encoding/json already gives.
	InstanceIDs []string `json:"instance_ids,omitempty"`
	// RateLimitRPM of 0 removes the limit.
	RateLimitRPM *int64 `json:"rate_limit_rpm,omitempty"`
	// ExpiresAt is an RFC 3339 instant; the empty string removes the expiry.
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// TokenUsageDTO is one row of `GET /api/v1/tokens/{id}/usage`.
//
// The two token counts stay nullable all the way to the wire. NULL means the
// upstream did not report them (section 9.3's tail tap abstains rather than
// guesses), and the UI says "not reported" rather than showing a zero that reads
// as an answer.
type TokenUsageDTO struct {
	TokenID          string `json:"token_id"`
	InstanceID       string `json:"instance_id"`
	Day              string `json:"day"`
	Requests         int64  `json:"requests"`
	Errors           int64  `json:"errors"`
	BytesIn          int64  `json:"bytes_in"`
	BytesOut         int64  `json:"bytes_out"`
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	DurationMS       int64  `json:"duration_ms"`
}

// TokenDetailDTO is `GET /api/v1/tokens/{id}`: the row plus the per-instance
// usage summary section 3.12 asks for.
type TokenDetailDTO struct {
	Token APITokenDTO `json:"token"`
	// Usage is this token's rows across every day on record, which the UI folds
	// by instance. It is the same shape the usage endpoint returns, so a client
	// needs one renderer rather than two.
	Usage []TokenUsageDTO `json:"usage"`
}

// GatewayDenialDTO is one row of `GET /api/v1/gateway/denials`.
type GatewayDenialDTO struct {
	InstanceID string `json:"instance_id"`
	Day        string `json:"day"`
	// Reason is one of missing, unknown, disabled, revoked, expired, scope,
	// rate_limited (section 2.9).
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

func (a *API) apiTokenRoutes() []Route {
	return []Route{
		a.listAPITokensRoute(),
		a.createAPITokenRoute(),
		a.getAPITokenRoute(),
		a.patchAPITokenRoute(),
		a.deleteAPITokenRoute(),
		a.getAPITokenUsageRoute(),
		a.gatewayDenialsRoute(),
	}
}

func (a *API) listAPITokensRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/tokens",
		Auth:        AuthSession,
		OperationID: "listAPITokens",
		Summary: "Every API token: prefix, scope, state, counts and last use. " +
			"Never a secret — none is stored.",
		Tag: "tokens",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.apiTokens()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			list, err := svc.List(r.Context())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			items := make([]APITokenDTO, 0, len(list))
			for _, t := range list {
				items = append(items, apiTokenDTO(t))
			}
			if err := WriteJSON(w, http.StatusOK, NewList(items, len(items), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "Every token this host has minted, revoked ones included.",
			Body:        List[APITokenDTO]{},
		},
	}
}

func (a *API) createAPITokenRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/tokens",
		Auth:        AuthSession,
		OperationID: "createAPIToken",
		Summary: "Mint a token. The 201 is the only response in this API that ever " +
			"contains the secret; it is stored as a sha256 and cannot be shown again.",
		Tag:         "tokens",
		RequestBody: CreateAPITokenRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.apiTokens()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body CreateAPITokenRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			expires, err := parseWireTime(body.ExpiresAt)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}

			scope := model.ScopeGlobal
			if body.Scope != nil {
				scope = model.TokenScope(*body.Scope)
			} else if len(body.InstanceIDs) > 0 {
				// A body that named instances but no scope meant `instances`;
				// minting a global token from it would silently grant more than
				// was asked for, which is the one mistake this endpoint must
				// never make.
				scope = model.ScopeInstances
			}

			minted, err := svc.Mint(r.Context(), tokens.MintParams{
				Name:         body.Name,
				Scope:        scope,
				InstanceIDs:  body.InstanceIDs,
				RateLimitRPM: body.RateLimitRPM,
				ExpiresAt:    expires,
			})
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusCreated, CreateAPITokenResponse{
				Token:  apiTokenDTO(minted.Token),
				Secret: minted.Secret,
				// internal/secrets owns the masking rule for a value we hold, and
				// this is the one moment the gateway's tokens are in that
				// position: head, ellipsis, tail.
				Hint: secrets.Hint(minted.Secret),
			}); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusCreated,
			Description: "The token, with its secret. Shown once.",
			Body:        CreateAPITokenResponse{},
		},
		Errors: []Response{{
			Status: http.StatusUnprocessableEntity,
			Description: "The token was refused: no name, a scope that is not one of " +
				"global/instances, an `instances` scope naming no instance, or a negative " +
				"rate limit.",
			Codes: []model.ErrorCode{
				tokens.CodeTokenNameRequired,
				tokens.CodeTokenScopeInvalid,
				tokens.CodeTokenRateLimitInvalid,
				CodeBadRequest,
			},
		}},
	}
}

func (a *API) getAPITokenRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/tokens/{id}",
		Auth:        AuthSession,
		OperationID: "getAPIToken",
		Summary:     "One token, with its per-instance usage summary.",
		Tag:         "tokens",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.apiTokens()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			id := r.PathValue("id")
			tok, err := svc.Get(r.Context(), id)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			rows, err := svc.Usage(r.Context(), id, store.UsageRange{})
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, TokenDetailDTO{
				Token: apiTokenDTO(tok),
				Usage: tokenUsageDTOs(rows),
			}); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The token and its usage.",
			Body:        TokenDetailDTO{},
		},
		Errors: []Response{{
			Status:      http.StatusNotFound,
			Description: "No token has this id.",
			Codes:       []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) patchAPITokenRoute() Route {
	return Route{
		Method:      http.MethodPatch,
		Pattern:     BasePath + "/tokens/{id}",
		Auth:        AuthSession,
		OperationID: "patchAPIToken",
		Summary: "Rename, disable, re-enable, rescope or rate-limit a token. Revoked is " +
			"terminal and cannot be edited.",
		Tag:         "tokens",
		RequestBody: PatchAPITokenRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.apiTokens()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body PatchAPITokenRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}

			p := tokens.PatchParams{
				Name:  body.Name,
				State: enumPtr[model.TokenState](body.State),
			}
			if body.InstanceIDs != nil {
				ids := body.InstanceIDs
				p.InstanceIDs = &ids
			}
			if body.RateLimitRPM != nil {
				v := body.RateLimitRPM
				p.RateLimitRPM = &v
			}
			if body.ExpiresAt != nil {
				expires, err := parseWireTime(body.ExpiresAt)
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				p.ExpiresAt = &expires
			}

			tok, err := svc.Patch(r.Context(), r.PathValue("id"), p)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, apiTokenDTO(tok)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The updated token.",
			Body:        APITokenDTO{},
		},
		Errors: []Response{
			{
				Status:      http.StatusNotFound,
				Description: "No token has this id.",
				Codes:       []model.ErrorCode{CodeNotFound},
			},
			{
				Status: http.StatusConflict,
				Description: "The token is revoked, which is terminal — the hash is retained " +
					"so a leaked secret can never be re-minted into validity.",
				Codes: []model.ErrorCode{tokens.CodeTokenRevoked},
			},
			{
				Status:      http.StatusUnprocessableEntity,
				Description: "The edit was refused; see createAPIToken for the codes.",
				Codes: []model.ErrorCode{
					tokens.CodeTokenNameRequired,
					tokens.CodeTokenStateInvalid,
					tokens.CodeTokenRateLimitInvalid,
					CodeBadRequest,
				},
			},
		},
	}
}

func (a *API) deleteAPITokenRoute() Route {
	return Route{
		Method:      http.MethodDelete,
		Pattern:     BasePath + "/tokens/{id}",
		Auth:        AuthSession,
		OperationID: "deleteAPIToken",
		Summary: "Revoke. Soft and terminal: the row and its hash are retained so the " +
			"secret can never become valid again.",
		Tag: "tokens",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.apiTokens()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := svc.Revoke(r.Context(), r.PathValue("id")); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			WriteNoContent(w)
		}),
		Success: Response{
			Status:      http.StatusNoContent,
			Description: "The token is revoked. Revoking one twice is a no-op, not an error.",
		},
		Errors: []Response{{
			Status:      http.StatusNotFound,
			Description: "No token has this id.",
			Codes:       []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) getAPITokenUsageRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/tokens/{id}/usage",
		Auth:        AuthSession,
		OperationID: "getAPITokenUsage",
		Summary: "This token's daily requests, errors, bytes and reported token counts, " +
			"per instance.",
		Tag: "tokens",
		Query: []QueryParam{
			{Name: "from", Description: "Inclusive first day, YYYY-MM-DD UTC."},
			{Name: "to", Description: "Inclusive last day, YYYY-MM-DD UTC."},
			{
				Name: "group",
				Description: "How the client intends to fold the rows. The server returns the " +
					"same per-day, per-instance rows either way; this is a hint the UI echoes.",
				Enum: []string{"day", "instance"},
			},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.apiTokens()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			rows, err := svc.Usage(r.Context(), r.PathValue("id"), usageRange(r))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			items := tokenUsageDTOs(rows)
			if err := WriteJSON(w, http.StatusOK, NewList(items, len(items), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "One row per day per instance.",
			Body:        List[TokenUsageDTO]{},
		},
		Errors: []Response{{
			Status:      http.StatusNotFound,
			Description: "No token has this id.",
			Codes:       []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) gatewayDenialsRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/gateway/denials",
		Auth:        AuthSession,
		OperationID: "listGatewayDenials",
		Summary:     "Denial counters per instance and reason (section 2.9).",
		Tag:         "tokens",
		Query: []QueryParam{
			{Name: "instance_id", Description: "Restrict to one instance."},
			{Name: "from", Description: "Inclusive first day, YYYY-MM-DD UTC."},
			{Name: "to", Description: "Inclusive last day, YYYY-MM-DD UTC."},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.gateway()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			rows, err := svc.Denials(r.Context(),
				strings.TrimSpace(r.URL.Query().Get("instance_id")), usageRange(r))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			items := make([]GatewayDenialDTO, 0, len(rows))
			for _, row := range rows {
				items = append(items, GatewayDenialDTO{
					InstanceID: row.InstanceID,
					Day:        row.Day,
					Reason:     string(row.Reason),
					Count:      row.Count,
				})
			}
			if err := WriteJSON(w, http.StatusOK, NewList(items, len(items), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "One row per instance, day and reason.",
			Body:        List[GatewayDenialDTO]{},
		},
	}
}

func (a *API) apiTokens() (APITokenService, error) {
	if a.cfg.APITokens == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without a token service")
	}
	return a.cfg.APITokens, nil
}

func (a *API) gateway() (GatewayService, error) {
	if a.cfg.Gateway == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without an inference gateway")
	}
	return a.cfg.Gateway, nil
}

func apiTokenDTO(t tokens.Token) APITokenDTO {
	ids := t.InstanceIDs
	if ids == nil {
		ids = []string{}
	}
	return APITokenDTO{
		ID:           t.ID,
		Name:         t.Name,
		Prefix:       t.Prefix,
		Hint:         t.Prefix + "…",
		Scope:        string(t.Scope),
		State:        string(t.State),
		InstanceIDs:  ids,
		RateLimitRPM: t.RateLimitRPM,
		ExpiresAt:    TimePtr(t.ExpiresAt),
		CreatedAt:    Time(t.CreatedAt),
		UpdatedAt:    Time(t.UpdatedAt),
		RevokedAt:    TimePtr(t.RevokedAt),
		LastUsedAt:   TimePtr(t.LastUsedAt),
		LastUsedIP:   t.LastUsedIP,
		RequestCount: t.RequestCount,
	}
}

func tokenUsageDTOs(rows []store.TokenUsageRow) []TokenUsageDTO {
	out := make([]TokenUsageDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, TokenUsageDTO{
			TokenID:          row.TokenID,
			InstanceID:       row.InstanceID,
			Day:              row.Day,
			Requests:         row.Requests,
			Errors:           row.Errors,
			BytesIn:          row.BytesIn,
			BytesOut:         row.BytesOut,
			PromptTokens:     row.PromptTokens,
			CompletionTokens: row.CompletionTokens,
			DurationMS:       row.DurationMS,
		})
	}
	return out
}

// usageRange reads the `?from=`/`?to=` day bounds both usage endpoints take.
// They are already the storage form — 'YYYY-MM-DD' UTC — so there is nothing to
// convert, and an unparseable one is simply not applied rather than refused: a
// dashboard that showed nothing because a bookmark carried `from=last-week`
// would be worse than one that showed everything.
func usageRange(r *http.Request) store.UsageRange {
	q := r.URL.Query()
	return store.UsageRange{
		From: usageDay(q.Get("from")),
		To:   usageDay(q.Get("to")),
	}
}

func usageDay(v string) string {
	v = strings.TrimSpace(v)
	if _, err := time.Parse(time.DateOnly, v); err != nil {
		return ""
	}
	return v
}

// parseWireTime turns an optional RFC 3339 instant into the Unix milliseconds
// the storage form uses. The empty string is an explicit "no instant", which is
// how a PATCH removes an expiry.
func parseWireTime(v *string) (*int64, error) {
	if v == nil || strings.TrimSpace(*v) == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(*v))
	if err != nil {
		return nil, BadRequest("expires_at must be an RFC 3339 instant")
	}
	ms := t.UTC().UnixMilli()
	return &ms, nil
}
