package archiverun

import (
	"errors"
	"sort"
	"strings"

	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

var ErrInvalidArchiveRunProgress = errors.New("archive run progress is invalid")

type FolderProgress struct {
	FolderID    string
	Disposition vocabulary.OrganizationDisposition
	StageOne    workflow.StepState
	Stored      bool
}

type TranscriptionGate struct {
	Ready             bool
	RemainingWork     int
	HoldCount         int
	EligibleFolderIDs []string
}

// EvaluateTranscriptionGate considers only folders owned by one ArchiveRun.
// Scheduler work belonging to other ArchiveRuns is intentionally not an input.
func EvaluateTranscriptionGate(folders []FolderProgress) (TranscriptionGate, error) {
	if len(folders) == 0 {
		return TranscriptionGate{}, ErrInvalidArchiveRunProgress
	}
	seen := make(map[string]bool, len(folders))
	result := TranscriptionGate{}
	for _, folder := range folders {
		folder.FolderID = strings.TrimSpace(folder.FolderID)
		if folder.FolderID == "" || seen[folder.FolderID] || !folder.Disposition.Valid() || !validStepState(folder.StageOne) {
			return TranscriptionGate{}, ErrInvalidArchiveRunProgress
		}
		seen[folder.FolderID] = true
		if folder.Disposition == vocabulary.OrganizationHold {
			result.HoldCount++
			continue
		}
		switch folder.StageOne {
		case workflow.Completed, workflow.Failed, workflow.Cancelled:
		default:
			result.RemainingWork++
		}
		if folder.Stored && folder.StageOne == workflow.Completed &&
			(folder.Disposition == vocabulary.OrganizationAsIs || folder.Disposition == vocabulary.OrganizationRenderRecordings) {
			result.EligibleFolderIDs = append(result.EligibleFolderIDs, folder.FolderID)
		}
	}
	sort.Strings(result.EligibleFolderIDs)
	result.Ready = result.RemainingWork == 0
	return result, nil
}

type ArchiveRunProgress struct {
	TranscriptionGate
	TranscriptionStarted bool
	StageTwoRemaining    int
	Complete             bool
}

func EvaluateCompletion(folders []FolderProgress, transcriptionStarted bool, stageTwoStates []workflow.StepState) (ArchiveRunProgress, error) {
	gate, err := EvaluateTranscriptionGate(folders)
	if err != nil {
		return ArchiveRunProgress{}, err
	}
	result := ArchiveRunProgress{TranscriptionGate: gate, TranscriptionStarted: transcriptionStarted}
	for _, state := range stageTwoStates {
		if !validStepState(state) {
			return ArchiveRunProgress{}, ErrInvalidArchiveRunProgress
		}
		switch state {
		case workflow.Completed, workflow.Failed, workflow.Cancelled:
		default:
			result.StageTwoRemaining++
		}
	}
	if !gate.Ready {
		return result, nil
	}
	if len(gate.EligibleFolderIDs) > 0 && !transcriptionStarted {
		return result, nil
	}
	result.Complete = result.StageTwoRemaining == 0
	return result, nil
}

func validStepState(state workflow.StepState) bool {
	switch state {
	case workflow.Queued, workflow.Waiting, workflow.Reserved, workflow.Running, workflow.Verifying,
		workflow.Paused, workflow.Failed, workflow.Completed, workflow.Cancelled:
		return true
	default:
		return false
	}
}
