package archiveintakeworkspace

import (
	"path"
	"slices"
	"time"

	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

type EventOutputEvidence struct {
	Items            *archiveworkspace.ItemProgress `json:"items,omitempty"`
	ObservedAt       time.Time                      `json:"observed_at,omitzero"`
	ManifestChecksum string                         `json:"manifest_checksum,omitempty"`
	ReviewReasons    []transcription.QualityReason  `json:"review_reasons,omitempty"`
	ArchivePackageID string                         `json:"archive_package_id,omitempty"`
	OriginalAssetIDs []string                       `json:"original_asset_ids,omitempty"`
	Role             string                         `json:"role,omitempty"`
	ProfileReference string                         `json:"profile_reference,omitempty"`
	ArchiveRunID     string                         `json:"archive_run_id"`
	InputRevision    uint64                         `json:"input_revision"`
	Folder           string                         `json:"folder,omitempty"`
	RelativeVideo    string                         `json:"relative_video"`
	PathState        string                         `json:"path_state,omitempty"`
	Stage            string                         `json:"stage"`
	Completed        int64                          `json:"completed,omitempty"`
	Total            int64                          `json:"total,omitempty"`
	Unit             string                         `json:"unit,omitempty"`
	CauseCode        string                         `json:"cause_code,omitempty"`
	IssueCount       int                            `json:"issue_count,omitempty"`
	Basis            []string                       `json:"basis"`
}

func eventOutput(run string, d organization.OrganizationDecision, first job.State, packages []storedOutput, second []job.State) []EventOutputEvidence {
	if d.Disposition == vocabulary.OrganizationHold {
		return nil
	}
	base := EventOutputEvidence{ArchiveRunID: run, InputRevision: d.InputRevision, Folder: d.Folder}
	var rows []EventOutputEvidence
	byPackage := make(map[string]int, len(packages))
	for _, pkg := range packages {
		if pkg.directory == "" || pkg.presentation.VideoFileName == "" || pkg.output.RelativePath == "" {
			continue
		}
		row := base
		row.ArchivePackageID, row.OriginalAssetIDs = pkg.output.ID, pkg.originalIDs
		row.Role, row.ProfileReference = pkg.role, pkg.profile
		row.RelativeVideo = path.Join(pkg.directory, pkg.presentation.VideoFileName)
		row.Stage, row.PathState = "STORED", "PUBLISHED_PACKAGE"
		row.ObservedAt, row.ManifestChecksum, row.Basis = pkg.recordedAt, pkg.output.Checksum, pkg.basis
		byPackage[pkg.output.ID] = len(rows)
		rows = append(rows, row)
	}
	var duration int64
	for _, input := range d.Inputs {
		duration += input.Inspection.DurationMillis
	}
	if len(rows) == 0 {
		if d.EventPreview == nil {
			return nil
		}
		row := base
		row.RelativeVideo = path.Join(d.EventPreview.Directory, d.EventPreview.Video)
		row.Stage, row.PathState, row.Basis = "PLANNED", "EVENT_PREVIEW", []string{d.ID}
		for _, step := range first.Steps {
			if step.Workflow.ID == first.CurrentStepID {
				row.observeStep(first.ID, step, duration)
			}
		}
		rows = append(rows, row)
	}
	// A transcription Job is pinned to one stage-one package. Older single
	// output Jobs without that field still have exactly one possible video.
	for _, j := range second {
		var packageID string
		for _, step := range j.Steps {
			if step.ExecutionSpec != nil && step.ExecutionSpec.Parameters.ArchivePackageID != "" {
				packageID = step.ExecutionSpec.Parameters.ArchivePackageID
				break
			}
		}
		index, found := byPackage[packageID]
		if !found && (len(byPackage) > 1 || (packageID != "" && len(byPackage) > 0)) {
			continue
		}
		rows = append(rows, subtitleOutput(rows[index], j, duration))
	}
	return rows
}

func subtitleOutput(sub EventOutputEvidence, j job.State, duration int64) EventOutputEvidence {
	sub.Stage = "SUBTITLE_PENDING"
	sub.ObservedAt, sub.ManifestChecksum = time.Time{}, ""
	sub.Completed, sub.Total, sub.Unit, sub.CauseCode = 0, 0, "", ""
	sub.Basis = []string{j.ID}
	published := false
	var quality *execution.StepOutput
	for _, step := range j.Steps {
		if step.Workflow.ID == j.CurrentStepID {
			sub.observeStep(j.ID, step, duration)
		}
		if step.OutputRecord == nil || step.Workflow.State != workflow.Completed {
			continue
		}
		for i, out := range step.OutputRecord.Outputs {
			if out.Kind == execution.OutputTranscript && out.TranscriptQuality != nil && out.TranscriptQuality.State == transcription.QualityReviewRecommended {
				quality = &step.OutputRecord.Outputs[i]
			}
			if out.Kind == execution.OutputArchiveRevision {
				published = true
				sub.ObservedAt, sub.ManifestChecksum = step.OutputRecord.RecordedAt, out.Checksum
				sub.Basis = append(sub.Basis, step.OutputRecord.VerificationReferences...)
			}
		}
	}
	if published && sub.Stage != "FAILED" {
		sub.Stage = "SUBTITLE_STORED"
		if quality != nil && len(quality.TranscriptQuality.Reasons) > 0 {
			sub.Stage = "SUBTITLE_REVIEW"
			sub.ReviewReasons = slices.Clone(quality.TranscriptQuality.Reasons)
			sub.CauseCode = string(quality.TranscriptQuality.Reasons[0])
			sub.IssueCount = quality.TranscriptQuality.IssueCount
			sub.Basis = append(sub.Basis, quality.ID, quality.RelativePath, quality.Checksum)
		}
	}
	return sub
}

func (row *EventOutputEvidence) observeStep(jobID string, step job.Step, duration int64) {
	if step.Workflow.State == workflow.Completed {
		return
	}
	row.Stage = string(step.Workflow.State)
	row.Basis = []string{jobID, step.Workflow.ID}
	row.Items = archiveworkspace.ObservedItems(step)
	if len(step.FailureEvidence) > 0 && (step.Workflow.State == workflow.Failed || step.Workflow.State == workflow.Waiting) {
		failure := step.FailureEvidence[len(step.FailureEvidence)-1]
		row.ObservedAt = failure.ObservedAt
		row.CauseCode = failure.CauseCode
		row.Basis = append(row.Basis, failure.EvidenceID)
		return
	}
	if step.Workflow.State != workflow.Running {
		return
	}
	if step.Workflow.ID == "render" || step.Workflow.ID == "sequence_render" {
		row.Stage = "RENDERING"
	}
	if len(step.ProgressEvidence) == 0 {
		return
	}
	progress := step.ProgressEvidence[len(step.ProgressEvidence)-1]
	if progress.FencingToken != step.Workflow.LastFencingToken {
		return
	}
	row.ObservedAt = progress.ObservedAt
	row.Basis = append(row.Basis, progress.EvidenceID)
	switch {
	case progress.Update.OutputMillis > 0:
		if row.Items != nil {
			duration = row.Items.DurationMillis
		}
		row.Completed, row.Total, row.Unit = progress.Update.OutputMillis, duration, "milliseconds"
	case progress.Update.ProcessPercent > 0:
		row.Completed, row.Total, row.Unit = int64(progress.Update.ProcessPercent), 100, "percent"
	}
}
