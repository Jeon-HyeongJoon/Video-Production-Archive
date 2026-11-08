package localmodel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"videoarchive/internal/application/archivistloop"
)

// Requirement: AGT-021
func TestLocalModelAdapterUsesLoopbackChatCompletionsAndPreservesToolReceipts(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Method != http.MethodPost || request.URL.Path != "/v1/chat/completions" || request.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected local model request: %s %s auth=%q", request.Method, request.URL.Path, request.Header.Get("Authorization"))
		}
		var payload struct {
			Model             string        `json:"model"`
			Messages          []chatMessage `json:"messages"`
			Tools             []any         `json:"tools"`
			Stream            bool          `json:"stream"`
			ParallelToolCalls bool          `json:"parallel_tool_calls"`
		}
		if json.NewDecoder(request.Body).Decode(&payload) != nil || payload.Model != "local-tool-model" || payload.Stream || payload.ParallelToolCalls || len(payload.Tools) != 1 {
			t.Fatalf("invalid local model request: %+v", payload)
		}
		if requests == 1 {
			if len(payload.Messages) != 2 || payload.Messages[0].Role != "system" || payload.Messages[1].Role != "user" {
				t.Fatalf("initial messages were not preserved: %+v", payload.Messages)
			}
			response := map[string]any{
				"id": "local-1",
				"choices": []any{map[string]any{"message": map[string]any{
					"role": "assistant", "content": nil,
					"tool_calls": []any{map[string]any{
						"id": "call-1", "type": "function",
						"function": map[string]any{"name": "archive_jobs_get_v1", "arguments": `{"job_id":"JOB-1"}`},
					}},
				}}},
			}
			_ = json.NewEncoder(writer).Encode(response)
			return
		}
		if len(payload.Messages) != 4 || payload.Messages[2].Role != "assistant" || len(payload.Messages[2].ToolCalls) != 1 ||
			payload.Messages[3].Role != "tool" || payload.Messages[3].ToolCallID != "call-1" || payload.Messages[3].Content != `{"status":"RUNNING"}` {
			t.Fatalf("tool receipt was not preserved: %+v", payload.Messages)
		}
		response := map[string]any{
			"id": "local-2",
			"choices": []any{map[string]any{"message": map[string]any{
				"role": "assistant", "content": "작업 상태를 확인했습니다.",
			}}},
		}
		_ = json.NewEncoder(writer).Encode(response)
	}))
	defer server.Close()

	client, err := New(Configuration{BaseURL: server.URL + "/v1", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	definition := archivistloop.ToolDefinition{
		Name: "archive.jobs.get.v1", ProviderName: "archive_jobs_get_v1", Description: "Get a job.",
		Parameters: map[string]any{"type": "object"},
	}
	first, err := client.Respond(context.Background(), archivistloop.ModelRequest{
		Model: "local-tool-model", Instructions: "Archive instructions", Input: []any{map[string]any{"role": "user", "content": "작업 상태"}}, Tools: []archivistloop.ToolDefinition{definition},
	})
	if err != nil || len(first.ToolCalls) != 1 || first.ToolCalls[0].CallID != "call-1" || len(first.Output) != 1 {
		t.Fatalf("first response=%+v err=%v", first, err)
	}
	secondInput := []any{map[string]any{"role": "user", "content": "작업 상태"}}
	secondInput = append(secondInput, first.Output...)
	secondInput = append(secondInput, map[string]any{"type": "function_call_output", "call_id": "call-1", "output": `{"status":"RUNNING"}`})
	second, err := client.Respond(context.Background(), archivistloop.ModelRequest{
		Model: "local-tool-model", Instructions: "Archive instructions", Input: secondInput, Tools: []archivistloop.ToolDefinition{definition},
	})
	if err != nil || second.Text != "작업 상태를 확인했습니다." || requests != 2 {
		t.Fatalf("second response=%+v requests=%d err=%v", second, requests, err)
	}
	if _, err := New(Configuration{BaseURL: "http://192.168.1.10:11434/v1"}); err == nil {
		t.Fatal("non-loopback unauthenticated model endpoint was accepted")
	}
}
