package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/api"
	"github.com/jlbyh2o/llamaman/internal/buildinfo"
	"github.com/jlbyh2o/llamaman/internal/hf"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/github"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/secrets"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Step 5's secrets half and the Hugging Face client that reads it (DESIGN
// sections 2.2, 3.6, 6.2, 11.1).
//
// The two credentials reach their clients as a FUNCTION rather than as a string
// captured at construction, and that is the whole reason this file exists. A
// client built at boot with a snapshot of the token would keep sending one the
// user deleted through `DELETE /api/v1/hf/token` until the next restart —
// which, on a token that was revoked upstream because it leaked, is exactly the
// wrong behavior. `secrets.Service.TokenFunc` reads the sealed row per request
// instead.

// UserAgent is what both clients send. It is the string section 7.1 pins.
func UserAgent() string {
	return "llamaman/" + buildinfo.Version + " (+https://github.com/jlbyh2o/llamaman)"
}

// buildSecrets opens (or creates) `<state_dir>/secret.key` and builds the sealed
// credential store over it — section 11.1 step 5.
//
// A key file whose mode has been widened is a hard failure rather than a repair:
// the key has been readable by something else for as long as it has been that
// way, and chmod-ing it quietly would hide that rather than fix its consequence.
func (d *daemon) buildSecrets() error {
	key, err := secrets.LoadOrCreateKey(d.stateDir)
	if err != nil {
		return fmt.Errorf("open the secret key: %w", err)
	}
	svc, err := secrets.New(secrets.Config{Store: d.store, Key: key, Now: d.opts.Now})
	if err != nil {
		return fmt.Errorf("build the secrets service: %w", err)
	}
	d.secrets = svc
	return nil
}

// buildHub constructs the Hugging Face client with the stored token behind it.
func (d *daemon) buildHub() {
	var token func(context.Context) (string, error)
	if d.secrets != nil {
		token = d.secrets.TokenFunc(model.SecretHFToken)
	}
	d.hfClient = hf.New(hf.Options{
		Endpoint:  d.hubEndpoint(),
		Token:     token,
		UserAgent: UserAgent(),
		Logger:    d.log,
	})
}

// hubEndpoint reads `settings['hf.endpoint']`, falling back to the default. It
// is read once at construction because the client holds it; a change is one of
// the settings the UI pairs with a restart.
func (d *daemon) hubEndpoint() string {
	if d.settings == nil {
		return ""
	}
	v, err := d.settings.GetString(context.Background(), "hf.endpoint")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

// -----------------------------------------------------------------------------
// The two credential triples (section 3.6)
// -----------------------------------------------------------------------------

// hfTokenService is `GET`/`PUT`/`DELETE /api/v1/hf/token`.
//
// PUT validates against `/api/whoami-v2` BEFORE sealing, which is section 3.6's
// rule and not a nicety: a token stored without validation is a token whose
// first failure happens inside a download an hour later, where the user cannot
// tell a bad credential from a bad network.
type hfTokenService struct {
	secrets  *secrets.Service
	endpoint string
	log      func() string // the user agent, deferred so buildinfo is read once
}

func (s hfTokenService) Status(ctx context.Context) (api.TokenStatus, error) {
	info, err := s.secrets.Info(ctx, model.SecretHFToken)
	if err != nil {
		return api.TokenStatus{}, err
	}
	return api.TokenStatus{
		Present: info.Present, Hint: info.Hint, Valid: info.Valid,
		User: info.User, Scopes: info.Scopes,
	}, nil
}

func (s hfTokenService) Validate(ctx context.Context, token string) (api.TokenStatus, error) {
	info, err := hf.ValidateToken(ctx, hf.Options{
		Endpoint: s.endpoint, UserAgent: s.log(),
	}, token)
	switch {
	case errors.Is(err, hf.ErrTokenInvalid):
		// The Hub REFUSED it. Anything else — a timeout, a 500, DNS — falls to
		// the branch below and is reported as itself.
		return api.TokenStatus{}, api.ErrTokenInvalid
	case err != nil:
		return api.TokenStatus{}, err
	}

	if err := s.secrets.Put(ctx, model.SecretHFToken, token, secrets.Verdict{
		Valid: true, User: info.Name, Scopes: info.Scopes,
	}); err != nil {
		return api.TokenStatus{}, err
	}
	return s.Status(ctx)
}

func (s hfTokenService) Delete(ctx context.Context) error {
	return s.secrets.Delete(ctx, model.SecretHFToken)
}

// githubTokenService is `GET`/`PUT`/`DELETE /api/v1/github/token` (section 6.2).
//
// Its status carries the api.github.com rate-limit headroom beside the token,
// which is section 6.2's on-screen answer to "why is the nightly list stale":
// anonymous is 60 requests an hour per IP, a token is 5000.
type githubTokenService struct {
	secrets *secrets.Service
	client  *github.Client
	agent   func() string
}

func (s githubTokenService) Status(ctx context.Context) (api.TokenStatus, error) {
	info, err := s.secrets.Info(ctx, model.SecretGitHubToken)
	if err != nil {
		return api.TokenStatus{}, err
	}
	out := api.TokenStatus{
		Present: info.Present, Hint: info.Hint, Valid: info.Valid,
		User: info.User, Scopes: info.Scopes,
	}
	if s.client != nil {
		rl := s.client.RateLimit()
		dto := api.RateLimitDTO{
			Remaining: rl.Remaining, Limit: rl.Limit,
			Authenticated: rl.Authenticated, Known: rl.Known,
		}
		if !rl.ResetAt.IsZero() {
			dto.ResetAt = api.Time(rl.ResetAt.UnixMilli())
		}
		out.RateLimit = &dto
	}
	return out, nil
}

func (s githubTokenService) Validate(ctx context.Context, token string) (api.TokenStatus, error) {
	info, err := github.ValidateToken(ctx, github.Options{UserAgent: s.agent()}, token)
	switch {
	case errors.Is(err, github.ErrTokenInvalid):
		return api.TokenStatus{}, api.ErrTokenInvalid
	case err != nil:
		return api.TokenStatus{}, err
	}

	if err := s.secrets.Put(ctx, model.SecretGitHubToken, token, secrets.Verdict{
		Valid: true, User: info.Login, Scopes: info.Scopes,
	}); err != nil {
		return api.TokenStatus{}, err
	}
	return s.Status(ctx)
}

func (s githubTokenService) Delete(ctx context.Context) error {
	// The release client reverts to anonymous by itself: it reads the token
	// through a function on every request, so there is nothing here to reset.
	return s.secrets.Delete(ctx, model.SecretGitHubToken)
}

// -----------------------------------------------------------------------------
// The local-availability annotation
// -----------------------------------------------------------------------------

// localIndex answers section 3.6's "does this host already have this
// repository", keyed by primary file so a tree can annotate each quantization
// individually rather than the repository as a whole.
type localIndex struct{ st *store.Store }

func (l localIndex) LocalModels(ctx context.Context, repoID string) (map[string]string, error) {
	out := map[string]string{}
	if repoID == "" {
		return out, nil
	}
	err := l.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		// `?q=` is a substring match over the repo id and the primary file, so
		// the exact-match filter happens below. The catalog is hundreds of rows,
		// not millions.
		rows, err := l.st.LocalModels(ctx, tx, store.ModelFilter{Query: repoID})
		if err != nil {
			return err
		}
		for _, m := range rows {
			if !strings.EqualFold(m.RepoID, repoID) || m.PrimaryFile == "" {
				continue
			}
			out[m.PrimaryFile] = m.ID
		}
		return nil
	})
	return out, err
}
