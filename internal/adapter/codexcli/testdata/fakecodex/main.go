package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func main() {
	if len(os.Args) < 2 || os.Args[1] != "app-server" {
		os.Exit(2)
	}
	tracePath := os.Getenv("ARCHIVE_CODEX_FIXTURE_TRACE")
	threadID := "codex-thread-fixed"
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request message
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(3)
		}
		appendTrace(tracePath, request.Method)
		switch request.Method {
		case "initialize":
			respond(encoder, request.ID, map[string]any{"userAgent": "fake-codex"})
		case "initialized":
		case "thread/start":
			respond(encoder, request.ID, map[string]any{"thread": map[string]any{"id": threadID}})
		case "thread/resume":
			var params struct {
				ThreadID string `json:"threadId"`
			}
			if json.Unmarshal(request.Params, &params) != nil || params.ThreadID != threadID {
				os.Exit(4)
			}
			respond(encoder, request.ID, map[string]any{"thread": map[string]any{"id": threadID}})
		case "turn/start":
			var params struct {
				ThreadID string `json:"threadId"`
			}
			if json.Unmarshal(request.Params, &params) != nil || params.ThreadID != threadID {
				os.Exit(5)
			}
			turnID := "codex-turn-fixture"
			respond(encoder, request.ID, map[string]any{"turn": map[string]any{"id": turnID}})
			notify(encoder, "item/agentMessage/delta", map[string]any{
				"threadId": threadID, "turnId": turnID, "delta": `PRIVATE C:\Users\fixture\secret.txt`,
			})
			notify(encoder, "item/reasoning/textDelta", map[string]any{
				"threadId": threadID, "turnId": turnID, "delta": "PRIVATE_REASONING",
			})
			notifyItem(encoder, "item/completed", threadID, turnID, map[string]any{
				"id": "message-commentary", "type": "agentMessage", "phase": "commentary",
				"text": `선택 범위를 C:\Archive\private\source 에서 확인합니다.`,
			})
			notifyItem(encoder, "item/started", threadID, turnID, map[string]any{
				"id": "tool-1", "type": "mcpToolCall", "server": "archive",
				"tool": "archive_run_status_v1", "status": "inProgress",
			})
			notifyItem(encoder, "item/completed", threadID, turnID, map[string]any{
				"id": "tool-1", "type": "mcpToolCall", "server": "archive",
				"tool": "archive_run_status_v1", "status": "completed",
			})
			notifyItem(encoder, "item/completed", threadID, turnID, map[string]any{
				"id": "message-final", "type": "agentMessage", "phase": "final_answer",
				"text": "아카이브 상태를 확인했습니다.",
			})
			notify(encoder, "turn/completed", map[string]any{
				"threadId": threadID,
				"turn":     map[string]any{"id": turnID, "status": "completed"},
			})
			_, _ = fmt.Fprintln(os.Stderr, `PRIVATE_STDERR C:\Users\fixture\token.txt`)
		default:
			if len(request.ID) != 0 {
				_ = encoder.Encode(map[string]any{
					"jsonrpc": "2.0", "id": request.ID,
					"error": map[string]any{"code": -32601, "message": "method not found"},
				})
			}
		}
	}
	if scanner.Err() != nil {
		os.Exit(6)
	}
}

func respond(encoder *json.Encoder, id json.RawMessage, result any) {
	_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func notify(encoder *json.Encoder, method string, params any) {
	_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func notifyItem(encoder *json.Encoder, method, threadID, turnID string, item any) {
	notify(encoder, method, map[string]any{"threadId": threadID, "turnId": turnID, "item": item})
}

func appendTrace(path, method string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(7)
	}
	_, _ = fmt.Fprintln(file, method)
	_ = file.Close()
}
