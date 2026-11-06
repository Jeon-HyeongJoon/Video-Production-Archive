package archivistloop_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/adapter/chatgptauth"
	"videoarchive/internal/adapter/chatgptresponses"
	"videoarchive/internal/application/archivistloop"
)

type liveTokenSource struct {
	manager   *chatgptauth.Manager
	reference string
}

func (source liveTokenSource) AccessToken(ctx context.Context) (string, error) {
	token, _, err := source.manager.AccessToken(ctx, source.reference)
	return token, err
}

type liveReadTool struct{ calls int }

func (*liveReadTool) List(context.Context) ([]archivistloop.AvailableTool, error) {
	return []archivistloop.AvailableTool{{Name: "archive.run.status.v1", ActionClass: "READ"}}, nil
}

func (tool *liveReadTool) Call(_ context.Context, envelope archivistloop.ToolEnvelope) (archivistloop.ToolResult, error) {
	tool.calls++
	return archivistloop.ToolResult{StatusCode: 200, Body: json.RawMessage(`{"data":{"archive_run_id":"MAC-LOCAL-ACCEPTANCE","state":"STT_READY"}}`)}, nil
}

// Acceptance target: LRH-015 (planned until an eligible ChatGPT plan completes this live test).
func TestMacOSChatGPTAccountRunsOneBoundedArchiveToolTurn(t *testing.T) {
	reference := strings.TrimSpace(os.Getenv("ARCHIVE_CHATGPT_PROFILE_REF"))
	model := strings.TrimSpace(os.Getenv("ARCHIVE_CHATGPT_MODEL"))
	if reference == "" || model == "" {
		t.Skip("set ARCHIVE_CHATGPT_PROFILE_REF and ARCHIVE_CHATGPT_MODEL for live macOS acceptance")
	}
	manager, err := chatgptauth.New(chatgptauth.Configuration{
		Store: chatgptauth.NewOSStore(), OpenBrowser: func(string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := chatgptresponses.New(chatgptresponses.Configuration{
		TokenSource: liveTokenSource{manager: manager, reference: reference},
	})
	if err != nil {
		t.Fatal(err)
	}
	tools := &liveReadTool{}
	loop, err := archivistloop.New(archivistloop.Configuration{
		Model: model, ModelClient: provider, ToolClient: tools, MaximumRounds: 4, MaximumCalls: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	result, err := loop.Run(ctx, archivistloop.RuntimeContext{
		SessionID: "MAC-LOCAL-SESSION", TurnID: "MAC-LOCAL-TURN", FencingToken: 1,
	}, "archive_run_status_v1 도구로 현재 실행 상태를 한 번 확인한 뒤 한국어로 알려줘.")
	if err != nil {
		t.Fatal(err)
	}
	if tools.calls != 1 || strings.TrimSpace(result.Summary) == "" {
		t.Fatalf("live loop did not complete one bounded tool turn: calls=%d result=%+v", tools.calls, result)
	}
}
