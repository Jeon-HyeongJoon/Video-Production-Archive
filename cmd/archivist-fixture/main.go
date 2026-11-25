package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	scenario := flag.String("scenario", "search", "fixture scenario")
	holdFile := flag.String("hold-file", "", "wait for this file before completing")
	traceFile := flag.String("trace-file", "", "append fixture launch evidence")
	flag.Parse()
	mcpURL := strings.TrimRight(os.Getenv("ARCHIVE_MCP_URL"), "/")
	sessionID := os.Getenv("ARCHIVE_SESSION_ID")
	turnID := os.Getenv("ARCHIVE_TURN_ID")
	fencingToken := os.Getenv("ARCHIVE_FENCING_TOKEN")
	if mcpURL == "" || sessionID == "" || turnID == "" || fencingToken == "" {
		fmt.Fprintln(os.Stderr, "runtime context is incomplete")
		os.Exit(2)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Get(mcpURL + "/mcp/v1/tools")
	if err != nil || response.StatusCode != http.StatusOK {
		if response != nil {
			response.Body.Close()
		}
		fmt.Fprintln(os.Stderr, "MCP tool discovery failed")
		os.Exit(3)
	}
	toolList, _ := io.ReadAll(io.LimitReader(response.Body, 256<<10))
	response.Body.Close()
	if !bytes.Contains(toolList, []byte("archive.assets.search.v1")) || bytes.Contains(bytes.ToLower(toolList), []byte("run_shell")) {
		fmt.Fprintln(os.Stderr, "MCP capability boundary mismatch")
		os.Exit(4)
	}
	emitProgress("업무 도구 목록 확인")
	var token uint64
	if _, err := fmt.Sscan(fencingToken, &token); err != nil || token == 0 {
		os.Exit(5)
	}
	appendTrace(*traceFile, "START", turnID, fencingToken)
	searchPayload := map[string]any{
		"session_id": sessionID, "turn_id": turnID, "fencing_token": token,
		"tool_call_id": "CALL-" + turnID, "command_id": "COMMAND-" + turnID,
		"idempotency_key": "fixture:" + turnID, "input_digest": "fixture-v1",
		"tool": "archive.assets.search.v1", "input": map[string]any{"text": ""},
	}
	if *scenario == "collaboration-resume" {
		searchPayload["tool"] = "archive.intake.selection.get.v1"
		searchPayload["input"] = map[string]any{}
	}
	var selected struct {
		Data struct {
			Bound bool   `json:"bound"`
			RunID string `json:"archive_run_id"`
		}
	}
	var searchResult any
	if *scenario == "collaboration-resume" {
		searchResult = &selected
	}
	if err := callMCP(client, mcpURL, searchPayload, http.StatusOK, searchResult); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(6)
	}
	emitProgress("아카이브 자료 조회 완료")
	resultIDs := []string{}
	summary := "Archive query completed"
	if *scenario == "collaboration-resume" {
		runID := "RUN-" + turnID
		if !selected.Data.Bound || token > 1 && selected.Data.RunID != runID {
			fmt.Fprintln(os.Stderr, "selected ArchiveRun does not match the durable Turn")
			os.Exit(7)
		}
		actionPayload := map[string]any{
			"session_id": sessionID, "turn_id": turnID, "fencing_token": token,
			"tool_call_id": "CALL-ACTION-" + turnID, "command_id": "COMMAND-ACTION-" + turnID,
			"idempotency_key": "fixture-action:" + turnID, "input_digest": "fixture-action-v1",
			"tool": "archive.run.start.v1", "input": map[string]any{"archive_run_id": runID},
		}
		var card struct {
			Data struct {
				ID    string `json:"action_card_id"`
				State string `json:"state"`
			}
		}
		if err := callMCP(client, mcpURL, actionPayload, http.StatusAccepted, &card); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(7)
		}
		expected := "PENDING"
		if token > 1 {
			expected = "COMPLETED"
		}
		if card.Data.State != expected || card.Data.ID != "ACTION-CALL-ACTION-"+turnID {
			fmt.Fprintln(os.Stderr, "unexpected durable ActionCard decision")
			os.Exit(7)
		}
		summary = fmt.Sprintf("아카이브 실행 제안 %s의 상태는 %s입니다.", card.Data.ID, card.Data.State)
		if selected.Data.RunID != "" {
			resultIDs = append(resultIDs, selected.Data.RunID)
			summary = fmt.Sprintf("보관 실행 %s의 시작 카드 상태는 %s입니다.", selected.Data.RunID, card.Data.State)
		}
	}
	appendTrace(*traceFile, "TOOLS_DONE", turnID, fencingToken)
	if *holdFile != "" {
		deadline := time.Now().Add(2 * time.Minute)
		for {
			if _, err := os.Stat(*holdFile); err == nil {
				break
			} else if !os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, "fixture hold file check failed")
				os.Exit(8)
			}
			if time.Now().After(deadline) {
				fmt.Fprintln(os.Stderr, "fixture hold timed out")
				os.Exit(9)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"type":       "final",
		"summary":    summary,
		"result_ids": resultIDs,
	})
}

func emitProgress(summary string) {
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "progress", "summary": summary})
}

func callMCP(client *http.Client, mcpURL string, payload map[string]any, expectedStatus int, target any) error {
	encoded, _ := json.Marshal(payload)
	request, _ := http.NewRequest(http.MethodPost, mcpURL+"/mcp/v1/call", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("MCP capability call failed: %w", err)
	}
	if response.StatusCode != expectedStatus {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		response.Body.Close()
		return fmt.Errorf("MCP capability call returned %d, want %d: %s", response.StatusCode, expectedStatus, strings.TrimSpace(string(body)))
	}
	defer response.Body.Close()
	if target == nil {
		_, err := io.Copy(io.Discard, io.LimitReader(response.Body, 256<<10))
		return err
	}
	return json.NewDecoder(io.LimitReader(response.Body, 256<<10)).Decode(target)
}

func appendTrace(path, phase, turnID, fencingToken string) {
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = fmt.Fprintf(file, "%s %s %s %d\n", phase, turnID, fencingToken, os.Getpid())
}
