package source

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// gitEnv is added to every git invocation.
//
// GIT_TERMINAL_PROMPT=0 is not a nicety: a daemon running under systemd has no
// terminal, and a private or mistyped URL makes git block forever waiting for a
// username it can never be given. With it, git fails immediately and the
// `fetch` phase reports a real error instead of a build that hangs until the
// job's lease expires.
var gitEnv = []string{
	"GIT_TERMINAL_PROMPT=0",
	"GIT_ASKPASS=",
	"GCM_INTERACTIVE=never",
}

var sha40Re = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// fetch is DESIGN section 6.5's `fetch` phase: one shared partial clone, then a
// worktree per version id.
//
// The partial clone (`--filter=blob:none --no-checkout`) plus worktrees is what
// makes the second build of the day cheap — only new objects are downloaded,
// and no second copy of the history exists per version.
func (b *Builder) fetch(ctx context.Context, req Request, sink *LogSink) (string, error) {
	repo := b.Layout.RepoDir()
	url := req.GitURLOrDefault()
	// Everything below that names the remote in a log line, an error message or
	// a failure record names the REDACTED form. The URL reaches `git` intact and
	// reaches nothing durable intact — see giturl.go.
	shown := RedactGitURL(url)

	for _, dir := range []string{
		filepath.Dir(repo),
		filepath.Dir(b.Layout.WorktreeDir(req.VersionID)),
		filepath.Join(b.Layout.StateDir, VersionsDirName),
	} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", &Failure{
				Phase: PhaseFetch, Code: CodeFetchFailed,
				Message: fmt.Sprintf("cannot create %s", dir), cause: err,
			}
		}
	}

	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		// A previous clone that died halfway leaves a directory with no .git;
		// it has nothing worth keeping and git refuses to clone into it.
		if err := os.RemoveAll(repo); err != nil {
			return "", &Failure{
				Phase: PhaseFetch, Code: CodeFetchFailed,
				Message: fmt.Sprintf("cannot clear the incomplete clone at %s", repo), cause: err,
			}
		}
		sink.Printf("cloning %s (blobless partial clone)", shown)
		if _, err := b.git(ctx, sink, "", "clone", "--filter=blob:none", "--no-checkout", url, repo); err != nil {
			return "", gitFailure("clone", shown, err)
		}
	} else {
		// The URL may have changed since the clone — a custom build pointed at
		// a fork reuses the same object store deliberately, which is most of
		// why the shared clone is worth having.
		if _, err := b.git(ctx, sink, repo, "remote", "set-url", "origin", url); err != nil {
			return "", gitFailure("remote set-url", shown, err)
		}
		sink.Printf("fetching %s", shown)
		if _, err := b.git(ctx, sink, repo, "fetch", "--tags", "--prune", "origin"); err != nil {
			return "", gitFailure("fetch", shown, err)
		}
	}

	commit, err := b.resolveRef(ctx, sink, repo, req.GitRef)
	if err != nil {
		return "", err
	}
	sink.Printf("building %s at %s", displayRef(req.GitRef), commit)

	if err := b.prepareWorktree(ctx, sink, req.VersionID, commit); err != nil {
		return "", err
	}
	return commit, nil
}

// resolveRef turns the request's ref into a concrete 40-hex commit, which is
// what makes "rebuild the same thing" reproducible (section 6.2's custom
// channel row).
func (b *Builder) resolveRef(ctx context.Context, sink *LogSink, repo, ref string) (string, error) {
	if ref == "" {
		ref = "HEAD"
	}

	candidates := []string{}
	if sha40Re.MatchString(ref) {
		candidates = append(candidates, ref+"^{commit}")
	}
	candidates = append(candidates,
		"refs/tags/"+ref+"^{commit}",
		"refs/remotes/origin/"+ref+"^{commit}",
		ref+"^{commit}",
	)

	fetched := false
	for {
		for _, c := range candidates {
			out, err := b.capture(ctx, nil, b.tools.Git, "-C", repo, "rev-parse", "--verify", "--quiet", c)
			if err != nil {
				continue
			}
			if sha := strings.TrimSpace(out); sha40Re.MatchString(sha) {
				return sha, nil
			}
		}
		if fetched || !sha40Re.MatchString(ref) {
			break
		}
		// A commit that is not on any branch we fetched — a force-pushed fork,
		// or a ref given as a SHA that upstream has since rewritten. Ask the
		// remote for that one object and try again, once.
		fetched = true
		sink.Printf("commit %s is not in the local object store; fetching it directly", ref)
		if _, err := b.git(ctx, sink, repo, "fetch", "--no-tags", "origin", ref); err != nil {
			break
		}
	}

	return "", &Failure{
		Phase: PhaseFetch,
		Code:  CodeFetchFailed,
		Message: fmt.Sprintf("the ref %q does not name a commit in this repository",
			displayRef(ref)),
		Hint: "Check the branch, tag or commit against the repository — `git ls-remote` on the same URL " +
			"lists what it actually has.",
	}
}

// prepareWorktree adds (or reuses) `build/<id>` checked out at commit.
//
// Reuse is D4's warm rerun in its most literal form: a worktree already sitting
// at the right commit is left exactly as it is, objects and all, so a Retry
// after an interrupted build re-runs `cmake --build` against warm output.
func (b *Builder) prepareWorktree(ctx context.Context, sink *LogSink, id, commit string) error {
	repo := b.Layout.RepoDir()
	wt := b.Layout.WorktreeDir(id)

	if _, err := os.Stat(filepath.Join(wt, ".git")); err == nil {
		out, err := b.capture(ctx, nil, b.tools.Git, "-C", wt, "rev-parse", "HEAD")
		if err == nil && strings.TrimSpace(out) == commit {
			sink.Printf("reusing the existing worktree at %s", wt)
			return nil
		}
		sink.Printf("the worktree at %s is at a different commit; replacing it", wt)
		if _, err := b.git(ctx, sink, repo, "worktree", "remove", "--force", wt); err != nil {
			// `worktree remove` refuses a worktree git has forgotten about; the
			// directory removal plus prune below is the repair for that.
			sink.Printf("note: `git worktree remove` failed (%v); removing the directory directly", err)
		}
	}
	if err := os.RemoveAll(wt); err != nil {
		return &Failure{
			Phase: PhaseFetch, Code: CodeFetchFailed,
			Message: fmt.Sprintf("cannot remove the old worktree at %s", wt), cause: err,
		}
	}
	if _, err := b.git(ctx, sink, repo, "worktree", "prune"); err != nil {
		return gitFailure("worktree prune", "", err)
	}
	if _, err := b.git(ctx, sink, repo, "worktree", "add", "--detach", "--force", wt, commit); err != nil {
		return gitFailure("worktree add", commit, err)
	}
	if _, err := b.git(ctx, sink, wt, "submodule", "update", "--init", "--recursive"); err != nil {
		return gitFailure("submodule update", "", err)
	}
	return nil
}

// removeWorktree is the cancellation cleanup of section 6.5: a build the user
// stopped keeps nothing. It is deliberately NOT what a daemon restart does —
// D4 keeps the directory precisely so Retry is a warm rebuild.
func (b *Builder) removeWorktree(ctx context.Context, id string) error {
	wt := b.Layout.WorktreeDir(id)
	if _, err := os.Stat(wt); os.IsNotExist(err) {
		return nil
	}
	if b.tools.Git != "" {
		repo := b.Layout.RepoDir()
		if _, err := b.git(ctx, nil, repo, "worktree", "remove", "--force", wt); err == nil {
			return nil
		}
	}
	if err := os.RemoveAll(wt); err != nil {
		return fmt.Errorf("source: remove worktree %s: %w", wt, err)
	}
	if b.tools.Git != "" {
		_, _ = b.git(ctx, nil, b.Layout.RepoDir(), "worktree", "prune")
	}
	return nil
}

// git runs one git command, in dir when dir is non-empty.
func (b *Builder) git(ctx context.Context, sink *LogSink, dir string, args ...string) (string, error) {
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	return b.capture(ctx, sink, b.tools.Git, full...)
}

func gitFailure(op, subject string, err error) error {
	msg := "git " + op + " failed"
	if subject != "" {
		msg += " for " + subject
	}
	return &Failure{
		Phase:    PhaseFetch,
		Code:     CodeFetchFailed,
		Message:  msg,
		ExitCode: exitCodeOf(err),
		cause:    err,
	}
}

func displayRef(ref string) string {
	if ref == "" {
		return "the default branch"
	}
	return ref
}
