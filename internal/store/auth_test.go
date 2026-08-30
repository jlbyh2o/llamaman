package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// Queries for `admin_account`, `sessions`, `login_attempts`, `lockouts` and
// `wizard_steps` (DESIGN sections 2.2 and 2.11). internal/auth and internal/setup
// prove the POLICY these statements serve; what is proven here is the statements
// themselves — that each one is the single atomic move its caller composes with
// another in one transaction.

func TestCreateAdminAccountIsOnceOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)

	acct := model.AdminAccount{PasswordHash: "$argon2id$first", PasswordSetAt: 1000, UpdatedAt: 1000}

	var first, second bool
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		if first, err = s.CreateAdminAccount(ctx, tx, acct); err != nil {
			return err
		}
		acct.PasswordHash = "$argon2id$second"
		second, err = s.CreateAdminAccount(ctx, tx, acct)
		return err
	})

	if !first {
		t.Fatal("the first CreateAdminAccount reported no insert")
	}
	if second {
		t.Fatal("a second CreateAdminAccount reported an insert; the claim race would have two winners")
	}

	// The loser's hash must not have overwritten the winner's.
	err := s.Read(ctx, func(ctx context.Context, tx Tx) error {
		got, err := s.AdminAccount(ctx, tx)
		if err != nil {
			return err
		}
		if got.PasswordHash != "$argon2id$first" {
			t.Errorf("password_hash = %q, want the first writer's", got.PasswordHash)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the account: %v", err)
	}
}

func TestAdminAccountAbsence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)

	err := s.Read(ctx, func(ctx context.Context, tx Tx) error {
		exists, err := s.AdminAccountExists(ctx, tx)
		if err != nil {
			return err
		}
		if exists {
			t.Error("a fresh database reports an admin account")
		}
		if _, err := s.AdminAccount(ctx, tx); !errors.Is(err, ErrNotFound) {
			t.Errorf("AdminAccount = %v, want ErrNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// SetAdminPassword never CREATES the row: a host with no account is claimed
	// through the setup flow, not through a password change.
	var updated bool
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		updated, err = s.SetAdminPassword(ctx, tx, "$argon2id$x", 2000)
		return err
	})
	if updated {
		t.Fatal("SetAdminPassword created an admin account")
	}
}

func TestSessionLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)

	const now = int64(10_000)
	rows := []model.Session{
		{ID: "01A", TokenHash: "hash-a", CSRFSecret: "csrf-a", CreatedAt: now,
			LastSeenAt: now, ExpiresAt: now + 1000, IP: ptr("10.0.0.1")},
		{ID: "01B", TokenHash: "hash-b", CSRFSecret: "csrf-b", CreatedAt: now,
			LastSeenAt: now, ExpiresAt: now + 1000},
		{ID: "01C", TokenHash: "hash-c", CSRFSecret: "csrf-c", CreatedAt: now,
			LastSeenAt: now, ExpiresAt: now - 1}, // already expired
	}
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		for _, r := range rows {
			if err := s.InsertSession(ctx, tx, r); err != nil {
				return err
			}
		}
		return nil
	})

	// Active means neither expired nor revoked.
	assertActive := func(t *testing.T, want ...string) {
		t.Helper()
		err := s.Read(ctx, func(ctx context.Context, tx Tx) error {
			got, err := s.ActiveSessions(ctx, tx, now)
			if err != nil {
				return err
			}
			if len(got) != len(want) {
				t.Fatalf("%d active sessions, want %d (%v)", len(got), len(want), want)
			}
			for i, id := range want {
				if got[i].ID != id {
					t.Errorf("active[%d] = %q, want %q", i, got[i].ID, id)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read sessions: %v", err)
		}
	}
	// id DESC is creation order with a unique tiebreak, so the newest is first.
	assertActive(t, "01B", "01A")

	// Touching slides the audit columns.
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.TouchSession(ctx, tx, "01A", now+50, ptr("10.0.0.2"), ptr("curl/8"))
	})
	err := s.Read(ctx, func(ctx context.Context, tx Tx) error {
		got, err := s.Session(ctx, tx, "01A")
		if err != nil {
			return err
		}
		if got.LastSeenAt != now+50 || got.IP == nil || *got.IP != "10.0.0.2" {
			t.Errorf("touched session = %+v, want the new last_seen_at and ip", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the touched session: %v", err)
	}

	// Revoking reports whether it changed anything, which is what tells DELETE
	// /auth/sessions/{id} to answer 404 rather than pretending.
	var first, second bool
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		if first, err = s.RevokeSession(ctx, tx, "01A", now+60); err != nil {
			return err
		}
		second, err = s.RevokeSession(ctx, tx, "01A", now+61)
		return err
	})
	if !first || second {
		t.Fatalf("RevokeSession twice = %v, %v; want true then false", first, second)
	}
	assertActive(t, "01B")

	// RevokeSessionsExcept("") revokes every one of them — reset-password's case.
	var revoked int64
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		revoked, err = s.RevokeSessionsExcept(ctx, tx, "", now+70)
		return err
	})
	if revoked != 2 {
		// 01B and the expired 01C, which is not revoked but is still live by the
		// column's own definition.
		t.Fatalf("RevokeSessionsExcept revoked %d rows, want 2", revoked)
	}
	assertActive(t)

	// Retention: rows past the cutoff go, the rest stay.
	var deleted int64
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		deleted, err = s.DeleteExpiredSessions(ctx, tx, now)
		return err
	})
	if deleted != 1 {
		t.Fatalf("DeleteExpiredSessions removed %d rows, want the 1 expired one", deleted)
	}
}

func TestLoginAttemptsAndLockouts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)

	const ip = "10.0.0.5"
	reason := func(r model.LoginReason) *model.LoginReason { return &r }
	attempts := []model.LoginAttempt{
		{ID: "01A", At: 1000, IP: ip, Success: false, Reason: reason(model.LoginBadPassword)},
		{ID: "01B", At: 2000, IP: ip, Success: false, Reason: reason(model.LoginBadPassword)},
		{ID: "01C", At: 3000, IP: ip, Success: false, Reason: reason(model.LoginLocked)},
		{ID: "01D", At: 4000, IP: ip, Success: true, Reason: reason(model.LoginOK)},
		{ID: "01E", At: 5000, IP: "10.0.0.6", Success: false, Reason: reason(model.LoginBadPassword)},
	}
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		for _, a := range attempts {
			if err := s.InsertLoginAttempt(ctx, tx, a); err != nil {
				return err
			}
		}
		return nil
	})

	cases := []struct {
		name    string
		ip      string
		since   int64
		reasons []model.LoginReason
		want    int
	}{
		{"every failure from this address", ip, 0, nil, 3},
		{"only the guesses", ip, 0, []model.LoginReason{model.LoginBadPassword}, 2},
		{"inside a narrower window", ip, 1500, []model.LoginReason{model.LoginBadPassword}, 1},
		{"another address is separate", "10.0.0.6", 0, []model.LoginReason{model.LoginBadPassword}, 1},
		{"an address with no history", "10.0.0.9", 0, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.Read(ctx, func(ctx context.Context, tx Tx) error {
				got, err := s.CountFailedLoginAttempts(ctx, tx, tc.ip, tc.since, tc.reasons)
				if err != nil {
					return err
				}
				if got != tc.want {
					t.Errorf("count = %d, want %d", got, tc.want)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("count: %v", err)
			}
		})
	}

	err := s.Read(ctx, func(ctx context.Context, tx Tx) error {
		at, ok, err := s.LastSuccessfulLoginAt(ctx, tx, ip)
		if err != nil {
			return err
		}
		if !ok || at != 4000 {
			t.Errorf("LastSuccessfulLoginAt = %d/%v, want 4000/true", at, ok)
		}
		if _, ok, err := s.LastSuccessfulLoginAt(ctx, tx, "10.0.0.9"); err != nil {
			return err
		} else if ok {
			t.Error("an address that never logged in reports a successful login")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the last success: %v", err)
	}

	// Lockouts upsert, and a successful login clears them.
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if err := s.PutLockout(ctx, tx, model.Lockout{
			IP: ip, LockedUntil: 9000, Strikes: 1, UpdatedAt: 3000,
		}); err != nil {
			return err
		}
		return s.PutLockout(ctx, tx, model.Lockout{
			IP: ip, LockedUntil: 18000, Strikes: 2, UpdatedAt: 6000,
		})
	})
	err = s.Read(ctx, func(ctx context.Context, tx Tx) error {
		got, err := s.Lockout(ctx, tx, ip)
		if err != nil {
			return err
		}
		if got.Strikes != 2 || got.LockedUntil != 18000 {
			t.Errorf("lockout = %+v, want the second write's values", got)
		}
		if !got.Locked(17999) || got.Locked(18000) {
			t.Errorf("Locked() disagrees with locked_until %d", got.LockedUntil)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the lockout: %v", err)
	}

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.DeleteLockout(ctx, tx, ip)
	})
	err = s.Read(ctx, func(ctx context.Context, tx Tx) error {
		if _, err := s.Lockout(ctx, tx, ip); !errors.Is(err, ErrNotFound) {
			t.Errorf("Lockout after DeleteLockout = %v, want ErrNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read after delete: %v", err)
	}

	// Retention prunes the audit trail by age.
	var pruned int64
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		pruned, err = s.DeleteLoginAttemptsBefore(ctx, tx, 3000)
		return err
	})
	if pruned != 2 {
		t.Fatalf("DeleteLoginAttemptsBefore removed %d rows, want 2", pruned)
	}
}

func TestWizardSteps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)

	// A fresh database has no rows, which internal/setup reads as "every step is
	// pending" rather than as an error.
	err := s.Read(ctx, func(ctx context.Context, tx Tx) error {
		rows, err := s.WizardSteps(ctx, tx)
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			t.Errorf("%d wizard rows on a fresh database, want 0", len(rows))
		}
		if _, err := s.WizardStep(ctx, tx, model.StepPassword); !errors.Is(err, ErrNotFound) {
			t.Errorf("WizardStep = %v, want ErrNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	data := `{"channel":"stable"}`
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if err := s.PutWizardStep(ctx, tx, model.WizardStepRow{
			Step: model.StepLlamacpp, State: model.WizardActive, DataJSON: &data, UpdatedAt: 1000,
		}); err != nil {
			return err
		}
		// A state change with no data must NOT clear the data a step stored.
		return s.PutWizardStep(ctx, tx, model.WizardStepRow{
			Step: model.StepLlamacpp, State: model.WizardComplete, UpdatedAt: 2000,
		})
	})

	err = s.Read(ctx, func(ctx context.Context, tx Tx) error {
		got, err := s.WizardStep(ctx, tx, model.StepLlamacpp)
		if err != nil {
			return err
		}
		if got.State != model.WizardComplete {
			t.Errorf("state = %q, want complete", got.State)
		}
		if got.DataJSON == nil || *got.DataJSON != data {
			t.Errorf("data_json = %v, want the value the first write stored", got.DataJSON)
		}
		if got.UpdatedAt != 2000 {
			t.Errorf("updated_at = %d, want 2000", got.UpdatedAt)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the step: %v", err)
	}

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.ClearWizardStepData(ctx, tx, model.StepLlamacpp, 3000)
	})
	err = s.Read(ctx, func(ctx context.Context, tx Tx) error {
		got, err := s.WizardStep(ctx, tx, model.StepLlamacpp)
		if err != nil {
			return err
		}
		if got.DataJSON != nil {
			t.Errorf("data_json = %v after ClearWizardStepData, want NULL", *got.DataJSON)
		}
		if got.State != model.WizardComplete {
			t.Errorf("state = %q; clearing data must not change it", got.State)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the cleared step: %v", err)
	}
}

// TestOpenReadOnlyCannotWrite is the promise the CLI depends on: a Store from
// OpenReadOnly has no write pool at all, so "read-only" is enforced by the
// absence of the connection rather than by a convention a later caller could
// forget.
func TestOpenReadOnlyCannotWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	live := newTestStore(t)
	ro, err := OpenReadOnly(ctx, live.Path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()

	if ro.RW != nil {
		t.Fatal("a read-only store has a write pool")
	}

	// Reads work, and the integrity check has a read-side form because the
	// write-pool one cannot run here.
	err = ro.Read(ctx, func(ctx context.Context, tx Tx) error {
		if err := ro.IntegrityCheckRead(ctx, tx, DefaultIntegrityMaxErrors); err != nil {
			return err
		}
		v, err := ro.SchemaVersion(ctx, tx)
		if err != nil {
			return err
		}
		if v <= 0 {
			t.Errorf("schema version = %d, want the migrated version", v)
		}
		// query_only(1) is on: a write through this pool is refused by SQLite
		// rather than by us.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at, updated_by) VALUES ('x', '1', 1, 'admin')`,
		); err == nil {
			t.Error("a write through the read-only pool succeeded")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read through the read-only store: %v", err)
	}
}
