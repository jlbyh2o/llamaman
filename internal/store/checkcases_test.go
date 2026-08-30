package store

import (
	"context"
	"testing"
)

// The illegal-insert table behind TestCheckConstraints.
//
// One case per CHECK constraint in DESIGN section 2, each built from a row that
// is otherwise valid, so the only reason the insert can fail is the constraint
// under test. The keys are `<table>.<column>` and TestCheckConstraints derives
// the same keys from the migration SQL, so this table cannot silently fall
// behind the schema.

// checkCase is one illegal insert: a valid row for the table, the column value
// that must be rejected, and any statements the row needs first.
type checkCase struct {
	valid   map[string]any
	illegal any
	pre     []string
}

// Illegal values by shape, named so the cases below read as intent.
var (
	notJSON  = "this is not json"
	notBool  = int64(2)  // for the 0/1 columns
	notOne   = int64(2)  // for the `id = 1` singletons
	lowPort  = int64(80) // below the 1024 floor
	badName  = "Bad Name!"
	fixtures = struct{ root, model, file, download, version, instance, run, point string }{
		root: "root1", model: "model1", file: "mf1", download: "dl1",
		version: "ver1", instance: "inst1", run: "run1", point: "pt1",
	}
)

// seedFixtures commits one parent row for every table the case rows reference,
// so a case that fails does so on its own CHECK and never on a foreign key.
func seedFixtures(t *testing.T, s *Store) {
	t.Helper()
	stmts := []string{
		`INSERT INTO hf_cache_roots (id, path, created_at) VALUES ('root1', '/fixture/hub', 1)`,

		`INSERT INTO models (id, root_id, repo_id, revision, kind, state, origin,
		                     snapshot_dir, primary_file, created_at, updated_at)
		 VALUES ('model1', 'root1', 'org/repo', 'abc123', 'text', 'ready', 'scanned',
		         '/fixture/hub/snap', 'shard1.gguf', 1, 1)`,

		`INSERT INTO model_files (id, model_id, filename, size_bytes, state, created_at, updated_at)
		 VALUES ('mf1', 'model1', 'shard1.gguf', 1024, 'present', 1, 1)`,

		`INSERT INTO downloads (id, model_id, state, created_at)
		 VALUES ('dl1', 'model1', 'queued', 1)`,

		`INSERT INTO llamacpp_versions (id, channel, tag, acquisition, backend, dir_name,
		                                state, created_at)
		 VALUES ('ver1', 'stable', 'b0', 'source', 'cpu', 'ver1', 'ready', 1)`,

		`INSERT INTO instances (id, name, public_port, internal_port, flags_json, config_hash,
		                        unit_name, created_at, updated_at)
		 VALUES ('inst1', 'fixture', 9000, 21500, '{}', 'hash', 'llamaman-instance@fixture.service', 1, 1)`,

		`INSERT INTO bench_runs (id, name, state, model_label, model_path, llamacpp_tag,
		                         llamacpp_backend, gpu_json, host_json, sweep_json, created_at)
		 VALUES ('run1', 'sweep', 'draft', 'qwen3-8b', '/m.gguf', 'b0', 'cpu', '{}', '{}', '{}', 1)`,

		`INSERT INTO bench_points (id, run_id, ordinal, state, args_json)
		 VALUES ('pt1', 'run1', 1, 'pending', '[]')`,
	}
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		for _, q := range stmts {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
}

// checkCases builds the table. add() pairs one valid row with a map of
// column → illegal value, so a table with eleven constraints is written once.
func checkCases() map[string]checkCase {
	out := map[string]checkCase{}
	add := func(table string, valid map[string]any, bad map[string]any, pre ...string) {
		for col, v := range bad {
			out[table+"."+col] = checkCase{valid: valid, illegal: v, pre: pre}
		}
	}

	// --- 2.1 Meta, settings, runtime facts -----------------------------------

	add("settings",
		map[string]any{"key": "ui.theme", "value": `"dark"`, "updated_at": 1, "updated_by": "admin"},
		map[string]any{"value": notJSON, "updated_by": "the cat"})

	add("runtime_info",
		map[string]any{"id": 1, "daemon_version": "v1.0.0", "daemon_commit": "abc"},
		map[string]any{
			"id":                  notOne,
			"systemd_scope":       "both",
			"systemd_control":     "telepathy",
			"journal_read":        "maybe",
			"polkit_ok":           notBool,
			"polkit_unit_files":   notBool,
			"listener_continuity": "partial",
		})

	// --- 2.2 Identity, sessions, secrets -------------------------------------

	add("admin_account",
		map[string]any{"id": 1, "password_hash": "$argon2id$…", "password_set_at": 1, "updated_at": 1},
		map[string]any{"id": notOne})

	add("setup_claim",
		map[string]any{"id": 1, "token_hash": "sha256hex", "created_at": 1},
		map[string]any{"id": notOne})

	add("login_attempts",
		map[string]any{"id": "la1", "at": 1, "ip": "127.0.0.1", "success": 1},
		map[string]any{"success": notBool})

	add("secrets",
		map[string]any{"name": "hf_token", "nonce": []byte{1}, "ciphertext": []byte{2},
			"created_at": 1, "updated_at": 1},
		map[string]any{"valid": notBool, "scope_json": notJSON})

	// --- 2.3 Jobs ------------------------------------------------------------

	add("jobs",
		map[string]any{"id": "job-case", "kind": "maintenance", "subject_type": "system",
			"subject_id": "case", "state": "queued", "run_after": 1, "created_at": 1},
		map[string]any{
			"kind":             "dance",
			"subject_type":     "planet",
			"state":            "confused",
			"cancel_requested": notBool,
			"progress_json":    notJSON,
			"params_json":      notJSON,
		})

	// --- 2.4 Toolchain and hardware ------------------------------------------

	add("toolchain_probes",
		map[string]any{"id": "tp1", "at": 1, "ok_cpu": 1, "ok_cuda": 0, "result_json": "{}"},
		map[string]any{"ok_cpu": notBool, "ok_cuda": notBool, "result_json": notJSON})

	add("gpus",
		map[string]any{"id": "g1", "uuid": "GPU-abc", "gpu_index": 0, "name": "RTX",
			"vram_total_bytes": 1, "first_seen_at": 1, "last_seen_at": 1},
		map[string]any{"present": notBool})

	// --- 2.5 llama.cpp versions ----------------------------------------------

	add("llamacpp_versions",
		map[string]any{"id": "ver-case", "channel": "stable", "tag": "b-case",
			"acquisition": "source", "backend": "cpu", "dir_name": "ver-case",
			"state": "ready", "created_at": 1},
		map[string]any{
			"channel":            "bleeding",
			"acquisition":        "wishful",
			"backend":            "quantum",
			"build_options_json": notJSON,
			"gpu_uuids_json":     notJSON,
			"state":              "nearly",
			"is_active":          notBool,
			"previous_active":    notBool,
			"binaries_json":      notJSON,
			"help_flags_json":    notJSON,
			"supports_fit":       notBool,
		})

	add("release_cache",
		map[string]any{"id": "rc1", "source": "llamacpp", "tag": "b1", "fetched_at": 1},
		map[string]any{"source": "somewhere", "prerelease": notBool, "assets_json": notJSON})

	add("build_lease",
		map[string]any{"id": 1},
		map[string]any{"id": notOne},
		`DELETE FROM build_lease`)

	// --- 2.6 Hugging Face cache, models, files -------------------------------

	add("hf_cache_roots",
		map[string]any{"id": "root-case", "path": "/case/hub", "created_at": 1},
		map[string]any{
			"is_primary":    notBool,
			"writable":      notBool,
			"symlinks_ok":   notBool,
			"detected_from": "vibes",
		})

	add("models",
		map[string]any{"id": "models-case", "root_id": fixtures.root, "repo_id": "org/repo",
			"revision": "abc123", "kind": "text", "state": "ready", "origin": "scanned",
			"snapshot_dir": "/case/snap", "primary_file": "case.gguf",
			"created_at": 1, "updated_at": 1},
		map[string]any{
			"kind":                "audio",
			"state":               "vanishing",
			"origin":              "somewhere",
			"mmproj_auto":         notBool,
			"has_vision":          notBool,
			"metadata_json":       notJSON,
			"tensor_summary_json": notJSON,
			"hf_gguf_json":        notJSON,
		})

	add("model_files",
		map[string]any{"id": "mf-case", "model_id": fixtures.model, "filename": "case.gguf",
			"size_bytes": 1, "state": "present", "created_at": 1, "updated_at": 1},
		map[string]any{"state": "elsewhere", "checksum_verified": notBool})

	add("cache_scans",
		map[string]any{"id": "cs1", "root_id": fixtures.root, "state": "queued",
			"trigger": "boot", "created_at": 1},
		map[string]any{"state": "rummaging"})

	// --- 2.7 Downloads -------------------------------------------------------

	add("downloads",
		map[string]any{"id": "dl-case", "model_id": fixtures.model, "state": "queued", "created_at": 1},
		map[string]any{"state": "dawdling", "include_mmproj": notBool})

	add("download_tasks",
		map[string]any{"id": "dt1", "download_id": fixtures.download, "model_file_id": fixtures.file,
			"url": "https://huggingface.co/x", "state": "queued"},
		map[string]any{"state": "dawdling"})

	// --- 2.8 Instances -------------------------------------------------------

	add("instances",
		map[string]any{"id": "inst-case", "name": "casename", "public_port": 9100,
			"internal_port": 21600, "flags_json": "{}", "config_hash": "hash",
			"unit_name": "llamaman-instance@casename.service", "created_at": 1, "updated_at": 1},
		map[string]any{
			"name":                  badName,
			"public_port":           lowPort,
			"internal_port":         lowPort,
			"auth_mode":             "honesty",
			"autostart":             notBool,
			"restart_policy":        "eventually",
			"flags_json":            notJSON,
			"desired_state":         "elsewhere",
			"draft_validation":      "probably",
			"pending_trigger":       "curiosity",
			"pending_override_json": notJSON,
		})

	add("instance_status",
		map[string]any{"instance_id": fixtures.instance, "state": "unknown", "last_change_at": 1},
		map[string]any{
			"state":           "vibing",
			"gpu_uuids_json":  notJSON,
			"gpu_attribution": "guessed",
			"fit_report_json": notJSON,
			"device_map_json": notJSON,
		})

	add("instance_starts",
		map[string]any{"id": "st-case", "instance_id": fixtures.instance, "at": 1,
			"trigger": "user", "config_hash": "hash"},
		map[string]any{
			"trigger":       "boredom",
			"override_json": notJSON,
			"argv_json":     notJSON,
			"outcome":       "ready", // D63: deliberately NOT a member
			"detail_json":   notJSON,
		})

	add("flag_presets",
		map[string]any{"id": "fp1", "name": "balanced", "flags_json": "{}",
			"created_at": 1, "updated_at": 1},
		map[string]any{"flags_json": notJSON, "builtin": notBool})

	// --- 2.9 Tokens and accounting -------------------------------------------

	add("api_tokens",
		map[string]any{"id": "tok-case", "name": "laptop", "prefix": "lm_abc123",
			"token_hash": "sha256hex", "created_at": 1, "updated_at": 1},
		map[string]any{"scope": "everything", "state": "resting"})

	add("instance_usage_daily",
		map[string]any{"instance_id": fixtures.instance, "day": "2026-01-01", "auth_mode": "token"},
		map[string]any{"auth_mode": "honesty"})

	// --- 2.10 Benchmarks -----------------------------------------------------

	add("bench_runs",
		map[string]any{"id": "run-case", "name": "sweep", "state": "draft",
			"model_label": "qwen3-8b", "model_path": "/m.gguf", "llamacpp_tag": "b0",
			"llamacpp_backend": "cpu", "gpu_json": "{}", "host_json": "{}",
			"sweep_json": "{}", "created_at": 1},
		map[string]any{
			"state":                  "pondering",
			"gpu_json":               notJSON,
			"host_json":              notJSON,
			"sweep_json":             notJSON,
			"stopped_instances_json": notJSON,
			"restore_done":           notBool,
		})

	add("bench_points",
		map[string]any{"id": "pt-case", "run_id": fixtures.run, "ordinal": 2,
			"state": "pending", "args_json": "[]"},
		map[string]any{"state": "pondering", "args_json": notJSON})

	add("bench_results",
		map[string]any{"id": "br1", "point_id": fixtures.point, "run_id": fixtures.run,
			"test_kind": "pp", "avg_ts": 1.5, "raw_json": "{}", "created_at": 1},
		map[string]any{"test_kind": "vibes", "samples_json": notJSON, "raw_json": notJSON})

	add("bench_lease",
		map[string]any{"id": 1},
		map[string]any{"id": notOne},
		`DELETE FROM bench_lease`)

	// --- 2.11 Self-update, events, notifications, fit, wizard ----------------

	add("self_updates",
		map[string]any{"id": "su1", "from_version": "v1.0.0", "to_version": "v1.1.0",
			"channel": "stable", "state": "planned", "created_at": 1},
		map[string]any{"state": "hoping", "signature_ok": notBool})

	add("events",
		map[string]any{"id": "ev-case", "at": 1, "level": "info", "category": "system",
			"action": "boot", "actor": "system", "message": "hello"},
		map[string]any{"level": "shouting", "actor": "the cat", "detail_json": notJSON})

	add("notifications",
		map[string]any{"id": "n1", "at": 1, "severity": "warn", "code": "ui_port_exhausted",
			"title": "t", "body": "b"},
		map[string]any{"severity": "shouting", "action_json": notJSON})

	add("fit_observations",
		map[string]any{"id": "fo1", "at": 1, "arch": "qwen3", "llamacpp_tag": "b0",
			"backend": "cuda", "predicted_bytes": 1, "source": "bench"},
		map[string]any{"oom": notBool, "source": "hearsay"})

	add("wizard_steps",
		map[string]any{"step": "password", "state": "pending", "updated_at": 1},
		map[string]any{"step": "vibes", "state": "pondering", "data_json": notJSON})

	return out
}
