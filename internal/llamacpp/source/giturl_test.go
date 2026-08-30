package source

import (
	"strings"
	"testing"
)

func TestValidateGitURL(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		wantIn string // empty means the URL is accepted
	}{
		{name: "upstream over https", url: DefaultGitURL},
		{name: "a fork over https with a .git suffix", url: "https://example.test/me/llama.cpp.git"},
		{name: "http", url: "http://example.test/me/llama.cpp"},
		{name: "ssh", url: "ssh://git@example.test/me/llama.cpp.git"},
		{name: "the git protocol", url: "git://example.test/me/llama.cpp"},
		{name: "git over ssh", url: "git+ssh://git@example.test/me/llama.cpp"},

		{
			// The proof of impact: git's ext:: transport hands its "URL" to
			// /bin/sh as the daemon's User=.
			name:   "the ext transport, which runs a shell command",
			url:    "ext::sh -c 'id > /tmp/pwn'",
			wantIn: "transport escape",
		},
		{
			name:   "a transport escape with no argument",
			url:    "ext::whoami",
			wantIn: "transport escape",
		},
		{
			name:   "an option smuggled in as the remote",
			url:    "--upload-pack=/bin/sh",
			wantIn: "starts with '-'",
		},
		{
			name:   "a local repository, which would disclose files on this host",
			url:    "file:///etc",
			wantIn: "not one of",
		},
		{
			name:   "a bare local path",
			url:    "/srv/private/repo.git",
			wantIn: "has no scheme",
		},
		{
			name:   "the link-local metadata service",
			url:    "gopher://169.254.169.254/",
			wantIn: "not one of",
		},
		{
			name:   "a URL carrying a token",
			url:    "https://user:ghp_XXXX@example.test/me/llama.cpp.git",
			wantIn: "carries credentials",
		},
		{
			// A bare username is an address, not a secret: `git@` is how every
			// SSH remote is written.
			name: "a URL carrying a bare username",
			url:  "https://someone@example.test/me/llama.cpp.git",
		},
		{name: "empty", url: "", wantIn: "empty"},
		{name: "whitespace inside", url: "https://example.test/a b", wantIn: "whitespace"},
		{name: "a control character", url: "https://example.test/a\nb", wantIn: "control character"},
		{name: "no host", url: "https:///me/llama.cpp", wantIn: "names no host"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateGitURL(tc.url)
			switch {
			case tc.wantIn == "" && err != nil:
				t.Fatalf("ValidateGitURL(%q) = %v, want accepted", tc.url, err)
			case tc.wantIn == "":
				return
			case err == nil:
				t.Fatalf("ValidateGitURL(%q) accepted it; want an error mentioning %q", tc.url, tc.wantIn)
			case !strings.Contains(err.Error(), tc.wantIn):
				t.Fatalf("ValidateGitURL(%q) = %v, want an error mentioning %q", tc.url, err, tc.wantIn)
			}
		})
	}
}

// TestValidateGitURLNeverEchoesACredential is the half that matters after the
// refusal: a 422 body, and the log line behind it, must not carry the token the
// user just pasted.
func TestValidateGitURLNeverEchoesACredential(t *testing.T) {
	const secret = "ghp_TOKENVALUE"
	err := ValidateGitURL("https://user:" + secret + "@example.test/me/llama.cpp.git")
	if err == nil {
		t.Fatal("a URL with credentials was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the rejection echoed the credential: %v", err)
	}
}

func TestRedactGitURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "no credentials is unchanged", in: DefaultGitURL, want: DefaultGitURL},
		{
			name: "a password is replaced",
			in:   "https://user:ghp_XXXX@example.test/me/llama.cpp.git",
			want: "https://user:redacted@example.test/me/llama.cpp.git",
		},
		{
			name: "a bare username is an address and is kept",
			in:   "ssh://git@example.test/me/llama.cpp.git",
			want: "ssh://git@example.test/me/llama.cpp.git",
		},
		{name: "empty", in: "", want: ""},
		{name: "unparseable is returned as given", in: "::::", want: "::::"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactGitURL(tc.in); got != tc.want {
				t.Errorf("RedactGitURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRequestValidateRejectsExecInjection is the builder-side half: this package
// hands the string to `exec`, so it applies the argv-safety rules again even
// though the service already applied the whole rule.
func TestRequestValidateRejectsExecInjection(t *testing.T) {
	for _, url := range []string{
		"ext::sh -c 'id'",
		"--upload-pack=/bin/sh",
		"https://user:ghp_XXXX@example.test/me/llama.cpp.git",
	} {
		req := Request{VersionID: "b1-cpu-src", Backend: "cpu", GitURL: url}
		if err := req.Validate(); err == nil {
			t.Errorf("Request.Validate accepted git_url %q", url)
		}
	}
}
