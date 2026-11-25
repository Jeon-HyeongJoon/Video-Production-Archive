package controlruntime_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/adapter/controlruntime"
	"videoarchive/internal/adapter/sqlitecommand"
	"videoarchive/internal/adapter/storageinventory"
	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/workerclient"
	"videoarchive/internal/application/workerexecutor"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/workflow"
)

// Requirement: APP-011
func TestDamagedRecordingStopsAfterOneWorkerAttemptAcrossControlRestart(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("FFmpeg required")
	}
	probe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("FFprobe required")
	}
	tmp := t.TempDir()
	src, dst, logs := filepath.Join(tmp, "source"), filepath.Join(tmp, "archive"), filepath.Join(tmp, "logs")
	const folder = "370105-행사_촬영-본당"
	for _, dir := range []string{filepath.Join(src, folder), dst, logs} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	originals := damagedRecordingSiblings(t, ffmpeg, probe, filepath.Join(src, folder))
	source, err := archive.OpenReadOnlyStorageRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ctx := context.Background()
	listing, err := storageinventory.ScanSource(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	cfg := registeredMediaConfiguration(t, tmp, src, dst, probe)
	openControl := func() *controlruntime.Runtime {
		t.Helper()
		r, err := controlruntime.OpenWithDependencies(ctx, cfg, "rc-test", controlruntime.Dependencies{Secrets: runtimeSecret("service-secret"), Now: time.Now})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// Each launch owns its HTTP server and SQLite connection. Close both before reopening.
	var jobID string
	func() {
		r := openControl()
		defer r.Close()
		server := httptest.NewServer(r.Handler())
		defer server.Close()
		address, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		w := workerconfig.Configuration{ID: "worker-main", ControlURL: address, CredentialReference: "wincred://worker/main",
			HeartbeatInterval: time.Second, ClaimInterval: 30 * time.Millisecond, AccessibleStorageIDs: []string{"source-main", "archive-main"},
			StorageRoots: []workerconfig.StorageRootConfiguration{{StorageID: "source-main", Root: src}, {StorageID: "archive-main", Root: dst}},
			Tools:        workerconfig.MediaTools{FFmpeg: workerconfig.LocalTool{Executable: ffmpeg, Version: "test"}, FFprobe: workerconfig.LocalTool{Executable: probe, Version: "test"}},
			ProcessLogs:  &workerconfig.ProcessLogConfiguration{Root: logs, MaxFileBytes: 1 << 20, TailBytes: 8192, Retention: time.Hour, MaxTotalBytes: 4 << 20},
		}
		roots, err := workerconfig.OpenStorageRoots(w)
		if err != nil {
			t.Fatal(err)
		}
		defer roots.Close()
		executor, err := workerexecutor.NewProduction(w, roots)
		if err != nil {
			t.Fatal(err)
		}
		client, err := workerclient.NewWithExecutor(w, runtimeSecret("service-secret"), server.Client(), runtimeWorkerProbe{now: time.Now()}, time.Now, executor)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Heartbeat(ctx); err != nil {
			t.Fatal(err)
		}
		cookie, token := runtimeSession(t, r.Handler())
		body := fmt.Sprintf(`{"archive_run_id":"RUN-DAMAGED","idempotency_key":"damaged:start","session_id":"SESSION-DAMAGED","source_volume_id":"source-volume","expected_candidate_revision":1,"archive_storage_id":"archive-main","input_revision":%d,"folder_ids":[%q]}`, listing.DiscoveryRevision, folder)
		start := runtimeRequest(r.Handler(), http.MethodPost, "/api/v1/archive-intake/runs", body, cookie, token, "http://127.0.0.1:8080")
		if start.Code != http.StatusCreated {
			t.Fatalf("start: %s", start.Body.String())
		}
		jobs := runtimeRequest(r.Handler(), http.MethodGet, "/api/v1/jobs", "", nil, "", "")
		var inbox struct {
			Data []struct {
				JobID    string `json:"job_id"`
				Template string `json:"template_name"`
			}
		}
		if err := json.Unmarshal(jobs.Body.Bytes(), &inbox); err != nil {
			t.Fatal(err)
		}
		for _, j := range inbox.Data {
			if j.Template == "archive-intake-render-recordings" {
				jobID = j.JobID
			}
		}
		if jobID == "" {
			t.Fatalf("no recording job: %s", jobs.Body.String())
		}
		runCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		failures := make(chan error, 16)
		go func() {
			defer close(done)
			client.Run(runCtx, func(err error) {
				select {
				case failures <- err:
				case <-runCtx.Done():
				}
			})
		}()
		defer func() { stop(); <-done }()
		waitForJobStateOrWorkerError(t, r.Handler(), jobID, "FAILED", 35*time.Second, failures)
	}()

	reopened := openControl()
	defer reopened.Close()
	handler := reopened.Handler()
	response := runtimeRequest(handler, http.MethodGet, "/api/v1/jobs/"+jobID, "", nil, "", "")
	var detail struct {
		Data struct {
			Steps []struct {
				ID              string
				State           workflow.StepState
				FailureEvidence []job.StepFailureEvidence `json:"failure_evidence"`
			}
		}
	}
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	var evidence []job.StepFailureEvidence
	for _, step := range detail.Data.Steps {
		if step.ID == "render" && step.State == workflow.Failed {
			evidence = step.FailureEvidence
		}
		if step.ID != "render" && step.State == workflow.Completed {
			t.Fatalf("downstream step %s completed", step.ID)
		}
	}
	if len(evidence) != 1 || evidence[0].CauseCode != "SOURCE_DECODE_FAILED" || evidence[0].ProcessExitCode == nil || *evidence[0].ProcessExitCode == 0 ||
		evidence[0].WorkflowAttempt != 1 || evidence[0].ResultingState != workflow.Failed || evidence[0].ProcessLog == nil || !strings.Contains(evidence[0].Summary, "3개 중 2번째") {
		t.Fatalf("expected a single terminal input failure for item 2/3 with a process log, got %+v", evidence)
	}
	now := time.Now().UTC()
	heartbeat := fmt.Sprintf(`{"worker_id":"worker-main","snapshot_id":%d,"observed_at":%q,"available_memory_bytes":34359738368,"available_vram_bytes":8589934592,"scratch_free_bytes":536870912000,"idle_stable_millis":360000,"accepting_work":true,"accessible_storage_ids":["source-main","archive-main"]}`, now.UnixNano(), now.Format(time.RFC3339Nano))
	if live := workerHeartbeatRequest(handler, heartbeat, "service-secret"); live.Code != http.StatusOK {
		t.Fatalf("worker could not resume after restart: %d %s", live.Code, live.Body.String())
	}
	claim := workerProtocolRequest(handler, "/worker/v1/steps/claim", "", "service-secret")
	if claim.Code != http.StatusNoContent {
		t.Fatalf("reassigned after restart: %d %s", claim.Code, claim.Body.String())
	}
	work := runtimeRequest(handler, http.MethodGet, "/api/v1/work/RUN-DAMAGED", "", nil, "", "")
	var run struct {
		Data struct {
			Summary struct {
				State   string
				Results *int `json:"result_count"`
			}
		}
	}
	if err := json.Unmarshal(work.Body.Bytes(), &run); err != nil || run.Data.Summary.State != "FAILED" || run.Data.Summary.Results == nil || *run.Data.Summary.Results != 0 {
		t.Fatalf("incorrect persisted work outcome: %s", work.Body.String())
	}
	destination, err := archive.OpenReadOnlyStorageRoot(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	if exists, err := destination.Exists("archive-catalog.csv"); err != nil || exists {
		t.Fatalf("unexpected destination catalog: exists=%v, err=%v", exists, err)
	}
	ledger, err := sqlitecommand.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	record, err := ledger.Load(jobID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := job.Decode(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range state.Steps {
		if step.Workflow.ID != "render" {
			continue
		}
		if step.ExecutionSpec == nil || len(step.ExecutionSpec.Outputs) != 3 || step.OutputRecord != nil {
			t.Fatal("expected three planned render targets and no complete Step output")
		}
		for i, out := range step.ExecutionSpec.Outputs {
			exists, err := destination.Exists(out.RelativePath)
			if err != nil || exists != (i == 0) {
				t.Fatalf("staging item %d exists=%v err=%v", i+1, exists, err)
			}
		}
	}
	for name, sum := range originals {
		_, after, err := source.FileSHA256(folder + "/" + name)
		if err != nil || after != sum {
			t.Fatalf("source %s changed: %s, %v", name, after, err)
		}
	}
}

func damagedRecordingSiblings(t *testing.T, ffmpeg, probe, folder string) map[string]string {
	t.Helper()
	first := filepath.Join(folder, "A 녹화.mp4")
	cmd := exec.Command(ffmpeg, "-v", "error", "-nostdin", "-f", "lavfi", "-i", "testsrc2=s=160x90:r=12", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "2", "-c:v", "libx264", "-g", "12", "-bf", "0", "-threads", "1", "-c:a", "aac", "-movflags", "+faststart", first)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate clip: %s (%v)", out, err)
	}
	healthy, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	packets, err := exec.Command(probe, "-v", "error", "-select_streams", "v:0", "-show_packets", "-show_entries", "packet=pos", "-of", "json", first).Output()
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Packets []struct {
			Pos int `json:"pos,string"`
		}
	}
	if err := json.Unmarshal(packets, &raw); err != nil || len(raw.Packets) != 24 {
		t.Fatalf("packet positions: %s (%v)", packets, err)
	}
	// Keep the headers and the first three decodable frames. Damage only the video payload.
	damaged := append([]byte(nil), healthy...)
	for _, p := range raw.Packets[3:] {
		copy(damaged[p.Pos:p.Pos+4], []byte{0x7f, 0xff, 0xff, 0xff})
	}
	files := map[string][]byte{"A 녹화.mp4": healthy, "B 녹화.mp4": damaged, "C0001.MP4": healthy}
	sums := make(map[string]string, len(files))
	for name, data := range files {
		if name != filepath.Base(first) {
			if err := os.WriteFile(filepath.Join(folder, name), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		sums[name] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	return sums
}
