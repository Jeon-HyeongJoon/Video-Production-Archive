package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"videoarchive/internal/adapter/processruntime"
	"videoarchive/internal/application/archivistloop"
	"videoarchive/internal/core/archivistruntime"
)

// Requirement: LRH-016
func TestLocalArchivistCLICompletesToolTurnThroughHostProcessContract(t *testing.T) {
	if os.Getenv("ARCHIVE_LOCAL_VERTICAL_HELPER") == "1" {
		os.Exit(run([]string{"run"}))
	}
	var modelRequests atomic.Int32
	modelFailures := make(chan string, 4)
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sequence := modelRequests.Add(1)
		var payload struct {
			Model    string `json:"model"`
			Messages []struct {
				Role       string `json:"role"`
				Content    any    `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
			Tools []any `json:"tools"`
		}
		if request.URL.Path != "/v1/chat/completions" || json.NewDecoder(request.Body).Decode(&payload) != nil || payload.Model != "archive-local-test" || len(payload.Tools) != 1 {
			select {
			case modelFailures <- fmt.Sprintf("invalid request %s model=%q tools=%d", request.URL.Path, payload.Model, len(payload.Tools)):
			default:
			}
		}
		writer.Header().Set("Content-Type", "application/json")
		if sequence == 1 {
			response := map[string]any{
				"id": "local-turn-1",
				"choices": []any{map[string]any{"message": map[string]any{
					"role": "assistant", "content": nil,
					"tool_calls": []any{map[string]any{
						"id": "call-local-job", "type": "function",
						"function": map[string]any{"name": "archive_jobs_get_v1", "arguments": `{"job_id":"JOB-LOCAL-1"}`},
					}},
				}}},
			}
			_ = json.NewEncoder(writer).Encode(response)
			return
		}
		foundReceipt := false
		for _, message := range payload.Messages {
			if message.Role == "tool" && message.ToolCallID == "call-local-job" && strings.Contains(fmt.Sprint(message.Content), "JOB-LOCAL-1") {
				foundReceipt = true
			}
		}
		if !foundReceipt {
			select {
			case modelFailures <- "second model round omitted the Archive MCP receipt":
			default:
			}
		}
		response := map[string]any{
			"id": "local-turn-2",
			"choices": []any{map[string]any{"message": map[string]any{
				"role": "assistant", "content": "로컬 모델이 작업 상태를 확인했습니다.",
			}}},
		}
		_ = json.NewEncoder(writer).Encode(response)
	}))
	defer modelServer.Close()

	var toolCalls atomic.Int32
	envelopes := make(chan archivistloop.ToolEnvelope, 2)
	mcpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/mcp/v1/tools":
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": []any{map[string]any{
				"name": "archive.jobs.get.v1", "action_class": "READ",
			}}})
		case request.Method == http.MethodPost && request.URL.Path == "/mcp/v1/call":
			var envelope archivistloop.ToolEnvelope
			if json.NewDecoder(request.Body).Decode(&envelope) == nil {
				toolCalls.Add(1)
				envelopes <- envelope
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"job_id": "JOB-LOCAL-1", "status": "RUNNING"})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer mcpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	progress := make([]string, 0, 1)
	response, err := processruntime.New().Run(ctx, archivistruntime.LaunchPlan{
		Executable: os.Args[0], Arguments: []string{"-test.run=^TestLocalArchivistCLICompletesToolTurnThroughHostProcessContract$"}, WorkingDirectory: t.TempDir(),
	}, "작업 JOB-LOCAL-1 상태를 확인해", mcpServer.URL, map[string]string{
		"ARCHIVE_LOCAL_VERTICAL_HELPER": "1",
		"ARCHIVE_MODEL_PROVIDER":        "local",
		"ARCHIVE_MODEL_API_BASE":        modelServer.URL + "/v1",
		"ARCHIVE_MODEL":                 "archive-local-test",
		"ARCHIVE_SESSION_ID":            "SESSION-LOCAL-1",
		"ARCHIVE_TURN_ID":               "TURN-LOCAL-1",
		"ARCHIVE_FENCING_TOKEN":         "7",
	}, func() error { return nil }, func(update archivistruntime.ProgressUpdate) error {
		progress = append(progress, update.Summary)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case failure := <-modelFailures:
		t.Fatal(failure)
	default:
	}
	if modelRequests.Load() != 2 || toolCalls.Load() != 1 || strings.Join(progress, ",") != "작업 상태 확인" {
		t.Fatalf("model=%d tools=%d progress=%v", modelRequests.Load(), toolCalls.Load(), progress)
	}
	if response.Summary != "로컬 모델이 작업 상태를 확인했습니다." || len(response.ResultIDs) != 1 || response.ResultIDs[0] != "JOB-LOCAL-1" {
		t.Fatalf("unexpected final response: %+v", response)
	}
	envelope := <-envelopes
	if envelope.SessionID != "SESSION-LOCAL-1" || envelope.TurnID != "TURN-LOCAL-1" || envelope.FencingToken != 7 ||
		envelope.Tool != "archive.jobs.get.v1" || !strings.HasPrefix(envelope.IdempotencyKey, "archive-archivist:") {
		t.Fatalf("unexpected tool envelope: %+v", envelope)
	}
}
