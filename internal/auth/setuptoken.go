package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The one-time setup token, end to end (DESIGN §2.2a, D38/D59).
//
// The token has to travel from the daemon that mints it to a human at a shell,
// and the database is the one place it must NOT travel through: `setup_claim`
// stores only sha256, precisely so `db-backups/`, `VACUUM INTO` snapshots and
// `llamaman diagnostics` bundles can never leak a live claim credential. The
// hand-off is a FILE, and that file is the single source every printer reads —
// `llamaman status`, `install.sh` step 10, and a human who lost the scrollback.
//
// The authorization to read it is therefore host access as root or the service
// identity, which is exactly the authorization claiming a host service should
// require, and exactly what §11.3 already demands of `reset-password`.

// SetupTokenFileName is §2.2a's `<state_dir>/setup-token`.
const SetupTokenFileName = "setup-token"

// SetupTokenPath returns the file the plaintext token lives in while it exists.
func SetupTokenPath(stateDir string) string { return filepath.Join(stateDir, SetupTokenFileName) }

// setupTokenBytes is §2.2a step 1's "32 crypto/rand bytes".
const setupTokenBytes = 32

// ErrAlreadyClaimed is returned by Claim when `admin_account` already exists —
// the loser of the claim race, and the replayed request. internal/api answers it
// with `409 setup_already_claimed`, which is what §3's Auth column says a `setup`
// route answers once the account exists.
var ErrAlreadyClaimed = errors.New("auth: this host has already been claimed")

// SetupToken is what EnsureSetupToken resolved on this boot.
type SetupToken struct {
	// Claimed is true when `admin_account` exists: there is nothing to mint and
	// any leftover file has been removed.
	Claimed bool
	// Minted is true when this boot created a NEW token — a first boot, or
	// §2.2a step 6's rotation after the file went missing.
	Minted bool
	// Token is the plaintext, and is non-empty only when Minted is true. §2.2a
	// step 2's journald announcement is the only thing that prints it from
	// memory; every later reader goes back to the file.
	Token string
	// Path is the file, empty once the claim is stamped.
	Path string
}

// EnsureSetupToken is DESIGN §11.1 step 8, and implements steps 1, 5 and 6 of
// §2.2a:
//
//   - `admin_account` exists → remove any stale `setup-token` file and forget its
//     path. That is the repair for a crash between the claim's commit and the
//     unlink, which §2.2a calls normal rather than an error.
//   - No account, a claim row, and the file is present → keep it. The token is
//     still the one the human was told about.
//   - No account and the file is missing → mint a NEW token and REPLACE the row.
//     Someone deleted it, or an `install.sh --purge` half-ran; a one-time
//     credential nobody can read is worse than a fresh one.
//
// The caller announces a minted token exactly once, at info, per §2.2a step 2.
func (s *Service) EnsureSetupToken(ctx context.Context) (SetupToken, error) {
	if s.stateDir == "" {
		return SetupToken{}, errors.New("auth: no state directory was configured for the setup token")
	}
	path := SetupTokenPath(s.stateDir)

	var (
		claimed  bool
		claim    model.SetupClaim
		haveRow  bool
		haveAcct bool
	)
	if err := s.repo.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		haveAcct, err = s.repo.AdminAccountExists(ctx, tx)
		if err != nil {
			return err
		}
		c, err := s.repo.SetupClaim(ctx, tx)
		switch {
		case err == nil:
			claim, haveRow, claimed = c, true, c.Claimed()
		case errors.Is(err, store.ErrNotFound):
		default:
			return err
		}
		return nil
	}); err != nil {
		return SetupToken{}, err
	}

	if haveAcct || claimed {
		// §2.2a step 5's repair. Removing the file is idempotent and a missing
		// one is the ordinary case.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("could not remove the claimed setup-token file", "path", path, "error", err)
		}
		if haveRow && claim.TokenPath != nil {
			if err := s.repo.Write(ctx, func(ctx context.Context, tx store.Tx) error {
				return s.repo.ClearSetupTokenPath(ctx, tx)
			}); err != nil {
				return SetupToken{}, err
			}
		}
		return SetupToken{Claimed: true}, nil
	}

	if haveRow {
		if _, err := os.Stat(path); err == nil {
			return SetupToken{Path: path}, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			// The file is there but this process cannot stat it, which on a
			// 0750 directory owned by the service identity means the daemon is
			// running as somebody else. Minting a new one would not help.
			return SetupToken{Path: path}, fmt.Errorf("auth: stat %s: %w", path, err)
		}
	}

	return s.mintSetupToken(ctx, path)
}

// mintSetupToken is §2.2a step 1, and step 6's rotation, which are the same
// write: a fresh hash and path REPLACING whatever was there.
//
// The row is written before the file. The two orderings fail differently and
// this one fails better: a row whose file is missing is exactly the state step 6
// rotates on the next boot, while a file whose row was never written would verify
// against nothing and would look valid to the human reading it.
func (s *Service) mintSetupToken(ctx context.Context, path string) (SetupToken, error) {
	raw := make([]byte, setupTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return SetupToken{}, fmt.Errorf("auth: mint setup token: %w", err)
	}
	token := base58Encode(raw)
	now := s.now().UnixMilli()

	if err := s.repo.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return s.repo.PutSetupClaim(ctx, tx, hashSecret(token), path, now)
	}); err != nil {
		return SetupToken{}, err
	}

	// O_EXCL after an explicit remove: the file must be created by this call,
	// never opened, so a symlink planted at the path cannot redirect a 0600
	// write somewhere else.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return SetupToken{}, fmt.Errorf("auth: remove the stale setup token at %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return SetupToken{}, fmt.Errorf("auth: write the setup token to %s: %w", path, err)
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		f.Close()
		return SetupToken{}, fmt.Errorf("auth: write the setup token to %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return SetupToken{}, fmt.Errorf("auth: write the setup token to %s: %w", path, err)
	}
	// A umask cannot widen a 0600 create, but a file restored or copied by an
	// operator can arrive wider; the chmod costs nothing and makes the mode a
	// property of this code rather than of the environment.
	if err := os.Chmod(path, 0o600); err != nil {
		return SetupToken{}, fmt.Errorf("auth: set the mode of %s: %w", path, err)
	}

	return SetupToken{Minted: true, Token: token, Path: path}, nil
}

// ReadSetupTokenFile reads the plaintext token from disk — §2.2a step 3, the
// path `llamaman status` and `install.sh` take.
//
// It is a package-level function rather than a method because its only callers
// have no Service and no database: the CLI reads the FILE, never the row, and
// `setup_claim` holds a sha256 that could never produce this string.
func ReadSetupTokenFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := trimToken(string(b))
	if token == "" {
		return "", fmt.Errorf("auth: %s is empty", path)
	}
	return token, nil
}

func trimToken(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// AuthorizeSetup is D38's first-run rule: a request from loopback may claim the
// daemon with no token at all — the overwhelmingly common case, and what
// preserves SPEC §3.9's acceptance test literally ("download → start → open
// browser → done") — while any other origin must present the one-time token.
func (s *Service) AuthorizeSetup(ctx context.Context, loopback bool, presented, ip string) error {
	if loopback {
		return nil
	}
	if presented == "" {
		return ErrSetupTokenRequired
	}

	var claim model.SetupClaim
	err := s.repo.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		claim, err = s.repo.SetupClaim(ctx, tx)
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrSetupTokenRequired
		}
		return err
	}
	if claim.Claimed() {
		return ErrSetupTokenRequired
	}

	// §2.2a step 4: compare sha256(presented) against token_hash with a
	// constant-time comparison, so the response time cannot be used to walk the
	// token one character at a time.
	if !equalHash(hashSecret(presented), claim.TokenHash) {
		if err := s.recordAttempt(ctx, ip, s.now().UnixMilli(), false, model.LoginBadSetupToken); err != nil {
			return err
		}
		return ErrSetupTokenRequired
	}
	return nil
}

// Claim is §2.2a step 5, the BURN: in ONE transaction, create `admin_account`,
// stamp `setup_claim.claimed_at`/`claimed_from_ip`, set `token_path` to NULL and
// mint the session that logs the browser in. The file is unlinked immediately
// after the commit, and a crash between the two is repaired by the next boot's
// EnsureSetupToken.
//
// The race that matters — two concurrent claims — is decided by the database:
// `CreateAdminAccount` is an INSERT … ON CONFLICT DO NOTHING inside a
// BEGIN IMMEDIATE transaction, so exactly one caller sees `true` and every other
// gets ErrAlreadyClaimed. There is no check-then-insert anywhere on this path.
func (s *Service) Claim(ctx context.Context, password, ip, userAgent string) (model.SessionCredential, error) {
	if err := ValidatePassword(password); err != nil {
		return model.SessionCredential{}, err
	}

	hash, err := s.params.Hash(password)
	if err != nil {
		return model.SessionCredential{}, err
	}

	now := s.now()
	nowMS := now.UnixMilli()
	pol := s.policy(ctx)

	var cred model.SessionCredential
	err = s.repo.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		created, err := s.repo.CreateAdminAccount(ctx, tx, model.AdminAccount{
			PasswordHash:  hash,
			PasswordSetAt: nowMS,
			UpdatedAt:     nowMS,
		})
		if err != nil {
			return err
		}
		if !created {
			return ErrAlreadyClaimed
		}
		if _, err := s.repo.ClaimSetup(ctx, tx, nowMS, ip); err != nil {
			return err
		}
		if err := s.repo.InsertLoginAttempt(ctx, tx,
			s.attempt(nowMS, ip, true, model.LoginOK)); err != nil {
			return err
		}
		if err := s.repo.AppendEvent(ctx, tx, model.Event{
			ID:       store.NewID(now),
			At:       nowMS,
			Level:    model.LevelInfo,
			Category: model.CategoryAuth,
			Action:   "setup_claimed",
			Actor:    model.ActorWizard,
			Message:  "this host was claimed and the admin account was created",
		}); err != nil {
			return err
		}
		cred, err = s.mintSession(ctx, tx, now, pol, ip, userAgent)
		return err
	})
	if err != nil {
		return model.SessionCredential{}, err
	}

	if s.stateDir != "" {
		path := SetupTokenPath(s.stateDir)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			// The row is already stamped, so the token is dead either way; the
			// next boot removes the file. Saying so is better than failing a
			// claim that has committed.
			s.log.Warn("could not remove the claimed setup-token file", "path", path, "error", err)
		}
	}
	return cred, nil
}

// ResetPassword is `llamaman reset-password` (§11.3): a fresh argon2id hash, no
// session survives, and an audit row records it. Authorization is filesystem
// access to the 0600 database — root or the service identity — which the CLI
// checks before it ever reaches this method.
func (s *Service) ResetPassword(ctx context.Context, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	hash, err := s.params.Hash(password)
	if err != nil {
		return err
	}
	now := s.now()
	nowMS := now.UnixMilli()

	return s.repo.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		updated, err := s.repo.SetAdminPassword(ctx, tx, hash, nowMS)
		if err != nil {
			return err
		}
		if !updated {
			// No account exists yet. Creating one here would claim the host from
			// the CLI, which is the setup flow's job and would leave
			// `setup_claim` unstamped; the caller is told to run the wizard.
			return store.ErrNotFound
		}
		revoked, err := s.repo.RevokeSessionsExcept(ctx, tx, "", nowMS)
		if err != nil {
			return err
		}
		return s.repo.AppendEvent(ctx, tx, model.Event{
			ID:       store.NewID(now),
			At:       nowMS,
			Level:    model.LevelWarn,
			Category: model.CategoryAuth,
			Action:   "password_reset",
			Actor:    model.ActorCLI,
			Message:  fmt.Sprintf("the admin password was reset from the host; %d session(s) were revoked", revoked),
		})
	})
}
