package chatgptresponses

import (
	"bufio"
	"bytes"
	"context"
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
	ErrInvalidConfiguration = errors.New("ChatGPT Responses configuration is invalid")
	ErrResponseFailed       = errors.New("ChatGPT response failed")
)

type TokenSource interface {
	AccessToken(context.Context) (string, error)
}

type Configuration struct {
	BaseURL     string
	HTTPClient  *http.Client
	TokenSource TokenSource
}

type Client struct {
	baseURL     string
	httpClient  *http.Client
	tokenSource TokenSource
}

func New(configuration Configuration) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(configuration.BaseURL), "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback(parsed.Hostname()))) || configuration.TokenSource == nil {
		return nil, ErrInvalidConfiguration
	}
	httpClient := configuration.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Minute}
	}
	return &Client{baseURL: base, httpClient: httpClient, tokenSource: configuration.TokenSource}, nil
}

func (client *Client) Respond(ctx context.Context, request archivistloop.ModelRequest) (archivistloop.ModelResponse, error) {
	token, err := client.accessToken(ctx)
	if err != nil {
		return archivistloop.ModelResponse{}, err
	}
	tools := make([]map[string]any, 0, len(request.Tools))
	for _, definition := range request.Tools {
		tools = append(tools, map[string]any{
			"type": "function", "name": definition.ProviderName, "description": definition.Description,
			"parameters": definition.Parameters, "strict": false,
		})
	}
	payload := map[string]any{
		"model": request.Model, "instructions": request.Instructions, "input": request.Input,
		"tools": tools, "tool_choice": "auto", "parallel_tool_calls": false,
		"store": false, "stream": true,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return archivistloop.ModelResponse{}, ErrInvalidConfiguration
	}
	httpRequest, _ := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/responses", bytes.NewReader(encoded))
	httpRequest.Header.Set("Authorization", "Bearer "+token)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	response, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return archivistloop.ModelResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return archivistloop.ModelResponse{}, fmt.Errorf("%w: HTTP %d", ErrResponseFailed, response.StatusCode)
	}
	return decodeStream(response.Body)
}

type streamEvent struct {
	Type     string          `json:"type"`
	Response json.RawMessage `json:"response"`
	Error    *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type completedResponse struct {
	ID     string            `json:"id"`
	Status string            `json:"status"`
	Output []json.RawMessage `json:"output"`
	Error  *struct {
		Code string `json:"code"`
	} `json:"error"`
}

func decodeStream(reader io.Reader) (archivistloop.ModelResponse, error) {
	scanner := bufio.NewScanner(io.LimitReader(reader, 16<<20))
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	var data strings.Builder
	completed := false
	result := archivistloop.ModelResponse{}
	process := func() error {
		if data.Len() == 0 {
			return nil
		}
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "[DONE]" || payload == "" {
			return nil
		}
		var event streamEvent
		if json.Unmarshal([]byte(payload), &event) != nil {
			return ErrResponseFailed
		}
		switch event.Type {
		case "response.completed":
			var response completedResponse
			if json.Unmarshal(event.Response, &response) != nil || response.ID == "" || response.Status != "completed" {
				return ErrResponseFailed
			}
			parsed, err := parseCompleted(response)
			if err != nil {
				return err
			}
			result = parsed
			completed = true
		case "response.failed", "response.incomplete", "error":
			return ErrResponseFailed
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := process(); err != nil {
				return archivistloop.ModelResponse{}, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return archivistloop.ModelResponse{}, ErrResponseFailed
	}
	if err := process(); err != nil {
		return archivistloop.ModelResponse{}, err
	}
	if !completed {
		return archivistloop.ModelResponse{}, ErrResponseFailed
	}
	return result, nil
}

func parseCompleted(response completedResponse) (archivistloop.ModelResponse, error) {
	result := archivistloop.ModelResponse{RequestID: response.ID, Output: make([]any, 0, len(response.Output))}
	var text strings.Builder
	for _, raw := range response.Output {
		var item map[string]any
		if json.Unmarshal(raw, &item) != nil {
			return archivistloop.ModelResponse{}, ErrResponseFailed
		}
		result.Output = append(result.Output, item)
		typeName, _ := item["type"].(string)
		switch typeName {
		case "function_call":
			callID, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			arguments, _ := item["arguments"].(string)
			if callID == "" || name == "" || arguments == "" {
				return archivistloop.ModelResponse{}, ErrResponseFailed
			}
			result.ToolCalls = append(result.ToolCalls, archivistloop.ToolCall{CallID: callID, Name: name, Arguments: arguments})
		case "message":
			contents, _ := item["content"].([]any)
			for _, content := range contents {
				part, _ := content.(map[string]any)
				if part["type"] == "output_text" {
					value, _ := part["text"].(string)
					text.WriteString(value)
				}
			}
		}
	}
	result.Text = strings.TrimSpace(text.String())
	return result, nil
}

type Model struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
}

func (client *Client) ListModels(ctx context.Context) ([]Model, error) {
	token, err := client.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, client.baseURL+"/models", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", ErrResponseFailed, response.StatusCode)
	}
	var payload struct {
		Models []Model `json:"models"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&payload) != nil {
		return nil, ErrResponseFailed
	}
	if len(payload.Models) > 0 {
		return payload.Models, nil
	}
	models := make([]Model, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.ID != "" {
			models = append(models, Model{Slug: item.ID, DisplayName: item.ID})
		}
	}
	return models, nil
}

func (client *Client) accessToken(ctx context.Context) (string, error) {
	token, err := client.tokenSource.AccessToken(ctx)
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", ErrResponseFailed
	}
	return token, nil
}

func isLoopback(host string) bool {
	return host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost")
}
