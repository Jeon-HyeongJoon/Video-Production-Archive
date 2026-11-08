package codexcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"videoarchive/internal/application/archivistloop"
)

// MCPServer exposes only the Archive business tools that Control advertises
// and the local catalog describes, as a stdio MCP server that Codex launches.
// Each call is forwarded to the Host loopback proxy with the current turn's
// correlation and deterministic idempotency identity.
type MCPServer struct {
	Runtime      archivistloop.RuntimeContext
	Tools        archivistloop.ToolClient
	MaximumCalls int
	// ResultFile, when set, receives one JSON line per completed call so the
	// runner can report the IDs the turn produced.
	ResultFile string

	mutex         sync.Mutex
	definitions   []archivistloop.ToolDefinition
	mapping       map[string]archivistloop.ToolDefinition
	actionClasses map[string]string
	calls         int
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ResultRecord is one completed call written to the ResultFile.
type ResultRecord struct {
	Tool      string   `json:"tool"`
	Status    int      `json:"status"`
	ResultIDs []string `json:"result_ids,omitempty"`
}

func (server *MCPServer) Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	if server.Tools == nil || server.Runtime.SessionID == "" || server.Runtime.TurnID == "" || server.Runtime.FencingToken == 0 {
		return errors.New("MCP server runtime context is incomplete")
	}
	if server.MaximumCalls <= 0 {
		server.MaximumCalls = 16
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	encoder := json.NewEncoder(output)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var message rpcMessage
		if json.Unmarshal([]byte(line), &message) != nil {
			if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": nil, "error": rpcError{Code: -32700, Message: "parse error"}}); err != nil {
				return err
			}
			continue
		}
		if len(message.ID) == 0 || message.Method == "" {
			continue
		}
		result, failure := server.handle(ctx, message)
		response := map[string]any{"jsonrpc": "2.0", "id": message.ID}
		if failure != nil {
			response["error"] = failure
		} else {
			response["result"] = result
		}
		if err := encoder.Encode(response); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (server *MCPServer) handle(ctx context.Context, message rpcMessage) (any, *rpcError) {
	switch message.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(message.Params, &params)
		version := params.ProtocolVersion
		if version == "" {
			version = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "archive", "version": "1"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		definitions, _, err := server.catalog(ctx)
		if err != nil {
			return nil, &rpcError{Code: -32603, Message: "Archive tool discovery failed"}
		}
		tools := make([]map[string]any, 0, len(definitions))
		for _, definition := range definitions {
			tools = append(tools, map[string]any{
				"name": definition.ProviderName, "description": definition.Description, "inputSchema": definition.Parameters,
				"annotations": map[string]any{"readOnlyHint": server.actionClasses[definition.Name] == "READ"},
			})
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		return server.call(ctx, message.Params)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

func (server *MCPServer) catalog(ctx context.Context) ([]archivistloop.ToolDefinition, map[string]archivistloop.ToolDefinition, error) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	if server.mapping != nil {
		return server.definitions, server.mapping, nil
	}
	available, err := server.Tools.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	definitions, mapping := archivistloop.SelectTools(available)
	if len(definitions) == 0 {
		return nil, nil, errors.New("no Archive tool is available")
	}
	server.actionClasses = make(map[string]string, len(available))
	for _, tool := range available {
		server.actionClasses[tool.Name] = tool.ActionClass
	}
	server.definitions, server.mapping = definitions, mapping
	return definitions, mapping, nil
}

func (server *MCPServer) call(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid tool call"}
	}
	_, mapping, err := server.catalog(ctx)
	if err != nil {
		return nil, &rpcError{Code: -32603, Message: "Archive tool discovery failed"}
	}
	definition, ok := mapping[params.Name]
	if !ok {
		return toolText(`{"error":"unknown Archive tool"}`, true), nil
	}
	arguments := strings.TrimSpace(string(params.Arguments))
	if arguments == "" || arguments == "null" {
		arguments = "{}"
	}
	canonical, err := archivistloop.CanonicalObject(arguments)
	if err != nil {
		return toolText(`{"error":"tool arguments must be one JSON object"}`, true), nil
	}
	server.mutex.Lock()
	server.calls++
	sequence := server.calls
	server.mutex.Unlock()
	if sequence > server.MaximumCalls {
		return toolText(`{"error":"Archive tool call limit reached for this turn"}`, true), nil
	}
	envelope := archivistloop.NewToolEnvelope("archive-codex", server.Runtime, fmt.Sprintf("codex-%d", sequence), definition.Name, canonical)
	result, err := server.Tools.Call(ctx, envelope)
	if err != nil {
		return toolText(`{"error":"Archive tool call failed"}`, true), nil
	}
	output := archivistloop.NormalizeToolOutput(result)
	server.record(definition.Name, result.StatusCode, output)
	return toolText(string(output), result.StatusCode >= 400), nil
}

func (server *MCPServer) record(toolName string, status int, output []byte) {
	if server.ResultFile == "" {
		return
	}
	found := make(map[string]bool)
	if status < 400 {
		archivistloop.CollectResultIDs(output, found)
	}
	record := ResultRecord{Tool: toolName, Status: status}
	for id := range found {
		record.ResultIDs = append(record.ResultIDs, id)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	server.mutex.Lock()
	defer server.mutex.Unlock()
	file, err := os.OpenFile(server.ResultFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(encoded, '\n'))
}

func toolText(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}
