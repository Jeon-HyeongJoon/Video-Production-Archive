package archivistruntime

import (
	"testing"

	"videoarchive/internal/core/command"
)

// Requirement: MCP-002
func TestArchivistToolCallMapsCorrelationsAndReplaysStoredReceipt(t *testing.T) {
	request := ToolCallRequest{
		ID: "CALL-1", CommandID: "COMMAND-1", ArchivistHostID: "host-a", ArchivistSessionID: "SESSION-1",
		ArchivistTurnID: "TURN-1", ArchivistID: "archivist-1", FencingToken: 7, ToolName: "job.cancel",
		InputDigest: "sha256:input", IdempotencyKey: "tool-call-1", TargetID: "JOB-1", ExpectedRevision: 3,
		Reason: "operator requested cancellation", Parameters: map[string]any{"mode": "safe"},
	}
	decision, err := PrepareToolCall(request, nil)
	if err != nil || decision.Command == nil || decision.Replay != nil {
		t.Fatalf("first call was not mapped to a command: decision=%+v err=%v", decision, err)
	}
	if err := decision.Command.Validate(); err != nil {
		t.Fatalf("mapped command is invalid: %v", err)
	}
	parameters := decision.Command.Parameters
	if parameters["archivist_host_id"] != "host-a" || parameters["archivist_session_id"] != "SESSION-1" ||
		parameters["archivist_turn_id"] != "TURN-1" || parameters["archivist_tool_call_id"] != "CALL-1" ||
		parameters["archivist_turn_lease_fencing_token"] != uint64(7) {
		t.Fatalf("tool correlations missing from command: %+v", parameters)
	}
	receipt := ToolCallReceipt{
		ArchivistToolCallID: "CALL-1", InputDigest: "sha256:input", IdempotencyKey: "tool-call-1",
		Result: command.Result{CommandID: "COMMAND-1", TargetID: "JOB-1", Revision: 4},
	}
	decision, err = PrepareToolCall(request, []ToolCallReceipt{receipt})
	if err != nil || decision.Command != nil || decision.Replay == nil || decision.Replay.Result.CommandID != "COMMAND-1" {
		t.Fatalf("stored tool receipt was not replayed: decision=%+v err=%v", decision, err)
	}
}
