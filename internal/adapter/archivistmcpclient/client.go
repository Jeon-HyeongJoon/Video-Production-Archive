package archivistmcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"videoarchive/internal/application/archivistloop"
)

var ErrInvalidConfiguration = errors.New("Archivist MCP client configuration is invalid")

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func New(baseURL string, httpClient *http.Client) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || !loopback(parsed.Hostname()) {
		return nil, ErrInvalidConfiguration
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{baseURL: baseURL, httpClient: httpClient}, nil
}

func (client *Client) List(ctx context.Context) ([]archivistloop.AvailableTool, error) {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, client.baseURL+"/mcp/v1/tools", nil)
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("MCP tool discovery failed")
	}
	var payload struct {
		Data []archivistloop.AvailableTool `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 256<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil || len(payload.Data) == 0 {
		return nil, errors.New("MCP tool discovery failed")
	}
	return payload.Data, nil
}

func (client *Client) Call(ctx context.Context, envelope archivistloop.ToolEnvelope) (archivistloop.ToolResult, error) {
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return archivistloop.ToolResult{}, err
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/mcp/v1/call", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return archivistloop.ToolResult{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (256<<10)+1))
	if err != nil || len(body) > 256<<10 {
		return archivistloop.ToolResult{}, errors.New("MCP tool response exceeds limit")
	}
	if len(body) == 0 {
		body = []byte(`{}`)
	}
	if !json.Valid(body) {
		return archivistloop.ToolResult{}, errors.New("MCP tool response is invalid")
	}
	return archivistloop.ToolResult{StatusCode: response.StatusCode, Body: body}, nil
}

func loopback(host string) bool {
	return host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost")
}
