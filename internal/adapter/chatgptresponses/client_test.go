package chatgptresponses

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"videoarchive/internal/application/archivistloop"
)

type staticToken string

func (token staticToken) AccessToken(context.Context) (string, error) { return string(token), nil }

// Requirement: AGT-018
func TestChatGPTResponsesUsesStatelessStreamingContractAndReturnsCompletedOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/responses" || request.Header.Get("Authorization") != "Bearer oauth-access" {
			t.Fatalf("unexpected request: %s %s auth=%q", request.Method, request.URL.Path, request.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(request.Body)
		var payload map[string]any
		if json.Unmarshal(body, &payload) != nil || payload["store"] != false || payload["stream"] != true || payload["model"] != "account-model" {
			t.Fatalf("invalid Responses contract: %s", body)
		}
		for _, unsupported := range []string{"previous_response_id", "conversation", "background", "max_output_tokens", "metadata", "temperature", "user"} {
			if _, exists := payload[unsupported]; exists {
				t.Fatalf("unsupported field %q sent: %s", unsupported, body)
			}
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		completed := map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "resp-1", "status": "completed", "output": []any{map[string]any{
				"id": "msg-1", "type": "message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "아카이브 상태를 확인했습니다.", "annotations": []any{}}},
			}},
		}}
		encoded, _ := json.Marshal(completed)
		_, _ = writer.Write([]byte("data: " + string(encoded) + "\n\n"))
	}))
	defer server.Close()
	client, err := New(Configuration{BaseURL: server.URL + "/v1", HTTPClient: server.Client(), TokenSource: staticToken("oauth-access")})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Respond(context.Background(), archivistloop.ModelRequest{
		Model: "account-model", Instructions: "Archive instructions", Input: []any{map[string]any{"role": "user", "content": "상태"}},
		Tools: []archivistloop.ToolDefinition{{
			Name: "archive.run.status.v1", ProviderName: "archive_run_status_v1", Description: "status",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		}},
	})
	if err != nil || response.RequestID != "resp-1" || response.Text != "아카이브 상태를 확인했습니다." || len(response.Output) != 1 {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	emptyTokenClient, err := New(Configuration{BaseURL: server.URL + "/v1", HTTPClient: server.Client(), TokenSource: staticToken("")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := emptyTokenClient.Respond(context.Background(), archivistloop.ModelRequest{Model: "account-model"}); !errors.Is(err, ErrResponseFailed) {
		t.Fatalf("empty OAuth token did not stop inference: %v", err)
	}
}
