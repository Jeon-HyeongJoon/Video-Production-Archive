package codexcli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"videoarchive/internal/application/archivistloop"
)

// Requirement: MCP-006
func TestCodexMCPBridgeExposesOnlyArchiveToolsAndPreservesTurnIdentity(t *testing.T) {
	tools := &fixtureTools{available: []archivistloop.AvailableTool{
		{Name: "archive.run.status.v1", ActionClass: "READ"},
		{Name: "run_shell", ActionClass: "PROCESS"},
	}}
	server := &MCPServer{
		Runtime: archivistloop.RuntimeContext{SessionID: "SESSION-9", TurnID: "TURN-7", FencingToken: 41},
		Tools:   tools, MaximumCalls: 1,
	}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"archive_run_status_v1","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"run_shell","arguments":{"command":"whoami"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"archive_run_status_v1","arguments":{}}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected five MCP responses, got %d: %s", len(lines), output.String())
	}
	var listed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(lines[1]), &listed) != nil || len(listed.Result.Tools) != 1 || listed.Result.Tools[0].Name != "archive_run_status_v1" {
		t.Fatalf("MCP exposed a tool outside the Archive allowlist: %s", lines[1])
	}
	if len(tools.calls) != 1 {
		t.Fatalf("only the first allowed call may reach Control, got %#v", tools.calls)
	}
	call := tools.calls[0]
	digest := sha256.Sum256([]byte(`{}`))
	if call.SessionID != "SESSION-9" || call.TurnID != "TURN-7" || call.FencingToken != 41 ||
		call.Tool != "archive.run.status.v1" || call.InputDigest != hex.EncodeToString(digest[:]) ||
		!strings.HasPrefix(call.ToolCallID, "CALL-") || !strings.HasPrefix(call.CommandID, "COMMAND-") ||
		!strings.HasPrefix(call.IdempotencyKey, "archive-codex:") {
		t.Fatalf("turn correlation or deterministic identity was not preserved: %#v", call)
	}
	if !strings.Contains(lines[3], "unknown Archive tool") || !strings.Contains(lines[4], "call limit reached") {
		t.Fatalf("unregistered and over-limit calls were not rejected locally: %s", output.String())
	}
}
