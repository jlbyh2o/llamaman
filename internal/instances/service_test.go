package instances

import (
	"context"
	"errors"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Service tests (DESIGN sections 2.8, 3.10, 3.10a, 3.10c).

// TestCreateWritesBothRowsInOneTransaction is §2.8's row lifecycle: the status
// row is created WITH the instance, which is what lets every reader use an inner
// join and what lets the derived flags reason about a new instance as
// `state='unknown'` rather than about an absent row.
func TestCreateWritesBothRowsInOneTransaction(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	v, err := h.create(ctx, "qwen", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if v.Status.State != model.InstanceUnknown {
		t.Errorf("state = %q, want unknown", v.Status.State)
	}
	if v.Status.RestartWindowResetAt != 0 {
		t.Errorf("restart_window_reset_at = %d, want 0", v.Status.RestartWindowResetAt)
	}
	if _, ok := h.store.status[v.ID]; !ok {
		t.Fatal("no instance_status row was written")
	}
	if v.Generation != 1 {
		t.Errorf("generation = %d, want 1", v.Generation)
	}
	if v.DesiredState != model.DesiredStopped {
		t.Errorf("desired_state = %q, want stopped — creating an instance does not start it", v.DesiredState)
	}
	if v.UnitName != "llamaman-instance@qwen.service" {
		t.Errorf("unit_name = %q", v.UnitName)
	}
	if v.ConfigHash == "" {
		t.Error("config_hash was not stamped")
	}
	if v.AuthMode != model.AuthToken {
		t.Errorf("auth_mode = %q, want the schema's default", v.AuthMode)
	}
	if v.RestartPolicy != model.RestartOnFailure || v.RestartMax != 5 || v.RestartWindowSec != 600 {
		t.Errorf("the restart defaults do not match the schema: %+v", v.Instance)
	}

	// One event, appended and then published — in that order.
	if got := h.events.actions(); len(got) != 1 || got[0] != "instance_created" {
		t.Errorf("appended events = %v, want one instance_created", got)
	}
	if len(h.events.published) != 1 {
		t.Errorf("published %d frames, want 1", len(h.events.published))
	}

	// And the four derived flags are all false for something that never ran.
	if v.Derived != (model.DerivedFlags{}) {
		t.Errorf("a new instance carries derived flags: %+v", v.Derived)
	}
}

// TestCreateAllocatesBothPorts is §3.10's "ports auto-allocated when omitted".
func TestCreateAllocatesBothPorts(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	first, err := h.create(ctx, "qwen", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if first.PublicPort != DefaultPublicPortBase {
		t.Errorf("public_port = %d, want %d", first.PublicPort, DefaultPublicPortBase)
	}
	if first.InternalPort != 21000 {
		t.Errorf("internal_port = %d, want the bottom of the pool", first.InternalPort)
	}

	second, err := h.create(ctx, "gemma", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if second.PublicPort == first.PublicPort || second.InternalPort == first.InternalPort {
		t.Errorf("the second instance reused a port: %d/%d", second.PublicPort, second.InternalPort)
	}
}

// TestCreateRefusals walks the save-time guards in the order a form hits them.
func TestCreateRefusals(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name   string
		mutate func(*CreateParams)
		want   model.ErrorCode
	}{
		{
			name:   "a name that is not a legal unit id",
			mutate: func(p *CreateParams) { p.Name = "Qwen 3" },
			want:   model.CodeInstanceNameInvalid,
		},
		{
			name:   "no model",
			mutate: func(p *CreateParams) { p.ModelID = "" },
			want:   model.CodeModelMissing,
		},
		{
			name:   "a model that does not exist",
			mutate: func(p *CreateParams) { p.ModelID = "m-nope" },
			want:   model.CodeModelMissing,
		},
		{
			name: "ngl auto with a tensor split",
			mutate: func(p *CreateParams) {
				p.Flags.NGpuLayers = &model.NGpuLayers{Mode: model.NGLAuto}
				p.Flags.TensorSplit = []float64{0.5, 0.5}
			},
			want: model.CodeNGLAutoConflict,
		},
		{
			name:   "extra_flags overriding the listener",
			mutate: func(p *CreateParams) { p.ExtraFlags = "--port 9999" },
			want:   model.CodeExtraFlagForbidden,
		},
		{
			name:   "a public port the management UI owns",
			mutate: func(p *CreateParams) { p.PublicPort = ptr(5526) },
			want:   model.CodePortUnavailable,
		},
		{
			name:   "an internal port outside the pool",
			mutate: func(p *CreateParams) { p.InternalPort = ptr(9000) },
			want:   model.CodePortUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			_, err := h.create(ctx, "qwen", tt.mutate)
			if !errCodeIs(err, tt.want) {
				t.Fatalf("Create = %v, want code %q", err, tt.want)
			}
			if len(h.store.instances) != 0 {
				t.Error("a refused create still wrote a row")
			}
			if len(h.events.published) != 0 {
				t.Error("a refused create published an event")
			}
		})
	}
}

// TestCreateRefusesADuplicateName, and TestSoftDeleteFreesTheName below, are the
// two halves of D68's promise at the service level.
func TestCreateRefusesADuplicateName(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	if _, err := h.create(ctx, "qwen", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err := h.create(ctx, "qwen", nil)
	if !errCodeIs(err, model.CodeInstanceNameTaken) {
		t.Fatalf("the duplicate name = %v, want %q", err, model.CodeInstanceNameTaken)
	}
}

// TestSoftDeleteFreesTheNameAndPortsForReuse is D68 through the service: the
// name and both ports are reusable the instant `deleted_at` is stamped, and the
// history is kept.
func TestSoftDeleteFreesTheNameAndPortsForReuse(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	first, err := h.create(ctx, "qwen", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	result, err := h.svc.Delete(ctx, first.ID, DeleteParams{})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if result.Purged {
		t.Error("the default delete purged")
	}
	if !h.deact.called {
		t.Error("the delete did not stop and disable the unit (§3.10c step 1)")
	}
	if _, ok := h.store.instances[first.ID]; !ok {
		t.Fatal("a soft delete removed the row; the accounting is a product feature (D68)")
	}

	// The same name and both ports, immediately.
	second, err := h.create(ctx, "qwen", func(p *CreateParams) {
		p.PublicPort = ptr(first.PublicPort)
		p.InternalPort = ptr(first.InternalPort)
	})
	if err != nil {
		t.Fatalf("reusing a deleted instance's name and ports: %v", err)
	}
	if second.ID == first.ID {
		t.Error("the new instance reused the old row rather than being a new one")
	}

	// And the deleted one is out of the default listing but visible with the
	// flag (§3.10c).
	live, err := h.svc.List(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ID != second.ID {
		t.Errorf("the default listing returned %d rows, want only the live one", len(live))
	}
	all, err := h.svc.List(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("?include_deleted=true returned %d rows, want 2", len(all))
	}
}

// TestDeleteHintsWhenTheUnitCannotBeDisabled is §3.10c's best-effort rule: a
// skipped or denied systemd call never fails the delete — it hands back the
// exact manual command instead.
func TestDeleteHintsWhenTheUnitCannotBeDisabled(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.deact.err = errors.New("polkit denied manage-unit-files")

	v, err := h.create(ctx, "qwen", func(p *CreateParams) { p.Autostart = ptr(true) })
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	result, err := h.svc.Delete(ctx, v.ID, DeleteParams{})
	if err != nil {
		t.Fatalf("a denied DisableUnitFiles failed the delete: %v", err)
	}
	if !containsHint(result.Hints, "systemctl disable llamaman-instance@qwen.service") {
		t.Errorf("hints = %v, want the manual disable command", result.Hints)
	}
	if !h.store.instances[v.ID].Deleted() {
		t.Error("the row was not soft-deleted")
	}
}

// TestPurgeIsTheExplicitHardDelete.
func TestPurgeIsTheExplicitHardDelete(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	v, err := h.create(ctx, "qwen", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	result, err := h.svc.Delete(ctx, v.ID, DeleteParams{Purge: true})
	if err != nil {
		t.Fatalf("Delete(purge): %v", err)
	}
	if !result.Purged {
		t.Error("the result does not report the purge")
	}
	if _, ok := h.store.instances[v.ID]; ok {
		t.Error("the row survived a purge")
	}
	if _, ok := h.store.status[v.ID]; ok {
		t.Error("instance_status did not cascade")
	}
	if got := h.events.actions(); got[len(got)-1] != "instance_purged" {
		t.Errorf("last event = %q, want instance_purged", got[len(got)-1])
	}
}

// TestDeleteKeepsTokensOnRequest is `?keep_tokens=true`.
func TestDeleteKeepsTokensOnRequest(t *testing.T) {
	ctx := context.Background()

	for _, keep := range []bool{false, true} {
		h := newHarness(t)
		v, err := h.create(ctx, "qwen", nil)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		h.store.tokens[v.ID] = true

		if _, err := h.svc.Delete(ctx, v.ID, DeleteParams{KeepTokens: keep}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if got := h.store.tokens[v.ID]; got != keep {
			t.Errorf("keep_tokens=%v left the scope rows = %v", keep, got)
		}
	}
}

// TestPatchGenerationGuard is §3's optimistic concurrency, from the handler's
// side: the code a stale form gets back is the one the UI branches on.
func TestPatchGenerationGuard(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	v, err := h.create(ctx, "qwen", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated, err := h.svc.Patch(ctx, v.ID, PatchParams{
		Generation:  v.Generation,
		Description: ptr(ptr("the everyday model")),
	})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if updated.Generation != 2 {
		t.Errorf("generation = %d, want 2", updated.Generation)
	}

	_, err = h.svc.Patch(ctx, v.ID, PatchParams{Generation: v.Generation, Name: ptr("qwen2")})
	if !errCodeIs(err, model.CodeConflictGeneration) {
		t.Fatalf("a stale PATCH = %v, want %q", err, model.CodeConflictGeneration)
	}
}

// TestPatchMovesConfigHashOnlyWhenTheConfigurationMoved is D52 at the service
// level: editing a flag moves the hash, and moving the internal port does not.
func TestPatchMovesConfigHashOnlyWhenTheConfigurationMoved(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	v, err := h.create(ctx, "qwen", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	movedPort, err := h.svc.Patch(ctx, v.ID, PatchParams{
		Generation:   v.Generation,
		InternalPort: ptr(21500),
	})
	if err != nil {
		t.Fatalf("Patch(internal_port): %v", err)
	}
	if movedPort.ConfigHash != v.ConfigHash {
		t.Error("moving the internal port moved config_hash; restart_required would flap (D52)")
	}

	flags := docExampleFlags()
	flags.CtxSize = ptr(4096)
	movedFlag, err := h.svc.Patch(ctx, movedPort.ID, PatchParams{
		Generation: movedPort.Generation,
		Flags:      &flags,
	})
	if err != nil {
		t.Fatalf("Patch(flags): %v", err)
	}
	if movedFlag.ConfigHash == v.ConfigHash {
		t.Error("editing ctx_size left config_hash alone")
	}
}

// TestPatchRefusesADeletedInstance: a soft-deleted row is not editable, and the
// answer is the same 409 a stale form gets rather than a silent success.
func TestPatchRefusesADeletedInstance(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	v, err := h.create(ctx, "qwen", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := h.svc.Delete(ctx, v.ID, DeleteParams{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := h.svc.Patch(ctx, v.ID, PatchParams{Generation: v.Generation}); err == nil {
		t.Fatal("a deleted instance was edited")
	}
}

// TestDraftValidationIsThreeValued is D34 through the service, including the
// deferred case the "queue the download, configure the instance" flow depends
// on.
func TestDraftValidationIsThreeValued(t *testing.T) {
	ctx := context.Background()

	t.Run("matching vocabularies", func(t *testing.T) {
		h := newHarness(t)
		h.resolver.models["m-draft"] = ModelInfo{
			ID: "m-draft", Path: "/cache/draft.gguf", Parsed: true,
			TokenizerModel: ptrTo("gpt2"), NVocab: ptrTo(int64(151936)),
		}
		v, err := h.create(ctx, "qwen", func(p *CreateParams) { p.DraftModelID = ptr("m-draft") })
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if v.DraftValidation != model.DraftOK {
			t.Errorf("draft_validation = %q, want ok", v.DraftValidation)
		}
		if len(v.Warnings) != 0 {
			t.Errorf("warnings = %v, want none", v.Warnings)
		}
	})

	t.Run("a mismatch is refused and nothing is saved", func(t *testing.T) {
		h := newHarness(t)
		h.resolver.models["m-draft"] = ModelInfo{
			ID: "m-draft", Path: "/cache/draft.gguf", Parsed: true,
			TokenizerModel: ptrTo("llama"), NVocab: ptrTo(int64(32000)),
		}
		_, err := h.create(ctx, "qwen", func(p *CreateParams) { p.DraftModelID = ptr("m-draft") })
		if !errCodeIs(err, model.CodeDraftVocabMismatch) {
			t.Fatalf("Create = %v, want %q", err, model.CodeDraftVocabMismatch)
		}
		if len(h.store.instances) != 0 {
			t.Error("a mismatch still wrote a row")
		}
	})

	t.Run("an unparsed draft model defers the check", func(t *testing.T) {
		h := newHarness(t)
		h.resolver.models["m-draft"] = ModelInfo{ID: "m-draft", State: model.ModelDownloading}
		v, err := h.create(ctx, "qwen", func(p *CreateParams) { p.DraftModelID = ptr("m-draft") })
		if err != nil {
			t.Fatalf("configuring against a model that is still downloading was refused: %v", err)
		}
		if v.DraftValidation != model.DraftDeferred {
			t.Errorf("draft_validation = %q, want deferred", v.DraftValidation)
		}
		if !v.Derived.DraftUnverified {
			t.Error("the draft_unverified badge is not raised")
		}
		if len(v.Warnings) != 1 || v.Warnings[0].Code != model.WarnDraftVocabUnverified {
			t.Errorf("warnings = %v, want draft_vocab_unverified", v.Warnings)
		}
	})
}

// TestCreateAgainstAModelThatIsStillDownloading is the flow §3.10a calls out:
// the instance saves, gets a stable `config_hash`, and simply has no command
// line to show yet.
func TestCreateAgainstAModelThatIsStillDownloading(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.resolver.models["m-pending"] = ModelInfo{ID: "m-pending", State: model.ModelDownloading}

	v, err := h.create(ctx, "qwen", func(p *CreateParams) { p.ModelID = "m-pending" })
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if v.ConfigHash == "" {
		t.Fatal("config_hash is NOT NULL; an unrenderable configuration still needs one")
	}

	detail, err := h.svc.Get(ctx, v.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if detail.Argv != nil {
		t.Errorf("argv = %v, want none until the model is on disk", detail.Argv)
	}

	// When the download completes, the models service recomputes through the
	// one method (D69) and the hash moves to the real path.
	h.resolver.models["m-pending"] = ModelInfo{ID: "m-pending", Path: "/cache/real.gguf"}
	if err := h.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return h.svc.RecomputeConfigHash(ctx, tx, v.ID)
	}); err != nil {
		t.Fatalf("RecomputeConfigHash: %v", err)
	}

	after := h.store.instances[v.ID]
	if after.ConfigHash == v.ConfigHash {
		t.Error("the resolved path did not move config_hash; restart_required would never fire")
	}
	if after.Generation != v.Generation {
		t.Error("RecomputeConfigHash bumped generation; an open edit form must survive it (D69)")
	}
}

// TestRecomputeConfigHashOnActivation is D69's other caller: a version flip
// changes an input for every instance at once, and the applied hash is
// deliberately left alone so `restart_required` lights up.
func TestRecomputeConfigHashOnActivation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	a, err := h.create(ctx, "qwen", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	b, err := h.create(ctx, "gemma", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Both are serving the configuration they were saved with.
	for _, id := range []string{a.ID, b.ID} {
		st := h.store.status[id]
		st.State = model.InstanceReady
		st.AppliedConfigHash = ptrTo(h.store.instances[id].ConfigHash)
		h.store.status[id] = st
	}

	h.resolver.runtime.ID = "b10700-cuda-src"
	if err := h.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return h.svc.RecomputeConfigHash(ctx, tx)
	}); err != nil {
		t.Fatalf("RecomputeConfigHash: %v", err)
	}

	views, err := h.svc.List(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("listed %d instances, want 2", len(views))
	}
	for _, v := range views {
		if !v.Derived.RestartRequired {
			t.Errorf("instance %s does not report restart_required after a version flip", v.Name)
		}
		if v.Status.AppliedConfigHash == nil {
			t.Errorf("instance %s lost applied_config_hash; that is what the flag compares", v.Name)
		}
	}
}

// TestSetDesiredState is the desired-state API: it writes the desired axis and
// stamps the trigger, and does NOT touch systemd — the supervisor reconciles.
func TestSetDesiredState(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	v, err := h.create(ctx, "qwen", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	started, err := h.svc.SetDesiredState(ctx, v.ID, model.DesiredRunning, model.TriggerUser)
	if err != nil {
		t.Fatalf("SetDesiredState(running): %v", err)
	}
	if started.DesiredState != model.DesiredRunning {
		t.Errorf("desired_state = %q, want running", started.DesiredState)
	}
	row := h.store.instances[v.ID]
	if row.PendingTrigger == nil || *row.PendingTrigger != model.TriggerUser {
		t.Errorf("pending_trigger = %v, want user — the launcher records `external` without it", row.PendingTrigger)
	}
	if row.Generation != v.Generation {
		t.Error("starting an instance bumped generation")
	}
	if row.PendingOverrideJSON != nil {
		t.Error("an ordinary start wrote a transient override")
	}

	stopped, err := h.svc.SetDesiredState(ctx, v.ID, model.DesiredStopped, "")
	if err != nil {
		t.Fatalf("SetDesiredState(stopped): %v", err)
	}
	if stopped.DesiredState != model.DesiredStopped {
		t.Errorf("desired_state = %q, want stopped", stopped.DesiredState)
	}

	// Three writes, three events.
	if got := len(h.events.appended); got != 3 {
		t.Errorf("appended %d events, want 3 (create, start, stop)", got)
	}
}

// TestSetDesiredStateRefusesADeletedInstance: a soft-deleted instance is not
// startable, and 404 is what the handler turns store.ErrNotFound into.
func TestSetDesiredStateRefusesADeletedInstance(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	v, err := h.create(ctx, "qwen", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := h.svc.Delete(ctx, v.ID, DeleteParams{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := h.svc.SetDesiredState(ctx, v.ID, model.DesiredRunning, model.TriggerUser); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("starting a deleted instance = %v, want ErrNotFound", err)
	}
}

// TestGetRendersTheCommandLine is §3.10's detail view.
func TestGetRendersTheCommandLine(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.resolver.runtime.Help = model.HelpFlags{"-m", "-c", "-ngl", "-b", "-ub", "-np", "-fa",
		"-ctk", "-ctv", "--alias", "--jinja", "--host", "--port", "--no-webui",
		"--props", "--slots", "--metrics"}

	v, err := h.create(ctx, "qwen", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	detail, err := h.svc.Get(ctx, v.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(detail.Argv) == 0 {
		t.Fatal("the detail view carries no rendered argv")
	}
	if got := valueAfter(detail.Argv, "--port"); got != "21000" {
		t.Errorf("--port = %q, want the allocated internal port", got)
	}
	if len(detail.UnknownFlags) != 0 {
		t.Errorf("unknown flags = %v, want none against a complete help capture", detail.UnknownFlags)
	}
	if len(detail.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", detail.Warnings)
	}
}

// TestGetWarnsAboutFlagChurn is §5.7's guard, which is a WARNING and never an
// error: llama.cpp ships ~10 nightlies a day.
func TestGetWarnsAboutFlagChurn(t *testing.T) {
	ctx := context.Background()

	t.Run("a flag this build does not advertise", func(t *testing.T) {
		h := newHarness(t)
		h.resolver.runtime.Help = model.HelpFlags{"-m", "--host", "--port"}

		v, err := h.create(ctx, "qwen", nil)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		detail, err := h.svc.Get(ctx, v.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(detail.UnknownFlags) == 0 {
			t.Fatal("no unknown flags were reported against a nearly empty help capture")
		}
		if len(detail.Warnings) == 0 || detail.Warnings[0].Code != model.WarnUnknownFlags {
			t.Errorf("warnings = %v, want unknown_flags", detail.Warnings)
		}
	})

	t.Run("a build with no help capture", func(t *testing.T) {
		h := newHarness(t)
		v, err := h.create(ctx, "qwen", nil)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		detail, err := h.svc.Get(ctx, v.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(detail.UnknownFlags) != 0 {
			t.Errorf("unknown flags = %v; a missing capture makes the check unavailable, "+
				"not universally failing", detail.UnknownFlags)
		}
		if len(detail.Warnings) != 1 || detail.Warnings[0].Code != model.WarnFlagCheckUnavailable {
			t.Errorf("warnings = %v, want flag_check_unavailable", detail.Warnings)
		}
	})

	t.Run("a build that predates --fit", func(t *testing.T) {
		h := newHarness(t)
		h.resolver.runtime.SupportsFit = false

		v, err := h.create(ctx, "qwen", func(p *CreateParams) {
			p.Flags.NGpuLayers = &model.NGpuLayers{Mode: model.NGLAuto}
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		detail, err := h.svc.Get(ctx, v.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !hasWarning(detail.Warnings, model.WarnNGLAutoWithoutFit) {
			t.Errorf("warnings = %v, want ngl_auto_without_fit", detail.Warnings)
		}
		if got := valueAfter(detail.Argv, "-ngl"); got != "999" {
			t.Errorf("-ngl = %q, want 999 — auto behaves as all on a build without --fit", got)
		}
	})
}

// TestCreateWithoutAnActiveRuntime: an instance may be created before llama.cpp
// is installed, and activation recomputes every hash when it happens (D69).
func TestCreateWithoutAnActiveRuntime(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.resolver.noActive = true

	v, err := h.create(ctx, "qwen", nil)
	if err != nil {
		t.Fatalf("creating an instance before llama.cpp is installed was refused: %v", err)
	}
	if v.ConfigHash == "" {
		t.Error("config_hash was not stamped")
	}

	detail, err := h.svc.Get(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Argv != nil {
		t.Error("a command line was rendered against no build at all")
	}

	h.resolver.noActive = false
	if err := h.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return h.svc.RecomputeConfigHash(ctx, tx)
	}); err != nil {
		t.Fatalf("RecomputeConfigHash: %v", err)
	}
	if h.store.instances[v.ID].ConfigHash == v.ConfigHash {
		t.Error("activating the first build did not move config_hash")
	}
}

// TestSuggestPortSkipsWhatIsTaken.
func TestSuggestPortSkipsWhatIsTaken(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	if _, err := h.create(ctx, "qwen", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}

	pub, err := h.svc.SuggestPort(ctx, PortPublic)
	if err != nil {
		t.Fatalf("SuggestPort(public): %v", err)
	}
	if pub != DefaultPublicPortBase+1 {
		t.Errorf("public suggestion = %d, want the next one after the instance's", pub)
	}

	if _, err := h.svc.SuggestPort(ctx, "sideways"); !errCodeIs(err, model.CodePortUnavailable) {
		t.Errorf("an unknown kind = %v, want a refusal", err)
	}
}

func hasWarning(warnings []model.Warning, code model.WarningCode) bool {
	for _, w := range warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// TestDeleteIsIdempotent: a retried delete must not append a second "deleted"
// line to an instance's history, and a purge after a soft delete must still
// work — discarding the history is a different operation from deleting the
// instance.
func TestDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	v, err := h.create(ctx, "qwen", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := h.svc.Delete(ctx, v.ID, DeleteParams{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	before := len(h.events.appended)

	if _, err := h.svc.Delete(ctx, v.ID, DeleteParams{}); err != nil {
		t.Fatalf("the second delete failed: %v", err)
	}
	if got := len(h.events.appended); got != before {
		t.Errorf("the second delete appended %d more events", got-before)
	}

	result, err := h.svc.Delete(ctx, v.ID, DeleteParams{Purge: true})
	if err != nil {
		t.Fatalf("purging a soft-deleted instance: %v", err)
	}
	if !result.Purged {
		t.Error("the purge did not run against an already soft-deleted instance")
	}
	if _, ok := h.store.instances[v.ID]; ok {
		t.Error("the row survived the purge")
	}
}
