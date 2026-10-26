package sqliteevidence_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"videoarchive/internal/adapter/sqliteevidence"
	"videoarchive/internal/application/capability"
	"videoarchive/internal/core/command"
)

// Requirement: OBS-001
func TestCapabilityEvidenceCorrelatesSuccessfulAndFailedExecutions(t *testing.T) {
	ctx := context.Background()
	sink, err := sqliteevidence.Open(t.TempDir() + "/evidence.db")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	commandStore := command.NewMemoryStore(map[string]command.Snapshot{
		"ASSET-1": {TargetID: "ASSET-1", State: json.RawMessage(`{"status":"new"}`)},
		"ASSET-2": {TargetID: "ASSET-2", State: json.RawMessage(`{"status":"new"}`)},
	})
	executionFailure := errors.New("transcription input is unreadable")
	bus := command.NewExecutor(commandStore, func(envelope command.Envelope, current command.Snapshot) (command.Change, error) {
		if envelope.CommandType == "transcript.fail" {
			return command.Change{}, executionFailure
		}
		return command.Change{
			Revision: current.Revision + 1,
			State:    json.RawMessage(`{"status":"recorded"}`),
			Value:    map[string]any{"result_id": "DESCRIPTION-1"},
			Events:   []command.Event{{EventID: "EVENT-1", EventType: "DescriptionRecorded"}},
		}, nil
	})
	registry, err := capability.NewRegistry(bus)
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range []capability.Definition{
		{
			Name: "description.record", Version: 1, Kind: capability.Immediate,
			ProjectReference: func(command.Result) (capability.Reference, error) {
				return capability.Reference{
					ResultID: "DESCRIPTION-1", VerificationReferences: []string{"checksum:sha256:verified-description"},
				}, nil
			},
		},
		{
			Name: "transcript.fail", Version: 1, Kind: capability.Immediate,
			ProjectReference: func(command.Result) (capability.Reference, error) {
				return capability.Reference{ResultID: "unreachable"}, nil
			},
		},
	} {
		if err := registry.Register(definition); err != nil {
			t.Fatal(err)
		}
	}
	observed, err := capability.NewObservedPort(registry, sink)
	if err != nil {
		t.Fatal(err)
	}

	successInput := observedInput("RUN-SUCCESS", "description.record", "CMD-SUCCESS", "ASSET-1", "JOB-1", "STEP-DESCRIBE")
	if _, err := observed.Execute(ctx, successInput); err != nil {
		t.Fatal(err)
	}
	failureInput := observedInput("RUN-FAILURE", "transcript.fail", "CMD-FAILURE", "ASSET-2", "JOB-2", "STEP-STT")
	if _, err := observed.Execute(ctx, failureInput); !errors.Is(err, executionFailure) {
		t.Fatalf("failure not returned: %v", err)
	}

	success := loadEvidence(t, ctx, sink, "RUN-SUCCESS")
	failure := loadEvidence(t, ctx, sink, "RUN-FAILURE")
	assertCorrelation(t, success, successInput)
	assertCorrelation(t, failure, failureInput)

	if phases := evidencePhases(success); !slices.Equal(phases, []capability.EvidencePhase{
		capability.EvidenceRequested, capability.EvidenceStarted, capability.EvidenceCompleted,
	}) {
		t.Fatalf("unexpected success phases: %v", phases)
	}
	completed := success[len(success)-1]
	if completed.Outcome != "SUCCEEDED" || !slices.Equal(completed.ResultIDs, []string{"DESCRIPTION-1"}) ||
		!slices.Equal(completed.VerificationReferences, []string{"checksum:sha256:verified-description"}) {
		t.Fatalf("success result evidence missing: %+v", completed)
	}

	if phases := evidencePhases(failure); !slices.Equal(phases, []capability.EvidencePhase{
		capability.EvidenceRequested, capability.EvidenceStarted, capability.EvidenceFailed,
	}) {
		t.Fatalf("unexpected failure phases: %v", phases)
	}
	failed := failure[len(failure)-1]
	if failed.Outcome != "FAILED" || failed.ErrorClass != "CAPABILITY_EXECUTION" || len(failed.ResultIDs) != 0 {
		t.Fatalf("failure evidence missing: %+v", failed)
	}
}

func observedInput(runID, name, commandID, targetID, jobID, stepID string) capability.Input {
	return capability.Input{
		Name: name, Version: 1,
		Command: command.Envelope{
			CommandID: commandID, CommandType: name,
			Actor:        command.Actor{Type: command.AgentActor, ID: "agent-1"},
			RequestedVia: "application-port", TargetID: targetID,
			IdempotencyKey: commandID + ":v1", Reason: "capability evidence verification",
		},
		Correlation: capability.Correlation{
			RunID: runID, JobID: jobID, StepID: stepID, WorkerID: "WORKER-1",
			WorkspaceDriveIDs: []string{"DRIVE-1"}, InputIDs: []string{targetID},
		},
	}
}

func loadEvidence(t *testing.T, ctx context.Context, sink *sqliteevidence.Store, runID string) []capability.Evidence {
	t.Helper()
	records, err := sink.ByRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func assertCorrelation(t *testing.T, records []capability.Evidence, input capability.Input) {
	t.Helper()
	if len(records) != 3 {
		t.Fatalf("%s evidence count=%d", input.Correlation.RunID, len(records))
	}
	for _, record := range records {
		if record.RunID != input.Correlation.RunID || record.CommandID != input.Command.CommandID ||
			record.JobID != input.Correlation.JobID || record.StepID != input.Correlation.StepID ||
			record.WorkerID != input.Correlation.WorkerID ||
			!slices.Equal(record.WorkspaceDriveIDs, input.Correlation.WorkspaceDriveIDs) {
			t.Fatalf("correlation was not preserved: %+v", record)
		}
	}
}

func evidencePhases(records []capability.Evidence) []capability.EvidencePhase {
	phases := make([]capability.EvidencePhase, 0, len(records))
	for _, record := range records {
		phases = append(phases, record.Phase)
	}
	return phases
}
