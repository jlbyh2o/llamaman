package auth

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// clock is a controllable time source. Every window in this package — the session
// TTL, the idle timeout, the lockout — is a duration, and a test that had to
// SLEEP through one would either be slow or would prove nothing.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock {
	return &clock{at: time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// fakeSettings is the five `security.*` keys, without a database or a registry.
type fakeSettings map[string]int64

func (f fakeSettings) GetInt(_ context.Context, key string) (int64, error) {
	if v, ok := f[key]; ok {
		return v, nil
	}
	return 0, errors.New("no such setting: " + key)
}

func newTestService(t *testing.T, clk *clock, settings fakeSettings) (*Service, *store.Store, string) {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "llamaman.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx, store.MigrateOptions{}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	svc, err := New(Config{
		Repo:     st,
		Settings: settings,
		StateDir: dir,
		Params:   testParams(),
		Now:      clk.Now,
		Logger:   quietLogger(),
		// One millisecond, so every ResolveSession in a test writes
		// `last_seen_at` and the idle window is exercised rather than skipped.
		TouchInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return svc, st, dir
}

// claim is the shorthand every test that needs an account starts with: it is
// also the only way to create one, which is the point of section 2.2a.
func claim(t *testing.T, svc *Service, password string) model.SessionCredential {
	t.Helper()
	cred, err := svc.Claim(context.Background(), password, "127.0.0.1", "go test")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return cred
}

func defaultSettings() fakeSettings {
	return fakeSettings{
		"security.session_ttl_hours":  720,
		"security.idle_timeout_hours": 168,
		"security.login_max_attempts": 8,
		"security.login_window_sec":   300,
		"security.lockout_sec":        900,
	}
}

func TestLoginAndSessionResolution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	clk := newClock()
	svc, _, _ := newTestService(t, clk, defaultSettings())
	claim(t, svc, "a good password")

	cred, err := svc.Login(ctx, "a good password", "10.0.0.5", "curl/8")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if cred.SessionCookie == "" || cred.CSRFToken == "" {
		t.Fatal("a successful login returned an empty credential")
	}

	cases := []struct {
		name    string
		cookie  string
		wantErr error
	}{
		{"the cookie login issued", cred.SessionCookie, nil},
		{"no cookie", "", ErrNoSession},
		{"no separator", "not-a-session", ErrNoSession},
		{"an unknown session id", "01JZZZZZZZZZZZZZZZZZZZZZZZ.secret", ErrNoSession},
		{"the right id with the wrong secret", cred.SessionID + ".wrong", ErrNoSession},
		{"an empty secret", cred.SessionID + ".", ErrNoSession},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess, err := svc.ResolveSession(ctx, tc.cookie, "10.0.0.5", "curl/8")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ResolveSession = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && sess.ID != cred.SessionID {
				t.Fatalf("resolved session %q, want %q", sess.ID, cred.SessionID)
			}
		})
	}
}

func TestLoginRejectsWrongPasswordAndMissingAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	clk := newClock()
	svc, _, _ := newTestService(t, clk, defaultSettings())

	// No account at all: the answer is `bad_credentials`, NOT a "there is no
	// account" that would make login an oracle for the claim state.
	if _, err := svc.Login(ctx, "anything at all", "10.0.0.9", ""); !isCode(err, model.CodeBadCredentials) {
		t.Fatalf("Login against an unclaimed host = %v, want bad_credentials", err)
	}

	claim(t, svc, "the real password")
	if _, err := svc.Login(ctx, "the wrong password", "10.0.0.9", ""); !isCode(err, model.CodeBadCredentials) {
		t.Fatalf("Login with a wrong password = %v, want bad_credentials", err)
	}
}

// TestSessionExpiry covers both halves of the window: the absolute expiry from
// `security.session_ttl_hours` and the sliding idle timeout from
// `security.idle_timeout_hours`. A session that is USED stays alive; one that is
// not falls out even though its absolute expiry is further away.
func TestSessionExpiry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []struct {
		name     string
		settings fakeSettings
		// steps advance the clock, optionally touching the session in between.
		advance []time.Duration
		touch   bool
		wantErr error
	}{
		{
			name:     "inside both windows",
			settings: fakeSettings{"security.session_ttl_hours": 24, "security.idle_timeout_hours": 2},
			advance:  []time.Duration{time.Hour},
			wantErr:  nil,
		},
		{
			name:     "past the idle timeout",
			settings: fakeSettings{"security.session_ttl_hours": 24, "security.idle_timeout_hours": 2},
			advance:  []time.Duration{3 * time.Hour},
			wantErr:  ErrNoSession,
		},
		{
			name:     "kept alive by use, then idle",
			settings: fakeSettings{"security.session_ttl_hours": 24, "security.idle_timeout_hours": 2},
			advance:  []time.Duration{90 * time.Minute, 90 * time.Minute, 3 * time.Hour},
			touch:    true,
			wantErr:  ErrNoSession,
		},
		{
			name:     "past the absolute expiry despite use",
			settings: fakeSettings{"security.session_ttl_hours": 1, "security.idle_timeout_hours": 168},
			advance:  []time.Duration{30 * time.Minute, 45 * time.Minute},
			touch:    true,
			wantErr:  ErrNoSession,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			settings := defaultSettings()
			for k, v := range tc.settings {
				settings[k] = v
			}
			clk := newClock()
			svc, _, _ := newTestService(t, clk, settings)
			claim(t, svc, "a good password")

			cred, err := svc.Login(ctx, "a good password", "10.0.0.5", "")
			if err != nil {
				t.Fatalf("Login: %v", err)
			}

			for i, d := range tc.advance {
				clk.Advance(d)
				last := i == len(tc.advance)-1
				if last || !tc.touch {
					continue
				}
				// A touch inside the idle window slides it forward.
				if _, err := svc.ResolveSession(ctx, cred.SessionCookie, "10.0.0.5", ""); err != nil {
					t.Fatalf("ResolveSession at step %d: %v", i, err)
				}
			}

			_, err = svc.ResolveSession(ctx, cred.SessionCookie, "10.0.0.5", "")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ResolveSession = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestLockoutTiming is SPEC section 4's "login rate-limited with lockout", with
// the escalation the `lockouts.strikes` column exists for.
func TestLockoutTiming(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	settings := defaultSettings()
	settings["security.login_max_attempts"] = 3
	settings["security.login_window_sec"] = 60
	settings["security.lockout_sec"] = 10

	clk := newClock()
	svc, _, _ := newTestService(t, clk, settings)
	claim(t, svc, "the real password")

	const ip = "10.0.0.7"
	fail := func(t *testing.T, wantCode model.ErrorCode) error {
		t.Helper()
		_, err := svc.Login(ctx, "wrong", ip, "")
		if !isCode(err, wantCode) {
			t.Fatalf("Login = %v, want %s", err, wantCode)
		}
		return err
	}

	// Two failures are inside the budget; the third exhausts it and the answer
	// changes to locked_out immediately, rather than one request later.
	fail(t, model.CodeBadCredentials)
	fail(t, model.CodeBadCredentials)
	err := fail(t, model.CodeLockedOut)
	if got := retryAfterSec(t, err); got != 10 {
		t.Fatalf("retry_after_sec = %d, want 10", got)
	}

	// While locked, even the CORRECT password is refused — and the refusal costs
	// no argon2id work, which is the second reason the lockout exists.
	if _, err := svc.Login(ctx, "the real password", ip, ""); !isCode(err, model.CodeLockedOut) {
		t.Fatalf("Login while locked = %v, want locked_out", err)
	}

	// Another address is unaffected: the block is per address, not global.
	if _, err := svc.Login(ctx, "the real password", "10.0.0.8", ""); err != nil {
		t.Fatalf("Login from a different address while another is locked: %v", err)
	}

	// Once the block expires the budget is fresh: a single failure must not
	// re-lock instantly, which is what counting only the attempts since the last
	// lockout buys.
	clk.Advance(11 * time.Second)
	fail(t, model.CodeBadCredentials)
	fail(t, model.CodeBadCredentials)
	err = fail(t, model.CodeLockedOut)
	if got := retryAfterSec(t, err); got != 20 {
		t.Fatalf("second lockout retry_after_sec = %d, want 20 (the block doubles per strike)", got)
	}

	// A successful login clears the block outright: the strike history exists to
	// slow an attacker down, not to punish the operator who finally remembered.
	clk.Advance(21 * time.Second)
	if _, err := svc.Login(ctx, "the real password", ip, ""); err != nil {
		t.Fatalf("Login after the block expired: %v", err)
	}
	// The clock is frozen between calls, and the budget is counted from AFTER
	// that success; a real second passes before the next attempt.
	clk.Advance(time.Second)
	for i := 0; i < 2; i++ {
		fail(t, model.CodeBadCredentials)
	}
	if _, err := svc.Login(ctx, "wrong", ip, ""); !isCode(err, model.CodeLockedOut) {
		t.Fatalf("Login = %v, want locked_out on the third failure of a fresh budget", err)
	}
}

// TestLockedRefusalIsAuditedOncePerEpisode is the write-amplification guard on
// the one unauthenticated endpoint that can be flooded.
//
// `POST /auth/login` takes no session and no CSRF token, and the write pool is a
// single connection (§2): a row per knock from a blocked address would let a
// flood from the LAN serialize ahead of supervisor status updates, ledger
// closures and job leases, and — since §2.11's 30-day sweep runs nightly at
// best — grow `login_attempts` without bound in between. One row per episode
// keeps the audit trail and removes the amplification.
func TestLockedRefusalIsAuditedOncePerEpisode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	settings := defaultSettings()
	settings["security.login_max_attempts"] = 1
	settings["security.login_window_sec"] = 60
	settings["security.lockout_sec"] = 30

	clk := newClock()
	svc, st, _ := newTestService(t, clk, settings)
	claim(t, svc, "the real password")

	const ip = "10.0.0.9"
	countLocked := func() int {
		t.Helper()
		var n int
		if err := st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
			var err error
			n, err = st.CountFailedLoginAttempts(ctx, tx, ip, 0,
				[]model.LoginReason{model.LoginLocked})
			return err
		}); err != nil {
			t.Fatalf("count `locked` rows: %v", err)
		}
		return n
	}

	// One failure exhausts the budget and writes the block.
	if _, err := svc.Login(ctx, "wrong", ip, ""); !isCode(err, model.CodeLockedOut) {
		t.Fatalf("Login = %v, want locked_out on the first failure of a 1-attempt budget", err)
	}

	// Twenty knocks against the block: the first is audited, the rest are not.
	for i := 0; i < 20; i++ {
		if _, err := svc.Login(ctx, "wrong", ip, ""); !isCode(err, model.CodeLockedOut) {
			t.Fatalf("knock %d while locked = %v, want locked_out", i, err)
		}
	}
	if n := countLocked(); n != 1 {
		t.Fatalf("`locked` audit rows after 20 refused requests = %d, want 1", n)
	}

	// A NEW episode is audited again: the cap is per lockout, not per address
	// forever, so the trail still shows every time an address was turned away.
	clk.Advance(31 * time.Second)
	if _, err := svc.Login(ctx, "wrong", ip, ""); !isCode(err, model.CodeLockedOut) {
		t.Fatalf("the first failure after the block expired = %v, want locked_out", err)
	}
	if _, err := svc.Login(ctx, "wrong", ip, ""); !isCode(err, model.CodeLockedOut) {
		t.Fatalf("a knock against the second block = %v, want locked_out", err)
	}
	if n := countLocked(); n != 2 {
		t.Fatalf("`locked` audit rows after a second episode = %d, want 2", n)
	}
}

func TestLockoutCapsAtMaxLockout(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		base    time.Duration
		strikes int
		want    time.Duration
	}{
		{"first strike", 15 * time.Minute, 1, 15 * time.Minute},
		{"second strike doubles", 15 * time.Minute, 2, 30 * time.Minute},
		{"fourth strike", 15 * time.Minute, 4, 2 * time.Hour},
		{"far past the cap", 15 * time.Minute, 40, MaxLockout},
		{"a zero base takes the default", 0, 1, 900 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := lockoutFor(tc.base, tc.strikes); got != tc.want {
				t.Fatalf("lockoutFor(%v, %d) = %v, want %v", tc.base, tc.strikes, got, tc.want)
			}
		})
	}
}

// TestCSRF is section 3's double-submit: the cookie and the header must equal
// each other AND the token the session's own secret derives.
func TestCSRF(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	clk := newClock()
	svc, _, _ := newTestService(t, clk, defaultSettings())
	claim(t, svc, "a good password")

	first, err := svc.Login(ctx, "a good password", "10.0.0.5", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	second, err := svc.Login(ctx, "a good password", "10.0.0.6", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	cases := []struct {
		name      string
		sessionID string
		cookie    string
		header    string
		want      bool
	}{
		{"the pair login issued", first.SessionID, first.CSRFToken, first.CSRFToken, true},
		{"no cookie", first.SessionID, "", first.CSRFToken, false},
		{"no header", first.SessionID, first.CSRFToken, "", false},
		{"header does not match cookie", first.SessionID, first.CSRFToken, second.CSRFToken, false},
		{"another session's token in both", first.SessionID, second.CSRFToken, second.CSRFToken, false},
		{"a forged pair", first.SessionID, "forged", "forged", false},
		{"an unknown session", "01JZZZZZZZZZZZZZZZZZZZZZZZ", first.CSRFToken, first.CSRFToken, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := svc.VerifyCSRF(ctx, tc.sessionID, tc.cookie, tc.header); got != tc.want {
				t.Fatalf("VerifyCSRF = %v, want %v", got, tc.want)
			}
		})
	}

	// The token is deterministic from the row, which is what lets a session
	// survive a daemon restart with its `lm_csrf` cookie intact.
	sess, err := svc.ResolveSession(ctx, first.SessionCookie, "10.0.0.5", "")
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	if got := CSRFToken(sess.ID, sess.CSRFSecret); got != first.CSRFToken {
		t.Fatal("the CSRF token is not reproducible from the stored session row")
	}
}

// TestSetupTokenClaimRace is section 2.2a's guarantee: two concurrent claims,
// one winner. The race is decided by the database — an INSERT … ON CONFLICT DO
// NOTHING inside a BEGIN IMMEDIATE transaction — so there is no check-then-insert
// anywhere on the path and no window in which both callers can see "unclaimed".
func TestSetupTokenClaimRace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	clk := newClock()
	svc, st, _ := newTestService(t, clk, defaultSettings())

	// A real first boot mints the token before anyone can claim (§11.1 step 8),
	// and the claim's burn is what stamps the row this test reads back.
	if _, err := svc.EnsureSetupToken(ctx); err != nil {
		t.Fatalf("EnsureSetupToken: %v", err)
	}

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []model.SessionCredential
		losers  int
		others  []error
	)
	start := make(chan struct{})

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			cred, err := svc.Claim(ctx, "a good password", "10.0.0.20", "racer")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners = append(winners, cred)
			case errors.Is(err, ErrAlreadyClaimed):
				losers++
			default:
				others = append(others, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(others) > 0 {
		t.Fatalf("a claim failed for a reason other than losing the race: %v", others[0])
	}
	if len(winners) != 1 {
		t.Fatalf("%d claims succeeded, want exactly 1", len(winners))
	}
	if losers != racers-1 {
		t.Fatalf("%d claims were refused as already-claimed, want %d", losers, racers-1)
	}

	// The winner is logged in, and the claim is stamped in the same transaction
	// that created the account.
	if _, err := svc.ResolveSession(ctx, winners[0].SessionCookie, "10.0.0.20", "racer"); err != nil {
		t.Fatalf("the winning claim's session does not resolve: %v", err)
	}
	err := st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		claimRow, err := st.SetupClaim(ctx, tx)
		if err != nil {
			return err
		}
		if !claimRow.Claimed() {
			t.Error("setup_claim.claimed_at is not stamped after a successful claim")
		}
		if claimRow.TokenPath != nil {
			t.Error("setup_claim.token_path was not cleared by the claim")
		}
		exists, err := st.AdminAccountExists(ctx, tx)
		if err != nil {
			return err
		}
		if !exists {
			t.Error("admin_account does not exist after a successful claim")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read back the claim: %v", err)
	}
}

// TestEnsureSetupToken walks section 2.2a's mint, keep, rotate and burn.
func TestEnsureSetupToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	clk := newClock()
	svc, _, dir := newTestService(t, clk, defaultSettings())
	path := SetupTokenPath(dir)

	minted, err := svc.EnsureSetupToken(ctx)
	if err != nil {
		t.Fatalf("EnsureSetupToken: %v", err)
	}
	if !minted.Minted || minted.Token == "" {
		t.Fatalf("the first boot did not mint a token: %+v", minted)
	}

	// The file is the hand-off, and its mode is the authorization: host access
	// as root or the service identity.
	fi, err := statFile(path)
	if err != nil {
		t.Fatalf("stat the token file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the token file is mode %04o, want 0600", perm)
	}
	onDisk, err := ReadSetupTokenFile(path)
	if err != nil {
		t.Fatalf("ReadSetupTokenFile: %v", err)
	}
	if onDisk != minted.Token {
		t.Fatalf("the file holds %q, want the minted token", onDisk)
	}

	// A second boot with the file present keeps it: the token the human was told
	// about is still the one that works, and it is not re-announced.
	again, err := svc.EnsureSetupToken(ctx)
	if err != nil {
		t.Fatalf("EnsureSetupToken: %v", err)
	}
	if again.Minted {
		t.Fatal("a boot that found an existing unclaimed token minted a new one")
	}

	// The token authorizes a non-loopback caller; a wrong one does not, and
	// loopback needs none at all.
	if err := svc.AuthorizeSetup(ctx, false, minted.Token, "10.0.0.30"); err != nil {
		t.Fatalf("AuthorizeSetup with the real token: %v", err)
	}
	if err := svc.AuthorizeSetup(ctx, false, "not the token", "10.0.0.30"); !errors.Is(err, ErrSetupTokenRequired) {
		t.Fatalf("AuthorizeSetup with a wrong token = %v, want ErrSetupTokenRequired", err)
	}
	if err := svc.AuthorizeSetup(ctx, false, "", "10.0.0.30"); !errors.Is(err, ErrSetupTokenRequired) {
		t.Fatalf("AuthorizeSetup with no token = %v, want ErrSetupTokenRequired", err)
	}
	if err := svc.AuthorizeSetup(ctx, true, "", "127.0.0.1"); err != nil {
		t.Fatalf("AuthorizeSetup from loopback with no token: %v", err)
	}

	// Section 2.2a step 6: the file is gone while the claim is unstamped, so the
	// next boot mints a NEW one. A one-time credential nobody can read is worse
	// than a fresh one.
	if err := removeFile(path); err != nil {
		t.Fatalf("remove the token file: %v", err)
	}
	rotated, err := svc.EnsureSetupToken(ctx)
	if err != nil {
		t.Fatalf("EnsureSetupToken after the file went missing: %v", err)
	}
	if !rotated.Minted || rotated.Token == minted.Token {
		t.Fatal("a missing token file did not rotate the token")
	}
	if err := svc.AuthorizeSetup(ctx, false, minted.Token, "10.0.0.30"); !errors.Is(err, ErrSetupTokenRequired) {
		t.Fatal("the rotated-away token still authorizes a setup call")
	}

	// The burn: claiming unlinks the file, and the next boot is a no-op.
	claim(t, svc, "a good password")
	if _, err := statFile(path); err == nil {
		t.Fatal("the token file survived the claim")
	}
	burned, err := svc.EnsureSetupToken(ctx)
	if err != nil {
		t.Fatalf("EnsureSetupToken after the claim: %v", err)
	}
	if !burned.Claimed || burned.Minted {
		t.Fatalf("a claimed host minted or offered a token: %+v", burned)
	}
	// A non-loopback setup call is refused now, whatever it presents.
	if err := svc.AuthorizeSetup(ctx, false, rotated.Token, "10.0.0.30"); !errors.Is(err, ErrSetupTokenRequired) {
		t.Fatal("a claimed host still accepts its old setup token")
	}
}

// TestStaleTokenFileIsRemovedOnBoot is section 2.2a step 5's repair: a crash
// between the claim's commit and the unlink leaves a file whose row is already
// claimed, and the next boot removes it. That state is normal, not an error.
func TestStaleTokenFileIsRemovedOnBoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	clk := newClock()
	svc, _, dir := newTestService(t, clk, defaultSettings())
	path := SetupTokenPath(dir)

	if _, err := svc.EnsureSetupToken(ctx); err != nil {
		t.Fatalf("EnsureSetupToken: %v", err)
	}
	claim(t, svc, "a good password")

	// Simulate the crash: the row is claimed, but the file is back on disk.
	if err := writeFile(path, "a token nobody can use\n"); err != nil {
		t.Fatalf("write the stale file: %v", err)
	}
	if _, err := svc.EnsureSetupToken(ctx); err != nil {
		t.Fatalf("EnsureSetupToken: %v", err)
	}
	if _, err := statFile(path); err == nil {
		t.Fatal("a stale setup-token file survived a boot on a claimed host")
	}
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	clk := newClock()
	svc, _, _ := newTestService(t, clk, defaultSettings())
	claim(t, svc, "the first password")

	keep, err := svc.Login(ctx, "the first password", "10.0.0.1", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	drop, err := svc.Login(ctx, "the first password", "10.0.0.2", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := svc.ChangePassword(ctx, keep.SessionID, "the wrong current", "the second password"); !isCode(err, model.CodeBadCredentials) {
		t.Fatalf("ChangePassword with a wrong current password = %v, want bad_credentials", err)
	}
	if err := svc.ChangePassword(ctx, keep.SessionID, "the first password", "short"); !isCode(err, model.CodePasswordInvalid) {
		t.Fatalf("ChangePassword to a short password = %v, want password_invalid", err)
	}
	if err := svc.ChangePassword(ctx, keep.SessionID, "the first password", "the second password"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	if _, err := svc.ResolveSession(ctx, keep.SessionCookie, "10.0.0.1", ""); err != nil {
		t.Fatalf("the caller's own session was revoked by their password change: %v", err)
	}
	if _, err := svc.ResolveSession(ctx, drop.SessionCookie, "10.0.0.2", ""); !errors.Is(err, ErrNoSession) {
		t.Fatal("another session survived a password change")
	}
	if _, err := svc.Login(ctx, "the first password", "10.0.0.1", ""); !isCode(err, model.CodeBadCredentials) {
		t.Fatal("the old password still logs in after a password change")
	}
	if _, err := svc.Login(ctx, "the second password", "10.0.0.1", ""); err != nil {
		t.Fatalf("the new password does not log in: %v", err)
	}
}

func TestLogoutAndRevokeSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	clk := newClock()
	svc, _, _ := newTestService(t, clk, defaultSettings())
	claim(t, svc, "a good password")

	first, err := svc.Login(ctx, "a good password", "10.0.0.1", "browser")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	second, err := svc.Login(ctx, "a good password", "10.0.0.2", "phone")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	list, err := svc.Sessions(ctx, first.SessionID)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(list) != 3 {
		// The claim minted one session too.
		t.Fatalf("%d active sessions, want 3", len(list))
	}
	current := 0
	for _, s := range list {
		if s.Current {
			current++
		}
	}
	if current != 1 {
		t.Fatalf("%d sessions are marked current, want exactly 1", current)
	}

	if err := svc.RevokeSession(ctx, second.SessionID); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if _, err := svc.ResolveSession(ctx, second.SessionCookie, "10.0.0.2", "phone"); !errors.Is(err, ErrNoSession) {
		t.Fatal("a revoked session still resolves")
	}
	if err := svc.RevokeSession(ctx, second.SessionID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoking an already-revoked session = %v, want store.ErrNotFound", err)
	}

	if err := svc.Logout(ctx, first.SessionID); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := svc.ResolveSession(ctx, first.SessionCookie, "10.0.0.1", "browser"); !errors.Is(err, ErrNoSession) {
		t.Fatal("a session survived its own logout")
	}
}

func TestResetPassword(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	clk := newClock()
	svc, _, _ := newTestService(t, clk, defaultSettings())

	// With no account there is nothing to reset, and creating one here would
	// claim the host from the CLI — which is the setup flow's job.
	if err := svc.ResetPassword(ctx, "a good password"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ResetPassword on an unclaimed host = %v, want store.ErrNotFound", err)
	}

	claim(t, svc, "the first password")
	live, err := svc.Login(ctx, "the first password", "10.0.0.1", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := svc.ResetPassword(ctx, "short"); !isCode(err, model.CodePasswordInvalid) {
		t.Fatalf("ResetPassword to a short password = %v, want password_invalid", err)
	}
	if err := svc.ResetPassword(ctx, "the reset password"); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}

	if _, err := svc.ResolveSession(ctx, live.SessionCookie, "10.0.0.1", ""); !errors.Is(err, ErrNoSession) {
		t.Fatal("a session survived a password reset; section 11.3 says every session is deleted")
	}
	if _, err := svc.Login(ctx, "the reset password", "10.0.0.1", ""); err != nil {
		t.Fatalf("the reset password does not log in: %v", err)
	}
}

func TestSetupCompleteFollowsTheAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	clk := newClock()
	svc, _, _ := newTestService(t, clk, defaultSettings())

	complete, err := svc.SetupComplete(ctx)
	if err != nil {
		t.Fatalf("SetupComplete: %v", err)
	}
	if complete {
		t.Fatal("an unclaimed host reports setup complete")
	}

	claim(t, svc, "a good password")
	complete, err = svc.SetupComplete(ctx)
	if err != nil {
		t.Fatalf("SetupComplete: %v", err)
	}
	if !complete {
		t.Fatal("a claimed host reports setup incomplete; the session gate would answer setup_required forever")
	}
}

// isCode reports whether err is a model.Error carrying code.
func isCode(err error, code model.ErrorCode) bool {
	var me model.Error
	return errors.As(err, &me) && me.Code == code
}

func retryAfterSec(t *testing.T, err error) int {
	t.Helper()
	var me model.Error
	if !errors.As(err, &me) {
		t.Fatalf("error %v is not a model.Error", err)
	}
	v, ok := me.Details["retry_after_sec"].(int)
	if !ok {
		t.Fatalf("details = %v, want an int retry_after_sec", me.Details)
	}
	return v
}
