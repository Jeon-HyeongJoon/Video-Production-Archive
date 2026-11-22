package workerclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/workflow"
)

type burstExecutor struct {
	first   <-chan struct{}
	release chan struct{}
}

func (e burstExecutor) Execute(ctx context.Context, _ Assignment) ExecutionResult {
	update := execution.ProgressUpdate{Operation: execution.OperationRenderRecording, Status: execution.ProgressPosition, OutputMillis: 1}
	_ = execution.ReportProgress(ctx, update)
	select {
	case <-e.first:
	case <-ctx.Done():
		return ExecutionResult{}
	}
	for i := 2; i <= 64; i++ {
		update.OutputMillis = int64(i)
		_ = execution.ReportProgress(ctx, update)
	}
	close(e.release)
	return ExecutionResult{FailureClass: workflow.FailureTransient, ErrorClass: "TEST_FAILURE"}
}

// Requirement: WRK-021
func TestWorkerSettlesWithProducedCountWhenProgressDeliveryFallsBehind(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var reports []struct {
		Seq    uint64                   `json:"report_sequence"`
		Update execution.ProgressUpdate `json:"update"`
	}
	var total uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/worker/v1/steps/progress":
			var sample struct {
				Seq    uint64                   `json:"report_sequence"`
				Update execution.ProgressUpdate `json:"update"`
			}
			if err := json.NewDecoder(r.Body).Decode(&sample); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			reports = append(reports, sample)
			n := len(reports)
			mu.Unlock()
			if n == 1 {
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		case "/worker/v1/steps/result":
			var result struct {
				Count   uint64 `json:"progress_report_count"`
				Success bool   `json:"success"`
			}
			if err := json.NewDecoder(r.Body).Decode(&result); err != nil || result.Success {
				t.Errorf("incorrect settlement %+v %v", result, err)
			}
			mu.Lock()
			total = result.Count
			mu.Unlock()
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	client, err := NewWithExecutor(workerconfig.Configuration{ID: "worker", ControlURL: endpoint, CredentialReference: "cred", HeartbeatInterval: time.Minute, ClaimInterval: time.Minute, AccessibleStorageIDs: []string{"source"}}, cancellationSecret("s"), server.Client(), cancellationProbe{}, time.Now, burstExecutor{started, release})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client.execute(ctx, Assignment{JobID: "J", StepID: "S", FencingToken: 1}, func(err error) { t.Error(err) })
	mu.Lock()
	defer mu.Unlock()
	if total != 64 || len(reports) < 3 || len(reports) >= 64 || reports[0] != reports[1] || reports[0].Seq != 1 || reports[0].Update.WorkerObservedAt.IsZero() {
		t.Fatalf("unaccounted or duplicated observations, reported=%d produced=%d", len(reports), total)
	}
	for i := 2; i < len(reports); i++ {
		if reports[i].Seq != reports[i-1].Seq+1 || reports[i].Update.OutputMillis != int64(reports[i].Seq) {
			t.Fatal("observation order changed")
		}
	}
}
