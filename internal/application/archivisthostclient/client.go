package archivisthostclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"videoarchive/internal/adapter/archivisthostconfig"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/vocabulary"
)

type SecretResolver interface {
	Resolve(string) (string, error)
}

type ProcessRunner interface {
	Run(context.Context, archivistruntime.LaunchPlan, string, string, map[string]string, func() error, func(archivistruntime.ProgressUpdate) error) (archivistruntime.FinalResponse, error)
}

type Client struct {
	configuration archivisthostconfig.Configuration
	credential    string
	httpClient    *http.Client
	runner        ProcessRunner
}

func New(configuration archivisthostconfig.Configuration, secrets SecretResolver, httpClient *http.Client, runner ProcessRunner) (*Client, error) {
	if secrets == nil || httpClient == nil || runner == nil {
		return nil, errors.New("archivist host dependencies are incomplete")
	}
	credential, err := secrets.Resolve(configuration.CredentialReference)
	if err != nil || strings.TrimSpace(credential) == "" {
		return nil, fmt.Errorf("resolve archivist host credential: %w", err)
	}
	return &Client{configuration: configuration, credential: credential, httpClient: httpClient, runner: runner}, nil
}

func (client *Client) Credential() string { return client.credential }

func (client *Client) Run(ctx context.Context, onError func(error)) error {
	if onError == nil {
		onError = func(error) {}
	}
	if err := client.heartbeat(ctx); err != nil {
		onError(err)
	}
	heartbeats := time.NewTicker(client.configuration.HeartbeatInterval)
	defer heartbeats.Stop()
	claims := time.NewTicker(client.configuration.ClaimInterval)
	defer claims.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-heartbeats.C:
			if err := client.heartbeat(ctx); err != nil {
				onError(err)
			}
		case <-claims.C:
			claimed, err := client.claim(ctx)
			if err != nil {
				onError(err)
				continue
			}
			if claimed == nil {
				continue
			}
			if err := client.execute(ctx, *claimed); err != nil {
				onError(err)
			}
		}
	}
}

type claimedTurn struct {
	SessionID          string                    `json:"session_id"`
	TurnID             string                    `json:"turn_id"`
	RequestID          string                    `json:"request_id"`
	Backend            vocabulary.RuntimeBackend `json:"backend"`
	CLIID              string                    `json:"cli_id"`
	WorkspaceRootID    string                    `json:"workspace_root_id"`
	Prompt             string                    `json:"prompt"`
	FencingToken       uint64                    `json:"fencing_token"`
	LeaseExpiresAt     time.Time                 `json:"lease_expires_at"`
	DeliveredRequestID string                    `json:"delivered_request_id"`
	LastRuntimeCursor  uint64                    `json:"last_runtime_cursor"`
}

func (client *Client) heartbeat(ctx context.Context) error {
	return client.request(ctx, http.MethodPost, client.configuration.ControlURL.String()+"/archivist/v1/heartbeat", nil, nil)
}

func (client *Client) claim(ctx context.Context) (*claimedTurn, error) {
	var response struct {
		Data claimedTurn `json:"data"`
	}
	status, err := client.requestStatus(ctx, http.MethodPost, client.configuration.ControlURL.String()+"/archivist/v1/turns/claim", nil, &response)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	if response.Data.TurnID == "" {
		return nil, errors.New("runtime claim response is incomplete")
	}
	return &response.Data, nil
}

func (client *Client) execute(ctx context.Context, claimed claimedTurn) error {
	session, err := archivistruntime.NewSession(archivistruntime.SessionSpec{
		ID: claimed.SessionID, ArchivistHostID: client.configuration.ID, Backend: claimed.Backend,
		CLIID: claimed.CLIID, WorkspaceRootID: claimed.WorkspaceRootID,
	})
	if err != nil {
		return client.fail(ctx, claimed, 1, err)
	}
	if session.Backend != vocabulary.RuntimeEmbeddedCore {
		return client.fail(ctx, claimed, 1, errors.New("runtime backend is not supported by the embedded host"))
	}
	plan, err := archivistruntime.BuildLaunchPlan(session, client.configuration.LaunchPolicy)
	if err != nil {
		return client.fail(ctx, claimed, 1, err)
	}
	var delivery struct {
		Data struct {
			Deliver bool `json:"deliver"`
		} `json:"data"`
	}
	if err := client.request(ctx, http.MethodPost, client.turnURL(claimed.TurnID)+"/delivery", map[string]any{
		"fencing_token": claimed.FencingToken, "request_id": claimed.RequestID,
	}, &delivery); err != nil {
		return err
	}
	if !delivery.Data.Deliver && claimed.DeliveredRequestID != claimed.RequestID {
		return errors.New("runtime delivery state is inconsistent")
	}
	cursor := claimed.LastRuntimeCursor
	prompt := claimed.Prompt
	environment := map[string]string{
		"ARCHIVE_SESSION_ID":    claimed.SessionID,
		"ARCHIVE_TURN_ID":       claimed.TurnID,
		"ARCHIVE_FENCING_TOKEN": strconv.FormatUint(claimed.FencingToken, 10),
	}
	if delivery.Data.Deliver {
		cursor++
		if err := client.event(ctx, claimed, archivistruntime.RuntimeEvent{Cursor: cursor, RequestID: claimed.RequestID, Kind: archivistruntime.RuntimeInputAccepted}); err != nil {
			return err
		}
	} else {
		environment["ARCHIVE_RESUME_MODE"] = string(archivistruntime.ResumeCheckpoint)
		environment["ARCHIVE_LAST_RUNTIME_CURSOR"] = strconv.FormatUint(claimed.LastRuntimeCursor, 10)
		prompt = checkpointPrompt(claimed)
	}
	executionContext, cancelExecution := context.WithCancel(ctx)
	defer cancelExecution()
	renewalStop := make(chan struct{})
	renewalError := make(chan error, 1)
	go client.renewLease(executionContext, claimed, renewalStop, renewalError, cancelExecution)
	var eventMu sync.Mutex
	nextEvent := func(event archivistruntime.RuntimeEvent) error {
		eventMu.Lock()
		defer eventMu.Unlock()
		cursor++
		event.Cursor = cursor
		event.RequestID = claimed.RequestID
		return client.event(ctx, claimed, event)
	}
	response, err := client.runner.Run(executionContext, plan, prompt, "http://"+client.configuration.MCPListenAddress, environment, func() error {
		return nextEvent(archivistruntime.RuntimeEvent{Kind: archivistruntime.RuntimeTurnStarted})
	}, func(update archivistruntime.ProgressUpdate) error {
		return nextEvent(archivistruntime.RuntimeEvent{Kind: archivistruntime.RuntimeTurnProgress, Progress: &update})
	})
	close(renewalStop)
	select {
	case renewErr := <-renewalError:
		err = errors.Join(err, renewErr)
	default:
	}
	if err != nil {
		failure, _ := archivistruntime.NewFailureReport("RUNTIME_FAILED", "Archivist 실행을 완료하지 못했습니다")
		if reportErr := nextEvent(archivistruntime.RuntimeEvent{Kind: archivistruntime.RuntimeTurnFailed, Failure: &failure}); reportErr != nil {
			return errors.Join(err, reportErr)
		}
		return err
	}
	return nextEvent(archivistruntime.RuntimeEvent{Kind: archivistruntime.RuntimeTurnCompleted, FinalResponse: &response})
}

func checkpointPrompt(claimed claimedTurn) string {
	return "Resume the durable ArchivistTurn without repeating completed effects.\n" +
		"session_id=" + claimed.SessionID + "\nturn_id=" + claimed.TurnID +
		"\nlast_runtime_cursor=" + strconv.FormatUint(claimed.LastRuntimeCursor, 10) +
		"\noriginal_goal:\n" + claimed.Prompt
}

func (client *Client) renewLease(ctx context.Context, claimed claimedTurn, stop <-chan struct{}, result chan<- error, cancel context.CancelFunc) {
	interval := time.Until(claimed.LeaseExpiresAt) / 3
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			if err := client.request(ctx, http.MethodPost, client.turnURL(claimed.TurnID)+"/lease", map[string]any{
				"fencing_token": claimed.FencingToken,
			}, nil); err != nil {
				select {
				case result <- err:
				default:
				}
				cancel()
				return
			}
		}
	}
}

func (client *Client) fail(ctx context.Context, claimed claimedTurn, cursor uint64, cause error) error {
	failure, _ := archivistruntime.NewFailureReport("RUNTIME_CONFIGURATION_FAILED", "Archivist 실행 설정을 확인할 수 없습니다")
	reportErr := client.event(ctx, claimed, archivistruntime.RuntimeEvent{
		Cursor: cursor, RequestID: claimed.RequestID, Kind: archivistruntime.RuntimeTurnFailed, Failure: &failure,
	})
	return errors.Join(cause, reportErr)
}

func (client *Client) event(ctx context.Context, claimed claimedTurn, event archivistruntime.RuntimeEvent) error {
	payload := map[string]any{
		"fencing_token": claimed.FencingToken, "cursor": event.Cursor, "request_id": event.RequestID, "kind": event.Kind,
	}
	if event.FinalResponse != nil {
		payload["final_response"] = event.FinalResponse
	}
	if event.Progress != nil {
		payload["progress"] = event.Progress
	}
	if event.Failure != nil {
		payload["failure"] = event.Failure
	}
	return client.request(ctx, http.MethodPost, client.turnURL(claimed.TurnID)+"/events", payload, nil)
}

func (client *Client) turnURL(turnID string) string {
	return client.configuration.ControlURL.String() + "/archivist/v1/turns/" + turnID
}

func (client *Client) request(ctx context.Context, method, target string, payload any, targetResponse any) error {
	_, err := client.requestStatus(ctx, method, target, payload, targetResponse)
	return err
}

func (client *Client) requestStatus(ctx context.Context, method, target string, payload any, targetResponse any) (int, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+client.credential)
	request.Header.Set("X-Archivist-Host-ID", client.configuration.ID)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return response.StatusCode, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return response.StatusCode, fmt.Errorf("runtime control returned %s: %s", response.Status, strings.TrimSpace(string(payload)))
	}
	if targetResponse != nil {
		if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(targetResponse); err != nil {
			return response.StatusCode, err
		}
	}
	return response.StatusCode, nil
}
