package workerclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
)

type cancellationSecret string

func (secret cancellationSecret) Resolve(string) (string, error) { return string(secret), nil }

type cancellationProbe struct{}

func (cancellationProbe) Snapshot(context.Context) (worker.Metrics, error) {
	return worker.Metrics{}, nil
}

type cancellationIgnoringExecutor struct{ release <-chan struct{} }

func (executor cancellationIgnoringExecutor) Execute(context.Context, Assignment) ExecutionResult {
	<-executor.release
	return ExecutionResult{}
}

func TestCancellationThatDoesNotStopReportsUncertaintyInsteadOfBlockingWorkerLoop(t *testing.T) {
	result := make(chan map[string]any, 1)
	var resultAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/worker/v1/steps/cancellation":
			writer.Write([]byte(`{"data":{"cancellation_id":"CANCEL-1","job_id":"JOB-1","step_id":"STEP-1","fencing_token":1,"requested_at":"2026-11-06T00:00:00Z"}}`))
		case "/worker/v1/steps/cancellation-result":
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			if resultAttempts.Add(1) == 1 {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			result <- payload
			writer.Write([]byte(`{"data":{"state":"SETTLED"}}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	controlURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	client, err := NewWithExecutor(workerconfig.Configuration{
		SchemaVersion: 1, ID: "worker-main", ControlURL: controlURL,
		CredentialReference: "secret", HeartbeatInterval: time.Second, ClaimInterval: 10 * time.Millisecond,
		AccessibleStorageIDs: []string{"scratch"},
	}, cancellationSecret("service-secret"), server.Client(), cancellationProbe{}, time.Now, cancellationIgnoringExecutor{release: release})
	if err != nil {
		t.Fatal(err)
	}
	assignment := Assignment{
		JobID: "JOB-1", StepID: "STEP-1", FencingToken: 1, LeaseRenewAfterMillis: 60_000,
		ExecutionSpec: execution.StepExecutionSpec{JobID: "JOB-1", StepID: "STEP-1"},
	}
	done := make(chan struct{})
	go func() {
		client.execute(context.Background(), assignment, func(err error) { t.Error(err) })
		close(done)
	}()
	select {
	case payload := <-result:
		if stopped, _ := payload["process_stopped"].(bool); stopped {
			t.Fatalf("unresponsive executor was reported as stopped: %+v", payload)
		}
		if resultAttempts.Load() != 2 {
			t.Fatalf("cancellation result was not retried exactly after transient failure: attempts=%d", resultAttempts.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("unresponsive executor blocked cancellation settlement")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Worker execution slot did not return after reporting uncertainty")
	}
	close(release)
}

func TestHeartbeatIncludesTheResourceClassOfTheCurrentlyExecutingStep(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/worker/v1/heartbeat" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		received <- payload
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte(`{"data":{"state":"ONLINE"}}`))
	}))
	defer server.Close()
	controlURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewWithExecutor(workerconfig.Configuration{
		SchemaVersion: 1, ID: "worker-main", ControlURL: controlURL,
		CredentialReference: "secret", HeartbeatInterval: time.Second,
		AccessibleStorageIDs: []string{"scratch"},
	}, cancellationSecret("service-secret"), server.Client(), cancellationProbe{}, time.Now, cancellationIgnoringExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	client.setExecutionResourceClass(vocabulary.ResourceHeavy)
	if err := client.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	payload := <-received
	if payload["heavy_running"] != float64(1) || payload["light_running"] != float64(0) {
		t.Fatalf("heartbeat omitted current execution resource class: %+v", payload)
	}
}
