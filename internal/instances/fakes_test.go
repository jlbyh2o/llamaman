package instances

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Test doubles for the service.
//
// The service's collaborators are faked rather than real for one reason that is
// also a design rule: only internal/store contains SQL (D49 invariant 1), so a
// test in THIS package must not carry an INSERT to seed a foreign key. The SQL
// semantics these fakes stand in for — the generation guard, the partial unique
// indexes, the correlated subqueries behind the derived flags — are pinned
// against a real database in internal/store/instances_test.go. What is tested
// here is the SERVICE's own decisions: validation order, the writer discipline,
// what a delete does, and what `config_hash` folds in.

// fakeStore is an in-memory Store.
type fakeStore struct {
	instances map[string]model.Instance
	status    map[string]model.InstanceStatus
	starts    map[string][]model.InstanceStart
	tokens    map[string]bool // instance id → token_instances rows exist

	// failWriteAfter, when set, makes the next Write return this error after
	// running fn, so a test can prove nothing was published on a rollback.
	failWrite error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		instances: map[string]model.Instance{},
		status:    map[string]model.InstanceStatus{},
		starts:    map[string][]model.InstanceStart{},
		tokens:    map[string]bool{},
	}
}

// The fake is its own transaction: it has no lock to take and nothing to
// commit, so Write and Read simply run fn with a nil Tx.
func (f *fakeStore) Write(ctx context.Context, fn func(context.Context, store.Tx) error) error {
	if err := fn(ctx, nil); err != nil {
		return err
	}
	return f.failWrite
}

func (f *fakeStore) Read(ctx context.Context, fn func(context.Context, store.Tx) error) error {
	return fn(ctx, nil)
}

func (f *fakeStore) InsertInstance(_ context.Context, _ store.Tx, i model.Instance) error {
	if _, dup := f.instances[i.ID]; dup {
		return fmt.Errorf("duplicate instance %s", i.ID)
	}
	f.instances[i.ID] = i
	return nil
}

func (f *fakeStore) Instance(_ context.Context, _ store.Tx, id string) (model.Instance, error) {
	i, ok := f.instances[id]
	if !ok {
		return model.Instance{}, store.ErrNotFound
	}
	return i, nil
}

func (f *fakeStore) InstanceByName(_ context.Context, _ store.Tx, name string) (model.Instance, error) {
	for _, i := range f.instances {
		if i.Name == name && !i.Deleted() {
			return i, nil
		}
	}
	return model.Instance{}, store.ErrNotFound
}

func (f *fakeStore) InstanceView(_ context.Context, _ store.Tx, id string) (model.InstanceView, error) {
	i, ok := f.instances[id]
	if !ok {
		return model.InstanceView{}, store.ErrNotFound
	}
	return f.viewOf(i), nil
}

func (f *fakeStore) InstanceViews(_ context.Context, _ store.Tx, filter store.InstanceFilter) ([]model.InstanceView, error) {
	wanted := map[string]bool{}
	for _, id := range filter.IDs {
		wanted[id] = true
	}
	var out []model.InstanceView
	for _, i := range f.instances {
		if i.Deleted() && !filter.IncludeDeleted {
			continue
		}
		if len(wanted) > 0 && !wanted[i.ID] {
			continue
		}
		out = append(out, f.viewOf(i))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

// viewOf assembles the join the real query does in SQL, including the two
// `instance_starts` facts: LAST_CLOSED excluding `inhibited`, and THE_OPEN_ROW.
func (f *fakeStore) viewOf(i model.Instance) model.InstanceView {
	v := model.InstanceView{Instance: i, Status: f.status[i.ID]}
	for _, r := range f.starts[i.ID] {
		if r.Open() {
			v.OpenOverride = ptrTo(r.OverrideJSON != nil)
			continue
		}
		if *r.Outcome == model.OutcomeInhibited {
			continue
		}
		if v.LastClosedOutcome == nil {
			v.LastClosedOutcome = r.Outcome
		}
	}
	return v
}

func (f *fakeStore) UpdateInstanceConfig(_ context.Context, _ store.Tx, i model.Instance) (bool, error) {
	cur, ok := f.instances[i.ID]
	if !ok || cur.Deleted() || cur.Generation != i.Generation {
		return false, nil
	}
	i.Generation = cur.Generation + 1
	i.Autostart, i.DesiredState = cur.Autostart, cur.DesiredState
	i.PendingTrigger, i.PendingOverrideJSON = cur.PendingTrigger, cur.PendingOverrideJSON
	f.instances[i.ID] = i
	return true, nil
}

func (f *fakeStore) SetInstanceDesiredState(_ context.Context, _ store.Tx,
	id string, desired model.DesiredState, at int64) (bool, error) {
	i, ok := f.instances[id]
	if !ok {
		return false, nil
	}
	i.DesiredState, i.UpdatedAt = desired, at
	f.instances[id] = i
	return true, nil
}

func (f *fakeStore) StampPendingStart(_ context.Context, _ store.Tx,
	id string, trigger model.PendingTrigger, overrideJSON *string, at int64) (bool, error) {
	i, ok := f.instances[id]
	if !ok {
		return false, nil
	}
	i.PendingTrigger, i.PendingOverrideJSON, i.UpdatedAt = &trigger, overrideJSON, at
	f.instances[id] = i
	return true, nil
}

func (f *fakeStore) SetInstanceConfigHash(_ context.Context, _ store.Tx, id, hash string, at int64) (bool, error) {
	i, ok := f.instances[id]
	if !ok || i.Deleted() {
		return false, nil
	}
	i.ConfigHash, i.UpdatedAt = hash, at
	f.instances[id] = i
	return true, nil
}

func (f *fakeStore) SoftDeleteInstance(_ context.Context, _ store.Tx, id string, at int64) (bool, error) {
	i, ok := f.instances[id]
	if !ok || i.Deleted() {
		return false, nil
	}
	i.DeletedAt, i.DesiredState, i.UpdatedAt = &at, model.DesiredStopped, at
	f.instances[id] = i
	return true, nil
}

func (f *fakeStore) PurgeInstance(_ context.Context, _ store.Tx, id string) (bool, error) {
	if _, ok := f.instances[id]; !ok {
		return false, nil
	}
	delete(f.instances, id)
	delete(f.status, id)
	delete(f.starts, id)
	delete(f.tokens, id)
	return true, nil
}

func (f *fakeStore) DeleteTokenInstances(_ context.Context, _ store.Tx, instanceID string) error {
	delete(f.tokens, instanceID)
	return nil
}

func (f *fakeStore) InstancePortHolders(_ context.Context, _ store.Tx) ([]model.InstancePorts, error) {
	var out []model.InstancePorts
	for _, i := range f.instances {
		if i.Deleted() {
			continue
		}
		out = append(out, model.InstancePorts{
			InstanceID: i.ID, Name: i.Name, PublicPort: i.PublicPort, InternalPort: i.InternalPort,
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].InstanceID < out[b].InstanceID })
	return out, nil
}

func (f *fakeStore) InsertInstanceStatus(_ context.Context, _ store.Tx, st model.InstanceStatus) error {
	if _, dup := f.status[st.InstanceID]; dup {
		return fmt.Errorf("duplicate instance_status %s", st.InstanceID)
	}
	f.status[st.InstanceID] = st
	return nil
}

func (f *fakeStore) ClearCrashLoopLatch(_ context.Context, _ store.Tx, instanceID string, now int64) (bool, error) {
	st, ok := f.status[instanceID]
	if !ok {
		return false, nil
	}
	if st.State == model.InstanceCrashLooping {
		st.State, st.LastChangeAt = model.InstanceStopped, now
	}
	st.ReconcileBackoffUntil, st.RestartWindowResetAt = nil, now
	f.status[instanceID] = st
	return true, nil
}

func (f *fakeStore) InstanceStarts(_ context.Context, _ store.Tx, instanceID string, limit int) ([]model.InstanceStart, error) {
	rows := f.starts[instanceID]
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// fakeSettings answers the four keys the port rules read.
type fakeSettings struct {
	ints    map[string]int64
	strings map[string]string
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{
		ints: map[string]int64{
			"ui.port_desired":             5526,
			"instances.internal_port_min": 21000,
			"instances.internal_port_max": 21999,
		},
		strings: map[string]string{"gateway.bind": "0.0.0.0"},
	}
}

func (f *fakeSettings) GetInt(_ context.Context, key string) (int64, error) {
	v, ok := f.ints[key]
	if !ok {
		return 0, fmt.Errorf("unknown setting %q", key)
	}
	return v, nil
}

func (f *fakeSettings) GetString(_ context.Context, key string) (string, error) {
	v, ok := f.strings[key]
	if !ok {
		return "", fmt.Errorf("unknown setting %q", key)
	}
	return v, nil
}

// fakeResolver stands in for internal/models and internal/llamacpp.
type fakeResolver struct {
	models  map[string]ModelInfo
	runtime Runtime
	// noActive makes ActiveRuntime report that no build is active, which is an
	// ordinary state on a fresh install.
	noActive bool
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{
		models: map[string]ModelInfo{
			"m-qwen": {
				ID: "m-qwen", Path: qwenModel().Path, Kind: model.ModelText, State: model.ModelReady,
				Parsed: true, TokenizerModel: ptrTo("gpt2"), NVocab: ptrTo(int64(151936)),
			},
		},
		runtime: cudaRuntime(),
	}
}

func (f *fakeResolver) Models(_ context.Context, _ store.Tx, ids []string) (map[string]ModelInfo, error) {
	out := map[string]ModelInfo{}
	for _, id := range ids {
		if info, ok := f.models[id]; ok {
			out[id] = info
		}
	}
	return out, nil
}

func (f *fakeResolver) ActiveRuntime(context.Context, store.Tx) (Runtime, error) {
	if f.noActive {
		return Runtime{}, ErrNoActiveRuntime
	}
	return f.runtime, nil
}

// fakeEvents records what was appended and what was published, so a test can
// assert the ordering rule: append inside the transaction, publish only after
// it commits.
type fakeEvents struct {
	appended  []model.Event
	published []model.Event
}

func (f *fakeEvents) Append(_ context.Context, _ store.Tx, ev model.Event) error {
	f.appended = append(f.appended, ev)
	return nil
}

func (f *fakeEvents) Publish(ev model.Event) { f.published = append(f.published, ev) }

func (f *fakeEvents) actions() []string {
	var out []string
	for _, ev := range f.appended {
		out = append(out, ev.Action)
	}
	return out
}

// fakeDeactivator stands in for the systemd and gateway calls a delete makes.
type fakeDeactivator struct {
	called bool
	hints  []string
	err    error
}

func (f *fakeDeactivator) DeactivateInstance(context.Context, model.Instance) ([]string, error) {
	f.called = true
	return f.hints, f.err
}

// harness is one wired Service plus the doubles behind it.
type harness struct {
	svc      *Service
	store    *fakeStore
	settings *fakeSettings
	resolver *fakeResolver
	events   *fakeEvents
	deact    *fakeDeactivator

	clock int64
	seq   int
}

func newHarness(t interface{ Fatalf(string, ...any) }) *harness {
	h := &harness{
		store:    newFakeStore(),
		settings: newFakeSettings(),
		resolver: newFakeResolver(),
		events:   &fakeEvents{},
		deact:    &fakeDeactivator{},
		clock:    1_700_000_000_000,
	}
	svc, err := New(Config{
		Store:       h.store,
		Settings:    h.settings,
		Resolver:    h.resolver,
		Events:      h.events,
		Deactivator: h.deact,
		Probe:       freeProbe,
		Now: func() time.Time {
			h.clock += 1000
			return time.UnixMilli(h.clock)
		},
		NewID: func(time.Time) string {
			h.seq++
			return fmt.Sprintf("01J%029d", h.seq)
		},
	})
	if err != nil {
		t.Fatalf("instances.New: %v", err)
	}
	h.svc = svc
	return h
}

// create is the common "make an instance" call, with the fields most tests do
// not care about filled in.
func (h *harness) create(ctx context.Context, name string, mutate func(*CreateParams)) (View, error) {
	p := CreateParams{
		Name:    name,
		ModelID: "m-qwen",
		Flags:   docExampleFlags(),
	}
	if mutate != nil {
		mutate(&p)
	}
	return h.svc.Create(ctx, p)
}

// errCodeIs reports whether err is a model.Error carrying code.
func errCodeIs(err error, code model.ErrorCode) bool {
	var me model.Error
	return errors.As(err, &me) && me.Code == code
}

// containsHint reports whether any hint mentions the substring.
func containsHint(hints []string, want string) bool {
	for _, h := range hints {
		if strings.Contains(h, want) {
			return true
		}
	}
	return false
}
