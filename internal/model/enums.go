package model

// Closed enums, and the one place Go and SQL are made to agree.
//
// Every `CHECK (col IN ('a','b',…))` in DESIGN section 2 is a closed set that
// two languages have to spell identically: SQL rejects a bad write at the
// storage boundary, Go rejects one before it gets there and gives the value a
// name. Nothing but a test can keep the two lists equal, so this file assembles
// every closed enum in the schema into one map keyed by `<table>.<column>`, and
// internal/store's schema test parses the migration's own CHECK clauses and
// compares them member for member, in order. Adding a member on one side alone
// fails that test.
//
// Columns whose CHECK is not a membership test — `id = 1`, `json_valid(x)`,
// `length(name) BETWEEN …`, the port ranges — are not enums and are absent here;
// they are proven instead by the illegal-insert table in the same test. Columns
// the schema documents with a comment but does NOT close with a CHECK
// (`login_attempts.reason`, `cache_scans.trigger`, `events.category`,
// `stray_files.reason`, `gateway_denials_daily.reason`,
// `llamacpp_versions.failing_step`) still get Go types below, because the
// application closes them even though the storage layer does not; each says so
// in its own doc comment and none appears in this map.

// ClosedEnums maps `<table>.<column>` to the members of the SQL CHECK
// constraint that closes it, in the order the constraint lists them.
func ClosedEnums() map[string][]string {
	return map[string][]string{
		// 2.1 Meta, settings, runtime facts
		"settings.updated_by":              enumStrings(SettingUpdatedByValues()),
		"runtime_info.systemd_scope":       enumStrings(SystemdScopeValues()),
		"runtime_info.systemd_control":     enumStrings(SystemdControlValues()),
		"runtime_info.journal_read":        enumStrings(JournalReadValues()),
		"runtime_info.listener_continuity": enumStrings(ListenerContinuityValues()),

		// 2.3 Jobs
		"jobs.kind":         enumStrings(JobKindValues()),
		"jobs.subject_type": enumStrings(JobSubjectTypeValues()),
		"jobs.state":        enumStrings(JobStateValues()),

		// 2.5 llama.cpp versions
		"llamacpp_versions.channel":     enumStrings(LlamacppChannelValues()),
		"llamacpp_versions.acquisition": enumStrings(AcquisitionValues()),
		"llamacpp_versions.backend":     enumStrings(BackendValues()),
		"llamacpp_versions.state":       enumStrings(VersionStateValues()),
		"release_cache.source":          enumStrings(ReleaseSourceValues()),

		// 2.6 Hugging Face cache, models, files
		"hf_cache_roots.detected_from": enumStrings(CacheRootDetectedFromValues()),
		"models.kind":                  enumStrings(ModelKindValues()),
		"models.state":                 enumStrings(ModelStateValues()),
		"models.origin":                enumStrings(ModelOriginValues()),
		"model_files.state":            enumStrings(ModelFileStateValues()),
		"cache_scans.state":            enumStrings(CacheScanStateValues()),

		// 2.7 Downloads
		"downloads.state":      enumStrings(DownloadStateValues()),
		"download_tasks.state": enumStrings(DownloadTaskStateValues()),

		// 2.8 Instances
		"instances.auth_mode":             enumStrings(AuthModeValues()),
		"instances.restart_policy":        enumStrings(RestartPolicyValues()),
		"instances.desired_state":         enumStrings(DesiredStateValues()),
		"instances.draft_validation":      enumStrings(DraftValidationValues()),
		"instances.pending_trigger":       enumStrings(PendingTriggerValues()),
		"instance_status.state":           enumStrings(InstanceStateValues()),
		"instance_status.gpu_attribution": enumStrings(GPUAttributionValues()),
		"instance_starts.trigger":         enumStrings(StartTriggerValues()),
		"instance_starts.outcome":         enumStrings(StartOutcomeValues()),
		"instance_usage_daily.auth_mode":  enumStrings(AuthModeValues()),

		// 2.9 Tokens
		"api_tokens.scope": enumStrings(TokenScopeValues()),
		"api_tokens.state": enumStrings(TokenStateValues()),

		// 2.10 Benchmarks
		"bench_runs.state":        enumStrings(BenchRunStateValues()),
		"bench_points.state":      enumStrings(BenchPointStateValues()),
		"bench_results.test_kind": enumStrings(BenchTestKindValues()),

		// 2.11 Self-update, events, notifications, fit calibration, wizard
		"self_updates.state":      enumStrings(SelfUpdateStateValues()),
		"events.level":            enumStrings(EventLevelValues()),
		"events.actor":            enumStrings(EventActorValues()),
		"notifications.severity":  enumStrings(NotificationSeverityValues()),
		"fit_observations.source": enumStrings(FitObservationSourceValues()),
		"wizard_steps.step":       enumStrings(WizardStepValues()),
		"wizard_steps.state":      enumStrings(WizardStepStateValues()),
	}
}

// enumStrings widens a slice of a string-kinded enum to plain strings, so
// ClosedEnums can hold every enum in one map without either losing the typed
// members or repeating their spellings.
func enumStrings[T ~string](vs []T) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}

// valid reports whether v is a member of vs. Every enum's Valid method is one
// call to this, so "valid" always means "listed in the CHECK constraint".
func valid[T ~string](v T, vs []T) bool {
	for _, m := range vs {
		if v == m {
			return true
		}
	}
	return false
}
