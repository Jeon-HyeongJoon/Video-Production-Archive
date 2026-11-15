package archivisthostclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"videoarchive/internal/adapter/archivisthostconfig"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/vocabulary"
)

type loginRequiredProcessRunner struct{}

func (loginRequiredProcessRunner) Run(
	_ context.Context, _ archivistruntime.LaunchPlan, _, _ string, _ map[string]string,
	turnStarted func() error, _ func(archivistruntime.ProgressUpdate) error,
) (archivistruntime.FinalResponse, error) {
	if err := turnStarted(); err != nil {
		return archivistruntime.FinalResponse{}, err
	}
	return archivistruntime.FinalResponse{}, &archivistruntime.RuntimeExecutionFailure{Report: archivistruntime.FailureReport{
		Code: "CODEX_LOGIN_REQUIRED", Summary: "Codex 로그인이 필요합니다",
	}}
}

type blockingProcessRunner struct {
	started chan struct{}
	release chan struct{}
}

func (runner blockingProcessRunner) Run(
	ctx context.Context,
	_ archivistruntime.LaunchPlan,
	_, _ string,
	_ map[string]string,
	turnStarted func() error,
	_ func(archivistruntime.ProgressUpdate) error,
) (archivistruntime.FinalResponse, error) {
	if err := turnStarted(); err != nil {
		return archivistruntime.FinalResponse{}, err
	}
	close(runner.started)
	select {
	case <-ctx.Done():
		return archivistruntime.FinalResponse{}, ctx.Err()
	case <-runner.release:
	}
	return archivistruntime.NewFinalResponse("완료", nil)
}

// Requirement: LRH-021
func TestHostKeepsHeartbeatOnlineWhileCLIProcessesTurn(t *testing.T) {
	var heartbeats atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/archivist/v1/heartbeat":
			heartbeats.Add(1)
			writer.WriteHeader(http.StatusOK)
		case request.URL.Path == "/archivist/v1/turns/TURN-1/delivery":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"data":{"deliver":true}}`))
		default:
			writer.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	controlURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	executable, workspace := "/bin/true", t.TempDir()
	if runtime.GOOS == "windows" {
		executable, workspace = `C:\fixture\archive-codex.exe`, `C:\fixture\workspace`
	}
	runner := blockingProcessRunner{started: make(chan struct{}), release: make(chan struct{})}
	client := &Client{
		configuration: archivisthostconfig.Configuration{
			ID: "HOST-1", ControlURL: controlURL, MCPListenAddress: "127.0.0.1:19091",
			HeartbeatInterval: 10 * time.Millisecond,
			LaunchPolicy: archivistruntime.LaunchPolicy{
				CLIs: map[string]archivistruntime.CLIExecutable{
					"codex": {Executable: executable},
				},
				WorkspaceRoots: map[string]string{"archive": workspace},
			},
		},
		credential: "fixture-secret", httpClient: server.Client(), runner: runner,
	}
	claimed := claimedTurn{
		SessionID: "SESSION-1", TurnID: "TURN-1", RequestID: "REQUEST-1",
		Backend: vocabulary.RuntimeEmbeddedCore, CLIID: "codex", WorkspaceRootID: "archive",
		Prompt: "상태 확인", FencingToken: 1, LeaseExpiresAt: time.Now().Add(time.Second),
	}
	done := make(chan error, 1)
	go func() {
		done <- client.execute(context.Background(), claimed, func(err error) { t.Errorf("heartbeat failed: %v", err) })
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("fixture CLI did not start")
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for heartbeats.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if heartbeats.Load() < 2 {
		t.Fatalf("Host heartbeat stopped while the CLI was running: count=%d", heartbeats.Load())
	}
	select {
	case err := <-done:
		t.Fatalf("Turn ended before the fixture CLI was released: %v", err)
	default:
	}
	close(runner.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Requirement: LRH-022
func TestCodexLoginAbsenceFailsOnlyTurnWithPublicReconnectNotice(t *testing.T) {
	var failures []archivistruntime.FailureReport
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/archivist/v1/turns/TURN-LOGIN/delivery":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"data":{"deliver":true}}`))
		case request.URL.Path == "/archivist/v1/turns/TURN-LOGIN/events":
			var payload struct {
				Failure *archivistruntime.FailureReport `json:"failure"`
			}
			if json.NewDecoder(request.Body).Decode(&payload) == nil && payload.Failure != nil {
				failures = append(failures, *payload.Failure)
			}
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	controlURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	executable, workspace := "/bin/true", t.TempDir()
	if runtime.GOOS == "windows" {
		executable, workspace = `C:\fixture\archive-codex.exe`, `C:\fixture\workspace`
	}
	client := &Client{
		configuration: archivisthostconfig.Configuration{
			ID: "HOST-LOGIN", ControlURL: controlURL, MCPListenAddress: "127.0.0.1:19091", HeartbeatInterval: time.Second,
			LaunchPolicy: archivistruntime.LaunchPolicy{
				CLIs:           map[string]archivistruntime.CLIExecutable{"codex": {Executable: executable}},
				WorkspaceRoots: map[string]string{"archive": workspace},
			},
		},
		credential: "fixture-secret", httpClient: server.Client(), runner: loginRequiredProcessRunner{},
	}
	claimed := claimedTurn{
		SessionID: "SESSION-LOGIN", TurnID: "TURN-LOGIN", RequestID: "REQUEST-LOGIN",
		Backend: vocabulary.RuntimeEmbeddedCore, CLIID: "codex", WorkspaceRootID: "archive",
		Prompt: "작업 확인", FencingToken: 1, LeaseExpiresAt: time.Now().Add(time.Minute),
	}
	err = client.execute(context.Background(), claimed, func(error) {})
	var runtimeFailure *archivistruntime.RuntimeExecutionFailure
	if !errors.As(err, &runtimeFailure) {
		t.Fatalf("expected local runtime failure, got %v", err)
	}
	if len(failures) != 1 || failures[0].Code != "CODEX_LOGIN_REQUIRED" || failures[0].Summary != "Codex 로그인이 필요합니다" {
		t.Fatalf("unexpected public failure events: %+v", failures)
	}
}
