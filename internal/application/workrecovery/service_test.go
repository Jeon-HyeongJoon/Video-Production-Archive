package workrecovery

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"videoarchive/internal/application/workdiagnostics"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

type fixedDiagnostics struct{ snapshot workdiagnostics.Snapshot }

func (fixture fixedDiagnostics) Diagnose(context.Context, archivistruntime.WorkReference) (workdiagnostics.Snapshot, error) {
	return fixture.snapshot, nil
}

type fixedStates struct{ snapshot command.Snapshot }

func (fixture fixedStates) Load(string) (command.Snapshot, error) { return fixture.snapshot, nil }

type creatingCommands struct {
	handler *job.Handler
	seen    []command.Envelope
}

func (fixture *creatingCommands) Execute(_ context.Context, envelope command.Envelope) (command.Result, error) {
	fixture.seen = append(fixture.seen, envelope)
	change, err := fixture.handler.Handle(envelope, command.Snapshot{TargetID: envelope.TargetID})
	if err != nil {
		return command.Result{}, err
	}
	return command.Result{CommandID: envelope.CommandID, TargetID: envelope.TargetID, Revision: change.Revision, Value: change.Value}, nil
}

// Requirement: WFL-015
func TestTerminalArchiveFailureRecoveryCreatesLinkedJobAndKeepsOriginalFailed(t *testing.T) {
	now := time.Date(2026, 11, 19, 16, 0, 0, 0, time.UTC)
	checksum := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: "JOB-SOURCE-FAILED", StepID: "archive_package_revision", TemplateName: "archive-transcription",
		Operation: execution.OperationReviseArchivePackage,
		Inputs: []execution.InputReference{
			{OriginalAssetID: "ASSET-1", ArtifactID: "TRANSCRIPT-1", StorageID: "ARCHIVE-1", RelativePath: "staging/transcript.json", Checksum: checksum, Kind: execution.OutputTranscript},
			{OriginalAssetID: "ASSET-1", ArtifactID: "TEXT-1", StorageID: "ARCHIVE-1", RelativePath: "staging/transcript.txt", Checksum: checksum, Kind: execution.OutputTranscriptText},
			{OriginalAssetID: "ASSET-1", ArtifactID: "SUBTITLE-1", StorageID: "ARCHIVE-1", RelativePath: "staging/subtitle.srt", Checksum: checksum, Kind: execution.OutputSubtitle},
		},
		Outputs: []execution.OutputTarget{{ID: "VP-20261119-TEST", Kind: execution.OutputArchiveRevision, StorageID: "ARCHIVE-1", RelativePath: "package/metadata.json"}},
		Parameters: execution.StepExecutionParameters{
			ArchiveRunID: "RUN-1", OrganizationDecisionID: "DECISION-1", WorkspaceDriveID: "VOLUME-1",
			ArchiveStorageID: "ARCHIVE-1", InputRevision: 1, ArchivePackageID: "VP-20261119-TEST", ArchivePackageRevision: 1,
			SourceInputs: []execution.InputReference{{OriginalAssetID: "ASSET-1", OriginalName: "source.mp4", StorageID: "SOURCE-1", RelativePath: "source.mp4", Checksum: checksum}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	failure := job.StepFailureEvidence{
		EvidenceID: "EVD-TERMINAL-PACKAGE", StepID: "archive_package_revision", Operation: execution.OperationReviseArchivePackage,
		FailureSequence: 3, WorkflowAttempt: 3, WorkerID: "WORKER-1", FencingToken: 9,
		FailureClass: workflow.FailureTransient, ErrorClass: "STEP_EXECUTION_FAILED", CauseCode: "REVISE_ARCHIVE_PACKAGE_FAILED",
		Summary: "Archive package revision operation failed", ObservedAt: now.Add(-time.Minute), ResultingState: workflow.Failed,
	}
	source := job.State{
		ID: "JOB-SOURCE-FAILED", Revision: 8, TemplateName: "archive-transcription", TargetTitle: "recording",
		CurrentStepID: "archive_package_revision", Steps: []job.Step{{
			Workflow:      workflow.Step{ID: "archive_package_revision", State: workflow.Failed, Attempt: 3, MaxAttempts: 3},
			ResourceClass: vocabulary.ResourceLight, ExecutorKind: vocabulary.ExecutorWorker,
			ExecutionPriority: "BACKGROUND", RequiredStorageIDs: []string{"ARCHIVE-1"}, ExecutionSpec: &spec,
			FailureEvidence: []job.StepFailureEvidence{failure}, ErrorClass: failure.ErrorClass, LastEventAt: failure.ObservedAt,
		}},
	}
	payload, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := "0123456789abcdef01234567"
	recoveryJobID := job.TerminalRecoveryJobID(source.ID, failure.EvidenceID)
	diagnostic := workdiagnostics.Snapshot{
		WorkKind: archivistruntime.WorkJob, WorkID: source.ID, WorkRevision: source.Revision, Fingerprint: fingerprint,
		Jobs: []workdiagnostics.JobDiagnostic{{JobID: source.ID, JobRevision: source.Revision, StepID: source.CurrentStepID, StepState: workflow.Failed}},
		RecoveryActions: []workdiagnostics.RecoveryAction{{
			ActionCode: job.RecoveryActionRetryTerminalArchivePublication, JobID: source.ID, JobRevision: source.Revision,
			StepID: source.CurrentStepID, FencingToken: failure.FencingToken, DiagnosticFingerprint: fingerprint,
			Basis: []string{failure.EvidenceID}, RecoveryJobID: recoveryJobID,
		}},
	}
	handler, err := job.NewHandler(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	commands := &creatingCommands{handler: handler}
	service, err := New(fixedDiagnostics{snapshot: diagnostic}, commands, fixedStates{snapshot: command.Snapshot{TargetID: source.ID, Revision: source.Revision, State: payload}})
	if err != nil {
		t.Fatal(err)
	}
	request := command.Envelope{
		CommandID: "COMMAND-RECOVERY", CommandType: CreateTerminalRecoveryCommand,
		Actor: command.Actor{Type: command.ArchivistActor, ID: "archivist:SESSION-1"}, RequestedVia: "mcp",
		TargetID: source.ID, ExpectedRevision: source.Revision, IdempotencyKey: "terminal-recovery", Reason: "recover archive publication",
		Parameters: map[string]any{
			"action_code": job.RecoveryActionRetryTerminalArchivePublication, "step_id": source.CurrentStepID,
			"fencing_token": failure.FencingToken, "diagnostic_fingerprint": fingerprint, "basis": []string{failure.EvidenceID},
		},
	}
	result, err := service.Apply(context.Background(), archivistruntime.WorkReference{Kind: archivistruntime.WorkJob, WorkID: source.ID, Revision: source.Revision}, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RecoveryJobID != recoveryJobID || len(commands.seen) != 1 || commands.seen[0].CommandType != job.CreateCommand ||
		commands.seen[0].TargetID != recoveryJobID || commands.seen[0].ExpectedRevision != 0 {
		t.Fatalf("replacement recovery Job was not created deterministically: result=%+v commands=%+v", result, commands.seen)
	}
	recovered, ok := result.Command.Value.(job.State)
	if !ok || recovered.RecoverySource == nil || recovered.RecoverySource.JobID != source.ID ||
		recovered.RecoverySource.FailureEvidenceID != failure.EvidenceID || recovered.Steps[0].ExecutionSpec == nil ||
		recovered.Steps[0].ExecutionSpec.Operation != execution.OperationReconcileArchivePublication {
		t.Fatalf("recovery Job lost the immutable source evidence link: %+v", result.Command.Value)
	}
	var unchanged job.State
	if json.Unmarshal(payload, &unchanged) != nil || unchanged.Steps[0].Workflow.State != workflow.Failed ||
		len(unchanged.Steps[0].FailureEvidence) != 1 {
		t.Fatalf("source terminal failure was mutated: %+v", unchanged)
	}
}
