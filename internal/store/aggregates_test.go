package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// -----------------------------------------------------------------------------
// settings (2.1)
// -----------------------------------------------------------------------------

// TestSettingsOverrideLayer covers the whole of what section 2.1 asks of this
// table: an absent row means the built-in default, a write creates or replaces
// the override, and a delete returns the setting to following the default.
func TestSettingsOverrideLayer(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.Setting(ctx, s.RO, "ui.port_desired"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unset key = %v, want ErrNotFound (the caller uses the registry default)", err)
	}

	first := model.Setting{Key: "ui.port_desired", Value: "5526", UpdatedAt: 100, UpdatedBy: model.UpdatedBySystem}
	mustWrite(t, s, func(ctx context.Context, tx Tx) error { return s.PutSetting(ctx, tx, first) })

	got, err := s.Setting(ctx, s.RO, "ui.port_desired")
	if err != nil {
		t.Fatalf("Setting: %v", err)
	}
	if diff := cmp.Diff(first, got); diff != "" {
		t.Errorf("setting mismatch (-want +got):\n%s", diff)
	}

	second := model.Setting{Key: "ui.port_desired", Value: "8080", UpdatedAt: 200, UpdatedBy: model.UpdatedByAdmin}
	mustWrite(t, s, func(ctx context.Context, tx Tx) error { return s.PutSetting(ctx, tx, second) })
	got, err = s.Setting(ctx, s.RO, "ui.port_desired")
	if err != nil {
		t.Fatalf("Setting: %v", err)
	}
	if diff := cmp.Diff(second, got); diff != "" {
		t.Errorf("upsert did not replace (-want +got):\n%s", diff)
	}

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.DeleteSetting(ctx, tx, "ui.port_desired")
	})
	if _, err := s.Setting(ctx, s.RO, "ui.port_desired"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete = %v, want ErrNotFound", err)
	}
	// Deleting a key that has no row is not an error.
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.DeleteSetting(ctx, tx, "never.existed")
	})
}

// TestPutSettingIfAbsent is §11.1 step 6b: `serve --port N` is a SEED, never an
// override. On a fresh install it writes the row; where a human already chose a
// port in the UI, the stored setting wins and the flag is only recorded.
func TestPutSettingIfAbsent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seed := model.Setting{Key: "ui.port_desired", Value: "9000", UpdatedAt: 1, UpdatedBy: model.UpdatedBySystem}

	var wrote bool
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		wrote, err = s.PutSettingIfAbsent(ctx, tx, seed)
		return err
	})
	if !wrote {
		t.Fatal("the seed was not written to a fresh install")
	}

	later := model.Setting{Key: "ui.port_desired", Value: "5526", UpdatedAt: 2, UpdatedBy: model.UpdatedBySystem}
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		wrote, err = s.PutSettingIfAbsent(ctx, tx, later)
		return err
	})
	if wrote {
		t.Error("the seed overwrote a stored setting; the flag is a seed, never an override")
	}

	got, err := s.Setting(ctx, s.RO, "ui.port_desired")
	if err != nil {
		t.Fatalf("Setting: %v", err)
	}
	if got.Value != "9000" {
		t.Errorf("value = %q, want the stored 9000", got.Value)
	}
}

// TestSettingsListing returns the override layer in key order, which is what the
// boot sequence loads once into the read-through cache.
func TestSettingsListing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		for _, key := range []string{"ui.theme", "hf.endpoint", "log.level"} {
			if err := s.PutSetting(ctx, tx, model.Setting{
				Key: key, Value: `"x"`, UpdatedAt: 1, UpdatedBy: model.UpdatedByAdmin,
			}); err != nil {
				return err
			}
		}
		return nil
	})

	got, err := s.Settings(ctx, s.RO)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	keys := make([]string, len(got))
	for i, v := range got {
		keys[i] = v.Key
	}
	if diff := cmp.Diff([]string{"hf.endpoint", "log.level", "ui.theme"}, keys); diff != "" {
		t.Errorf("keys mismatch (-want +got):\n%s", diff)
	}
}

// -----------------------------------------------------------------------------
// runtime_info (2.1)
// -----------------------------------------------------------------------------

// TestRuntimeInfoRoundTrip proves all 28 columns survive, and — the part that
// matters for F14 — that a NULL comes back as nil rather than as a zero that
// reads like an answer.
func TestRuntimeInfoRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.RuntimeInfo(ctx, s.RO); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before the first boot = %v, want ErrNotFound", err)
	}

	want := model.RuntimeInfo{
		DaemonVersion: "v1.0.0", DaemonCommit: "abc1234",
		PID: ptr(int64(4242)), BootID: ptr("01JBOOT"), BootAt: ptr(int64(1700)),
		HostBootID: ptr("host-uuid"), HostBootAt: ptr(int64(1600)),
		UIBindAddr: ptr("0.0.0.0"), UIPort: ptr(int64(5526)), UIPortFlag: ptr(int64(5526)),
		UIURLHint:   ptr("http://10.0.0.5:5526"),
		ServiceUser: ptr("llamaman"), ServiceUID: ptr(int64(999)),
		ServiceGroup: ptr("llamaman"), ServiceGID: ptr(int64(999)),
		SystemdScope: ptr(model.ScopeSystem), SystemdControl: ptr(model.ControlDBus),
		JournalRead: ptr(model.JournalOK),
		PolkitOK:    ptr(true), PolkitDetail: ptr("granted"), PolkitUnitFiles: ptr(false),
		ListenerContinuity: ptr(model.ContinuityFDStore),
		BinaryPath:         ptr("/usr/local/bin/llamaman"),
		HFHubDir:           ptr("/var/lib/llamaman/hf/hub"), HFHome: ptr("/var/lib/llamaman/hf"),
		StateDir: ptr("/var/lib/llamaman"), SchemaVersion: ptr(int64(1)),
		LastHeartbeatAt: ptr(int64(1800)),
	}
	mustWrite(t, s, func(ctx context.Context, tx Tx) error { return s.PutRuntimeInfo(ctx, tx, want) })

	got, err := s.RuntimeInfo(ctx, s.RO)
	if err != nil {
		t.Fatalf("RuntimeInfo: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("runtime_info mismatch (-want +got):\n%s", diff)
	}

	// A second write updates in place rather than adding a row — the CHECK
	// (id = 1) would reject a second one, and this proves the upsert path.
	want.DaemonVersion = "v1.1.0"
	mustWrite(t, s, func(ctx context.Context, tx Tx) error { return s.PutRuntimeInfo(ctx, tx, want) })
	got, err = s.RuntimeInfo(ctx, s.RO)
	if err != nil {
		t.Fatalf("RuntimeInfo: %v", err)
	}
	if got.DaemonVersion != "v1.1.0" {
		t.Errorf("daemon_version = %q, want v1.1.0", got.DaemonVersion)
	}
}

// TestRuntimeInfoNullsStayNull is F14's rule at the storage boundary: a fact
// that has not been learned is NULL, and NULL is not zero. `polkit_ok` NULL in
// user scope means "not asked, not denied" and must not read as "denied".
func TestRuntimeInfoNullsStayNull(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.PutRuntimeInfo(ctx, tx, model.RuntimeInfo{
			DaemonVersion: "v1.0.0", DaemonCommit: "abc",
			SystemdScope: ptr(model.ScopeUser),
		})
	})

	got, err := s.RuntimeInfo(ctx, s.RO)
	if err != nil {
		t.Fatalf("RuntimeInfo: %v", err)
	}
	if got.PolkitOK != nil {
		t.Errorf("polkit_ok = %v, want nil in user scope", *got.PolkitOK)
	}
	if got.PolkitUnitFiles != nil {
		t.Errorf("polkit_unit_files = %v, want nil in user scope", *got.PolkitUnitFiles)
	}
	if got.HostBootID != nil {
		t.Errorf("host_boot_id = %v, want nil before the supervisor stamps it", *got.HostBootID)
	}
	if got.SystemdScope == nil || *got.SystemdScope != model.ScopeUser {
		t.Errorf("systemd_scope = %v, want user", got.SystemdScope)
	}
}

// TestSetHostBootIsNarrow proves the single-writer column can be stamped without
// disturbing anything else — which is what lets §11.1 step 9 read it and write
// nothing while §5.8 step 1 does the writing.
func TestSetHostBootIsNarrow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.PutRuntimeInfo(ctx, tx, model.RuntimeInfo{
			DaemonVersion: "v1.0.0", DaemonCommit: "abc", BootID: ptr("01JBOOT"),
		})
	})
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.SetHostBoot(ctx, tx, "host-uuid", 1600)
	})
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.SetListenerState(ctx, tx, 1, 5526, model.ContinuityNone)
	})
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.SetRuntimeHeartbeat(ctx, tx, 9999)
	})

	got, err := s.RuntimeInfo(ctx, s.RO)
	if err != nil {
		t.Fatalf("RuntimeInfo: %v", err)
	}
	if got.HostBootID == nil || *got.HostBootID != "host-uuid" {
		t.Errorf("host_boot_id = %v, want host-uuid", got.HostBootID)
	}
	if got.HostBootAt == nil || *got.HostBootAt != 1600 {
		t.Errorf("host_boot_at = %v, want 1600", got.HostBootAt)
	}
	if got.BootID == nil || *got.BootID != "01JBOOT" {
		t.Errorf("boot_id = %v, want the daemon's own, untouched", got.BootID)
	}
	if got.ListenerContinuity == nil || *got.ListenerContinuity != model.ContinuityNone {
		t.Errorf("listener_continuity = %v, want none", got.ListenerContinuity)
	}
	if got.SchemaVersion == nil || *got.SchemaVersion != 1 {
		t.Errorf("schema_version = %v, want 1", got.SchemaVersion)
	}
	if got.LastHeartbeatAt == nil || *got.LastHeartbeatAt != 9999 {
		t.Errorf("last_heartbeat_at = %v, want 9999", got.LastHeartbeatAt)
	}
}

// -----------------------------------------------------------------------------
// setup_claim (2.2 / 2.2a)
// -----------------------------------------------------------------------------

// TestSetupClaimLifecycle walks §2.2a: mint, burn, and the rotate that replaces a
// token nobody can read.
func TestSetupClaimLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.SetupClaim(ctx, s.RO); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before minting = %v, want ErrNotFound", err)
	}

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.PutSetupClaim(ctx, tx, "hash-1", "/var/lib/llamaman/setup-token", 1000)
	})
	got, err := s.SetupClaim(ctx, s.RO)
	if err != nil {
		t.Fatalf("SetupClaim: %v", err)
	}
	if got.TokenHash != "hash-1" {
		t.Errorf("token_hash = %q, want hash-1", got.TokenHash)
	}
	if got.TokenPath == nil || *got.TokenPath != "/var/lib/llamaman/setup-token" {
		t.Errorf("token_path = %v, want the state-directory file", got.TokenPath)
	}
	if got.Claimed() {
		t.Error("a freshly minted claim reads as claimed")
	}

	// Rotate: a fresh hash and path replace the row, and the claim columns stay
	// clear. A one-time credential nobody can read is worse than a new one.
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.PutSetupClaim(ctx, tx, "hash-2", "/var/lib/llamaman/setup-token", 2000)
	})
	got, err = s.SetupClaim(ctx, s.RO)
	if err != nil {
		t.Fatalf("SetupClaim: %v", err)
	}
	if got.TokenHash != "hash-2" || got.CreatedAt != 2000 || got.Claimed() {
		t.Errorf("after rotate = %+v, want a fresh unclaimed row", got)
	}

	// Burn.
	var ok bool
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		ok, err = s.ClaimSetup(ctx, tx, 3000, "10.0.0.9")
		return err
	})
	if !ok {
		t.Fatal("ClaimSetup on an unclaimed row = false")
	}
	got, err = s.SetupClaim(ctx, s.RO)
	if err != nil {
		t.Fatalf("SetupClaim: %v", err)
	}
	if !got.Claimed() || *got.ClaimedAt != 3000 {
		t.Errorf("claimed_at = %v, want 3000", got.ClaimedAt)
	}
	if got.ClaimedFromIP == nil || *got.ClaimedFromIP != "10.0.0.9" {
		t.Errorf("claimed_from_ip = %v, want 10.0.0.9", got.ClaimedFromIP)
	}
	if got.TokenPath != nil {
		t.Errorf("token_path = %v, want NULL once the claim is stamped", *got.TokenPath)
	}

	// A second claim changes nothing and says so, which is how a replayed
	// request or two racing browsers are distinguished from the first one.
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		ok, err = s.ClaimSetup(ctx, tx, 4000, "10.0.0.10")
		return err
	})
	if ok {
		t.Error("a second ClaimSetup reported success")
	}
	got, err = s.SetupClaim(ctx, s.RO)
	if err != nil {
		t.Fatalf("SetupClaim: %v", err)
	}
	if *got.ClaimedAt != 3000 {
		t.Errorf("claimed_at moved to %d; the first claim is the claim", *got.ClaimedAt)
	}
}

// TestClearSetupTokenPath is the repair for a commit that landed with its unlink
// missing, and for the stale file §11.1 step 8 removes.
func TestClearSetupTokenPath(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.PutSetupClaim(ctx, tx, "hash", "/state/setup-token", 1)
	})
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.ClearSetupTokenPath(ctx, tx)
	})

	got, err := s.SetupClaim(ctx, s.RO)
	if err != nil {
		t.Fatalf("SetupClaim: %v", err)
	}
	if got.TokenPath != nil {
		t.Errorf("token_path = %v, want NULL", *got.TokenPath)
	}
	if got.Claimed() {
		t.Error("clearing the path claimed the token; those are different facts")
	}
}

// -----------------------------------------------------------------------------
// idempotency_keys (2.3, D65)
// -----------------------------------------------------------------------------

// TestIdempotencyWindow is D65's central claim: the same key is refused inside
// the window and reusable the moment it closes.
func TestIdempotencyWindow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const created = 1_000_000
	expires := created + model.IdempotencyWindow.Milliseconds()

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if err := s.InsertJob(ctx, tx, newJob("j1", model.JobModelDownload, "dl-1")); err != nil {
			return err
		}
		return s.InsertIdempotencyKey(ctx, tx, model.IdempotencyKey{
			Key: "k1", Route: "POST /api/v1/downloads", RequestFingerprint: "fp-a",
			JobID: "j1", CreatedAt: created, ExpiresAt: expires,
		}, created)
	})

	t.Run("a replay inside the window finds the job", func(t *testing.T) {
		got, err := s.LiveIdempotencyKey(ctx, s.RO, "k1", created+1000)
		if err != nil {
			t.Fatalf("LiveIdempotencyKey: %v", err)
		}
		if got.JobID != "j1" || got.RequestFingerprint != "fp-a" {
			t.Errorf("= %+v, want the recorded job and fingerprint", got)
		}
	})

	t.Run("a second job under the same key inside the window is refused", func(t *testing.T) {
		err := s.Write(ctx, func(ctx context.Context, tx Tx) error {
			if err := s.InsertJob(ctx, tx, newJob("j2", model.JobModelDownload, "dl-2")); err != nil {
				return err
			}
			return s.InsertIdempotencyKey(ctx, tx, model.IdempotencyKey{
				Key: "k1", Route: "POST /api/v1/downloads", RequestFingerprint: "fp-b",
				JobID: "j2", CreatedAt: created + 1, ExpiresAt: expires + 1,
			}, created+1)
		})
		if err == nil {
			t.Fatal("a concurrent double-submit was accepted")
		}
	})

	t.Run("after the window the key reads as a miss", func(t *testing.T) {
		_, err := s.LiveIdempotencyKey(ctx, s.RO, "k1", expires+1)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("= %v, want ErrNotFound", err)
		}
	})

	t.Run("after the window the key is reusable", func(t *testing.T) {
		now := expires + 1
		mustWrite(t, s, func(ctx context.Context, tx Tx) error {
			if err := s.InsertJob(ctx, tx, newJob("j3", model.JobModelDownload, "dl-3")); err != nil {
				return err
			}
			return s.InsertIdempotencyKey(ctx, tx, model.IdempotencyKey{
				Key: "k1", Route: "POST /api/v1/downloads", RequestFingerprint: "fp-c",
				JobID: "j3", CreatedAt: now, ExpiresAt: now + model.IdempotencyWindow.Milliseconds(),
			}, now)
		})
		got, err := s.LiveIdempotencyKey(ctx, s.RO, "k1", now+1)
		if err != nil {
			t.Fatalf("LiveIdempotencyKey: %v", err)
		}
		if got.JobID != "j3" {
			t.Errorf("job = %q, want the new j3", got.JobID)
		}
	})
}

// TestIdempotencyWindowIsTenMinutes pins D65's constant, which the schema's own
// comment states as `created_at + 600_000 ms`.
func TestIdempotencyWindowIsTenMinutes(t *testing.T) {
	if got := model.IdempotencyWindow.Milliseconds(); got != 600_000 {
		t.Errorf("IdempotencyWindow = %d ms, want 600000", got)
	}
}

// TestDeleteIdempotencyKeysBefore is the nightly sweep, which runs well past the
// window so a late replay still finds the row that explains it.
func TestDeleteIdempotencyKeysBefore(t *testing.T) {
	s := newTestStore(t)

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if err := s.InsertJob(ctx, tx, newJob("j1", model.JobModelDownload, "dl-1")); err != nil {
			return err
		}
		return s.InsertIdempotencyKey(ctx, tx, model.IdempotencyKey{
			Key: "k1", Route: "r", RequestFingerprint: "fp", JobID: "j1",
			CreatedAt: 1, ExpiresAt: 600_001,
		}, 1)
	})

	var swept int64
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		swept, err = s.DeleteIdempotencyKeysBefore(ctx, tx, 600_000)
		return err
	})
	if swept != 0 {
		t.Errorf("swept %d rows that had not expired", swept)
	}

	cutoff := int64(600_001) + (24 * time.Hour).Milliseconds()
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		swept, err = s.DeleteIdempotencyKeysBefore(ctx, tx, cutoff)
		return err
	})
	if swept != 1 {
		t.Errorf("swept %d rows, want 1", swept)
	}
}

// -----------------------------------------------------------------------------
// events (2.11)
// -----------------------------------------------------------------------------

func newEvent(id string, at int64, cat model.EventCategory, level model.EventLevel) model.Event {
	return model.Event{
		ID: id, At: at, Level: level, Category: cat, Action: "state_changed",
		Actor: model.ActorSystem, Message: "something happened",
	}
}

// TestEventsRoundTripAndFilters covers GET /events/log's query surface.
func TestEventsRoundTripAndFilters(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	full := model.Event{
		ID: "ev-0001", At: 100, Level: model.LevelWarn, Category: model.CategoryInstance,
		SubjectType: ptr("instance"), SubjectID: ptr("inst-1"), Action: "start_failed",
		FromState: ptr("starting"), ToState: ptr("failed"), Actor: model.ActorSystemd,
		Message: "unit failed", DetailJSON: ptr(`{"exit_code":78}`),
	}
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if err := s.AppendEvent(ctx, tx, full); err != nil {
			return err
		}
		for _, e := range []model.Event{
			newEvent("ev-0002", 200, model.CategorySystem, model.LevelInfo),
			newEvent("ev-0003", 300, model.CategoryDownload, model.LevelError),
		} {
			if err := s.AppendEvent(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	})

	t.Run("round trip", func(t *testing.T) {
		got, err := s.Events(ctx, s.RO, EventFilter{
			SubjectType: "instance", SubjectID: "inst-1",
		})
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d events, want 1", len(got))
		}
		if diff := cmp.Diff(full, got[0]); diff != "" {
			t.Errorf("event mismatch (-want +got):\n%s", diff)
		}
	})

	tests := []struct {
		name   string
		filter EventFilter
		want   []string
	}{
		{name: "newest first", filter: EventFilter{}, want: []string{"ev-0003", "ev-0002", "ev-0001"}},
		{
			name:   "by category",
			filter: EventFilter{Categories: []model.EventCategory{model.CategorySystem}},
			want:   []string{"ev-0002"},
		},
		{
			name:   "by level",
			filter: EventFilter{Levels: []model.EventLevel{model.LevelWarn, model.LevelError}},
			want:   []string{"ev-0003", "ev-0001"},
		},
		{name: "paged with before", filter: EventFilter{Before: "ev-0003"}, want: []string{"ev-0002", "ev-0001"}},
		{name: "limited", filter: EventFilter{Limit: 2}, want: []string{"ev-0003", "ev-0002"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.Events(ctx, s.RO, tt.filter)
			if err != nil {
				t.Fatalf("Events: %v", err)
			}
			ids := make([]string, len(got))
			for i, e := range got {
				ids[i] = e.ID
			}
			if diff := cmp.Diff(tt.want, ids); diff != "" {
				t.Errorf("ids mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestEventsAfterIsTheSSECursor: a reconnecting client replays from its
// Last-Event-ID, OLDEST first, which only works because the id sorts by
// creation.
func TestEventsAfterIsTheSSECursor(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	var ids []string
	base := time.Unix(1_700_000_000, 0)
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		for i := range 5 {
			id := NewID(base.Add(time.Duration(i) * time.Millisecond))
			ids = append(ids, id)
			if err := s.AppendEvent(ctx, tx,
				newEvent(id, base.UnixMilli()+int64(i), model.CategorySystem, model.LevelInfo)); err != nil {
				return err
			}
		}
		return nil
	})

	got, err := s.EventsAfter(ctx, s.RO, ids[1], 10)
	if err != nil {
		t.Fatalf("EventsAfter: %v", err)
	}
	gotIDs := make([]string, len(got))
	for i, e := range got {
		gotIDs[i] = e.ID
	}
	if diff := cmp.Diff(ids[2:], gotIDs); diff != "" {
		t.Errorf("replay mismatch (-want +got):\n%s", diff)
	}

	all, err := s.EventsAfter(ctx, s.RO, "", 10)
	if err != nil {
		t.Fatalf("EventsAfter with no cursor: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("replay from the beginning returned %d events, want 5", len(all))
	}
}

// -----------------------------------------------------------------------------
// ids (2)
// -----------------------------------------------------------------------------

// TestNewIDSortsByCreation is the property everything that treats an id as a
// cursor depends on: ids minted in a loop, even within one millisecond, sort in
// the order they were minted.
func TestNewIDSortsByCreation(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	prev := NewID(at)
	for range 1000 {
		id := NewID(at)
		if id <= prev {
			t.Fatalf("id %q does not sort after %q", id, prev)
		}
		prev = id
	}

	later := NewID(at.Add(time.Second))
	if later <= prev {
		t.Errorf("a later id %q does not sort after %q", later, prev)
	}

	got, err := ParseIDTime(prev)
	if err != nil {
		t.Fatalf("ParseIDTime: %v", err)
	}
	if !got.Equal(at) {
		t.Errorf("ParseIDTime = %v, want %v", got, at)
	}
	if _, err := ParseIDTime("not-a-ulid"); err == nil {
		t.Error("ParseIDTime accepted a non-ULID")
	}
}
