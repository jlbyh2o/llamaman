package tokens

import "github.com/jlbyh2o/llamaman/internal/model"

// The refusals this package can answer with.
//
// They are declared here rather than in internal/model for the reason
// internal/api/errors.go states about internal/sse's `invalid_topic`: a code is
// declared beside the code path that returns it, and DESIGN section 3's catalog
// grows as the endpoints that answer with it arrive. internal/model closes the
// codes that a COLUMN constrains; these constrain a request body.

const (
	// CodeTokenNameRequired is the 422 for a mint or a rename with no name. A
	// token's name is the only thing that will identify it once the secret is
	// gone, which is one HTTP response later.
	CodeTokenNameRequired model.ErrorCode = "token_name_required"
	// CodeTokenScopeInvalid is the 422 for a scope that is not one of
	// `global`/`instances`, and for `instances` with an empty list — a scope
	// that reaches nothing is a token that cannot be used, and silently minting
	// one would be a support ticket rather than a feature.
	CodeTokenScopeInvalid model.ErrorCode = "token_scope_invalid"
	// CodeTokenStateInvalid is the 422 for a `state` outside §2.9's three.
	CodeTokenStateInvalid model.ErrorCode = "token_state_invalid"
	// CodeTokenRevoked is the 409 for editing a revoked token. `revoked` is
	// TERMINAL (§2.9) and the hash is retained precisely so a leaked secret can
	// never be re-minted into validity, so an admin who believes they just
	// re-enabled one must be told they did not.
	CodeTokenRevoked model.ErrorCode = "token_revoked"
	// CodeTokenRateLimitInvalid is the 422 for a negative `rate_limit_rpm`.
	CodeTokenRateLimitInvalid model.ErrorCode = "token_rate_limit_invalid"
)
