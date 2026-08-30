package secrets

import (
	"encoding/json"

	"github.com/jlbyh2o/llamaman/internal/store"
)

// `secrets.scope_json`: what a validation call learned about the token, beyond
// yes or no.
//
// It is one small JSON object rather than two columns because the two providers
// report different things — Hugging Face's `/api/whoami-v2` names an account and
// a set of auth scopes, GitHub's `x-oauth-scopes` header names a comma-separated
// list — and a schema that tried to hold both in typed columns would carry a
// NULL for whichever provider a row is not.

// scopeRecord is the stored shape of a Verdict's non-boolean half.
type scopeRecord struct {
	User   string   `json:"user,omitempty"`
	Scopes []string `json:"scopes,omitempty"`
}

// scopeJSON renders a Verdict's scope record, or nil when it has nothing to say
// — a NULL column rather than `{}`, so "we know nothing about this token's
// scopes" is distinguishable from "it has none".
func (v Verdict) scopeJSON() *string {
	if v.User == "" && len(v.Scopes) == 0 {
		return nil
	}
	b, err := json.Marshal(scopeRecord{User: v.User, Scopes: v.Scopes})
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

// infoOf projects a stored row onto the shape the API and the settings screen
// read. The sealed bytes are not carried into it at all: a struct that held them
// is a struct someone eventually marshals.
func infoOf(sec store.Secret) Info {
	out := Info{
		Name:       sec.Name,
		Present:    true,
		Valid:      sec.Valid,
		CreatedAt:  sec.CreatedAt,
		UpdatedAt:  sec.UpdatedAt,
		LastUsedAt: sec.LastUsedAt,
	}
	if sec.Hint != nil {
		out.Hint = *sec.Hint
	}
	if sec.ScopeJSON != nil {
		var rec scopeRecord
		// A record that will not decode is dropped rather than reported: it is
		// display metadata, and losing it must not turn "you have a token" into
		// an error page.
		if err := json.Unmarshal([]byte(*sec.ScopeJSON), &rec); err == nil {
			out.User, out.Scopes = rec.User, rec.Scopes
		}
	}
	return out
}
