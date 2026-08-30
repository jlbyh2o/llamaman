package llamacpp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/github"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/prebuilt"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/source"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// TestInstallBranches walks D71's five-row table (§6.2), which is the whole
// contract of `POST /llamacpp/versions` for an id that already exists. Every row
// of that table is a case here, because "UNIQUE constraint failed" is not a
// user-facing answer and each branch is the answer that replaced it.
func TestInstallBranches(t *testing.T) {
	t.Parallel()

	// One resolver for every case: the nightly channel resolving to b10621 with
	// no prebuilt asset, which makes the acquisition a source build (§6.3's
	// "otherwise") and the id `b10621-cpu-src`.
	resolver := fakeResolver{res: github.Resolution{
		Channel: model.ChannelNightly, Tag: "b10621",
	}}
	req := InstallRequest{Channel: model.ChannelNightly, Tag: "b10621"}
	const wantID = "b10621-cpu-src"

	t.Run("no row inserts pending and enqueues", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, func(c *Config) { c.Releases = resolver })

		res, err := f.svc.Install(context.Background(), req)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if res.Version.ID != wantID {
			t.Errorf("version id = %q, want %q — D60's identity is three-part", res.Version.ID, wantID)
		}
		if res.Version.State != model.VersionPending {
			t.Errorf("state = %q, want pending", res.Version.State)
		}
		if res.Job.ID == "" || res.Reused {
			t.Errorf("result = %+v, want a queued job and reused=false", res)
		}
		if f.job(res.Job.ID).Kind != model.JobLlamacppInstall {
			t.Errorf("job kind = %q, want llamacpp_install", f.job(res.Job.ID).Kind)
		}
	})

	t.Run("live row is build_in_flight", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, func(c *Config) { c.Releases = resolver })
		if _, err := f.svc.Install(context.Background(), req); err != nil {
			t.Fatalf("first Install: %v", err)
		}

		_, err := f.svc.Install(context.Background(), req)
		assertCode(t, err, CodeBuildInFlight)
	})

	t.Run("ready row with matching options is reused", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, func(c *Config) { c.Releases = resolver })
		f.seedVersion(wantID, model.VersionReady)

		res, err := f.svc.Install(context.Background(), req)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if !res.Reused || res.Job.ID != "" {
			t.Errorf("result = %+v, want reused with no job", res)
		}
	})

	t.Run("ready row with different options is version_options_differ", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, func(c *Config) { c.Releases = resolver })
		f.seedVersion(wantID, model.VersionReady)
		// The seeded row carries no build options at all, so any request that
		// names one differs from it.
		differing := req
		differing.CMakeExtra = []string{"-DGGML_AVX512=ON"}

		_, err := f.svc.Install(context.Background(), differing)
		assertCode(t, err, CodeVersionOptionsDiffer)
	})

	t.Run("terminal failure is reused and reset", func(t *testing.T) {
		t.Parallel()
		for _, state := range []model.VersionState{
			model.VersionFailed, model.VersionFailedVerification,
			model.VersionCanceled, model.VersionDeleted,
		} {
			f := newFixture(t, func(c *Config) { c.Releases = resolver })
			f.seedVersion(wantID, state)

			res, err := f.svc.Install(context.Background(), req)
			if err != nil {
				t.Fatalf("Install over %s: %v", state, err)
			}
			if res.Version.State != model.VersionPending {
				t.Errorf("state after reset from %s = %q, want pending",
					state, res.Version.State)
			}
			if res.Job.ID == "" {
				t.Errorf("reset from %s enqueued no job", state)
			}
		}
	})

	t.Run("force_rebuild is refused while a process is executing from it", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, func(c *Config) { c.Releases = resolver })
		f.seedVersion(wantID, model.VersionReady)
		f.guard.inUse, f.guard.pid = true, 4242

		forced := req
		forced.ForceRebuild = true
		_, err := f.svc.Install(context.Background(), forced)
		assertCode(t, err, CodeVersionInUse)
	})
}

// TestInstallJobStatePairs asserts §2.3a's invariant table for
// `kind = llamacpp_install`: every `jobs.state` is paired with the
// `llamacpp_versions.state` that row names, written by the same transaction.
func TestInstallJobStatePairs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		build       func(*fakeSource)
		wantJob     model.JobState
		wantVersion model.VersionState
	}{
		{
			name:        "a build that installs is succeeded and ready",
			build:       func(s *fakeSource) {},
			wantJob:     model.JobSucceeded,
			wantVersion: model.VersionReady,
		},
		{
			name: "a build that fails a phase is failed and failed",
			build: func(s *fakeSource) {
				s.err = errors.New("cmake exited 1")
			},
			wantJob:     model.JobFailed,
			wantVersion: model.VersionFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, func(c *Config) {
				c.Releases = fakeResolver{res: github.Resolution{
					Channel: model.ChannelNightly, Tag: "b10621",
				}}
			})
			builder := &fakeSource{}
			tc.build(builder)
			f.registerInstall(builder)

			res, err := f.svc.Install(context.Background(),
				InstallRequest{Channel: model.ChannelNightly, Tag: "b10621"})
			if err != nil {
				t.Fatalf("Install: %v", err)
			}
			// `queued` pairs with `pending`, before anything runs.
			if got := f.version(res.Version.ID).State; got != model.VersionPending {
				t.Fatalf("queued job pairs with version state %q, want pending", got)
			}

			f.runOne()

			if got := f.job(res.Job.ID).State; got != tc.wantJob {
				t.Errorf("job state = %q, want %q", got, tc.wantJob)
			}
			if got := f.version(res.Version.ID).State; got != tc.wantVersion {
				t.Errorf("version state = %q, want %q", got, tc.wantVersion)
			}
		})
	}
}

// TestBuildLeaseIsReleasedOnEveryTerminalState is D70's other half: a lease held
// by a job that is over would make the next build wait out its expiry for no
// reason, and the next boot's release is a backstop rather than the mechanism.
func TestBuildLeaseIsReleasedOnEveryTerminalState(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		build *fakeSource
	}{
		{name: "success", build: &fakeSource{}},
		{name: "failure", build: &fakeSource{err: errors.New("compile failed")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, func(c *Config) {
				c.Releases = fakeResolver{res: github.Resolution{
					Channel: model.ChannelNightly, Tag: "b10621",
				}}
			})
			f.registerInstall(tc.build)

			if _, err := f.svc.Install(context.Background(),
				InstallRequest{Channel: model.ChannelNightly, Tag: "b10621"}); err != nil {
				t.Fatalf("Install: %v", err)
			}
			f.runOne()

			var lease store.BuildLease
			if err := f.store.Read(context.Background(),
				func(ctx context.Context, tx store.Tx) error {
					var err error
					lease, err = f.store.BuildLease(ctx, tx)
					return err
				}); err != nil {
				t.Fatalf("read the build lease: %v", err)
			}
			if lease.Held() {
				t.Errorf("the build lease is still held after a terminal state: %+v", lease)
			}
		})
	}
}

// TestSecondBuildWaitsForTheLease is D70 itself: two `llamacpp_install` jobs on
// two DIFFERENT ids are legal under `idx_jobs_one_live_per_subject`, so only the
// singleton row can express "one build at a time". The second job must go back
// to `queued` — a queue, which is what a user expects — rather than fail.
func TestSecondBuildWaitsForTheLease(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	f.registerInstall(&fakeSource{})
	first := f.seedVersion("b10621-cpu-src", model.VersionPending)
	second := f.seedVersion("b10622-cpu-src", model.VersionPending)

	ctx := context.Background()
	// The holder is a real job row — `build_lease.job_id` references `jobs(id)` —
	// parked an hour out so the runner cannot claim it, and owned by ANOTHER
	// boot, which is exactly the case an in-process mutex cannot see.
	holder, err := f.queue.Enqueue(ctx, jobs.EnqueueParams{
		Kind:     model.JobLlamacppInstall,
		DomainID: first.ID,
		RunAfter: f.clock.Now().Add(time.Hour),
		Params: installParams{
			VersionID: first.ID, Tag: first.Tag,
			Backend: model.BackendCPU, Acquisition: model.AcquisitionSource,
		},
	})
	if err != nil {
		t.Fatalf("enqueue the holding build: %v", err)
	}
	if err := f.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		ok, err := f.store.AcquireBuildLease(ctx, tx, holder.Job.ID, first.ID,
			"another-boot", f.clock.Now().UnixMilli(),
			f.clock.Now().Add(DefaultLeaseTTL).UnixMilli())
		if err == nil && !ok {
			t.Fatal("the fixture could not take a free build lease")
		}
		return err
	}); err != nil {
		t.Fatalf("hold the build lease: %v", err)
	}

	res, err := f.queue.Enqueue(ctx, jobs.EnqueueParams{
		Kind:     model.JobLlamacppInstall,
		DomainID: second.ID,
		Params: installParams{
			VersionID: second.ID, Tag: second.Tag,
			Backend: model.BackendCPU, Acquisition: model.AcquisitionSource,
		},
	})
	if err != nil {
		t.Fatalf("enqueue the second build: %v", err)
	}

	f.runOne()

	j := f.job(res.Job.ID)
	if j.State != model.JobQueued {
		t.Errorf("second build state = %q, want queued — D70's lease is a queue, not a 409", j.State)
	}
	if j.ErrorCode != nil {
		t.Errorf("second build wears error_code %q; waiting for the lease is not a failure",
			*j.ErrorCode)
	}
}

// TestPrebuiltVerificationFallsBackToSource is D18 end to end, and it is the
// reason D60 made the identity three-part: the failed prebuilt row SURVIVES as
// the record of why, and a source row of the same tag and backend is inserted
// beside it — an insert a two-part key could not have made.
func TestPrebuiltVerificationFallsBackToSource(t *testing.T) {
	t.Parallel()

	f := newFixture(t, func(c *Config) {
		c.Releases = fakeResolver{res: github.Resolution{
			Channel: model.ChannelNightly, Tag: "b10621",
			AssetFound: true,
			Asset: github.Asset{
				Name:        "llama-b10621-bin-ubuntu-x64.tar.gz",
				DownloadURL: "https://example.invalid/llama-b10621-bin-ubuntu-x64.tar.gz",
			},
		}}
	})
	f.registerInstallWith(&fakeSource{}, fakePrebuilt{
		result: prebuilt.InstallResult{
			SourceFallback: true,
			FailingStep:    model.StepVerify,
			Verify: prebuilt.VerifyResult{
				FailingCheck:   "execute",
				SourceFallback: true,
				Diagnosis: &prebuilt.Diagnosis{
					Summary: "requires GLIBC_2.38, host has 2.36",
				},
			},
		},
		err: errors.New("prebuilt: verification failed"),
	})

	res, err := f.svc.Install(context.Background(),
		InstallRequest{Channel: model.ChannelNightly, Tag: "b10621"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Version.ID != "b10621-cpu-bin" {
		t.Fatalf("version id = %q, want the prebuilt id", res.Version.ID)
	}
	f.runOne()

	failed := f.version("b10621-cpu-bin")
	if failed.State != model.VersionFailedVerification {
		t.Errorf("prebuilt state = %q, want failed_verification", failed.State)
	}
	if failed.SupersededBy == nil || *failed.SupersededBy != "b10621-cpu-src" {
		t.Errorf("superseded_by = %v, want b10621-cpu-src", failed.SupersededBy)
	}
	replacement := f.version("b10621-cpu-src")
	if replacement.State != model.VersionPending {
		t.Errorf("replacement state = %q, want pending", replacement.State)
	}
	if replacement.Acquisition != model.AcquisitionSource {
		t.Errorf("replacement acquisition = %q, want source", replacement.Acquisition)
	}
}

// registerInstall wires an install worker over a fake source builder.
func (f *fixture) registerInstall(builder SourceBuilder) {
	f.t.Helper()
	f.registerInstallWith(builder, fakePrebuilt{})
}

func (f *fixture) registerInstallWith(builder SourceBuilder, pre PrebuiltInstaller) {
	f.t.Helper()
	w, err := f.svc.NewInstallWorker(InstallWorkerConfig{Source: builder, Prebuilt: pre})
	if err != nil {
		f.t.Fatalf("NewInstallWorker: %v", err)
	}
	if err := f.queue.Register(w); err != nil {
		f.t.Fatalf("register the install worker: %v", err)
	}
}

// assertCode fails unless err is a model.Error carrying code.
func assertCode(t *testing.T, err error, code model.ErrorCode) {
	t.Helper()
	var me model.Error
	if !errors.As(err, &me) {
		t.Fatalf("error = %v, want a model.Error with code %q", err, code)
	}
	if me.Code != code {
		t.Fatalf("error code = %q, want %q", me.Code, code)
	}
}

// versionStates reads the `to_state` of every event recorded against one version
// row, OLDEST FIRST — the transition sequence §2.5's table describes, as the
// database actually recorded it.
func (f *fixture) versionStates(id string) []string {
	f.t.Helper()
	var out []string
	if err := f.store.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		rows, err := f.store.Events(ctx, tx, store.EventFilter{
			SubjectType: string(model.SubjectLlamacppVersion), SubjectID: id,
		})
		if err != nil {
			return err
		}
		for i := len(rows) - 1; i >= 0; i-- {
			if rows[i].ToState != nil {
				out = append(out, *rows[i].ToState)
			}
		}
		return nil
	}); err != nil {
		f.t.Fatalf("read the version's events: %v", err)
	}
	return out
}

// TestVerifyingIsWrittenOnBothPipelines is §2.5's transition table where it was
// previously skipped.
//
// The table gives `verifying` two entry edges — "hardened extract ok" from
// `fetching` and "compile + install exit 0" from `building` — and two exits,
// `ready` and `failed_verification`. A row that went straight from `fetching` to
// `ready` took an edge the table does not define, and the state D18's whole
// fallback hangs off would have appeared in the enum and nowhere else.
func TestVerifyingIsWrittenOnBothPipelines(t *testing.T) {
	t.Parallel()

	t.Run("a source build passes through verifying on its way to ready", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, nil)
		f.registerInstall(&fakeSource{phases: []source.Phase{
			source.PhaseConfigure, source.PhaseCompile, source.PhaseInstall,
			source.PhaseVerify, source.PhasePublish,
		}})

		res, err := f.svc.Install(context.Background(), InstallRequest{
			Channel: model.ChannelCustom,
			GitRef:  "0123456789abcdef0123456789abcdef01234567",
		})
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		f.runOne()

		if got := f.version(res.Version.ID).State; got != model.VersionReady {
			t.Fatalf("state = %q, want ready", got)
		}
		assertStateOrder(t, f.versionStates(res.Version.ID),
			string(model.VersionBuilding), string(model.VersionVerifying), string(model.VersionReady))
	})

	t.Run("a prebuilt passes through verifying on its way to ready", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, func(c *Config) {
			c.Releases = fakeResolver{res: github.Resolution{
				Channel: model.ChannelNightly, Tag: "b10621", AssetFound: true,
				Asset: github.Asset{
					Name:        "llama-b10621-bin-ubuntu-x64.tar.gz",
					DownloadURL: "https://example.invalid/llama-b10621-bin-ubuntu-x64.tar.gz",
				},
			}}
		})
		f.registerInstallWith(&fakeSource{}, fakePrebuilt{
			steps: []model.FailingStep{model.StepFetch, model.StepVerify, model.StepInstall},
		})

		res, err := f.svc.Install(context.Background(),
			InstallRequest{Channel: model.ChannelNightly, Tag: "b10621"})
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		f.runOne()

		if got := f.version(res.Version.ID).State; got != model.VersionReady {
			t.Fatalf("state = %q, want ready", got)
		}
		assertStateOrder(t, f.versionStates(res.Version.ID),
			string(model.VersionFetching), string(model.VersionVerifying), string(model.VersionReady))
	})

	t.Run("failed_verification is reached from verifying, not from fetching", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, func(c *Config) {
			c.Releases = fakeResolver{res: github.Resolution{
				Channel: model.ChannelNightly, Tag: "b10621", AssetFound: true,
				Asset: github.Asset{
					Name:        "llama-b10621-bin-ubuntu-x64.tar.gz",
					DownloadURL: "https://example.invalid/llama-b10621-bin-ubuntu-x64.tar.gz",
				},
			}}
		})
		f.registerInstallWith(&fakeSource{}, fakePrebuilt{
			steps: []model.FailingStep{model.StepFetch, model.StepVerify},
			result: prebuilt.InstallResult{
				SourceFallback: true, FailingStep: model.StepVerify,
				Verify: prebuilt.VerifyResult{
					FailingCheck: "execute", SourceFallback: true,
					Diagnosis: &prebuilt.Diagnosis{Summary: "requires GLIBC_2.38, host has 2.36"},
				},
			},
			err: errors.New("prebuilt: verification failed"),
		})

		res, err := f.svc.Install(context.Background(),
			InstallRequest{Channel: model.ChannelNightly, Tag: "b10621"})
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		f.runOne()

		if got := f.version(res.Version.ID).State; got != model.VersionFailedVerification {
			t.Fatalf("state = %q, want failed_verification", got)
		}
		assertStateOrder(t, f.versionStates(res.Version.ID),
			string(model.VersionFetching), string(model.VersionVerifying),
			string(model.VersionFailedVerification))
	})
}

// assertStateOrder fails unless want appears in got in that order, allowing
// other states in between: the test is about the edges §2.5 defines, not about
// every event a build happens to emit.
func assertStateOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	i := 0
	for _, s := range got {
		if i < len(want) && s == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Errorf("state sequence %v does not contain %v in order", got, want)
	}
}

// TestInstallRefusesAHostileGitURL is §6.2's "`git ls-remote` validates it
// before the row leaves `resolving`", at the layer where a refusal is still a
// `422` on `POST /api/v1/llamacpp/versions`.
//
// `force_source` on the stable channel carries a `git_url` into the row exactly
// as a custom build does, which is why the check cannot live in the custom
// branch alone: the proof of impact for git's `ext::` shell transport used
// `{"channel":"stable","force_source":true,"git_url":"ext::…"}`.
func TestInstallRefusesAHostileGitURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  InstallRequest
	}{
		{
			name: "the ext transport on the stable channel via force_source",
			req: InstallRequest{
				Channel: model.ChannelStable, Tag: "b10621", ForceSource: true,
				GitURL: "ext::sh -c 'id > /tmp/pwn'",
			},
		},
		{
			name: "an option smuggled in as the remote of a custom build",
			req: InstallRequest{
				Channel: model.ChannelCustom, GitURL: "--upload-pack=/bin/sh",
				GitRef: "0123456789abcdef0123456789abcdef01234567",
			},
		},
		{
			name: "a local repository",
			req: InstallRequest{
				Channel: model.ChannelCustom, GitURL: "file:///etc",
				GitRef: "0123456789abcdef0123456789abcdef01234567",
			},
		},
		{
			name: "a URL carrying a token",
			req: InstallRequest{
				Channel: model.ChannelCustom,
				GitURL:  "https://user:ghp_XXXX@example.test/me/llama.cpp.git",
				GitRef:  "0123456789abcdef0123456789abcdef01234567",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, func(c *Config) {
				c.Releases = fakeResolver{res: github.Resolution{
					Channel: model.ChannelStable, Tag: "b10621",
				}}
			})
			f.registerInstall(&fakeSource{})

			_, err := f.svc.Install(context.Background(), tc.req)
			if err == nil {
				t.Fatalf("Install accepted git_url %q", tc.req.GitURL)
			}
			assertCode(t, err, model.CodeBadFlags)

			// Nothing was inserted: a refused request leaves no row for a worker
			// to pick up and hand to `git`.
			var rows int
			if err := f.store.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
				vs, err := f.store.LlamacppVersions(ctx, tx, store.LlamacppVersionFilter{})
				rows = len(vs)
				return err
			}); err != nil {
				t.Fatalf("list versions: %v", err)
			}
			if rows != 0 {
				t.Errorf("%d version rows exist after a refusal", rows)
			}
		})
	}
}
