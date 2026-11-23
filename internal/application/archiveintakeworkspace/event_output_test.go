package archiveintakeworkspace

import (
	"slices"
	"testing"
	"time"

	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

// Guard the producer as well as the MCP wire contract: stale attempts cannot
// supply progress, and subtitle review must retain the verified video receipt.
func TestEventOutputUsesFencedProgressAndKeepsVideoPublicationAlongsideSubtitleReview(t *testing.T) {
	d := organization.OrganizationDecision{ID: "D", InputRevision: 7, Folder: "391216-행사/PGM", Disposition: vocabulary.OrganizationRenderRecordings, EventPreview: &organization.EventPreview{Directory: "391216-행사", Video: "PGM.mp4"}, Inputs: []organization.Input{{OriginalAssetID: "A", RelativePath: "391216-행사/PGM/master.mov", Inspection: mediaanalysis.MediaInspection{DurationMillis: 60000}}}}
	step := job.Step{Workflow: workflow.Step{ID: "render", State: workflow.Running, LastFencingToken: 2}, ProgressEvidence: []job.StepProgressEvidence{{FencingToken: 1, EvidenceID: "stale", Update: execution.ProgressUpdate{OutputMillis: 20000}}}}
	state := job.State{ID: "J", CurrentStepID: "render", Steps: []job.Step{step}}
	rows := eventOutput("RUN", d, state, nil)
	if len(rows) != 1 || rows[0].Completed != 0 || slices.Contains(rows[0].Basis, "stale") {
		t.Fatal(rows)
	}
	state.Steps[0].ProgressEvidence[0].FencingToken = 2
	state.Steps[0].ProgressEvidence[0].EvidenceID = "progress"
	observed := time.Unix(123, 0).UTC()
	state.Steps[0].ProgressEvidence[0].ObservedAt = observed
	rows = eventOutput("RUN", d, state, nil)
	if rows[0].Stage != "RENDERING" || rows[0].Completed != 20000 || rows[0].Total != 60000 || !rows[0].ObservedAt.Equal(observed) || !slices.Contains(rows[0].Basis, "progress") {
		t.Fatal(rows)
	}
	state.Steps[0].Workflow.State = workflow.Failed
	state.Steps[0].FailureEvidence = []job.StepFailureEvidence{{CauseCode: "ENCODER_UNAVAILABLE", EvidenceID: "failed-encoder"}}
	rows = eventOutput("RUN", d, state, nil)
	if rows[0].Stage != "FAILED" || rows[0].CauseCode != "ENCODER_UNAVAILABLE" || !slices.Contains(rows[0].Basis, "failed-encoder") {
		t.Fatal(rows)
	}
	state.Steps = []job.Step{
		{Workflow: workflow.Step{ID: "qc", State: workflow.Completed}, OutputRecord: &execution.StepOutputRecord{VerificationReferences: []string{"qc-proof"}}},
		{Workflow: workflow.Step{ID: "organize", State: workflow.Completed}, OutputRecord: &execution.StepOutputRecord{VerificationReferences: []string{"manifest-proof"}, Outputs: []execution.StepOutput{{Kind: execution.OutputOrganizedMedia, ID: "VP-20261124-abc", RelativePath: "stored/.archive-package.json", Checksum: "verified-manifest"}}}},
	}
	sub := job.State{ID: "SUB", Steps: []job.Step{
		{Workflow: workflow.Step{ID: "transcript", State: workflow.Completed}, OutputRecord: &execution.StepOutputRecord{Outputs: []execution.StepOutput{{Kind: execution.OutputTranscript, ID: "transcript", RelativePath: "staging/raw.json", Checksum: "checksum", TranscriptQuality: &transcription.QualityAssessment{State: transcription.QualityReviewRecommended, Reasons: []transcription.QualityReason{transcription.QualityReasonSegmentTooLong}, IssueCount: 3}}}}},
		{Workflow: workflow.Step{ID: "archive_package_revision", State: workflow.Completed}, OutputRecord: &execution.StepOutputRecord{Outputs: []execution.StepOutput{{Kind: execution.OutputArchiveRevision}}, VerificationReferences: []string{"revision-proof"}}},
	}}
	rows = eventOutput("RUN", d, state, []job.State{sub})
	if len(rows) != 2 || rows[0].Stage != "STORED" || rows[0].ManifestChecksum != "verified-manifest" || rows[0].RelativeVideo != "stored/master.mp4" || !slices.Contains(rows[0].Basis, "qc-proof") || !slices.Contains(rows[0].Basis, "manifest-proof") || rows[1].Stage != "SUBTITLE_REVIEW" || rows[1].IssueCount != 3 || !slices.Contains(rows[1].Basis, "staging/raw.json") {
		t.Fatal(rows)
	}
}
