package codexcli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"videoarchive/internal/application/archivistloop"
)

var (
	ErrInvalidOptions         = errors.New("Codex runtime options are invalid")
	ErrCodexFailed            = errors.New("Codex turn failed")
	ErrNoFinalMessage         = errors.New("Codex turn ended without a final message")
	ErrCodexBoundaryViolation = errors.New("Codex crossed the Archive runtime boundary")
)

// Instructions separate native workspace work from Archive domain effects.
// Codex may use its local development tools, but the Archive MCP remains the
// only authenticated route into Control commands and durable business state.
const Instructions = archivistloop.DefaultInstructions + `
Archive business tools are available only through the MCP server named "archive". Use native terminal, file, skill, and child-agent tools when they help with work in the configured workspace. Use Archive MCP tools for every Archive domain read or effect, including selections, classifications, runs, approvals, and durable records; never bypass that boundary with shell commands or direct Control requests. Do not expose reasoning, credentials, raw command output, or absolute paths in user-facing messages.`

// Native workspace tools and child agents are part of the Archivist surface.
// Browser, connector, plugin, and media surfaces remain outside the v1
// boundary; app-server receives only the Archive MCP configuration below.
var disabledFeatures = []string{
	"apps", "plugins", "image_generation", "browser_use", "computer_use", "remote_plugin", "in_app_browser",
}

var enabledFeatures = []string{
	"shell_tool", "multi_agent", "skill_search",
}

type Options struct {
	CodexExecutable  string
	MCPExecutable    string
	WorkingDirectory string
	StateDirectory   string
	Model            string
	MCPURL           string
	Runtime          archivistloop.RuntimeContext
	MaximumCalls     int
	// Log receives protocol method names and normalized lifecycle labels only.
	// Codex stderr, prompts, model messages, reasoning and paths never enter it.
	Log io.Writer
}

// Emit writes one Archivist Host runtime record: progress or final.
type Emit func(kind, summary string, resultIDs []string) error

type appServerMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code int `json:"code"`
	} `json:"error,omitempty"`
}

type appServerItem struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Text   string `json:"text"`
	Phase  string `json:"phase"`
	Server string `json:"server"`
	Tool   string `json:"tool"`
	Status string `json:"status"`
}

type nativeThreadRecord struct {
	SchemaVersion uint32 `json:"schema_version"`
	SessionID     string `json:"session_id"`
	ThreadID      string `json:"thread_id"`
}

// Run executes one ArchivistTurn through Codex app-server. Each invocation has
// a fresh app-server process, while the native Codex thread reference is kept
// per durable ArchivistSession and resumed on the next invocation.
func Run(ctx context.Context, options Options, tools archivistloop.ToolClient, prompt string, emit Emit) error {
	prompt = strings.TrimSpace(prompt)
	if !filepath.IsAbs(options.CodexExecutable) || !filepath.IsAbs(options.MCPExecutable) ||
		!filepath.IsAbs(options.WorkingDirectory) || !filepath.IsAbs(options.StateDirectory) || options.MCPURL == "" ||
		options.Runtime.SessionID == "" || options.Runtime.TurnID == "" || options.Runtime.FencingToken == 0 ||
		prompt == "" || len(prompt) > 64<<10 || tools == nil || emit == nil {
		return ErrInvalidOptions
	}
	if options.MaximumCalls <= 0 {
		options.MaximumCalls = 16
	}
	if options.MaximumCalls > 64 {
		return ErrInvalidOptions
	}
	logf := func(format string, values ...any) {
		if options.Log != nil {
			_, _ = fmt.Fprintf(options.Log, format+"\n", values...)
		}
	}
	available, err := tools.List(ctx)
	if err != nil {
		return err
	}
	definitions, mapping := archivistloop.SelectTools(available)
	if len(definitions) == 0 {
		return errors.New("no Archive tool is available")
	}
	if err := emit("progress", "업무 도구 목록 확인", nil); err != nil {
		return err
	}

	temporaryDirectory, err := os.MkdirTemp("", "archive-codex-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporaryDirectory)
	resultFile := filepath.Join(temporaryDirectory, "results.jsonl")
	arguments, err := appServerArguments(options, resultFile)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, options.CodexExecutable, arguments...)
	command.Dir = options.WorkingDirectory
	command.Stderr = io.Discard
	stdin, err := command.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return err
	}
	finished := false
	defer func() {
		if !finished && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()

	messages := make(chan appServerMessage, 32)
	readError := make(chan error, 1)
	go readAppServerMessages(stdout, messages, readError)
	encoder := json.NewEncoder(stdin)
	send := func(id int, method string, params any) error {
		request := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
		if id != 0 {
			request["id"] = id
		}
		return encoder.Encode(request)
	}
	rejectServerRequest := func(message appServerMessage) error {
		if len(message.ID) == 0 {
			return nil
		}
		return encoder.Encode(map[string]any{
			"jsonrpc": "2.0", "id": message.ID,
			"error": map[string]any{"code": -32601, "message": "Archive client does not expose this method"},
		})
	}
	next := func() (appServerMessage, error) {
		select {
		case <-ctx.Done():
			return appServerMessage{}, ctx.Err()
		case err := <-readError:
			if err == nil {
				err = io.EOF
			}
			return appServerMessage{}, err
		case message, ok := <-messages:
			if !ok {
				return appServerMessage{}, io.EOF
			}
			return message, nil
		}
	}
	waitResponse := func(id int) (json.RawMessage, error) {
		for {
			message, err := next()
			if err != nil {
				return nil, err
			}
			if message.Method != "" {
				logf("notification: %s", message.Method)
				if len(message.ID) != 0 {
					_ = rejectServerRequest(message)
					return nil, ErrCodexBoundaryViolation
				}
				continue
			}
			if !sameRPCID(message.ID, id) {
				continue
			}
			if message.Error != nil {
				logf("response: %d error %d", id, message.Error.Code)
				return nil, ErrCodexFailed
			}
			logf("response: %d ok", id)
			return message.Result, nil
		}
	}

	if err := send(1, "initialize", map[string]any{"clientInfo": map[string]any{
		"name": "archive_archivist", "title": "Archive Archivist", "version": "1",
	}}); err != nil {
		return err
	}
	if _, err := waitResponse(1); err != nil {
		return fmt.Errorf("%w: initialize", ErrCodexFailed)
	}
	if err := send(0, "initialized", map[string]any{}); err != nil {
		return err
	}

	record, err := loadNativeThread(options.StateDirectory, options.Runtime.SessionID)
	if err != nil {
		return err
	}
	threadParams := map[string]any{
		"cwd": options.WorkingDirectory, "approvalPolicy": "never", "sandbox": "danger-full-access",
		"developerInstructions": Instructions,
	}
	method := "thread/start"
	if record.ThreadID != "" {
		method = "thread/resume"
		threadParams["threadId"] = record.ThreadID
		threadParams["excludeTurns"] = true
	} else {
		threadParams["ephemeral"] = false
		threadParams["serviceName"] = "archive_archivist"
	}
	if model := strings.TrimSpace(options.Model); model != "" {
		threadParams["model"] = model
	}
	if err := send(2, method, threadParams); err != nil {
		return err
	}
	threadResult, err := waitResponse(2)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrCodexFailed, method)
	}
	var opened struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(threadResult, &opened) != nil || !validNativeID(opened.Thread.ID) ||
		(record.ThreadID != "" && opened.Thread.ID != record.ThreadID) {
		return ErrCodexFailed
	}
	if record.ThreadID == "" {
		if err := saveNativeThread(options.StateDirectory, nativeThreadRecord{
			SchemaVersion: 1, SessionID: options.Runtime.SessionID, ThreadID: opened.Thread.ID,
		}); err != nil {
			return err
		}
	}

	turnParams := map[string]any{
		"threadId":       opened.Thread.ID,
		"input":          []map[string]any{{"type": "text", "text": prompt}},
		"approvalPolicy": "never", "cwd": options.WorkingDirectory,
		"sandboxPolicy": map[string]any{"type": "dangerFullAccess"},
	}
	if model := strings.TrimSpace(options.Model); model != "" {
		turnParams["model"] = model
	}
	if err := send(3, "turn/start", turnParams); err != nil {
		return err
	}
	turnResult, err := waitResponse(3)
	if err != nil {
		return fmt.Errorf("%w: turn/start", ErrCodexFailed)
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(turnResult, &started) != nil || !validNativeID(started.Turn.ID) {
		return ErrCodexFailed
	}

	announcedItems := make(map[string]bool)
	finalMessage := ""
	pendingUnknownMessage := ""
	emitProgress := func(value string) error {
		value = sanitizePublicMessage(value, 4<<10)
		if value == "" {
			return nil
		}
		return emit("progress", value, nil)
	}
	for {
		message, err := next()
		if err != nil {
			return fmt.Errorf("%w: event stream", ErrCodexFailed)
		}
		if message.Method == "" {
			continue
		}
		logf("notification: %s", message.Method)
		if len(message.ID) != 0 {
			_ = rejectServerRequest(message)
			return ErrCodexBoundaryViolation
		}
		switch message.Method {
		case "item/started", "item/completed":
			var params struct {
				ThreadID string        `json:"threadId"`
				TurnID   string        `json:"turnId"`
				Item     appServerItem `json:"item"`
			}
			if json.Unmarshal(message.Params, &params) != nil || params.ThreadID != opened.Thread.ID || params.TurnID != started.Turn.ID {
				return ErrCodexFailed
			}
			switch params.Item.Type {
			case "mcpToolCall":
				if params.Item.Server != "archive" {
					return ErrCodexBoundaryViolation
				}
				if announcedItems[params.Item.ID] {
					continue
				}
				announcedItems[params.Item.ID] = true
				name := params.Item.Tool
				if definition, ok := mapping[name]; ok {
					name = definition.Name
				}
				if err := emitProgress(archivistloop.ToolProgress(name)); err != nil {
					return err
				}
			case "agentMessage":
				if message.Method != "item/completed" {
					continue
				}
				text := sanitizePublicMessage(params.Item.Text, 16<<10)
				if text == "" {
					continue
				}
				switch params.Item.Phase {
				case "commentary", "partial_answer":
					if err := emitProgress(text); err != nil {
						return err
					}
				case "final_answer":
					if pendingUnknownMessage != "" {
						if err := emitProgress(pendingUnknownMessage); err != nil {
							return err
						}
						pendingUnknownMessage = ""
					}
					finalMessage = text
				default:
					if pendingUnknownMessage != "" {
						if err := emitProgress(pendingUnknownMessage); err != nil {
							return err
						}
					}
					pendingUnknownMessage = text
				}
			case "reasoning", "userMessage", "functionCallOutput":
				// Reasoning and protocol bookkeeping never become public entries.
			case "commandExecution", "fileChange", "collabAgentToolCall":
				if announcedItems[params.Item.ID] {
					continue
				}
				announcedItems[params.Item.ID] = true
				summary := map[string]string{
					"commandExecution":    "터미널 명령 실행",
					"fileChange":          "파일 변경",
					"collabAgentToolCall": "보조 Agent 작업",
				}[params.Item.Type]
				if err := emitProgress(summary); err != nil {
					return err
				}
			case "webSearch", "imageGeneration":
				return ErrCodexBoundaryViolation
			}
		case "item/agentMessage/delta", "item/reasoning/summaryTextDelta", "item/reasoning/summaryPartAdded", "item/reasoning/textDelta", "mcpToolCall/progress":
			// Per-token text, private reasoning and raw tool progress are never
			// copied. Completed public messages and normalized tool rows above are
			// the stable web-chat units.
		case "turn/completed":
			var params struct {
				ThreadID string `json:"threadId"`
				Turn     struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"turn"`
			}
			if json.Unmarshal(message.Params, &params) != nil || params.ThreadID != opened.Thread.ID ||
				params.Turn.ID != started.Turn.ID || params.Turn.Status != "completed" {
				return ErrCodexFailed
			}
			if finalMessage == "" {
				finalMessage = pendingUnknownMessage
			}
			finalMessage = sanitizePublicMessage(finalMessage, 16<<10)
			if finalMessage == "" {
				return ErrNoFinalMessage
			}
			if err := emit("final", finalMessage, readResultIDs(resultFile)); err != nil {
				return err
			}
			_ = stdin.Close()
			if err := waitForProcess(command, 5*time.Second); err != nil {
				return fmt.Errorf("%w: app-server exit", ErrCodexFailed)
			}
			finished = true
			return nil
		}
	}
}

func appServerArguments(options Options, resultFile string) ([]string, error) {
	values := map[string]string{
		"ARCHIVE_MCP_URL":           options.MCPURL,
		"ARCHIVE_SESSION_ID":        options.Runtime.SessionID,
		"ARCHIVE_TURN_ID":           options.Runtime.TurnID,
		"ARCHIVE_FENCING_TOKEN":     fmt.Sprint(options.Runtime.FencingToken),
		"ARCHIVE_CODEX_RESULT_FILE": resultFile,
		"ARCHIVE_CODEX_MAX_CALLS":   fmt.Sprint(options.MaximumCalls),
	}
	arguments := []string{"app-server", "--listen", "stdio://"}
	for _, feature := range enabledFeatures {
		arguments = append(arguments, "--enable", feature)
	}
	for _, feature := range disabledFeatures {
		arguments = append(arguments, "--disable", feature)
	}
	settings := [][2]string{
		{"mcp_servers", "{}"},
		{"web_search", `"disabled"`},
		{"approval_policy", `"never"`},
		{"sandbox_mode", `"danger-full-access"`},
	}
	command, err := tomlString(options.MCPExecutable)
	if err != nil {
		return nil, err
	}
	settings = append(settings,
		[2]string{"mcp_servers.archive.command", command},
		[2]string{"mcp_servers.archive.args", `["mcp"]`},
		[2]string{"mcp_servers.archive.required", "true"},
		[2]string{"mcp_servers.archive.default_tools_approval_mode", `"approve"`},
		[2]string{"mcp_servers.archive.startup_timeout_sec", "20"},
		[2]string{"mcp_servers.archive.tool_timeout_sec", "120"},
	)
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		encoded, err := tomlString(values[name])
		if err != nil {
			return nil, err
		}
		settings = append(settings, [2]string{"mcp_servers.archive.env." + name, encoded})
	}
	for _, setting := range settings {
		arguments = append(arguments, "-c", setting[0]+"="+setting[1])
	}
	return arguments, nil
}

func readAppServerMessages(input io.Reader, messages chan<- appServerMessage, result chan<- error) {
	defer close(messages)
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var message appServerMessage
		if json.Unmarshal([]byte(line), &message) != nil {
			result <- ErrCodexFailed
			return
		}
		messages <- message
	}
	result <- scanner.Err()
}

func sameRPCID(raw json.RawMessage, expected int) bool {
	return strings.TrimSpace(string(raw)) == strconv.Itoa(expected)
}

func waitForProcess(command *exec.Cmd, limit time.Duration) error {
	result := make(chan error, 1)
	go func() { result <- command.Wait() }()
	select {
	case err := <-result:
		return err
	case <-time.After(limit):
		_ = command.Process.Kill()
		<-result
		return context.DeadlineExceeded
	}
}

func nativeThreadPath(directory, sessionID string) string {
	digest := sha256.Sum256([]byte(sessionID))
	return filepath.Join(directory, hex.EncodeToString(digest[:])+".json")
}

func loadNativeThread(directory, sessionID string) (nativeThreadRecord, error) {
	payload, err := os.ReadFile(nativeThreadPath(directory, sessionID))
	if errors.Is(err, os.ErrNotExist) {
		return nativeThreadRecord{}, nil
	}
	if err != nil || len(payload) > 4<<10 {
		return nativeThreadRecord{}, ErrCodexFailed
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var record nativeThreadRecord
	if decoder.Decode(&record) != nil || record.SchemaVersion != 1 || record.SessionID != sessionID || !validNativeID(record.ThreadID) {
		return nativeThreadRecord{}, ErrCodexFailed
	}
	return record, nil
}

func saveNativeThread(directory string, record nativeThreadRecord) error {
	if record.SchemaVersion != 1 || record.SessionID == "" || !validNativeID(record.ThreadID) {
		return ErrInvalidOptions
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	payload, _ := json.Marshal(record)
	temporary, err := os.CreateTemp(directory, ".native-thread-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(payload, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, nativeThreadPath(directory, record.SessionID))
}

func validNativeID(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\x00\r\n")
}

// sanitizePublicMessage applies the stable boundary shared by progress and
// final rows. It removes absolute filesystem path tokens while retaining the
// model's user-facing prose; raw event kinds are filtered before this point.
func sanitizePublicMessage(value string, limit int) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\x00", ""))
	words := strings.Fields(value)
	for index, word := range words {
		trimmed := strings.Trim(word, "()[]{}<>,.;:'\"")
		if isAbsolutePathToken(trimmed) {
			words[index] = strings.Replace(word, trimmed, "[경로]", 1)
		}
	}
	return truncate(strings.Join(words, " "), limit)
}

func isAbsolutePathToken(value string) bool {
	if len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) &&
		value[1] == ':' && (value[2] == '\\' || value[2] == '/') {
		return true
	}
	return len(value) >= 2 && value[0] == '/' && value[1] != '/'
}

// tomlString encodes a value as a TOML basic string for a -c override.
func tomlString(value string) (string, error) {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, character := range value {
		switch {
		case character == '"':
			builder.WriteString(`\"`)
		case character == '\\':
			builder.WriteString(`\\`)
		case character == '\n':
			builder.WriteString(`\n`)
		case character < 0x20 || character == 0x7f:
			return "", ErrInvalidOptions
		default:
			builder.WriteRune(character)
		}
	}
	builder.WriteByte('"')
	return builder.String(), nil
}

func readResultIDs(path string) []string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	found := make(map[string]bool)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record ResultRecord
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		for _, id := range record.ResultIDs {
			if len(found) < 64 {
				found[id] = true
			}
		}
	}
	ids := make([]string, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
