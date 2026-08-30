package model

// Tokens and accounting (DESIGN section 2.9).

// TokenScope is `api_tokens.scope` (§2.9): `global` reaches every instance,
// `instances` only the ones named in `token_instances`.
type TokenScope string

const (
	ScopeGlobal    TokenScope = "global"
	ScopeInstances TokenScope = "instances"
)

// TokenScopeValues lists the members of the `api_tokens.scope` CHECK constraint,
// in order.
func TokenScopeValues() []TokenScope { return []TokenScope{ScopeGlobal, ScopeInstances} }

// Valid reports whether s is a member of the CHECK constraint.
func (s TokenScope) Valid() bool { return valid(s, TokenScopeValues()) }

// TokenState is `api_tokens.state` (§2.9).
//
// Transitions: active ⇄ disabled; active|disabled → revoked, which is TERMINAL —
// and the hash is retained on a revoked row precisely so a leaked secret can
// never be re-minted into validity. Every write bumps an in-memory epoch that
// invalidates the gateway's verified-token cache within one request.
type TokenState string

const (
	TokenActive   TokenState = "active"
	TokenDisabled TokenState = "disabled"
	TokenRevoked  TokenState = "revoked"
)

// TokenStateValues lists the members of the `api_tokens.state` CHECK constraint,
// in order.
func TokenStateValues() []TokenState { return []TokenState{TokenActive, TokenDisabled, TokenRevoked} }

// Valid reports whether s is a member of the CHECK constraint.
func (s TokenState) Valid() bool { return valid(s, TokenStateValues()) }

// DenialReason is `gateway_denials_daily.reason` (§2.9): why the gateway turned
// a request away. Documented by a comment rather than closed by a CHECK, so it
// is absent from ClosedEnums.
type DenialReason string

const (
	DeniedMissing     DenialReason = "missing"
	DeniedUnknown     DenialReason = "unknown"
	DeniedDisabled    DenialReason = "disabled"
	DeniedRevoked     DenialReason = "revoked"
	DeniedExpired     DenialReason = "expired"
	DeniedScope       DenialReason = "scope"
	DeniedRateLimited DenialReason = "rate_limited"
)

// DenialReasonValues lists the reasons the design names, in the order of the
// column's comment.
func DenialReasonValues() []DenialReason {
	return []DenialReason{
		DeniedMissing, DeniedUnknown, DeniedDisabled, DeniedRevoked,
		DeniedExpired, DeniedScope, DeniedRateLimited,
	}
}

// Valid reports whether r is one of the reasons the design names.
func (r DenialReason) Valid() bool { return valid(r, DenialReasonValues()) }
