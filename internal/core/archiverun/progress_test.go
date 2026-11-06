package archiverun

import (
	"reflect"
	"testing"

	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

// Requirement: PIP-008
func TestTranscriptionGateUsesOnlyOwningArchiveRunStageOneAndExcludesHold(t *testing.T) {
	folders := []FolderProgress{
		{FolderID: "recording-b", Disposition: vocabulary.OrganizationRenderRecordings, StageOne: workflow.Completed, Stored: true, TranscriptionEligible: true},
		{FolderID: "hold", Disposition: vocabulary.OrganizationHold, StageOne: workflow.Waiting},
		{FolderID: "failed", Disposition: vocabulary.OrganizationAsIs, StageOne: workflow.Failed},
		{FolderID: "source", Disposition: vocabulary.OrganizationCompileSourceSequence, StageOne: workflow.Completed, Stored: true},
		{FolderID: "silent", Disposition: vocabulary.OrganizationRenderRecordings, StageOne: workflow.Completed, Stored: true},
	}
	gate, err := EvaluateTranscriptionGate(folders)
	if err != nil {
		t.Fatal(err)
	}
	if !gate.Ready || gate.RemainingWork != 0 || gate.HoldCount != 1 || !reflect.DeepEqual(gate.EligibleFolderIDs, []string{"recording-b"}) {
		t.Fatalf("archive-run-local gate did not exclude HOLD and non-recordings: %+v", gate)
	}
	folders = append(folders, FolderProgress{
		FolderID: "retry", Disposition: vocabulary.OrganizationRenderRecordings, StageOne: workflow.Queued,
	})
	blocked, err := EvaluateTranscriptionGate(folders)
	if err != nil || blocked.Ready || blocked.RemainingWork != 1 {
		t.Fatalf("queued retry did not block its own archive run: gate=%+v err=%v", blocked, err)
	}
}

// Requirement: ORG-013
func TestArchiveRunCompletesWithHoldReportedSeparatelyWhenNoExecutableWorkRemains(t *testing.T) {
	folders := []FolderProgress{
		{FolderID: "recording", Disposition: vocabulary.OrganizationAsIs, StageOne: workflow.Completed, Stored: true, TranscriptionEligible: true},
		{FolderID: "hold", Disposition: vocabulary.OrganizationHold, StageOne: workflow.Waiting},
	}
	available, err := EvaluateCompletion(folders, false, nil)
	if err != nil || available.Complete || !available.Ready || available.HoldCount != 1 {
		t.Fatalf("archive run completed before available STT was started: progress=%+v err=%v", available, err)
	}
	complete, err := EvaluateCompletion(folders, true, []workflow.StepState{workflow.Completed})
	if err != nil || !complete.Complete || complete.HoldCount != 1 || complete.StageTwoRemaining != 0 {
		t.Fatalf("HOLD incorrectly prevented executable-work completion: progress=%+v err=%v", complete, err)
	}
}
