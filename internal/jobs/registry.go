package jobs

import (
	"context"
	"fmt"
	"sync"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Registry maps a `jobs.kind` to the one Worker that runs it (DESIGN section 1).
//
// It is closed by construction in two directions, and both matter. A kind may be
// registered only once, because two workers for one kind would mean two
// subsystems moving the same domain row — the drift §2.3a exists to prevent. And
// the queue leases only the kinds registered here, so a daemon that has not
// wired a worker never claims work it cannot run: the row stays `queued` until a
// daemon that can run it arrives, rather than being burned through its attempt
// budget by one that cannot.
type Registry struct {
	mu     sync.RWMutex
	byKind map[model.JobKind]Worker
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byKind: make(map[model.JobKind]Worker, len(model.JobKindValues()))}
}

// Register adds w under w.Kind(). It refuses a kind outside the `jobs.kind`
// CHECK constraint and a kind that already has a worker; both are composition
// bugs that a test in the composition root should catch, not conditions a
// running daemon should paper over.
func (r *Registry) Register(w Worker) error {
	if w == nil {
		return fmt.Errorf("jobs: cannot register a nil Worker")
	}
	kind := w.Kind()
	if !kind.Valid() {
		return fmt.Errorf("jobs: %w: %q", errUnknownKind, kind)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byKind[kind]; dup {
		return fmt.Errorf("jobs: a worker for kind %q is already registered", kind)
	}
	r.byKind[kind] = w
	return nil
}

// Worker returns the worker for a kind, if one is registered.
func (r *Registry) Worker(kind model.JobKind) (Worker, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	w, ok := r.byKind[kind]
	return w, ok
}

// Kinds lists the registered kinds in the order of the `jobs.kind` CHECK
// constraint, which makes the lease query's `kind IN (…)` argument list stable
// across calls — a map iteration would reorder the placeholders on every poll
// and defeat the statement cache for no reason.
func (r *Registry) Kinds() []model.JobKind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]model.JobKind, 0, len(r.byKind))
	for _, kind := range model.JobKindValues() {
		if _, ok := r.byKind[kind]; ok {
			out = append(out, kind)
		}
	}
	return out
}

// Len is the number of registered workers.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byKind)
}

// domainWriter returns the DomainWriter for a kind — the hook the queue uses for
// the three transitions it performs with no worker running: boot triage, a
// cancel of a job no worker holds, and a retry (§2.3, §2.3a).
func (q *Queue) domainWriter(kind model.JobKind) (DomainWriter, bool) {
	w, ok := q.reg.Worker(kind)
	if !ok {
		return nil, false
	}
	dw, ok := w.(DomainWriter)
	return dw, ok
}

// cancelGuard returns the CancelGuard for a kind, for the two kinds that carry a
// cut-off rather than a blanket accept (§3.14, D96).
func (q *Queue) cancelGuard(kind model.JobKind) (CancelGuard, bool) {
	w, ok := q.reg.Worker(kind)
	if !ok {
		return nil, false
	}
	g, ok := w.(CancelGuard)
	return g, ok
}

// commitDomain moves the domain row alongside a job transition the queue made on
// a worker's behalf. A kind with no registered DomainWriter writes nothing,
// which is correct for `maintenance` — whose job row IS the record (§2.3a) — and
// is the reason this is not an error.
func (q *Queue) commitDomain(ctx context.Context, tx store.Tx, j model.Job, state model.JobState) error {
	dw, ok := q.domainWriter(j.Kind)
	if !ok {
		return nil
	}
	return dw.SetDomainState(ctx, tx, j, state)
}
