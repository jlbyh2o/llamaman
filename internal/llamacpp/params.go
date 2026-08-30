package llamacpp

import (
	"encoding/json"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// `jobs.params_json` for the three kinds this package runs.
//
// A worker reads its inputs back out of this column after a restart, so
// everything a resumed run cannot re-derive is written here — the resolved asset
// URL above all, because a resolution is a network call whose answer can change
// between the request and the retry, and D4's warm rerun must build the same
// thing the first attempt was building.

// installParams is `llamacpp_install`.
type installParams struct {
	VersionID   string                `json:"version_id"`
	Channel     model.LlamacppChannel `json:"channel"`
	Tag         string                `json:"tag"`
	BuildTag    string                `json:"build_tag,omitempty"`
	Backend     model.Backend         `json:"backend"`
	Acquisition model.Acquisition     `json:"acquisition"`

	GitURL string `json:"git_url,omitempty"`
	GitRef string `json:"git_ref,omitempty"`
	Commit string `json:"resolved_commit,omitempty"`

	AssetName       string `json:"asset_name,omitempty"`
	AssetURL        string `json:"asset_url,omitempty"`
	AssetSHA256     string `json:"asset_sha256,omitempty"`
	AssetReleaseTag string `json:"asset_release_tag,omitempty"`

	ExtraCMake   []string `json:"extra_cmake_flags,omitempty"`
	CUDAArchList string   `json:"cuda_arch_list,omitempty"`

	// Diagnosis is D18's fallback carrying its reason forward: the row this
	// source build replaces failed to execute on this host, and "requires
	// GLIBC_2.38, host has 2.36" is the sentence the UI shows beside the new
	// build. It is empty for every install a user asked for directly.
	Diagnosis string `json:"diagnosis,omitempty"`
	// SupersedesID is the `failed_verification` prebuilt row this build was
	// enqueued in place of.
	SupersedesID string `json:"supersedes_id,omitempty"`
}

// params renders a resolved identity into the job's inputs.
func (id identity) params(req InstallRequest) installParams {
	return installParams{
		VersionID:       id.ID,
		Channel:         id.Channel,
		Tag:             id.Tag,
		BuildTag:        id.BuildTag,
		Backend:         id.Backend,
		Acquisition:     id.Acquisition,
		GitURL:          id.gitURLOrDefault(),
		GitRef:          id.GitRef,
		Commit:          id.Commit,
		AssetName:       id.AssetName,
		AssetURL:        id.AssetURL,
		AssetSHA256:     id.AssetSHA256,
		AssetReleaseTag: id.AssetReleaseTag,
		ExtraCMake:      id.ExtraCMake,
		CUDAArchList:    id.CUDAArchList,
	}
}

// activateParams is `llamacpp_activate`.
//
// KeepPrevious is snapshotted at enqueue rather than read at run: §6.6 step 2
// selects between two behaviors that differ in what is DELETED afterwards, and a
// setting flipped while an activation is queued must not make the roll and its
// cleanup disagree about which one happened.
type activateParams struct {
	VersionID        string `json:"version_id"`
	RestartInstances string `json:"restart_instances"`
	CanaryInstanceID string `json:"canary_instance_id,omitempty"`
	KeepPrevious     bool   `json:"keep_previous"`
	Rollback         bool   `json:"rollback,omitempty"`

	// DeletionCandidateID is §6.6 step 2's record: the version that lost its
	// rollback slot. The `llamacpp_delete` job is enqueued only when THIS job
	// reaches `succeeded`, because step 5 may revert the whole activation and
	// cannot revert a directory a delete worker has already removed. It is
	// written by the worker, after the step-3 transaction commits.
	DeletionCandidateID string `json:"deletion_candidate_id,omitempty"`
}

// deleteParams is `llamacpp_delete`.
type deleteParams struct {
	VersionID string `json:"version_id"`
}

// decodeParams reads a job's `params_json` into v.
func decodeParams(raw *string, v any) error {
	if raw == nil || *raw == "" {
		return fmt.Errorf("llamacpp: the job carries no params_json")
	}
	if err := json.Unmarshal([]byte(*raw), v); err != nil {
		return fmt.Errorf("llamacpp: params_json: %w", err)
	}
	return nil
}
