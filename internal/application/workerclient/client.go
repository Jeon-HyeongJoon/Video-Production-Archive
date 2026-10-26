package workerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"videoarchive/internal/adapter/workerconfig"
)

var (
	ErrCredentialUnavailable = errors.New("worker credential is unavailable")
	ErrHeartbeatRejected     = errors.New("worker heartbeat was rejected")
)

type SecretResolver interface {
	Resolve(reference string) (string, error)
}

type Client struct {
	configuration workerconfig.Configuration
	secrets       SecretResolver
	http          *http.Client
}

func New(configuration workerconfig.Configuration, secrets SecretResolver, httpClient *http.Client) (*Client, error) {
	if configuration.ID == "" || configuration.ControlURL == nil || configuration.HeartbeatInterval <= 0 ||
		len(configuration.AccessibleStorageIDs) == 0 || secrets == nil || httpClient == nil {
		return nil, workerconfig.ErrInvalidConfiguration
	}
	return &Client{configuration: configuration, secrets: secrets, http: httpClient}, nil
}

func (client *Client) Heartbeat(ctx context.Context) error {
	credential, err := client.secrets.Resolve(client.configuration.CredentialReference)
	if err != nil {
		return fmt.Errorf("resolve worker credential: %w", err)
	}
	if credential == "" {
		return ErrCredentialUnavailable
	}
	payload, err := json.Marshal(map[string]any{
		"worker_id": client.configuration.ID, "accepting_work": true, "heavy_running": 0,
		"accessible_storage_ids": client.configuration.AccessibleStorageIDs,
	})
	if err != nil {
		return err
	}
	endpoint := *client.configuration.ControlURL
	endpoint.Path = "/worker/v1/heartbeat"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential)
	response, err := client.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return fmt.Errorf("%w: HTTP %d", ErrHeartbeatRejected, response.StatusCode)
	}
	return nil
}

func (client *Client) Run(ctx context.Context, report func(error)) error {
	if report == nil {
		report = func(error) {}
	}
	ticker := time.NewTicker(client.configuration.HeartbeatInterval)
	defer ticker.Stop()
	for {
		if err := client.Heartbeat(ctx); err != nil && !errors.Is(err, context.Canceled) {
			report(err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
