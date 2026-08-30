package model

// Identity, sessions, secrets (DESIGN section 2.2 / 2.2a).

// SecretName is the primary key of `secrets` (§2.2). The column carries no
// CHECK, but the set is closed by the application and by the design's own rule:
// "a value in this set with no endpoint behind it would be dead schema". Both
// members are reachable — 'hf_token' through GET/PUT/DELETE /api/v1/hf/token and
// the wizard's `hf` step, 'github_token' through GET/PUT/DELETE
// /api/v1/github/token and /settings → Builds (§3.6, §6.2).
type SecretName string

const (
	SecretHFToken     SecretName = "hf_token"
	SecretGitHubToken SecretName = "github_token"
)

// SecretNameValues lists the secrets this design stores, in order.
func SecretNameValues() []SecretName { return []SecretName{SecretHFToken, SecretGitHubToken} }

// Valid reports whether n names a secret this design stores.
func (n SecretName) Valid() bool { return valid(n, SecretNameValues()) }

// LoginReason is `login_attempts.reason` (§2.2). The column is documented by a
// comment rather than closed by a CHECK — the audit trail must be able to record
// a reason a future release invents without a migration — so this type is the
// application's own closed set and is deliberately absent from ClosedEnums.
type LoginReason string

const (
	LoginOK            LoginReason = "ok"
	LoginBadPassword   LoginReason = "bad_password"
	LoginLocked        LoginReason = "locked"
	LoginNoAccount     LoginReason = "no_account"
	LoginBadSetupToken LoginReason = "bad_setup_token"
)

// LoginReasonValues lists the reasons the design names, in the order of the
// column's comment.
func LoginReasonValues() []LoginReason {
	return []LoginReason{LoginBadPassword, LoginLocked, LoginOK, LoginNoAccount, LoginBadSetupToken}
}

// Valid reports whether r is one of the reasons the design names.
func (r LoginReason) Valid() bool { return valid(r, LoginReasonValues()) }

// SetupClaim is the singleton `setup_claim` row (§2.2, §2.2a): the one-time
// token that lets a non-loopback caller claim a fresh install.
//
// The database holds only the sha256. The plaintext travels through a file —
// `<state_dir>/setup-token`, mode 0600 inside the 0750 state directory — and
// TokenPath names it while it exists. That separation is the point: `db-backups/`
// entries, `VACUUM INTO` snapshots and `llamaman diagnostics` bundles can never
// leak a live claim credential, and the authorization to read the token is host
// access as root or the service identity, which is exactly the authorization
// claiming a host service should require.
//
// The six steps of §2.2a, in order:
//
//  1. Mint — first boot with an empty `admin_account`: 32 crypto/rand bytes,
//     base58, insert this row with TokenHash and TokenPath, write the plaintext
//     O_CREAT|O_EXCL|O_WRONLY 0600.
//  2. Announce — the same string logged once to journald at info.
//  3. Print — `llamaman status` reads the FILE, never the database, whenever
//     ClaimedAt is nil and the file is readable.
//  4. Verify — POST /api/v1/setup/password compares sha256(presented) against
//     TokenHash with crypto/subtle.ConstantTimeCompare. Loopback skips it.
//  5. Burn — in the same transaction that creates `admin_account`, ClaimedAt and
//     ClaimedFromIP are stamped and TokenPath is set to NULL; the file is
//     unlinked right after the commit. A missing file with a non-nil ClaimedAt is
//     normal, and a crash between commit and unlink is repaired on the next boot.
//  6. Rotate — a missing file while ClaimedAt is nil mints a NEW token and
//     replaces the row. A one-time credential nobody can read is worse than a
//     fresh one.
type SetupClaim struct {
	TokenHash     string // sha256 of a 32-byte base58 token
	TokenPath     *string
	CreatedAt     int64
	ClaimedAt     *int64
	ClaimedFromIP *string
}

// Claimed reports whether the token has been burned, which is the condition
// `llamaman status` and the boot sequence both branch on.
func (c SetupClaim) Claimed() bool { return c.ClaimedAt != nil }
