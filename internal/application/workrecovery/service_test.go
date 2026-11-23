package workrecovery

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"videoarchive/internal/application/workdiagnostics"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/pipeline"
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

// Requirement: WFL-016
func TestTranscriptionRecoveryReusesVerifiedAudioAndIsBoundToOneOutputFailure(t *testing.T) {
	now := time.Now().UTC()
	const checksum = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	video := execution.InputReference{OriginalAssetID: "ASSET-1", ArtifactID: "VIDEO-1", StorageID: "archive", RelativePath: "package/video.mp4", Kind: execution.OutputRendition, Checksum: checksum}
	audio := execution.OutputTarget{OriginalAssetID: "ASSET-1", ID: "AUDIO-1", StorageID: "archive", RelativePath: "staging/original/audio.wav", Kind: execution.OutputAudio}
	base, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: "STT-ORIGINAL", TemplateName: "archive-transcription", StepID: "audio", Operation: execution.OperationExtractSpeechAudio,
		Inputs: []execution.InputReference{video}, Outputs: []execution.OutputTarget{audio}, ProfileReference: "speech-ko-enhanced-16k@v1",
		Parameters: execution.StepExecutionParameters{ArchiveRunID: "RUN-1", WorkspaceDriveID: "VOLUME-1", ArchiveStorageID: "archive", InputRevision: 1, OrganizationDecisionID: "DECISION-1", ArchivePackageRevision: 1, SourceInputs: []execution.InputReference{video}},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := execution.NewStepOutputRecord(base, 1, "worker", now, []execution.StepOutput{{ID: audio.ID, Kind: audio.Kind, StorageID: audio.StorageID, RelativePath: audio.RelativePath, Checksum: checksum}}, []string{"verified-audio"})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := execution.MaterializeDependentStepExecutionSpec(execution.MaterializeRequest{JobID: base.JobID, TemplateName: base.TemplateName, StepID: "transcript", Completed: []execution.CompletedStep{{StepID: "audio", Spec: base, OutputRecord: record}}})
	if err != nil {
		t.Fatal(err)
	}
	exit := 11
	failure := job.StepFailureEvidence{EvidenceID: "EVD-TEMP-OPEN", Operation: spec.Operation, StepID: spec.StepID, FencingToken: 3, WorkflowAttempt: 3, FailureSequence: 3, FailureClass: workflow.FailureTransient, ResultingState: workflow.Failed, ProcessExitCode: &exit, CauseCode: execution.WhisperLongOutputOpenFailed}
	source := job.State{ID: base.JobID, TemplateName: base.TemplateName, Revision: 23, CurrentStepID: spec.StepID, TargetTitle: "recording", Steps: []job.Step{
		{Workflow: workflow.Step{ID: "audio", State: workflow.Completed}, ExecutionSpec: &base, OutputRecord: &record},
		{Workflow: workflow.Step{ID: spec.StepID, State: workflow.Failed}, ExecutionSpec: &spec, FailureEvidence: []job.StepFailureEvidence{failure}},
	}}
	fingerprint := "0123456789abcdef01234567"
	id := job.TerminalRecoveryJobID(source.ID, failure.EvidenceID)
	action := workdiagnostics.RecoveryAction{ActionCode: job.RecoveryActionRetryTranscriptionShortPath, JobID: source.ID, JobRevision: source.Revision, StepID: spec.StepID, FencingToken: 3, Basis: []string{failure.EvidenceID}, DiagnosticFingerprint: fingerprint, RecoveryJobID: id}
	diag := workdiagnostics.Snapshot{WorkKind: archivistruntime.WorkJob, WorkID: source.ID, WorkRevision: source.Revision, Fingerprint: fingerprint, Jobs: []workdiagnostics.JobDiagnostic{{JobID: source.ID}}, RecoveryActions: []workdiagnostics.RecoveryAction{action}}
	envelope := command.Envelope{CommandType: CreateTerminalRecoveryCommand, CommandID: "CMD-STT-RECOVERY", Actor: command.Actor{Type: command.ArchivistActor, ID: "archivist:session"}, RequestedVia: "mcp", TargetID: source.ID, ExpectedRevision: source.Revision, IdempotencyKey: "recover-short", Reason: "verify a shorter staging path", Parameters: map[string]any{"action_code": action.ActionCode, "step_id": spec.StepID, "fencing_token": uint64(3), "diagnostic_fingerprint": fingerprint, "basis": action.Basis}}
	for _, test := range []string{"observed failure", "legacy exit", "different cause", "unverified audio", "stale fencing token", "failed recovery Job"} {
		t.Run(test, func(t *testing.T) {
			state := source
			state.Steps = append([]job.Step(nil), source.Steps...)
			observation := failure
			switch test {
			case "legacy exit":
				observation.CauseCode = "PROCESS_EXIT_NONZERO"
			case "different cause":
				observation.CauseCode = "WHISPER_MODEL_INVALID"
			case "unverified audio":
				state.Steps[0].OutputRecord = nil
			case "stale fencing token":
				observation.FencingToken++
			case "failed recovery Job":
				state.TemplateName = pipeline.ArchiveTranscriptionRecoveryName
			}
			state.Steps[1].FailureEvidence = []job.StepFailureEvidence{observation}
			payload, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			handler, _ := job.NewHandler(func() time.Time { return now })
			commands := &creatingCommands{handler: handler}
			svc, _ := New(fixedDiagnostics{snapshot: diag}, commands, fixedStates{snapshot: command.Snapshot{TargetID: state.ID, Revision: state.Revision, State: payload}})
			result, err := svc.Apply(context.Background(), archivistruntime.WorkReference{Kind: archivistruntime.WorkJob, WorkID: state.ID, Revision: state.Revision}, envelope)
			if test != "observed failure" {
				if !errors.Is(err, ErrRecoveryNotSafe) || len(commands.seen) != 0 {
					t.Fatalf("unexpected recovery: %+v %v", result, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			replacement := result.Command.Value.(job.State)
			if replacement.ID != id || replacement.RecoverySource.FailureEvidenceID != failure.EvidenceID || replacement.CurrentStepID != "transcript" || len(replacement.Steps) != 5 {
				t.Fatalf("unexpected replacement: %+v", replacement)
			}
			for _, step := range replacement.Steps {
				if step.Workflow.ID != "transcript" {
					continue
				}
				retry := step.ExecutionSpec
				if retry == nil || retry.Validate() != nil || !reflect.DeepEqual(retry.Inputs, spec.Inputs) || retry.Outputs[0].ID != spec.Outputs[0].ID || retry.Outputs[0].StorageID != audio.StorageID || len(retry.Outputs[0].RelativePath) >= len(spec.Outputs[0].RelativePath) || retry.Parameters.ArchivePackageRevision != 1 {
					t.Fatalf("unsafe replacement spec: %+v", retry)
				}
			}
		})
	}
}
