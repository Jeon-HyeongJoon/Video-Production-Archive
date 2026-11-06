package archivistloop

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type scriptedModel struct {
	requests []ModelRequest
	steps    []ModelResponse
}

func (model *scriptedModel) Respond(_ context.Context, request ModelRequest) (ModelResponse, error) {
	model.requests = append(model.requests, request)
	if len(model.steps) == 0 {
		return ModelResponse{}, errors.New("unexpected model request")
	}
	result := model.steps[0]
	model.steps = model.steps[1:]
	return result, nil
}

type recordingTools struct {
	available []AvailableTool
	calls     []ToolEnvelope
}

func (tools *recordingTools) List(context.Context) ([]AvailableTool, error) {
	return tools.available, nil
}

func (tools *recordingTools) Call(_ context.Context, envelope ToolEnvelope) (ToolResult, error) {
	tools.calls = append(tools.calls, envelope)
	return ToolResult{StatusCode: 200, Body: json.RawMessage(`{"data":{"archive_run_id":"RUN-20261107-1","state":"STAGE_ONE_RUNNING"}}`)}, nil
}

// Requirement: AGT-019
func TestArchivistLoopExecutesOnlyAdvertisedArchiveToolAndContinuesWithItsReceipt(t *testing.T) {
	model := &scriptedModel{steps: []ModelResponse{
		{
			RequestID: "response-tools", Output: []any{map[string]any{
				"type": "function_call", "call_id": "provider-call-1", "name": "archive_run_status_v1", "arguments": `{}`,
			}}, ToolCalls: []ToolCall{{CallID: "provider-call-1", Name: "archive_run_status_v1", Arguments: `{}`}},
		},
		{RequestID: "response-final", Text: "1단계 작업이 진행 중입니다.", Output: []any{map[string]any{"type": "message"}}},
	}}
	tools := &recordingTools{available: []AvailableTool{{Name: "archive.run.status.v1", ActionClass: "READ"}, {Name: "run_shell", ActionClass: "PROCESS"}}}
	var progress []string
	loop, err := New(Configuration{
		Model: "account-model", ModelClient: model, ToolClient: tools,
		Progress: func(summary string) error { progress = append(progress, summary); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), RuntimeContext{SessionID: "SESSION-1", TurnID: "TURN-1", FencingToken: 7}, "진행 상태를 확인해")
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary != "1단계 작업이 진행 중입니다." || len(tools.calls) != 1 || len(progress) != 1 {
		t.Fatalf("result=%+v calls=%+v progress=%v", result, tools.calls, progress)
	}
	call := tools.calls[0]
	if call.Tool != "archive.run.status.v1" || call.SessionID != "SESSION-1" || call.TurnID != "TURN-1" || call.FencingToken != 7 ||
		call.ToolCallID == "" || call.CommandID == "" || call.IdempotencyKey == "" || call.InputDigest == "" {
		t.Fatalf("correlation envelope is incomplete: %+v", call)
	}
	if len(model.requests) != 2 || len(model.requests[0].Tools) != 1 || model.requests[0].Tools[0].ProviderName != "archive_run_status_v1" {
		t.Fatalf("unsafe or unavailable tools reached model: %+v", model.requests)
	}
	lastInput := model.requests[1].Input[len(model.requests[1].Input)-1]
	output, ok := lastInput.(map[string]any)
	if !ok || output["type"] != "function_call_output" || output["call_id"] != "provider-call-1" {
		t.Fatalf("tool receipt was not returned to model: %#v", lastInput)
	}
}

// Requirement: AGT-020
func TestArchivistLoopStopsBeforeUnboundedModelOrToolExecution(t *testing.T) {
	model := &scriptedModel{steps: []ModelResponse{
		{RequestID: "r1", Output: []any{map[string]any{"type": "function_call"}}, ToolCalls: []ToolCall{{CallID: "c1", Name: "archive_run_status_v1", Arguments: `{}`}}},
		{RequestID: "r2", Output: []any{map[string]any{"type": "function_call"}}, ToolCalls: []ToolCall{{CallID: "c2", Name: "archive_run_status_v1", Arguments: `{}`}}},
	}}
	tools := &recordingTools{available: []AvailableTool{{Name: "archive.run.status.v1", ActionClass: "READ"}}}
	loop, err := New(Configuration{Model: "account-model", ModelClient: model, ToolClient: tools, MaximumRounds: 2, MaximumCalls: 8})
	if err != nil {
		t.Fatal(err)
	}
	_, err = loop.Run(context.Background(), RuntimeContext{SessionID: "SESSION-1", TurnID: "TURN-1", FencingToken: 9}, "계속 확인해")
	if !errors.Is(err, ErrExecutionLimit) || len(model.requests) != 2 || len(tools.calls) != 2 {
		t.Fatalf("unbounded loop was not stopped: requests=%d calls=%d err=%v", len(model.requests), len(tools.calls), err)
	}
}
