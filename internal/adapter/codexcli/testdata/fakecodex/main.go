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
	nativeTools := os.Getenv("ARCHIVE_CODEX_FIXTURE_NATIVE_TOOLS") == "1"
	if nativeTools && !validNativeArguments(os.Args[2:]) {
		os.Exit(8)
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
			if nativeTools && !validThreadParams(request.Params) {
				os.Exit(9)
			}
			respond(encoder, request.ID, map[string]any{"thread": map[string]any{"id": threadID}})
		case "thread/resume":
			var params struct {
				ThreadID       string `json:"threadId"`
				ApprovalPolicy string `json:"approvalPolicy"`
				Sandbox        string `json:"sandbox"`
			}
			if json.Unmarshal(request.Params, &params) != nil || params.ThreadID != threadID {
				os.Exit(4)
			}
			if nativeTools && (params.ApprovalPolicy != "never" || params.Sandbox != "danger-full-access") {
				os.Exit(9)
			}
			respond(encoder, request.ID, map[string]any{"thread": map[string]any{"id": threadID}})
		case "turn/start":
			var params struct {
				ThreadID       string `json:"threadId"`
				ApprovalPolicy string `json:"approvalPolicy"`
				SandboxPolicy  struct {
					Type string `json:"type"`
				} `json:"sandboxPolicy"`
			}
			if json.Unmarshal(request.Params, &params) != nil || params.ThreadID != threadID {
				os.Exit(5)
			}
			if nativeTools && (params.ApprovalPolicy != "never" || params.SandboxPolicy.Type != "dangerFullAccess") {
				os.Exit(10)
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
			if nativeTools {
				notifyItem(encoder, "item/started", threadID, turnID, map[string]any{
					"id": "command-1", "type": "commandExecution", "status": "inProgress",
					"command": `type C:\Users\fixture\secret.txt`,
				})
				notifyItem(encoder, "item/completed", threadID, turnID, map[string]any{
					"id": "file-1", "type": "fileChange", "status": "completed",
				})
				notifyItem(encoder, "item/started", threadID, turnID, map[string]any{
					"id": "agent-1", "type": "collabAgentToolCall", "status": "inProgress",
				})
				notifyItem(encoder, "item/completed", threadID, turnID, map[string]any{
					"id": "command-1", "type": "commandExecution", "status": "completed",
				})
				notifyItem(encoder, "item/completed", threadID, turnID, map[string]any{
					"id": "agent-1", "type": "collabAgentToolCall", "status": "completed",
				})
			}
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

func validNativeArguments(arguments []string) bool {
	required := map[string]bool{
		"shell_tool":   false,
		"multi_agent":  false,
		"skill_search": false,
	}
	settings := map[string]bool{
		`approval_policy="never"`:           false,
		`sandbox_mode="danger-full-access"`: false,
	}
	for index := 0; index+1 < len(arguments); index++ {
		switch arguments[index] {
		case "--enable":
			if _, ok := required[arguments[index+1]]; ok {
				required[arguments[index+1]] = true
			}
		case "--disable":
			if _, ok := required[arguments[index+1]]; ok {
				return false
			}
		case "-c":
			if _, ok := settings[arguments[index+1]]; ok {
				settings[arguments[index+1]] = true
			}
		}
	}
	for _, present := range required {
		if !present {
			return false
		}
	}
	for _, present := range settings {
		if !present {
			return false
		}
	}
	return true
}

func validThreadParams(payload json.RawMessage) bool {
	var params struct {
		ApprovalPolicy string `json:"approvalPolicy"`
		Sandbox        string `json:"sandbox"`
	}
	return json.Unmarshal(payload, &params) == nil && params.ApprovalPolicy == "never" && params.Sandbox == "danger-full-access"
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
