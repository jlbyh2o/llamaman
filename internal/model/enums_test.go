package model

import "testing"

// TestEveryEnumRejectsAWrongValue runs the same two assertions over every closed
// enum in the package: each declared member is Valid, and a value that is not a
// member is not. Together with internal/store's TestGoAndSQLEnumsAgree — which
// compares these same lists against the migration's CHECK constraints — that is
// the whole of "Go and SQL agree on every closed enum".
func TestEveryEnumRejectsAWrongValue(t *testing.T) {
	tests := []struct {
		name    string
		members []string
		valid   func(string) bool
	}{
		{"SettingUpdatedBy", enumStrings(SettingUpdatedByValues()),
			func(v string) bool { return SettingUpdatedBy(v).Valid() }},
		{"SystemdScope", enumStrings(SystemdScopeValues()),
			func(v string) bool { return SystemdScope(v).Valid() }},
		{"SystemdControl", enumStrings(SystemdControlValues()),
			func(v string) bool { return SystemdControl(v).Valid() }},
		{"JournalRead", enumStrings(JournalReadValues()),
			func(v string) bool { return JournalRead(v).Valid() }},
		{"ListenerContinuity", enumStrings(ListenerContinuityValues()),
			func(v string) bool { return ListenerContinuity(v).Valid() }},
		{"SecretName", enumStrings(SecretNameValues()),
			func(v string) bool { return SecretName(v).Valid() }},
		{"LoginReason", enumStrings(LoginReasonValues()),
			func(v string) bool { return LoginReason(v).Valid() }},
		{"JobKind", enumStrings(JobKindValues()),
			func(v string) bool { return JobKind(v).Valid() }},
		{"JobSubjectType", enumStrings(JobSubjectTypeValues()),
			func(v string) bool { return JobSubjectType(v).Valid() }},
		{"JobState", enumStrings(JobStateValues()),
			func(v string) bool { return JobState(v).Valid() }},
		{"LlamacppChannel", enumStrings(LlamacppChannelValues()),
			func(v string) bool { return LlamacppChannel(v).Valid() }},
		{"Acquisition", enumStrings(AcquisitionValues()),
			func(v string) bool { return Acquisition(v).Valid() }},
		{"Backend", enumStrings(BackendValues()),
			func(v string) bool { return Backend(v).Valid() }},
		{"VersionState", enumStrings(VersionStateValues()),
			func(v string) bool { return VersionState(v).Valid() }},
		{"FailingStep", enumStrings(FailingStepValues()),
			func(v string) bool { return FailingStep(v).Valid() }},
		{"ReleaseSource", enumStrings(ReleaseSourceValues()),
			func(v string) bool { return ReleaseSource(v).Valid() }},
		{"CacheRootDetectedFrom", enumStrings(CacheRootDetectedFromValues()),
			func(v string) bool { return CacheRootDetectedFrom(v).Valid() }},
		{"ModelKind", enumStrings(ModelKindValues()),
			func(v string) bool { return ModelKind(v).Valid() }},
		{"ModelState", enumStrings(ModelStateValues()),
			func(v string) bool { return ModelState(v).Valid() }},
		{"ModelOrigin", enumStrings(ModelOriginValues()),
			func(v string) bool { return ModelOrigin(v).Valid() }},
		{"ModelFileState", enumStrings(ModelFileStateValues()),
			func(v string) bool { return ModelFileState(v).Valid() }},
		{"CacheScanState", enumStrings(CacheScanStateValues()),
			func(v string) bool { return CacheScanState(v).Valid() }},
		{"CacheScanTrigger", enumStrings(CacheScanTriggerValues()),
			func(v string) bool { return CacheScanTrigger(v).Valid() }},
		{"StrayReason", enumStrings(StrayReasonValues()),
			func(v string) bool { return StrayReason(v).Valid() }},
		{"DownloadState", enumStrings(DownloadStateValues()),
			func(v string) bool { return DownloadState(v).Valid() }},
		{"DownloadTaskState", enumStrings(DownloadTaskStateValues()),
			func(v string) bool { return DownloadTaskState(v).Valid() }},
		{"AuthMode", enumStrings(AuthModeValues()),
			func(v string) bool { return AuthMode(v).Valid() }},
		{"RestartPolicy", enumStrings(RestartPolicyValues()),
			func(v string) bool { return RestartPolicy(v).Valid() }},
		{"DesiredState", enumStrings(DesiredStateValues()),
			func(v string) bool { return DesiredState(v).Valid() }},
		{"DraftValidation", enumStrings(DraftValidationValues()),
			func(v string) bool { return DraftValidation(v).Valid() }},
		{"PendingTrigger", enumStrings(PendingTriggerValues()),
			func(v string) bool { return PendingTrigger(v).Valid() }},
		{"InstanceState", enumStrings(InstanceStateValues()),
			func(v string) bool { return InstanceState(v).Valid() }},
		{"GPUAttribution", enumStrings(GPUAttributionValues()),
			func(v string) bool { return GPUAttribution(v).Valid() }},
		{"StartTrigger", enumStrings(StartTriggerValues()),
			func(v string) bool { return StartTrigger(v).Valid() }},
		{"StartOutcome", enumStrings(StartOutcomeValues()),
			func(v string) bool { return StartOutcome(v).Valid() }},
		{"InhibitReason", enumStrings(InhibitReasonValues()),
			func(v string) bool { return InhibitReason(v).Valid() }},
		{"TokenScope", enumStrings(TokenScopeValues()),
			func(v string) bool { return TokenScope(v).Valid() }},
		{"TokenState", enumStrings(TokenStateValues()),
			func(v string) bool { return TokenState(v).Valid() }},
		{"DenialReason", enumStrings(DenialReasonValues()),
			func(v string) bool { return DenialReason(v).Valid() }},
		{"BenchRunState", enumStrings(BenchRunStateValues()),
			func(v string) bool { return BenchRunState(v).Valid() }},
		{"BenchPointState", enumStrings(BenchPointStateValues()),
			func(v string) bool { return BenchPointState(v).Valid() }},
		{"BenchTestKind", enumStrings(BenchTestKindValues()),
			func(v string) bool { return BenchTestKind(v).Valid() }},
		{"SelfUpdateState", enumStrings(SelfUpdateStateValues()),
			func(v string) bool { return SelfUpdateState(v).Valid() }},
		{"EventLevel", enumStrings(EventLevelValues()),
			func(v string) bool { return EventLevel(v).Valid() }},
		{"EventCategory", enumStrings(EventCategoryValues()),
			func(v string) bool { return EventCategory(v).Valid() }},
		{"EventActor", enumStrings(EventActorValues()),
			func(v string) bool { return EventActor(v).Valid() }},
		{"NotificationSeverity", enumStrings(NotificationSeverityValues()),
			func(v string) bool { return NotificationSeverity(v).Valid() }},
		{"FitObservationSource", enumStrings(FitObservationSourceValues()),
			func(v string) bool { return FitObservationSource(v).Valid() }},
		{"WizardStep", enumStrings(WizardStepValues()),
			func(v string) bool { return WizardStep(v).Valid() }},
		{"WizardStepState", enumStrings(WizardStepStateValues()),
			func(v string) bool { return WizardStepState(v).Valid() }},
		{"PortReason", enumStrings(PortReasonValues()),
			func(v string) bool { return PortReason(v).Valid() }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.members) == 0 {
				t.Fatal("enum has no members")
			}
			for _, m := range tt.members {
				if !tt.valid(m) {
					t.Errorf("declared member %q is not Valid", m)
				}
			}
			for _, bad := range []string{"", "nonsense", "READY", "ready "} {
				if tt.valid(bad) {
					t.Errorf("%q was accepted as a member", bad)
				}
			}
			seen := map[string]bool{}
			for _, m := range tt.members {
				if seen[m] {
					t.Errorf("member %q is listed twice", m)
				}
				seen[m] = true
			}
		})
	}
}

// TestClosedEnumsAreNonEmpty guards the registry itself: an entry that resolves
// to an empty list would make internal/store's agreement test pass vacuously.
func TestClosedEnumsAreNonEmpty(t *testing.T) {
	enums := ClosedEnums()
	if len(enums) == 0 {
		t.Fatal("ClosedEnums is empty")
	}
	for key, members := range enums {
		if len(members) == 0 {
			t.Errorf("%s has no members", key)
		}
		for _, m := range members {
			if m == "" {
				t.Errorf("%s has an empty member", key)
			}
		}
	}
}
