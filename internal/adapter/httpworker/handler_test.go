package httpworker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/adapter/sqlitecommand"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/application/dispatchcoordinator"
	"videoarchive/internal/application/workercontrol"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

type testVerifier string

func (verifier testVerifier) Verify(_, _, presented string) bool {
	return presented == string(verifier)
}

// Requirement: WRK-010
func TestWorkerClaimReturnsOnlyValidatedFencedStepExecutionSpec(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		withSpec   bool
		tamper     bool
		wantStatus int
	}{
		{name: "valid", withSpec: true, wantStatus: http.StatusOK},
		{name: "missing", wantStatus: http.StatusConflict},
		{name: "digest mismatch", withSpec: true, tamper: true, wantStatus: http.StatusConflict},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			handler, store, expected := claimHarness(t, scenario.withSpec, scenario.tamper)
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/worker/v1/steps/claim", nil)
			request.Header.Set("Authorization", "Bearer service-secret")
			request.Header.Set("X-Worker-ID", "worker-windows")
			handler.ServeHTTP(response, request)
			if response.Code != scenario.wantStatus {
				t.Fatalf("claim returned %d: %s", response.Code, response.Body.String())
			}
			if scenario.wantStatus == http.StatusOK {
				var payload struct {
					Data struct {
						JobID         string                      `json:"job_id"`
						StepID        string                      `json:"step_id"`
						FencingToken  uint64                      `json:"fencing_token"`
						ExecutionSpec execution.StepExecutionSpec `json:"execution_spec"`
					} `json:"data"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if payload.Data.JobID != expected.JobID || payload.Data.StepID != expected.StepID || payload.Data.FencingToken != 1 ||
					payload.Data.ExecutionSpec.Digest != expected.Digest || payload.Data.ExecutionSpec.Validate() != nil ||
					strings.Contains(response.Body.String(), "service-secret") {
					t.Fatalf("claim did not return the exact safe spec: %s", response.Body.String())
				}
				return
			}
			snapshot, err := store.Load("JOB-SPEC")
			if err != nil {
				t.Fatal(err)
			}
			state, err := job.Decode(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			step := claimTestStep(&state, "render")
			if step == nil || step.Workflow.State != workflow.Reserved {
				t.Fatalf("rejected spec changed Step state: %+v", step)
			}
		})
	}
}

// Requirement: WRK-011
func TestWorkerResultAtomicallyStoresOnlyExpectedStepOutputRecord(t *testing.T) {
	handler, store, spec := claimHarness(t, true, false)
	claim := httptest.NewRecorder()
	claimRequest := httptest.NewRequest(http.MethodPost, "/worker/v1/steps/claim", nil)
	claimRequest.Header.Set("Authorization", "Bearer service-secret")
	claimRequest.Header.Set("X-Worker-ID", "worker-windows")
	handler.ServeHTTP(claim, claimRequest)
	if claim.Code != http.StatusOK {
		t.Fatalf("claim returned %d: %s", claim.Code, claim.Body.String())
	}
	record, err := execution.NewStepOutputRecord(
		spec, 1, "worker-windows", time.Date(2026, 10, 30, 20, 0, 1, 0, time.UTC),
		[]execution.StepOutput{{
			ID: spec.Outputs[0].ID, Kind: spec.Outputs[0].Kind, StorageID: spec.Outputs[0].StorageID,
			RelativePath: spec.Outputs[0].RelativePath, Checksum: strings.Repeat("b", 64),
		}}, []string{"evidence://render-verified"},
	)
	if err != nil {
		t.Fatal(err)
	}
	outside := record
	outside.Outputs = append([]execution.StepOutput(nil), record.Outputs...)
	outside.Outputs[0].RelativePath = "output/unexpected.mp4"
	if response := workerResult(handler, outside); response.Code != http.StatusConflict {
		t.Fatalf("out-of-spec output returned %d: %s", response.Code, response.Body.String())
	}
	snapshot, err := store.Load("JOB-SPEC")
	if err != nil {
		t.Fatal(err)
	}
	state, err := job.Decode(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	step := claimTestStep(&state, "render")
	if step == nil || step.Workflow.State != workflow.Running || step.OutputRecord != nil {
		t.Fatalf("rejected output partially changed the Step: %+v", step)
	}

	accepted := workerResult(handler, record)
	if accepted.Code != http.StatusOK {
		t.Fatalf("valid output returned %d: %s", accepted.Code, accepted.Body.String())
	}
	snapshot, err = store.Load("JOB-SPEC")
	if err != nil {
		t.Fatal(err)
	}
	state, err = job.Decode(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	step = claimTestStep(&state, "render")
	if step == nil || step.Workflow.State != workflow.Completed || step.OutputRecord == nil ||
		step.OutputRecord.SpecDigest != spec.Digest || step.OutputRecord.FencingToken != 1 ||
		len(step.OutputRecord.Outputs) != 1 || step.OutputRecord.Outputs[0].Checksum != strings.Repeat("b", 64) {
		t.Fatalf("accepted output was not stored with completion: %+v", step)
	}
	revision := state.Revision
	if replay := workerResult(handler, record); replay.Code != http.StatusOK {
		t.Fatalf("duplicate output returned %d: %s", replay.Code, replay.Body.String())
	}
	after, err := store.Load("JOB-SPEC")
	if err != nil || after.Revision != revision {
		t.Fatalf("duplicate output changed state: revision=%d err=%v", after.Revision, err)
	}
}

// Requirement: APP-004
func TestDriveWorkerOutputsMaterializeEveryDependentStepSpecExactlyOnce(t *testing.T) {
	handler, store, _ := claimHarness(t, true, false)
	expected := []struct {
		stepID    string
		operation execution.Operation
	}{
		{stepID: "render", operation: execution.OperationRenderRecording},
		{stepID: "qc", operation: execution.OperationQCRendition},
		{stepID: "organize", operation: execution.OperationOrganizeMedia},
		{stepID: "archive_catalog", operation: execution.OperationWriteArchiveCatalog},
	}
	digests := make(map[string]string, len(expected))
	for index, want := range expected {
		claim := httptest.NewRecorder()
		claimRequest := httptest.NewRequest(http.MethodPost, "/worker/v1/steps/claim", nil)
		claimRequest.Header.Set("Authorization", "Bearer service-secret")
		claimRequest.Header.Set("X-Worker-ID", "worker-windows")
		handler.ServeHTTP(claim, claimRequest)
		if claim.Code != http.StatusOK {
			t.Fatalf("claim %s returned %d: %s", want.stepID, claim.Code, claim.Body.String())
		}
		var claimed struct {
			Data struct {
				JobID        string                      `json:"job_id"`
				StepID       string                      `json:"step_id"`
				FencingToken uint64                      `json:"fencing_token"`
				Spec         execution.StepExecutionSpec `json:"execution_spec"`
			} `json:"data"`
		}
		if err := json.Unmarshal(claim.Body.Bytes(), &claimed); err != nil {
			t.Fatal(err)
		}
		spec := claimed.Data.Spec
		if claimed.Data.JobID != "JOB-SPEC" || claimed.Data.StepID != want.stepID || spec.Operation != want.operation ||
			spec.Validate() != nil || strings.Contains(string(claim.Body.Bytes()), `C:\reported`) {
			t.Fatalf("dependent claim %s has an invalid spec: %s", want.stepID, claim.Body.String())
		}
		if index > 0 {
			previousSnapshot, err := store.Load("JOB-SPEC")
			if err != nil {
				t.Fatal(err)
			}
			previousState, err := job.Decode(previousSnapshot)
			if err != nil {
				t.Fatal(err)
			}
			for _, input := range spec.Inputs {
				matched := false
				for _, completed := range previousState.Steps {
					if completed.OutputRecord == nil {
						continue
					}
					for _, output := range completed.OutputRecord.Outputs {
						if input.ArtifactID == output.ID && input.StorageID == output.StorageID &&
							input.RelativePath == output.RelativePath && input.Checksum == output.Checksum {
							matched = true
						}
					}
				}
				if input.ArtifactID != "" && !matched {
					t.Fatalf("%s input was not derived from a stored output: %+v", want.stepID, input)
				}
			}
		}
		digests[want.stepID] = spec.Digest
		outputs := make([]execution.StepOutput, 0, len(spec.Outputs))
		for _, target := range spec.Outputs {
			outputs = append(outputs, execution.StepOutput{
				ID: target.ID, Kind: target.Kind, StorageID: target.StorageID,
				RelativePath: target.RelativePath, Checksum: strings.Repeat(string("abcdef0"[index]), 64),
			})
		}
		record, err := execution.NewStepOutputRecord(
			spec, claimed.Data.FencingToken, "worker-windows", time.Date(2026, 10, 30, 20, index+1, 0, 0, time.UTC),
			outputs, []string{"evidence://" + want.stepID + "-verified"},
		)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(map[string]any{
			"job_id": "JOB-SPEC", "step_id": want.stepID, "fencing_token": claimed.Data.FencingToken,
			"success": true, "failure_class": "", "wait_reason": "", "error_class": "",
			"log_reference": `C:\reported\must-not-become-input.mp4`, "output_record": record,
		})
		if err != nil {
			t.Fatal(err)
		}
		result := httptest.NewRecorder()
		resultRequest := httptest.NewRequest(http.MethodPost, "/worker/v1/steps/result", bytes.NewReader(payload))
		resultRequest.Header.Set("Content-Type", "application/json")
		resultRequest.Header.Set("Authorization", "Bearer service-secret")
		resultRequest.Header.Set("X-Worker-ID", "worker-windows")
		handler.ServeHTTP(result, resultRequest)
		if result.Code != http.StatusOK {
			t.Fatalf("settle %s returned %d: %s", want.stepID, result.Code, result.Body.String())
		}
	}
	finalSnapshot, err := store.Load("JOB-SPEC")
	if err != nil {
		t.Fatal(err)
	}
	finalState, err := job.Decode(finalSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range finalState.Steps {
		if digest, expectedStep := digests[step.Workflow.ID]; expectedStep {
			if step.ExecutionSpec == nil || step.ExecutionSpec.Digest != digest || step.OutputRecord == nil ||
				strings.Contains(step.ExecutionSpec.Digest, `C:\reported`) {
				t.Fatalf("materialized spec changed or was not completed: %+v", step)
			}
		}
	}
}

func workerResult(handler http.Handler, record execution.StepOutputRecord) *httptest.ResponseRecorder {
	payload, _ := json.Marshal(map[string]any{
		"job_id": "JOB-SPEC", "step_id": "render", "fencing_token": 1, "success": true,
		"failure_class": "", "wait_reason": "", "error_class": "", "log_reference": "evidence://worker",
		"output_record": record,
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/steps/result", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set("X-Worker-ID", "worker-windows")
	handler.ServeHTTP(response, request)
	return response
}

type discardProjection struct{}

func (discardProjection) Replace(context.Context, archiveworkspace.JobView) error { return nil }

func claimHarness(t *testing.T, withSpec, tamper bool) (http.Handler, *sqlitecommand.Store, execution.StepExecutionSpec) {
	t.Helper()
	now := time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC)
	store, err := sqlitecommand.Open(filepath.Join(t.TempDir(), "claim.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	sourceInput := execution.InputReference{
		OriginalAssetID: "ASSET-1", OriginalName: "clip.mov", InspectionID: "INSPECTION-1", StorageID: "scratch-main",
		RelativePath: "source/clip.mov", Checksum: strings.Repeat("a", 64), DurationMillis: 10_000,
		VideoCodec: "h264", Width: 1920, Height: 1080, VideoBitRateBPS: 6_000_000, HasAudio: true,
	}
	spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: "JOB-SPEC", StepID: "render", TemplateName: "archive-intake-render-recordings",
		Operation: execution.OperationRenderRecording,
		Inputs:    []execution.InputReference{sourceInput},
		Outputs: []execution.OutputTarget{{
			ID: "RENDITION-1", Kind: execution.OutputRendition, StorageID: "scratch-main", RelativePath: "output/render.mp4",
			OriginalAssetID: "ASSET-1",
		}},
		ProfileReference: "archive-1080p-6mbps@v1",
		Parameters: execution.StepExecutionParameters{
			OrganizationDecisionID: "DECISION-1", WorkspaceDriveID: "DRIVE-1", ArchiveStorageID: "scratch-main", InputRevision: 1,
			MediaFolder: "recordings", ArchiveDate: "20261030",
			Disposition: vocabulary.OrganizationRenderRecordings, SelectedProfileReference: "archive-1080p-6mbps@v1",
			TranscriptionEligible: true, SourceInputs: []execution.InputReference{sourceInput},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	parameters := map[string]any{
		"template_name": "archive-intake-render-recordings", "target_title": "render",
		"required_storage_ids": []string{"scratch-main"},
	}
	if withSpec {
		parameters["step_execution_specs"] = map[string]execution.StepExecutionSpec{"render": spec}
	}
	jobHandler, err := job.NewHandler(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	executor := command.NewAuthorizedCreatingExecutor(store, func(command.Envelope) error { return nil }, jobHandler.Handle)
	if _, err := executor.Execute(command.Envelope{
		CommandID: "CREATE-SPEC", CommandType: job.CreateCommand,
		Actor: command.Actor{Type: vocabulary.ActorSystem, ID: "test"}, RequestedVia: "TEST",
		TargetID: "JOB-SPEC", ExpectedRevision: 0, IdempotencyKey: "JOB-SPEC:create", Parameters: parameters, Reason: "claim fixture",
	}); err != nil {
		t.Fatal(err)
	}
	registry := worker.NewRegistry([]worker.Definition{{
		ID: "worker-windows", Enabled: true, CredentialRef: "wincred://workers/windows",
		ProcessingKinds: map[vocabulary.ProcessingKind]bool{
			vocabulary.ProcessingRender: true, vocabulary.ProcessingQC: true, vocabulary.ProcessingOrganize: true,
			vocabulary.ProcessingAudioExtract: true, vocabulary.ProcessingSTT: true,
		},
		AllowedStorageIDs: map[string]bool{"scratch-main": true},
	}}, testVerifier("service-secret"), 30*time.Second)
	if err := registry.Connect("worker-windows", "service-secret", now, worker.Metrics{
		SnapshotID: 1, ObservedAt: now, IdleStableFor: time.Hour, AvailableMemoryBytes: 32 << 30,
		ScratchFreeBytes: 100 << 30, BusyPhysicalDeviceIDs: map[string]bool{},
		AccessibleStorageIDs: map[string]bool{"scratch-main": true}, AcceptingWork: true,
	}); err != nil {
		t.Fatal(err)
	}
	dispatcher, err := dispatchcoordinator.New(store, registry, discardProjection{}, func() time.Time { return now }, 30*time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.DispatchAvailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tamper {
		snapshot, err := store.Load("JOB-SPEC")
		if err != nil {
			t.Fatal(err)
		}
		state, err := job.Decode(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		step := claimTestStep(&state, "render")
		if step == nil || step.ExecutionSpec == nil {
			t.Fatal("render Step spec is missing before tamper")
		}
		step.ExecutionSpec.Digest = strings.Repeat("0", 64)
		encoded, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Seed(context.Background(), command.Snapshot{TargetID: state.ID, Revision: state.Revision, State: encoded}); err != nil {
			t.Fatal(err)
		}
	}
	service, err := workercontrol.New(store, discardProjection{}, dispatcher, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewWithExecution(registry, func() time.Time { return now }, dispatcher, service)
	if err != nil {
		t.Fatal(err)
	}
	return handler, store, spec
}

func claimTestStep(state *job.State, stepID string) *job.Step {
	for index := range state.Steps {
		if state.Steps[index].Workflow.ID == stepID {
			return &state.Steps[index]
		}
	}
	return nil
}

// Requirement: WRK-008
func TestWorkerHeartbeatStoresOnlyLatestMonotonicCompleteResourceSnapshotForDispatch(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	registry := worker.NewRegistry([]worker.Definition{{
		ID: "worker-windows", Enabled: true, CredentialRef: "wincred://workers/windows",
		ProcessingKinds:   map[vocabulary.ProcessingKind]bool{vocabulary.ProcessingRender: true},
		AllowedStorageIDs: map[string]bool{"scratch": true},
	}}, testVerifier("service-secret"), 30*time.Second)
	handler, err := New(registry, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	latest := `{"worker_id":"worker-windows","snapshot_id":10,"observed_at":"2026-10-29T12:00:00Z","cpu_average_5m":12.5,"gpu_average_5m":3.5,"idle_stable_millis":360000,"available_memory_bytes":17179869184,"available_vram_bytes":4294967296,"scratch_free_bytes":107374182400,"heavy_running":1,"light_running":2,"busy_physical_device_ids":["disk-7"],"accessible_storage_ids":["scratch"],"accepting_work":true}`
	if response := heartbeat(handler, latest); response.Code != http.StatusOK {
		t.Fatalf("latest snapshot returned %d: %s", response.Code, response.Body.String())
	}
	reversedID := bytes.Replace([]byte(latest), []byte(`"snapshot_id":10`), []byte(`"snapshot_id":9`), 1)
	if response := heartbeat(handler, string(reversedID)); response.Code != http.StatusConflict {
		t.Fatalf("reversed snapshot ID returned %d: %s", response.Code, response.Body.String())
	}
	reversedTime := bytes.Replace([]byte(latest), []byte(`"snapshot_id":10`), []byte(`"snapshot_id":11`), 1)
	reversedTime = bytes.Replace(reversedTime, []byte(`2026-10-29T12:00:00Z`), []byte(`2026-10-29T11:59:59Z`), 1)
	if response := heartbeat(handler, string(reversedTime)); response.Code != http.StatusConflict {
		t.Fatalf("reversed observation time returned %d: %s", response.Code, response.Body.String())
	}

	snapshot, available := registry.DispatchSnapshot("worker-windows", now)
	if !available || snapshot.SnapshotID != 10 || snapshot.AverageCPU5m != 12.5 || snapshot.AverageGPU5m != 3.5 ||
		snapshot.IdleStableFor != 6*time.Minute || snapshot.AvailableMemoryBytes != 16<<30 ||
		snapshot.AvailableVRAMBytes != 4<<30 || snapshot.ScratchFreeBytes != 100<<30 ||
		snapshot.HeavyRunning != 1 || snapshot.LightRunning != 2 || !snapshot.BusyPhysicalDeviceIDs["disk-7"] ||
		!snapshot.AccessibleStorageIDs["scratch"] || !snapshot.AcceptingWork {
		t.Fatalf("dispatch snapshot is incomplete or was overwritten: available=%v snapshot=%+v", available, snapshot)
	}
	now = now.Add(31 * time.Second)
	if _, available := registry.DispatchSnapshot("worker-windows", now); available {
		t.Fatal("expired resource snapshot remained dispatchable")
	}
}

func heartbeat(handler http.Handler, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/heartbeat", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer service-secret")
	handler.ServeHTTP(response, request)
	return response
}
