package archiveintakeworkspace

import (
	"path"
	"slices"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

type EventOutputEvidence struct {
	ObservedAt       time.Time                     `json:"observed_at,omitzero"`
	ManifestChecksum string                        `json:"manifest_checksum,omitempty"`
	ReviewReasons    []transcription.QualityReason `json:"review_reasons,omitempty"`
	ArchiveRunID     string                        `json:"archive_run_id"`
	InputRevision    uint64                        `json:"input_revision"`
	Folder           string                        `json:"folder,omitempty"`
	RelativeVideo    string                        `json:"relative_video"`
	PathState        string                        `json:"path_state,omitempty"`
	Stage            string                        `json:"stage"`
	Completed        int64                         `json:"completed,omitempty"`
	Total            int64                         `json:"total,omitempty"`
	Unit             string                        `json:"unit,omitempty"`
	CauseCode        string                        `json:"cause_code,omitempty"`
	IssueCount       int                           `json:"issue_count,omitempty"`
	Basis            []string                      `json:"basis"`
}

func eventOutput(run string, d organization.OrganizationDecision, first job.State, second []job.State) []EventOutputEvidence {
	if d.EventPreview == nil || d.Disposition == vocabulary.OrganizationHold {
		return nil
	}
	row := EventOutputEvidence{ArchiveRunID: run, InputRevision: d.InputRevision, Folder: d.Folder,
		RelativeVideo: path.Join(d.EventPreview.Directory, d.EventPreview.Video), PathState: "EVENT_PREVIEW", Stage: "PLANNED", Basis: []string{d.ID}}
	var qcRefs []string
	var duration int64
	for _, v := range d.Inputs {
		duration += v.Inspection.DurationMillis
	}
	for _, step := range first.Steps {
		if step.Workflow.ID == "qc" && step.Workflow.State == workflow.Completed && step.OutputRecord != nil {
			qcRefs = slices.Clone(step.OutputRecord.VerificationReferences)
		}
		if step.Workflow.ID == "organize" && step.Workflow.State == workflow.Completed && step.OutputRecord != nil {
			for _, out := range step.OutputRecord.Outputs {
				if out.Kind != execution.OutputOrganizedMedia || out.RelativePath == "" {
					continue
				}
				names, _ := completedWorkSources(d, "")
				present, err := archive.PresentPackage(out.ID, d.Folder, names)
				if err != nil {
					continue
				}
				row.RelativeVideo = path.Join(path.Dir(out.RelativePath), present.VideoFileName)
				row.PathState = "PUBLISHED_PACKAGE"
				row.Stage = "STORED"
				row.ObservedAt, row.ManifestChecksum = step.OutputRecord.RecordedAt, out.Checksum
				row.Completed, row.Total, row.Unit = 0, 0, ""
				row.Basis = append(slices.Clone(qcRefs), step.OutputRecord.VerificationReferences...)
				row.Basis = append(row.Basis, out.RelativePath)
			}
		}
		if step.Workflow.ID == first.CurrentStepID && row.PathState != "PUBLISHED_PACKAGE" {
			row.observeStep(first.ID, step, duration)
		}
	}
	// Keep video publication evidence when the separate subtitle job is running or fails.
	rows := []EventOutputEvidence{row}
	for _, j := range second {
		sub := row
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
		rows = append(rows, sub)
	}
	return rows
}

func (row *EventOutputEvidence) observeStep(jobID string, step job.Step, duration int64) {
	if step.Workflow.State == workflow.Completed {
		return
	}
	row.Stage = string(step.Workflow.State)
	row.Basis = []string{jobID, step.Workflow.ID}
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
		row.Completed, row.Total, row.Unit = progress.Update.OutputMillis, duration, "milliseconds"
	case progress.Update.ProcessPercent > 0:
		row.Completed, row.Total, row.Unit = int64(progress.Update.ProcessPercent), 100, "percent"
	}
}
