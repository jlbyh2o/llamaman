package source

import (
	"fmt"
	"net/url"
	"strings"
)

// The git remote a custom build clones from is the one string in this pipeline
// that a user types and a subprocess executes, so it is checked here before it
// reaches `git` and redacted here before it reaches a log, a manifest, a row or
// the API.
//
// # Why an allowlist rather than a blocklist
//
// `git clone <url>` does not merely open a URL. Its transport layer resolves the
// scheme against a table that includes `ext::`, whose "URL" is a SHELL COMMAND
// git runs as the calling identity — `ext::sh -c 'id > /tmp/pwn'` is a remote
// git will happily "fetch" from. `--upload-pack=` smuggled in as a leading `-`
// is the same class of hole through argv rather than through the scheme table.
// Neither is reachable by enumerating bad strings, because the transport table
// is git's and grows; only naming the four schemes this product actually
// supports is a rule that stays true.
//
// DESIGN section 6.2 puts this check at the point where a request becomes a row
// ("`git ls-remote` validates it before the row leaves `resolving`"), so
// ValidateGitURL is called by internal/llamacpp when it resolves an install
// request — a rejection is a `422` on `POST /api/v1/llamacpp/versions` rather
// than a build that fails four minutes in. Request.Validate calls the exec-safety
// half again on the way into the builder, because a package that hands strings
// to `exec` cannot depend on its callers for that.
//
// # Why credentials are refused rather than carried
//
// A URL of the form `https://user:ghp_x@host/repo.git` would be written into
// `logs/build/<id>.log`, `versions/<id>/build.log`, `manifest.json`,
// `llamacpp_versions.git_url` and the `git_url` field of
// `GET /api/v1/llamacpp/versions/{id}` — four durable places and one API
// response. DESIGN sections 2.2 and 7.1 say a credential is "never logged, never
// returned by the API", and the only way to keep that true of a string the user
// supplies is to refuse it at the door. RedactGitURL is the second half of the
// same rule: it strips userinfo from anything on its way to a log, a manifest or
// the wire, so a row written before this check existed — or one hand-edited into
// the database — cannot leak either.

// GitURLSchemes are the transports a llama.cpp source build may be fetched over.
//
// `file` and `ext` are deliberately absent. `ext` executes a shell command;
// `file` clones a repository off the local disk as the service identity, which
// on the `--dedicated-user` topology reads directories the admin never intended
// to publish into a build log.
var GitURLSchemes = []string{"https", "http", "ssh", "git", "git+ssh"}

// ValidateGitURL is the full check: an allowlisted scheme, no embedded
// credentials, no argv injection, and a host to connect to.
func ValidateGitURL(raw string) error {
	if err := validateGitURLSafety(raw); err != nil {
		return err
	}
	trimmed := strings.TrimSpace(raw)

	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("source: git url %q could not be parsed: %w", RedactGitURL(trimmed), err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		return fmt.Errorf("source: git url %q has no scheme; it must be one of %s",
			RedactGitURL(trimmed), strings.Join(GitURLSchemes, ", "))
	}
	if !allowedScheme(scheme) {
		return fmt.Errorf("source: git url scheme %q is not one of %s",
			scheme, strings.Join(GitURLSchemes, ", "))
	}
	if u.Host == "" {
		return fmt.Errorf("source: git url %q names no host", RedactGitURL(trimmed))
	}
	return nil
}

// validateGitURLSafety is the half that governs handing the string to `exec`:
// no leading `-`, no whitespace or control characters, no `::` transport
// escape, and no credentials. It is deliberately separate from the scheme
// allowlist so the builder can apply it to a remote that did not come from the
// API — a local path in a real-toolchain test, say — without the builder
// becoming a second, quieter definition of what the API accepts.
func validateGitURLSafety(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("source: git url is empty")
	}
	if len(trimmed) > 2048 {
		return fmt.Errorf("source: git url is longer than 2048 characters")
	}
	if strings.HasPrefix(trimmed, "-") {
		// `git clone --upload-pack=…` reads as an option, not as a remote.
		return fmt.Errorf("source: git url %q starts with '-', which git reads as an option",
			RedactGitURL(trimmed))
	}
	// `ext::sh -c …` and every other `<transport>::<argument>` remote. This is
	// tested BEFORE the whitespace and scheme rules, because it is the specific
	// thing being refused and it deserves the specific message: `ext::sh -c 'id'`
	// also contains spaces, and "git url contains whitespace" would send a
	// reader looking in the wrong place. It is tested before `url.Parse` too,
	// which reports the scheme of `ext::sh` as `ext` and hides the rest in the
	// path.
	if i := strings.Index(trimmed, "::"); i >= 0 && !strings.Contains(trimmed[:i], "/") {
		return fmt.Errorf("source: git url uses the %q transport escape, which runs a command",
			trimmed[:i]+"::")
	}
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("source: git url contains a control character")
		}
		if r == ' ' || r == '\t' {
			return fmt.Errorf("source: git url contains whitespace")
		}
	}
	// A PASSWORD is refused; a bare username is not. `ssh://git@github.com/…`
	// and `git+ssh://git@…` name an SSH login, which is part of the address and
	// not a secret — refusing it would rule out the transport DESIGN section 6.2
	// allows. What must never be stored is the `:<token>@` half.
	if u, err := url.Parse(trimmed); err == nil && u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			return fmt.Errorf("source: git url carries credentials; " +
				"remove the ':<password>' from it — the build log, the manifest, the version row " +
				"and the API all record this URL, and a credential must appear in none of them")
		}
	}
	return nil
}

func allowedScheme(scheme string) bool {
	for _, s := range GitURLSchemes {
		if scheme == s {
			return true
		}
	}
	return false
}

// RedactGitURL returns the URL with any embedded password replaced, for a log
// line, a manifest, a database row or an API field.
//
// The username is KEPT, for the same reason ValidateGitURL accepts one:
// `ssh://git@host/repo` names an SSH login, and rewriting it would print a URL
// that is not the one being fetched. Only the `:<password>@` half is replaced.
//
// It is applied at every durable site rather than only where a credential could
// arrive today: ValidateGitURL refuses a password at the door, and this is what
// keeps a row that predates that refusal — or one edited into the database by
// hand — from leaking through a build log the API serves.
func RedactGitURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return trimmed
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.User == nil {
		return trimmed
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		return trimmed
	}
	u.User = url.UserPassword(u.User.Username(), "redacted")
	return u.String()
}
