package localmodel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"videoarchive/internal/application/archivistloop"
)

var (
	ErrInvalidConfiguration = errors.New("local model configuration is invalid")
	ErrResponseFailed       = errors.New("local model response failed")
)

type Configuration struct {
	BaseURL    string
	HTTPClient *http.Client
}

// Client adapts a loopback OpenAI-compatible Chat Completions endpoint to the
// provider-neutral Archivist model port. It deliberately has no credential
// source: v1 local inference is reachable only on the same Host.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

func New(configuration Configuration) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(configuration.BaseURL), "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || !isLoopback(parsed.Hostname()) {
		return nil, ErrInvalidConfiguration
	}
	client := configuration.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	} else {
		clone := *client
		client = &clone
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: base, httpClient: client}, nil
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type completionResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

func (client *Client) Respond(ctx context.Context, request archivistloop.ModelRequest) (archivistloop.ModelResponse, error) {
	if strings.TrimSpace(request.Model) == "" {
		return archivistloop.ModelResponse{}, ErrInvalidConfiguration
	}
	messages, err := messagesFor(request)
	if err != nil {
		return archivistloop.ModelResponse{}, err
	}
	tools := make([]map[string]any, 0, len(request.Tools))
	for _, definition := range request.Tools {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name": definition.ProviderName, "description": definition.Description, "parameters": definition.Parameters,
		}})
	}
	payload := map[string]any{
		"model": request.Model, "messages": messages, "tools": tools,
		"tool_choice": "auto", "parallel_tool_calls": false, "stream": false,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return archivistloop.ModelResponse{}, ErrInvalidConfiguration
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return archivistloop.ModelResponse{}, ErrInvalidConfiguration
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return archivistloop.ModelResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return archivistloop.ModelResponse{}, fmt.Errorf("%w: HTTP %d", ErrResponseFailed, response.StatusCode)
	}
	var completed completionResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 8<<20))
	if decoder.Decode(&completed) != nil || len(completed.Choices) != 1 {
		return archivistloop.ModelResponse{}, ErrResponseFailed
	}
	message := completed.Choices[0].Message
	if message.Role != "assistant" {
		return archivistloop.ModelResponse{}, ErrResponseFailed
	}
	result := archivistloop.ModelResponse{RequestID: strings.TrimSpace(completed.ID)}
	for index, call := range message.ToolCalls {
		name := strings.TrimSpace(call.Function.Name)
		arguments := strings.TrimSpace(call.Function.Arguments)
		if call.Type != "function" || name == "" || arguments == "" {
			return archivistloop.ModelResponse{}, ErrResponseFailed
		}
		callID := strings.TrimSpace(call.ID)
		if callID == "" {
			callID = stableCallID(index, name, arguments)
			message.ToolCalls[index].ID = callID
		}
		result.ToolCalls = append(result.ToolCalls, archivistloop.ToolCall{CallID: callID, Name: name, Arguments: arguments})
	}
	text, ok := message.Content.(string)
	if message.Content != nil && !ok {
		return archivistloop.ModelResponse{}, ErrResponseFailed
	}
	result.Text = strings.TrimSpace(text)
	if len(result.ToolCalls) == 0 && result.Text == "" {
		return archivistloop.ModelResponse{}, ErrResponseFailed
	}
	output := map[string]any{"role": "assistant", "content": result.Text}
	if len(message.ToolCalls) > 0 {
		output["tool_calls"] = message.ToolCalls
	}
	result.Output = []any{output}
	if result.RequestID == "" {
		digest := sha256.Sum256(mustJSON(output))
		result.RequestID = "local-response-" + hex.EncodeToString(digest[:8])
	}
	return result, nil
}

func messagesFor(request archivistloop.ModelRequest) ([]chatMessage, error) {
	messages := make([]chatMessage, 0, len(request.Input)+1)
	if instructions := strings.TrimSpace(request.Instructions); instructions != "" {
		messages = append(messages, chatMessage{Role: "system", Content: instructions})
	}
	for _, raw := range request.Input {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, ErrInvalidConfiguration
		}
		var item map[string]json.RawMessage
		if json.Unmarshal(encoded, &item) != nil {
			return nil, ErrInvalidConfiguration
		}
		var itemType string
		_ = json.Unmarshal(item["type"], &itemType)
		if itemType == "function_call_output" {
			var callID, output string
			if json.Unmarshal(item["call_id"], &callID) != nil || json.Unmarshal(item["output"], &output) != nil || strings.TrimSpace(callID) == "" {
				return nil, ErrInvalidConfiguration
			}
			messages = append(messages, chatMessage{Role: "tool", Content: output, ToolCallID: callID})
			continue
		}
		var message chatMessage
		if json.Unmarshal(encoded, &message) != nil || (message.Role != "user" && message.Role != "assistant") {
			return nil, ErrInvalidConfiguration
		}
		messages = append(messages, message)
	}
	if len(messages) == 0 {
		return nil, ErrInvalidConfiguration
	}
	return messages, nil
}

func stableCallID(index int, name, arguments string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", index, name, arguments)))
	return "local-call-" + hex.EncodeToString(digest[:8])
}

func mustJSON(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}

func isLoopback(host string) bool {
	return host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost")
}
