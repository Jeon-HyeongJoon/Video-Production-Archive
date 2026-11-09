package capability_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"videoarchive/internal/adapter/sqlitecommand"
	"videoarchive/internal/application/capability"
	"videoarchive/internal/core/command"
)

func TestRegisteredCapabilitiesExecuteDirectlyThroughSharedCommandBus(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitecommand.Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, targetID := range []string{"ASSET-1", "JOB-1"} {
		if err := store.Seed(ctx, command.Snapshot{
			TargetID: targetID,
			Revision: 0,
			State:    json.RawMessage(`{"status":"new"}`),
		}); err != nil {
			t.Fatal(err)
		}
	}

	bus := command.NewExecutor(store, func(envelope command.Envelope, current command.Snapshot) (command.Change, error) {
		result := map[string]any{}
		eventType := ""
		switch envelope.CommandType {
		case "description.record":
			result["result_id"] = "DESCRIPTION-1"
			eventType = "DescriptionRecorded"
		case "rendition.queue":
			result["job_id"] = "JOB-1"
			result["step_id"] = "STEP-RENDER-1"
			eventType = "RenditionQueued"
		default:
			return command.Change{}, fmt.Errorf("unexpected command type %q", envelope.CommandType)
		}
		return command.Change{
			Revision: current.Revision + 1,
			State:    json.RawMessage(`{"status":"accepted"}`),
			Value:    result,
			Events: []command.Event{{
				EventID:   "EVENT-" + envelope.CommandID,
				EventType: eventType,
			}},
		}, nil
	})
	registry, err := capability.NewRegistry(bus)
	if err != nil {
		t.Fatal(err)
	}

	if err := registry.Register(capability.Definition{
		Name: "description.record", Version: 1, Kind: capability.Immediate,
		ProjectReference: func(result command.Result) (capability.Reference, error) {
			return capability.Reference{ResultID: result.Value.(map[string]any)["result_id"].(string)}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(capability.Definition{
		Name: "rendition.queue", Version: 1, Kind: capability.LongRunning,
		ProjectReference: func(result command.Result) (capability.Reference, error) {
			value := result.Value.(map[string]any)
			return capability.Reference{JobID: value["job_id"].(string), StepID: value["step_id"].(string)}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	immediate, err := registry.Execute(ctx, capability.Input{
		Name: "description.record", Version: 1,
		Command: directCommand("CMD-DESCRIBE-1", "description.record", "ASSET-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if immediate.Command.CommandID != "CMD-DESCRIBE-1" || immediate.Reference.ResultID != "DESCRIPTION-1" {
		t.Fatalf("short capability returned wrong receipt: %+v", immediate)
	}

	longRunning, err := registry.Execute(ctx, capability.Input{
		Name: "rendition.queue", Version: 1,
		Command: directCommand("CMD-RENDER-1", "rendition.queue", "JOB-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if longRunning.Command.CommandID != "CMD-RENDER-1" || longRunning.Reference.JobID != "JOB-1" || longRunning.Reference.StepID != "STEP-RENDER-1" {
		t.Fatalf("long-running capability returned wrong handles: %+v", longRunning)
	}

	for _, targetID := range []string{"ASSET-1", "JOB-1"} {
		snapshot, err := store.Load(targetID)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Revision != 1 {
			t.Fatalf("%s command did not pass through shared bus: revision=%d", targetID, snapshot.Revision)
		}
	}
}

func directCommand(commandID, commandType, targetID string) command.Envelope {
	return command.Envelope{
		CommandID: commandID, CommandType: commandType,
		Actor:        command.Actor{Type: command.HumanActor, ID: "operator-1"},
		RequestedVia: "application-port", TargetID: targetID, ExpectedRevision: 0,
		IdempotencyKey: commandID + ":v1", Reason: "direct capability execution",
	}
}
