package controlruntime_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"videoarchive/internal/adapter/bootconfig"
	"videoarchive/internal/adapter/controlruntime"
	"videoarchive/internal/adapter/mcpproxy"
	"videoarchive/internal/adapter/sqlitecommand"
	"videoarchive/internal/adapter/sqlitestorage"
	"videoarchive/internal/adapter/windowstask"
	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/archiveorganization"
	"videoarchive/internal/application/archivistcontrol"
	"videoarchive/internal/application/workdiagnostics"
	"videoarchive/internal/application/workerclient"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/asset"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/intake"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/resource"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
)

type recordingCommandRunner struct {
	mu    sync.Mutex
	calls [][]string
}

func (runner *recordingCommandRunner) Run(_ context.Context, executable string, arguments ...string) error {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.calls = append(runner.calls, append([]string{executable}, arguments...))
	return nil
}

func (runner *recordingCommandRunner) Calls() [][]string {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	result := make([][]string, len(runner.calls))
	for index := range runner.calls {
		result[index] = append([]string(nil), runner.calls[index]...)
	}
	return result
}

// Requirement: CTL-002
func TestControlRuntimeBootstrapsPersistentStorageAndHTTPAcrossRestart(t *testing.T) {
	directory := t.TempDir()
	configurationPath := filepath.Join(directory, "config.yaml")
	valid := `schema_version: 1
database: {path: data/control.db}
server:
  listen_address: 127.0.0.1:0
  allowed_cidrs: [127.0.0.0/8]
  allowed_origins: [http://127.0.0.1:8080]
  trusted_proxies: []
security: {auth_mode: disabled, mcp_listen_address: 127.0.0.1:8081}
features: {}
provider_accounts: []
workers:
  heartbeat_ttl: 30s
  definitions:
    - id: worker-main
      enabled: true
      credential_ref: wincred://worker/main
      processing_kinds: [INSPECT, RENDER]
      allowed_storage_ids: [scratch-main]
storage:
  - id: scratch-main
    role: SCRATCH
    physical_device_id: drive-1
    partition_id: partition-1
    volume_id: volume-1
    root: storage/scratch
`
	if err := os.WriteFile(configurationPath, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := bootconfig.Load(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		runtime, err := controlruntime.Open(context.Background(), configuration, "v1-test")
		if err != nil {
			t.Fatalf("runtime boot %d failed: %v", attempt, err)
		}
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		request.RemoteAddr = "127.0.0.1:45000"
		runtime.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"version":"v1-test"`) {
			t.Fatalf("runtime health response=%d %s", recorder.Code, recorder.Body.String())
		}
		if err := runtime.Close(); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := sqlitestorage.Open(configuration.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	access, err := repository.ResolveAccess(context.Background(), "scratch-main")
	repository.Close()
	if err != nil || access.WorkspaceDriveID != "volume-1" || access.PhysicalDeviceID != "drive-1" || access.Role != vocabulary.StorageScratch || access.Root != "storage/scratch" {
		t.Fatalf("boot storage topology was not persisted: access=%+v err=%v", access, err)
	}

	conflictPath := filepath.Join(directory, "conflict.yaml")
	conflict := strings.Replace(valid, "root: storage/scratch", "root: storage/other", 1)
	if err := os.WriteFile(conflictPath, []byte(conflict), 0o600); err != nil {
		t.Fatal(err)
	}
	conflictingConfiguration, err := bootconfig.Load(conflictPath)
	if err != nil {
		t.Fatal(err)
	}
	conflictingConfiguration.DatabasePath = configuration.DatabasePath
	if _, err := controlruntime.Open(context.Background(), conflictingConfiguration, "v1-test"); !errors.Is(err, sqlitestorage.ErrTopologyConflict) {
		t.Fatalf("conflicting boot topology was accepted: %v", err)
	}
}

// Requirement: APP-006
func TestArchiveRunStartQueuesWorkerInspectionAndCreatesSelectedFolderJobsOnce(t *testing.T) {
	directory := t.TempDir()
	archiveRoot := filepath.Join(directory, "archive")
	if err := os.MkdirAll(archiveRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	configurationPath := filepath.Join(directory, "control.yaml")
	payload := fmt.Sprintf(`schema_version: 1
database: {path: data/control.db}
server:
  listen_address: 127.0.0.1:0
  allowed_cidrs: [127.0.0.0/8]
  allowed_origins: [http://127.0.0.1:8080]
  trusted_proxies: []
security: {auth_mode: disabled, mcp_listen_address: 127.0.0.1:0}
features: {}
provider_accounts: []
workers:
  heartbeat_ttl: 30s
  definitions:
    - id: worker-main
      enabled: true
      credential_ref: wincred://worker/main
      processing_kinds: [INSPECT, RENDER, QC, ORGANIZE]
      allowed_storage_ids: [archive-main]
storage:
  - id: archive-main
    role: ARCHIVE_PRIMARY
    physical_device_id: archive-drive
    partition_id: archive-partition
    volume_id: archive-volume
    root: %s
`, archiveRoot)
	if err := os.WriteFile(configurationPath, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := bootconfig.Load(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("service-secret"), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	heartbeat := `{"worker_id":"worker-main","snapshot_id":1,"observed_at":"2026-11-04T03:00:00Z","cpu_average_5m":2,"gpu_average_5m":0,"idle_stable_millis":60000,"available_memory_bytes":34359738368,"available_vram_bytes":8589934592,"scratch_free_bytes":536870912000,"heavy_running":0,"light_running":0,"busy_physical_device_ids":[],"accepting_work":true,"accessible_storage_ids":["source-attached","source-attached-archive","archive-main"],"attached_volumes":[{"physical_device_id":"source-device","volume_id":"source-volume","storage_id":"source-attached","archive_storage_id":"source-attached-archive","model":"TEST DISK","label":"10GB 선택","file_system":"NTFS","capacity_bytes":2000000000000,"free_bytes":1000000000000}],"storage_observations":[{"storage_id":"source-attached","connected":true,"access_kind":"ATTACHED_VOLUME","discovery_state":"READY","capacity_bytes":2000000000000,"free_bytes":1000000000000,"discovery_revision":41,"folders":[{"folder":"event-a","video_count":1,"input_bytes":10000000000},{"folder":"event-b","video_count":2,"input_bytes":20000000000}]},{"storage_id":"archive-main","connected":true,"access_kind":"LOCAL","discovery_state":"READY","capacity_bytes":500000000000,"free_bytes":300000000000,"discovery_revision":1,"folders":[]}]}`
	if response := workerHeartbeatRequest(runtime.Handler(), heartbeat, "service-secret"); response.Code != http.StatusOK {
		t.Fatalf("dynamic Worker inventory returned %d: %s", response.Code, response.Body.String())
	}
	cookie, csrfToken := runtimeSession(t, runtime.Handler())
	requestBody := `{"archive_run_id":"RUN-REMOTE-1","idempotency_key":"remote:start:1","session_id":"SESSION-REMOTE-1","source_volume_id":"source-volume","expected_candidate_revision":1,"archive_storage_id":"archive-main","input_revision":41,"folder_ids":["event-a"]}`
	created := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archive-intake/runs", requestBody, cookie, csrfToken, "http://127.0.0.1:8080")
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"folder_count":1`) {
		t.Fatalf("remote Archive Run start returned %d: %s", created.Code, created.Body.String())
	}
	pendingReplay := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archive-intake/runs", requestBody, cookie, csrfToken, "http://127.0.0.1:8080")
	if pendingReplay.Code != http.StatusOK || !strings.Contains(pendingReplay.Body.String(), `"replayed":true`) {
		t.Fatalf("pending Archive Run replay returned %d: %s", pendingReplay.Code, pendingReplay.Body.String())
	}
	wrongKey := strings.Replace(requestBody, `"idempotency_key":"remote:start:1"`, `"idempotency_key":"remote:start:other"`, 1)
	if conflict := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archive-intake/runs", wrongKey, cookie, csrfToken, "http://127.0.0.1:8080"); conflict.Code != http.StatusConflict {
		t.Fatalf("same ArchiveRun ID with another idempotency key was replayed: %d %s", conflict.Code, conflict.Body.String())
	}
	pending := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/archive-intake", "", nil, "", "")
	if pending.Code != http.StatusOK || !strings.Contains(pending.Body.String(), `"archive_run_id":"RUN-REMOTE-1"`) ||
		!strings.Contains(pending.Body.String(), `"archive_run_state":"STAGE_ONE_RUNNING"`) ||
		!strings.Contains(pending.Body.String(), `"folder_count":1`) || !strings.Contains(pending.Body.String(), `"discovered_folder_count":2`) {
		t.Fatalf("fixed pending scope was not projected: %d %s", pending.Code, pending.Body.String())
	}
	claimResponse := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/claim", "", "service-secret")
	if claimResponse.Code != http.StatusOK || strings.Contains(claimResponse.Body.String(), archiveRoot) {
		t.Fatalf("Worker inspection claim returned %d: %s", claimResponse.Code, claimResponse.Body.String())
	}
	var claim struct {
		Data workerclient.Assignment `json:"data"`
	}
	if err := json.Unmarshal(claimResponse.Body.Bytes(), &claim); err != nil || claim.Data.ExecutionSpec.Operation != execution.OperationInspectMedia ||
		len(claim.Data.ExecutionSpec.Inputs) != 0 || len(claim.Data.ExecutionSpec.Parameters.FolderIDs) != 1 || claim.Data.ExecutionSpec.Parameters.FolderIDs[0] != "event-a" {
		t.Fatalf("invalid remote inspection assignment: %+v err=%v", claim.Data, err)
	}
	checksum := strings.Repeat("a", 64)
	identity := []string{claim.Data.ExecutionSpec.Parameters.ArchiveIntakeSessionID, "41", "event-a/clip.mp4"}
	observation, err := intake.NewInputFileObservation(intake.InputFileObservation{
		ID: runtimeStableID("OBSERVATION", identity...), StorageID: "source-attached", WorkspaceDriveID: "source-volume",
		RelativePath: "event-a/clip.mp4", OriginalName: "clip.mp4", SizeBytes: 1_024, Checksum: checksum,
		ObservedBy: "worker-main", ObservedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	original, err := asset.RegisterOriginalAsset(
		runtimeStableID("ASSET", identity...), observation, claim.Data.ExecutionSpec.Parameters.RequestedBy, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := mediaanalysis.NewMediaInspection(mediaanalysis.MediaInspection{
		ID:              runtimeStableID("INSPECTION", append([]string{"RUN-REMOTE-1"}, identity...)...),
		OriginalAssetID: original.ID, OriginalChecksum: checksum, Containers: []string{"mp4"},
		DurationMillis: 60_000, SizeBytes: 1_024, BitRate: 6_000_000,
		VideoStreams: []mediaanalysis.VideoStream{{
			Index: 0, Codec: "h264", Width: 1920, Height: 1080,
			FrameRate: mediaanalysis.Rational{Numerator: 30, Denominator: 1}, BitRate: 6_000_000,
		}}, ProcessorVersion: strings.Repeat("ffprobe-test-v1-", 5_000), ObservedBy: "worker-main", InspectedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	target := claim.Data.ExecutionSpec.Outputs[0]
	output := execution.StepOutput{ID: target.ID, Kind: target.Kind, StorageID: target.StorageID, RelativePath: target.RelativePath, Checksum: strings.Repeat("b", 64)}
	record, err := execution.NewStepOutputRecordWithInspections(
		claim.Data.ExecutionSpec, claim.Data.FencingToken, "worker-main", now.Add(time.Second),
		[]execution.StepOutput{output}, []string{"worker://inspection/verified"},
		[]execution.InspectedMedia{{Folder: "event-a", Observation: observation, Original: original, Inspection: inspection}},
	)
	if err != nil {
		t.Fatal(err)
	}
	resultPayload, err := json.Marshal(map[string]any{
		"job_id": claim.Data.JobID, "step_id": claim.Data.StepID, "fencing_token": claim.Data.FencingToken,
		"success": true, "failure_class": "", "wait_reason": "", "error_class": "",
		"log_reference": "worker://inspection/completed", "output_record": record,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resultPayload) <= 64<<10 {
		t.Fatalf("large Worker inspection result fixture is too small: %d", len(resultPayload))
	}
	settled := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/result", string(resultPayload), "service-secret")
	if settled.Code != http.StatusOK {
		detail := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs/"+claim.Data.JobID, "", nil, "", "")
		workspace := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/archive-intake", "", nil, "", "")
		t.Fatalf("Worker inspection result returned %d: %s job=%s workspace=%s", settled.Code, settled.Body.String(), detail.Body.String(), workspace.Body.String())
	}
	jobs := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs", "", nil, "", "")
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) &&
		strings.Count(jobs.Body.String(), `"template_name":"archive-intake-as-is"`) != 1; {
		time.Sleep(20 * time.Millisecond)
		jobs = runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs", "", nil, "", "")
	}
	if jobs.Code != http.StatusOK || strings.Count(jobs.Body.String(), `"template_name":"archive-intake-inspection"`) != 1 ||
		strings.Count(jobs.Body.String(), `"template_name":"archive-intake-as-is"`) != 1 || strings.Contains(jobs.Body.String(), "event-b") {
		t.Fatalf("selected Folder Job was not created exactly once: %d %s", jobs.Code, jobs.Body.String())
	}
	replayed := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archive-intake/runs", requestBody, cookie, csrfToken, "http://127.0.0.1:8080")
	if replayed.Code != http.StatusOK || !strings.Contains(replayed.Body.String(), `"replayed":true`) {
		t.Fatalf("Archive Run replay returned %d: %s", replayed.Code, replayed.Body.String())
	}
	invalid := strings.Replace(requestBody, `"archive_run_id":"RUN-REMOTE-1"`, `"archive_run_id":"RUN-REMOTE-BAD"`, 1)
	invalid = strings.Replace(invalid, `"idempotency_key":"remote:start:1"`, `"idempotency_key":"remote:start:bad"`, 1)
	invalid = strings.Replace(invalid, `"input_revision":41`, `"input_revision":40`, 1)
	rejected := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archive-intake/runs", invalid, cookie, csrfToken, "http://127.0.0.1:8080")
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("stale folder revision returned %d: %s", rejected.Code, rejected.Body.String())
	}
	after := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs", "", nil, "", "")
	if strings.Count(after.Body.String(), `"template_name":"archive-intake-inspection"`) != 1 {
		t.Fatalf("invalid start changed the Job queue: %s", after.Body.String())
	}
}

func runtimeStableID(prefix string, values ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return prefix + "-" + hex.EncodeToString(digest[:12])
}

// Requirement: DSH-006
func TestControlRuntimeStartsArchiveOrganizationFromRegisteredSourceStorage(t *testing.T) {
	directory := t.TempDir()
	sourceRoot := filepath.Join(directory, "source")
	workingRoot := filepath.Join(directory, "working")
	if err := os.MkdirAll(filepath.Join(sourceRoot, "recordings"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workingRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "recordings", "master.mov"), []byte("immutable-source-fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	configurationPath := filepath.Join(directory, "control.yaml")
	payload := fmt.Sprintf(`schema_version: 1
database: {path: data/control.db}
server:
  listen_address: 127.0.0.1:0
  allowed_cidrs: [127.0.0.0/8]
  allowed_origins: [http://127.0.0.1:8080]
  trusted_proxies: []
security: {auth_mode: disabled, mcp_listen_address: 127.0.0.1:0}
features: {}
provider_accounts: []
workers:
  heartbeat_ttl: 30s
  definitions:
    - id: worker-main
      enabled: true
      credential_ref: wincred://worker/main
      processing_kinds: [INSPECT, RENDER, AUDIO_EXTRACT, STT, QC, ORGANIZE]
      allowed_storage_ids: [source-main, archive-main]
storage:
  - id: source-main
    role: SOURCE_MEDIA
    physical_device_id: source-drive
    partition_id: source-partition
    volume_id: source-volume
    root: %s
  - id: archive-main
    role: ARCHIVE_PRIMARY
    physical_device_id: archive-drive
    partition_id: archive-partition
    volume_id: archive-volume
    root: %s
archive_organization:
  ffprobe:
    executable: %s
    version: ffprobe-fixture-v1
`, sourceRoot, workingRoot, executable)
	if err := os.WriteFile(configurationPath, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := bootconfig.Load(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 30, 22, 0, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("service-secret"), Now: func() time.Time { return now },
		ArchiveIntakeAnalyzers: driveAnalyzerFactoryFixture{now: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	heartbeat := `{"worker_id":"worker-main","snapshot_id":1,"observed_at":"2026-10-30T22:00:00Z","cpu_average_5m":2,"gpu_average_5m":0,"idle_stable_millis":60000,"available_memory_bytes":34359738368,"available_vram_bytes":8589934592,"scratch_free_bytes":536870912000,"heavy_running":0,"light_running":0,"busy_physical_device_ids":[],"accepting_work":true,"accessible_storage_ids":["source-main","archive-main"]}`
	if response := workerHeartbeatRequest(runtime.Handler(), heartbeat, "service-secret"); response.Code != http.StatusOK {
		t.Fatalf("Worker heartbeat before Archive Run start returned %d: %s", response.Code, response.Body.String())
	}
	cookie, csrfToken := runtimeSession(t, runtime.Handler())
	origin := "http://127.0.0.1:8080"
	requestBody := `{"archive_run_id":"RUN-DRIVE-1","idempotency_key":"drive:start:1","session_id":"SESSION-DRIVE-1","source_volume_id":"source-volume","expected_candidate_revision":1,"archive_storage_id":"archive-main","input_revision":1,"folder_ids":["recordings"]}`
	created := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archive-intake/runs", requestBody, cookie, csrfToken, origin)
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"folder_count":1`) ||
		strings.Contains(created.Body.String(), executable) {
		t.Fatalf("Archive Run start returned %d: %s", created.Code, created.Body.String())
	}
	projection := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/archive-intake", "", nil, "", "")
	if projection.Code != http.StatusOK || !strings.Contains(projection.Body.String(), `"folder":"recordings"`) ||
		!strings.Contains(projection.Body.String(), `"disposition":"RENDER_RECORDINGS"`) ||
		!strings.Contains(projection.Body.String(), `"current_step_id":"render"`) {
		t.Fatalf("Archive Organization projection is incomplete: %d %s", projection.Code, projection.Body.String())
	}
	replayed := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archive-intake/runs", requestBody, cookie, csrfToken, origin)
	if replayed.Code != http.StatusOK || !strings.Contains(replayed.Body.String(), `"replayed":true`) {
		t.Fatalf("Archive Run replay returned %d: %s", replayed.Code, replayed.Body.String())
	}
	invalidRevision := strings.Replace(requestBody, `"expected_candidate_revision":1`, `"expected_candidate_revision":2`, 1)
	bad := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archive-intake/runs", invalidRevision, cookie, csrfToken, origin)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("stale candidate revision returned %d: %s", bad.Code, bad.Body.String())
	}
	jobs := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs", "", nil, "", "")
	if jobs.Code != http.StatusOK || strings.Count(jobs.Body.String(), `"template_name":"archive-intake-render-recordings"`) != 1 {
		t.Fatalf("Archive Run start did not create exactly one Folder Job: %d %s", jobs.Code, jobs.Body.String())
	}
	store, err := sqlitecommand.Open(configuration.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := store.ListSnapshots()
	store.Close()
	if err != nil || len(snapshots) != 2 {
		t.Fatalf("ArchiveRun and Folder Job snapshots=%d err=%v", len(snapshots), err)
	}
	var state job.State
	for _, snapshot := range snapshots {
		decoded, decodeErr := job.Decode(snapshot)
		if decodeErr == nil {
			state = decoded
		}
	}
	if state.ID == "" {
		t.Fatal("Archive Organization Folder Job snapshot is missing")
	}
	for _, step := range state.Steps {
		if step.ExecutorKind == vocabulary.ExecutorWorker && step.ExecutionPriority != resource.Immediate {
			t.Fatalf("user-started Worker Step lost IMMEDIATE priority: %+v", step)
		}
	}
	heartbeat = strings.Replace(heartbeat, `"snapshot_id":1`, `"snapshot_id":2`, 1)
	if response := workerHeartbeatRequest(runtime.Handler(), heartbeat, "service-secret"); response.Code != http.StatusOK {
		t.Fatalf("short-idle Worker heartbeat/dispatch returned %d: %s", response.Code, response.Body.String())
	}
	claimed := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/claim", "", "service-secret")
	if claimed.Code != http.StatusOK || !strings.Contains(claimed.Body.String(), `"step_id":"render"`) {
		t.Fatalf("IMMEDIATE render was not claimable before five idle minutes: %d %s", claimed.Code, claimed.Body.String())
	}
	stageOneSteps := []string{settleClaimedFixtureStep(t, runtime.Handler(), configuration.DatabasePath, claimed)}
	for len(stageOneSteps) < 4 {
		claim := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/claim", "", "service-secret")
		if claim.Code != http.StatusOK {
			t.Fatalf("claim stage one step %d returned %d: %s", len(stageOneSteps)+1, claim.Code, claim.Body.String())
		}
		stageOneSteps = append(stageOneSteps, settleClaimedFixtureStep(t, runtime.Handler(), configuration.DatabasePath, claim))
	}
	if strings.Join(stageOneSteps, ",") != "render,qc,organize,archive_catalog" {
		t.Fatalf("stage one was not split from transcription: %v", stageOneSteps)
	}
	stt := runtimeRequest(
		runtime.Handler(), http.MethodPost, "/api/v1/archive-runs/RUN-DRIVE-1/transcription",
		`{"idempotency_key":"drive:stt:1","expected_archive_run_revision":1}`, cookie, csrfToken, origin,
	)
	if stt.Code != http.StatusCreated || !strings.Contains(stt.Body.String(), `"eligible_count":1`) ||
		!strings.Contains(stt.Body.String(), `"hold_count":0`) {
		t.Fatalf("separate stage two start returned %d: %s", stt.Code, stt.Body.String())
	}
	heartbeat = strings.Replace(heartbeat, `"snapshot_id":2`, `"snapshot_id":3`, 1)
	if response := workerHeartbeatRequest(runtime.Handler(), heartbeat, "service-secret"); response.Code != http.StatusOK {
		t.Fatalf("stage two dispatch heartbeat returned %d: %s", response.Code, response.Body.String())
	}
	stageTwoSteps := make([]string, 0, 6)
	for len(stageTwoSteps) < 6 {
		claim := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/claim", "", "service-secret")
		if claim.Code != http.StatusOK {
			t.Fatalf("claim stage two step %d returned %d: %s", len(stageTwoSteps)+1, claim.Code, claim.Body.String())
		}
		stageTwoSteps = append(stageTwoSteps, settleClaimedFixtureStep(t, runtime.Handler(), configuration.DatabasePath, claim))
	}
	if strings.Join(stageTwoSteps, ",") != "audio,transcript,subtitle,transcript_text,archive_package_revision,archive_catalog" {
		t.Fatalf("stage two order is invalid: %v", stageTwoSteps)
	}
	finalJobs := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs", "", nil, "", "")
	if finalJobs.Code != http.StatusOK ||
		strings.Count(finalJobs.Body.String(), `"template_name":"archive-intake-render-recordings"`) != 1 ||
		strings.Count(finalJobs.Body.String(), `"template_name":"archive-transcription"`) != 1 {
		t.Fatalf("separate stage Jobs are missing from the projection: %d %s", finalJobs.Code, finalJobs.Body.String())
	}
}

type driveAnalyzerFactoryFixture struct{ now time.Time }

func (factory driveAnalyzerFactoryFixture) New(root *archive.ReadOnlyStorageRoot) (archiveorganization.MediaAnalyzer, error) {
	return driveAnalyzerFixture{root: root, now: factory.now}, nil
}

type driveAnalyzerFixture struct {
	root *archive.ReadOnlyStorageRoot
	now  time.Time
}

func (analyzer driveAnalyzerFixture) Inspect(_ context.Context, inspectionID string, original asset.OriginalAsset, relativePath, workerID string) (mediaanalysis.MediaInspection, error) {
	size, checksum, err := analyzer.root.FileSHA256(relativePath)
	if err != nil || size != original.SizeBytes || checksum != original.Checksum {
		return mediaanalysis.MediaInspection{}, errors.New("fixture source changed")
	}
	return mediaanalysis.NewMediaInspection(mediaanalysis.MediaInspection{
		ID: inspectionID, OriginalAssetID: original.ID, OriginalChecksum: checksum, Containers: []string{"mov"},
		DurationMillis: 600_000, SizeBytes: size, BitRate: 42_000_000,
		VideoStreams: []mediaanalysis.VideoStream{{
			Index: 0, Codec: "prores", Width: 3840, Height: 2160,
			FrameRate: mediaanalysis.Rational{Numerator: 30, Denominator: 1}, BitRate: 40_000_000,
		}},
		AudioStreams:     []mediaanalysis.AudioStream{{Index: 1, Codec: "pcm_s24le", SampleRateHz: 48_000, Channels: 2}},
		ProcessorVersion: "ffprobe-fixture-v1", ObservedBy: workerID, InspectedAt: analyzer.now,
	})
}

// Requirement: CTL-003
func TestBrowserCommandCreatesPersistentJobWithCSRFAndIdempotency(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	runtime, err := controlruntime.Open(context.Background(), configuration, "v1-test")
	if err != nil {
		t.Fatal(err)
	}

	create := `{"job_id":"JOB-HTTP-1","command_id":"COMMAND-CREATE-1","idempotency_key":"JOB-HTTP-1:create","reason":"operator created occasion job","template_name":"occasion-recording","target_title":"가을 행사","expected_revision":0}`
	denied := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs", create, nil, "", "")
	if denied.Code != http.StatusForbidden {
		t.Fatalf("mutation without browser proof returned %d: %s", denied.Code, denied.Body.String())
	}
	cookie, token := runtimeSession(t, runtime.Handler())
	secondTab := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	secondTab.RemoteAddr = "127.0.0.1:45000"
	secondTab.AddCookie(cookie)
	secondSession := httptest.NewRecorder()
	runtime.Handler().ServeHTTP(secondSession, secondTab)
	if secondSession.Code != http.StatusOK || !strings.Contains(secondSession.Body.String(), token) || len(secondSession.Result().Cookies()) != 0 {
		t.Fatalf("second tab rotated the shared CSRF session: %d %s cookies=%v", secondSession.Code, secondSession.Body.String(), secondSession.Result().Cookies())
	}
	created := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs", create, cookie, token, "http://127.0.0.1:8080")
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"revision":1`) {
		t.Fatalf("create returned %d: %s", created.Code, created.Body.String())
	}
	replayPayload := strings.Replace(create, "COMMAND-CREATE-1", "COMMAND-CREATE-2", 1)
	replayed := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs", replayPayload, cookie, token, "http://127.0.0.1:8080")
	if replayed.Code != http.StatusCreated || !strings.Contains(replayed.Body.String(), `"command_id":"COMMAND-CREATE-1"`) {
		t.Fatalf("idempotent replay returned %d: %s", replayed.Code, replayed.Body.String())
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	ledger, err := sql.Open("sqlite", configuration.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"aggregate_states": 1, "command_records": 2, "domain_events": 1, "idempotency_receipts": 1} {
		var count int
		if err := ledger.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE "+ledgerTargetColumn(table)+" = ?", ledgerTargetValue(table)).Scan(&count); err != nil {
			ledger.Close()
			t.Fatal(err)
		}
		if count != want {
			ledger.Close()
			t.Fatalf("%s count=%d want=%d", table, count, want)
		}
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := controlruntime.Open(context.Background(), configuration, "v1-test")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	jobView := runtimeRequest(restarted.Handler(), http.MethodGet, "/api/v1/jobs/JOB-HTTP-1", "", nil, "", "")
	if jobView.Code != http.StatusOK || !strings.Contains(jobView.Body.String(), `"template_name":"occasion-recording"`) ||
		!strings.Contains(jobView.Body.String(), `"target_title":"가을 행사"`) || !strings.Contains(jobView.Body.String(), `"step_state":"QUEUED"`) {
		t.Fatalf("persistent job view returned %d: %s", jobView.Code, jobView.Body.String())
	}
}

// Requirement: DSH-032
func TestArchiveWorkspaceCreatesPersistentJobAndNavigatesToItsDetail(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	server := httptest.NewUnstartedServer(nil)
	configuration.AllowedOrigins = []string{"http://" + server.Listener.Addr().String()}
	runtime, err := controlruntime.Open(context.Background(), configuration, "v1-test")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	server.Config.Handler = runtime.Handler()
	server.Start()
	defer server.Close()

	python := os.Getenv("ARCHIVE_WORKSPACE_GOLDEN_PYTHON")
	if python == "" {
		python = "python3"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	check := exec.CommandContext(ctx, python, filepath.Join("testdata", "check_archive_workspace_job_create.py"), server.URL)
	if output, runErr := check.CombinedOutput(); runErr != nil {
		t.Fatalf("check Archive Workspace job creation: %v\n%s\nrun `make visual-deps` before the test", runErr, output)
	}
}

// Requirement: CTL-007
func TestControlRebuildsMissingJobProjectionFromAuthoritativeLedgerAtStartup(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	runtime, err := controlruntime.Open(context.Background(), configuration, "v1-test")
	if err != nil {
		t.Fatal(err)
	}
	cookie, token := runtimeSession(t, runtime.Handler())
	create := `{"job_id":"JOB-REBUILD-1","command_id":"COMMAND-REBUILD-1","idempotency_key":"JOB-REBUILD-1:create","reason":"create rebuild fixture","template_name":"occasion-recording","target_title":"재구축 작업","expected_revision":0}`
	created := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs", create, cookie, token, "http://127.0.0.1:8080")
	if created.Code != http.StatusCreated {
		runtime.Close()
		t.Fatalf("create rebuild fixture returned %d: %s", created.Code, created.Body.String())
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := sql.Open("sqlite", configuration.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`DELETE FROM archive_workspace_job_projections WHERE job_id = ?`, "JOB-REBUILD-1"); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO aggregate_states(target_id, revision, state) VALUES (?, ?, ?)`,
		"ARCHIVE-RUN-NOT-A-JOB", 1, []byte(`{"id":"ARCHIVE-RUN-NOT-A-JOB","revision":1,"worker_id":"WORKER-1"}`)); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := controlruntime.Open(context.Background(), configuration, "v1-test")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	detail := runtimeRequest(restarted.Handler(), http.MethodGet, "/api/v1/jobs/JOB-REBUILD-1", "", nil, "", "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"target_title":"재구축 작업"`) ||
		!strings.Contains(detail.Body.String(), `"step_state":"QUEUED"`) {
		t.Fatalf("rebuilt Job projection returned %d: %s", detail.Code, detail.Body.String())
	}
	inbox := runtimeRequest(restarted.Handler(), http.MethodGet, "/api/v1/jobs", "", nil, "", "")
	if inbox.Code != http.StatusOK || strings.Contains(inbox.Body.String(), "ARCHIVE-RUN-NOT-A-JOB") {
		t.Fatalf("non-Job aggregate entered the Job projection: %d %s", inbox.Code, inbox.Body.String())
	}
}

// Requirement: CTL-004
func TestBrowserCancelCommandUsesExpectedRevisionAndWorkflowTransition(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	runtime, err := controlruntime.Open(context.Background(), configuration, "v1-test")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, token := runtimeSession(t, runtime.Handler())
	origin := "http://127.0.0.1:8080"
	create := `{"job_id":"JOB-CANCEL-1","command_id":"COMMAND-CREATE","idempotency_key":"JOB-CANCEL-1:create","reason":"create cancellable job","template_name":"final-edit","target_title":"최종 편집본","expected_revision":0}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs", create, cookie, token, origin); response.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %s", response.Code, response.Body.String())
	}
	cancel := `{"command_id":"COMMAND-CANCEL","idempotency_key":"JOB-CANCEL-1:cancel","reason":"operator cancelled queued job","expected_revision":1}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs/JOB-CANCEL-1/cancel", cancel, cookie, token, origin); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"revision":2`) {
		t.Fatalf("cancel returned %d: %s", response.Code, response.Body.String())
	}
	stale := strings.Replace(cancel, "JOB-CANCEL-1:cancel", "JOB-CANCEL-1:stale-cancel", 1)
	stale = strings.Replace(stale, "COMMAND-CANCEL", "COMMAND-STALE-CANCEL", 1)
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs/JOB-CANCEL-1/cancel", stale, cookie, token, origin); response.Code != http.StatusConflict {
		t.Fatalf("stale cancel returned %d: %s", response.Code, response.Body.String())
	}
	jobView := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs/JOB-CANCEL-1", "", nil, "", "")
	if jobView.Code != http.StatusOK || !strings.Contains(jobView.Body.String(), `"step_state":"CANCELLED"`) ||
		!strings.Contains(jobView.Body.String(), `"revision":2`) {
		t.Fatalf("cancelled projection returned %d: %s", jobView.Code, jobView.Body.String())
	}
}

// Requirement: CTL-006
func TestArchivistAddAPIAcceptsOnlyNameRegisteredHostAndDetectedCLI(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	runtime, err := controlruntime.Open(context.Background(), configuration, "v1-test")
	if err != nil {
		t.Fatal(err)
	}
	cookie, token := runtimeSession(t, runtime.Handler())
	origin := "http://127.0.0.1:8080"
	payload := `{"session_id":"SESSION-HTTP-1","name":"Archive Archivist","archivist_host_id":"archivist-windows","cli_id":"codex"}`
	if denied := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions", payload, nil, "", ""); denied.Code != http.StatusForbidden {
		t.Fatalf("session mutation without browser proof returned %d: %s", denied.Code, denied.Body.String())
	}
	created := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions", payload, cookie, token, origin)
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"session_id":"SESSION-HTTP-1"`) ||
		!strings.Contains(created.Body.String(), `"name":"Archive Archivist"`) ||
		!strings.Contains(created.Body.String(), `"backend":"EMBEDDED_CORE"`) ||
		!strings.Contains(created.Body.String(), `"workspace_root_id":"video-project"`) {
		t.Fatalf("session create returned %d: %s", created.Code, created.Body.String())
	}
	unsafe := strings.Replace(payload, `"cli_id":"codex"`, `"cli_id":"shell"`, 1)
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions", unsafe, cookie, token, origin); response.Code != http.StatusBadRequest {
		t.Fatalf("unregistered runtime binding returned %d: %s", response.Code, response.Body.String())
	}
	unregistered := strings.Replace(payload, `"archivist_host_id":"archivist-windows"`, `"archivist_host_id":"archivist-unknown"`, 1)
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions", unregistered, cookie, token, origin); response.Code != http.StatusBadRequest {
		t.Fatalf("unregistered Host returned %d: %s", response.Code, response.Body.String())
	}
	injected := strings.TrimSuffix(payload, "}") + `,"backend":"EMBEDDED_CORE","workspace_root_id":"other","job_id":"JOB-INJECTED"}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions", injected, cookie, token, origin); response.Code != http.StatusBadRequest {
		t.Fatalf("runtime-owned binding input returned %d: %s", response.Code, response.Body.String())
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := controlruntime.Open(context.Background(), configuration, "v1-test")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	view := runtimeRequest(restarted.Handler(), http.MethodGet, "/api/v1/archivist-sessions", "", nil, "", "")
	if view.Code != http.StatusOK || !strings.Contains(view.Body.String(), `"archivist_host_id":"archivist-windows"`) ||
		!strings.Contains(view.Body.String(), `"workspace_root_id":"video-project"`) || strings.Contains(view.Body.String(), `"control_mode"`) {
		t.Fatalf("durable pinned session response=%d %s", view.Code, view.Body.String())
	}
}

// Requirement: CTL-009
func TestControlStartsPinnedOfflineArchivistHostWithoutDuplicatingOnlineStart(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	runner := &recordingCommandRunner{}
	starter, err := windowstask.New(configuration.ArchivistHostTasks, runner)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 15, 4, 0, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("runtime-service-secret"), Now: func() time.Time { return now }, ArchivistHostStarter: starter,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, token := runtimeSession(t, runtime.Handler())
	origin := "http://127.0.0.1:8080"
	path := "/api/v1/archivist-hosts/archivist-windows/start"
	if denied := runtimeRequest(runtime.Handler(), http.MethodPost, path, `{}`, nil, "", ""); denied.Code != http.StatusForbidden {
		t.Fatalf("Host start without browser proof returned %d: %s", denied.Code, denied.Body.String())
	}
	started := runtimeRequest(runtime.Handler(), http.MethodPost, path, `{}`, cookie, token, origin)
	if started.Code != http.StatusAccepted || !strings.Contains(started.Body.String(), `"state":"CONNECTING"`) {
		t.Fatalf("offline Host start returned %d: %s", started.Code, started.Body.String())
	}
	if calls := runner.Calls(); len(calls) != 1 || strings.Join(calls[0], "|") != "schtasks.exe|/Run|/TN|Archive Archivist Host" {
		t.Fatalf("unexpected Host start calls: %v", calls)
	}
	if heartbeat := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/heartbeat", "", "runtime-service-secret"); heartbeat.Code != http.StatusOK {
		t.Fatalf("Host heartbeat returned %d: %s", heartbeat.Code, heartbeat.Body.String())
	}
	alreadyOnline := runtimeRequest(runtime.Handler(), http.MethodPost, path, `{}`, cookie, token, origin)
	if alreadyOnline.Code != http.StatusAccepted || !strings.Contains(alreadyOnline.Body.String(), `"state":"ONLINE"`) {
		t.Fatalf("online Host start returned %d: %s", alreadyOnline.Code, alreadyOnline.Body.String())
	}
	if calls := runner.Calls(); len(calls) != 1 {
		t.Fatalf("online Host was started again: %v", calls)
	}
}

// Requirement: CTL-008
func TestArchivistRemoveAPIRetiresIdleSessionWithoutDeletingHistory(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	now := time.Date(2026, 11, 11, 3, 30, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("runtime-service-secret"), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie, token := runtimeSession(t, runtime.Handler())
	origin := "http://127.0.0.1:8080"
	createSession := func(id string) {
		t.Helper()
		payload := fmt.Sprintf(`{"session_id":%q,"name":%q,"archivist_host_id":"archivist-windows","cli_id":"codex"}`, id, id)
		if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions", payload, cookie, token, origin); response.Code != http.StatusCreated {
			t.Fatalf("create %s returned %d: %s", id, response.Code, response.Body.String())
		}
	}
	createTurn := func(sessionID, turnID string) {
		t.Helper()
		payload := fmt.Sprintf(`{"turn_id":%q,"request_id":%q,"prompt":"상태 확인"}`, turnID, "REQUEST-"+turnID)
		if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions/"+sessionID+"/turns", payload, cookie, token, origin); response.Code != http.StatusCreated {
			t.Fatalf("create %s returned %d: %s", turnID, response.Code, response.Body.String())
		}
	}
	createSession("SESSION-REMOVE-1")
	createTurn("SESSION-REMOVE-1", "TURN-REMOVE-1")
	if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/heartbeat", "", "runtime-service-secret"); response.Code != http.StatusOK {
		t.Fatalf("heartbeat returned %d: %s", response.Code, response.Body.String())
	}
	if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/turns/claim", "", "runtime-service-secret"); response.Code != http.StatusOK {
		t.Fatalf("claim returned %d: %s", response.Code, response.Body.String())
	}
	delivery := `{"fencing_token":1,"request_id":"REQUEST-TURN-REMOVE-1"}`
	if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/turns/TURN-REMOVE-1/delivery", delivery, "runtime-service-secret"); response.Code != http.StatusOK {
		t.Fatalf("delivery returned %d: %s", response.Code, response.Body.String())
	}
	for cursor, kind := range []string{"INPUT_ACCEPTED", "TURN_STARTED"} {
		event := fmt.Sprintf(`{"fencing_token":1,"cursor":%d,"request_id":"REQUEST-TURN-REMOVE-1","kind":%q}`, cursor+1, kind)
		if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/turns/TURN-REMOVE-1/events", event, "runtime-service-secret"); response.Code != http.StatusOK {
			t.Fatalf("%s returned %d: %s", kind, response.Code, response.Body.String())
		}
	}
	completed := `{"fencing_token":1,"cursor":3,"request_id":"REQUEST-TURN-REMOVE-1","kind":"TURN_COMPLETED","final_response":{"summary":"완료","result_ids":[]}}`
	if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/turns/TURN-REMOVE-1/events", completed, "runtime-service-secret"); response.Code != http.StatusOK {
		t.Fatalf("completion returned %d: %s", response.Code, response.Body.String())
	}
	removePath := "/api/v1/archivist-sessions/SESSION-REMOVE-1/remove"
	if denied := runtimeRequest(runtime.Handler(), http.MethodPost, removePath, `{}`, nil, "", ""); denied.Code != http.StatusForbidden {
		t.Fatalf("remove without browser proof returned %d: %s", denied.Code, denied.Body.String())
	}
	if removed := runtimeRequest(runtime.Handler(), http.MethodPost, removePath, `{}`, cookie, token, origin); removed.Code != http.StatusOK || !strings.Contains(removed.Body.String(), `"state":"REMOVED"`) {
		t.Fatalf("remove returned %d: %s", removed.Code, removed.Body.String())
	}
	view := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/archivist-sessions", "", nil, "", "")
	if strings.Contains(view.Body.String(), "SESSION-REMOVE-1") {
		t.Fatalf("retired Agent remained selectable: %s", view.Body.String())
	}
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions/SESSION-REMOVE-1/turns",
		`{"turn_id":"TURN-AFTER-REMOVE","request_id":"REQUEST-AFTER-REMOVE","prompt":"상태 확인"}`, cookie, token, origin); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "ARCHIVIST_SESSION_REMOVED") {
		t.Fatalf("retired Agent accepted a new Turn: %d %s", response.Code, response.Body.String())
	}
	ledger, err := sql.Open("sqlite", configuration.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	for table, identity := range map[string][2]string{
		"archivist_sessions": {"session_id", "SESSION-REMOVE-1"},
		"archivist_turns":    {"turn_id", "TURN-REMOVE-1"},
	} {
		var count int
		query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = ?", table, identity[0])
		if err := ledger.QueryRow(query, identity[1]).Scan(&count); err != nil || count != 1 {
			t.Fatalf("retirement deleted durable history: table=%s count=%d err=%v", table, count, err)
		}
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	createSession("SESSION-BUSY-1")
	createTurn("SESSION-BUSY-1", "TURN-BUSY-1")
	busyPath := "/api/v1/archivist-sessions/SESSION-BUSY-1/remove"
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, busyPath, `{}`, cookie, token, origin); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "ARCHIVIST_SESSION_BUSY") {
		t.Fatalf("unfinished Agent was removed: %d %s", response.Code, response.Body.String())
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := controlruntime.Open(context.Background(), configuration, "v1-test")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restartedView := runtimeRequest(restarted.Handler(), http.MethodGet, "/api/v1/archivist-sessions", "", nil, "", "")
	if strings.Contains(restartedView.Body.String(), "SESSION-REMOVE-1") || !strings.Contains(restartedView.Body.String(), "SESSION-BUSY-1") {
		t.Fatalf("retired projection was not durable: %s", restartedView.Body.String())
	}
}

// Requirement: CTL-005
func TestBrowserPersistsArchivistTurnBeforeRuntimeDeliveryAndReturnsDurableIDs(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	runtime, err := controlruntime.Open(context.Background(), configuration, "v1-test")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, token := runtimeSession(t, runtime.Handler())
	origin := "http://127.0.0.1:8080"
	session := `{"session_id":"SESSION-TURN-1","archivist_host_id":"archivist-windows","backend":"EMBEDDED_CORE","cli_id":"codex","workspace_root_id":"video-project","job_id":"JOB-TURN-1"}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions", session, cookie, token, origin); response.Code != http.StatusCreated {
		t.Fatalf("session create returned %d: %s", response.Code, response.Body.String())
	}
	turn := `{"turn_id":"TURN-HTTP-1","request_id":"REQUEST-HTTP-1","prompt":"현재 작업 상태를 확인"}`
	if denied := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions/SESSION-TURN-1/turns", turn, nil, "", ""); denied.Code != http.StatusForbidden {
		t.Fatalf("turn mutation without browser proof returned %d: %s", denied.Code, denied.Body.String())
	}
	created := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions/SESSION-TURN-1/turns", turn, cookie, token, origin)
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"turn_id":"TURN-HTTP-1"`) ||
		!strings.Contains(created.Body.String(), `"state":"QUEUED"`) {
		t.Fatalf("turn create returned %d: %s", created.Code, created.Body.String())
	}
	ledger, err := sql.Open("sqlite", configuration.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	var state string
	if err := ledger.QueryRow(`SELECT state FROM archivist_turns WHERE turn_id = ?`, "TURN-HTTP-1").Scan(&state); err != nil || state != "QUEUED" {
		t.Fatalf("turn was not durable before runtime delivery: state=%q err=%v", state, err)
	}
}

// Requirement: DSH-002
func TestArchivistWorkspaceProjectsSanitizedHostSessionAndTurnState(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("runtime-service-secret"), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/heartbeat", "", "runtime-service-secret"); response.Code != http.StatusOK {
		t.Fatalf("heartbeat returned %d: %s", response.Code, response.Body.String())
	}
	cookie, token := runtimeSession(t, runtime.Handler())
	origin := "http://127.0.0.1:8080"
	session := `{"session_id":"SESSION-VIEW-1","archivist_host_id":"archivist-windows","backend":"EMBEDDED_CORE","cli_id":"codex","workspace_root_id":"video-project","job_id":"JOB-VIEW-1"}`
	runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions", session, cookie, token, origin)
	turn := `{"turn_id":"TURN-VIEW-1","request_id":"REQUEST-VIEW-1","prompt":"raw terminal secret resume-token-1"}`
	runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions/SESSION-VIEW-1/turns", turn, cookie, token, origin)
	if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/turns/claim", "", "runtime-service-secret"); response.Code != http.StatusOK {
		t.Fatalf("claim returned %d: %s", response.Code, response.Body.String())
	}
	delivery := `{"fencing_token":1,"request_id":"REQUEST-VIEW-1"}`
	archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/turns/TURN-VIEW-1/delivery", delivery, "runtime-service-secret")
	for cursor, kind := range []string{"INPUT_ACCEPTED", "TURN_STARTED"} {
		event := fmt.Sprintf(`{"fencing_token":1,"cursor":%d,"request_id":"REQUEST-VIEW-1","kind":%q}`, cursor+1, kind)
		archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/turns/TURN-VIEW-1/events", event, "runtime-service-secret")
	}
	toolCall := `{"session_id":"SESSION-VIEW-1","turn_id":"TURN-VIEW-1","fencing_token":1,"tool_call_id":"CALL-VIEW-1","command_id":"COMMAND-VIEW-1","idempotency_key":"CALL-VIEW-1","input_digest":"sha256:view","tool":"archive.assets.search.v1","input":{"text":"none"}}`
	if response := mcpRequest(runtime.MCPHandler(), http.MethodPost, "/mcp/v1/call", toolCall, "runtime-service-secret"); response.Code != http.StatusOK {
		t.Fatalf("tool call returned %d: %s", response.Code, response.Body.String())
	}
	hosts := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/archivist-hosts", "", nil, "", "")
	sessions := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/archivist-sessions", "", nil, "", "")
	combined := hosts.Body.String() + sessions.Body.String()
	if hosts.Code != http.StatusOK || sessions.Code != http.StatusOK || !strings.Contains(combined, `"state":"ONLINE"`) ||
		!strings.Contains(combined, `"turn_id":"TURN-VIEW-1"`) || !strings.Contains(combined, `"tool_name":"archive.assets.search.v1"`) ||
		!strings.Contains(combined, `"state":"COMPLETED"`) || strings.Contains(combined, "runtime-service-secret") ||
		strings.Contains(combined, "wincred://") || strings.Contains(combined, "resume-token-1") || strings.Contains(combined, "raw terminal") {
		t.Fatalf("sanitized runtime projection failed: %s", combined)
	}
}

// Requirement: LRH-001
func TestArchivistHostAuthenticatesHeartbeatsBeforeClaimingItsPinnedTurn(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("runtime-service-secret"), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, token := runtimeSession(t, runtime.Handler())
	origin := "http://127.0.0.1:8080"
	session := `{"session_id":"SESSION-CLAIM-1","archivist_host_id":"archivist-windows","backend":"EMBEDDED_CORE","cli_id":"codex","workspace_root_id":"video-project","job_id":"JOB-CLAIM-1"}`
	runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions", session, cookie, token, origin)
	turn := `{"turn_id":"TURN-CLAIM-1","request_id":"REQUEST-CLAIM-1","prompt":"job state"}`
	runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions/SESSION-CLAIM-1/turns", turn, cookie, token, origin)
	if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/turns/claim", "", "runtime-service-secret"); response.Code != http.StatusConflict {
		t.Fatalf("claim before heartbeat returned %d: %s", response.Code, response.Body.String())
	}
	if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/heartbeat", "", "wrong-secret"); response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid heartbeat returned %d: %s", response.Code, response.Body.String())
	}
	if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/heartbeat", "", "runtime-service-secret"); response.Code != http.StatusOK {
		t.Fatalf("valid heartbeat returned %d: %s", response.Code, response.Body.String())
	}
	claimed := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/turns/claim", "", "runtime-service-secret")
	if claimed.Code != http.StatusOK || !strings.Contains(claimed.Body.String(), `"turn_id":"TURN-CLAIM-1"`) ||
		!strings.Contains(claimed.Body.String(), `"fencing_token":1`) {
		t.Fatalf("authenticated claim returned %d: %s", claimed.Code, claimed.Body.String())
	}
}

func TestControlMCPExposesOnlyAuthenticatedVersionedBusinessCapabilities(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("runtime-service-secret"), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	upstream := httptest.NewServer(runtime.MCPHandler())
	defer upstream.Close()
	if _, err := mcpproxy.New(mcpproxy.Configuration{
		ListenAddress: "0.0.0.0:8091", ControlURL: upstream.URL,
		ArchivistHostID: "archivist-windows", Credential: "runtime-service-secret",
	}); !errors.Is(err, mcpproxy.ErrInvalidConfiguration) {
		t.Fatalf("non-loopback MCP proxy listen was accepted: %v", err)
	}
	proxy, err := mcpproxy.New(mcpproxy.Configuration{
		ListenAddress: "127.0.0.1:8091", ControlURL: upstream.URL,
		ArchivistHostID: "archivist-windows", Credential: "runtime-service-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response := mcpRequest(runtime.MCPHandler(), http.MethodGet, "/mcp/v1/tools", "", "wrong-secret"); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated tool list returned %d: %s", response.Code, response.Body.String())
	}
	tools := mcpRequest(proxy, http.MethodGet, "/mcp/v1/tools", "", "local-cli-does-not-receive-service-secret")
	body := tools.Body.String()
	if tools.Code != http.StatusOK || !strings.Contains(body, "archive.jobs.get.v1") ||
		!strings.Contains(body, "archive.assets.search.v1") || !strings.Contains(body, "archive.jobs.create.v1") {
		t.Fatalf("MCP capability list returned %d: %s", tools.Code, body)
	}
	for _, forbidden := range []string{"shell", "terminal", "arbitrary_path", "run_command"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Fatalf("MCP exposed forbidden capability %q: %s", forbidden, body)
		}
	}
	setupRunningArchivistTurn(t, runtime, "SESSION-MCP-1", "TURN-MCP-1", "REQUEST-MCP-1")
	unsafe := `{"session_id":"SESSION-MCP-1","turn_id":"TURN-MCP-1","fencing_token":1,"tool_call_id":"CALL-UNSAFE","command_id":"COMMAND-UNSAFE","idempotency_key":"unsafe:1","input_digest":"sha256:unsafe","tool":"run_shell","input":{}}`
	response := mcpRequest(proxy, http.MethodPost, "/mcp/v1/call", unsafe, "local-cli-does-not-receive-service-secret")
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "MCP_CAPABILITY_NOT_FOUND") {
		t.Fatalf("unsafe MCP capability returned %d: %s", response.Code, response.Body.String())
	}
}

func TestArchivistDiagnosesAndDirectlyRecoversExpiredExecutionVertically(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	clock := time.Date(2026, 11, 17, 7, 0, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("service-secret"), Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, token := runtimeSession(t, runtime.Handler())
	createRunningWorkerJob(t, runtime.Handler(), configuration.DatabasePath, cookie, token, "JOB-DIRECT-RECOVERY", 1, clock)
	clock = clock.Add(2 * time.Minute)
	origin := "http://127.0.0.1:8080"
	session := `{"session_id":"SESSION-DIRECT-RECOVERY","name":"Recovery Archivist","archivist_host_id":"archivist-windows","cli_id":"codex"}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions", session, cookie, token, origin); response.Code != http.StatusCreated {
		t.Fatalf("session create returned %d: %s", response.Code, response.Body.String())
	}
	turn := `{"turn_id":"TURN-DIRECT-RECOVERY","request_id":"REQUEST-DIRECT-RECOVERY","prompt":"stalled work recovery","work_reference":{"kind":"JOB","work_id":"JOB-DIRECT-RECOVERY","revision":3}}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions/SESSION-DIRECT-RECOVERY/turns", turn, cookie, token, origin); response.Code != http.StatusCreated {
		t.Fatalf("turn create returned %d: %s", response.Code, response.Body.String())
	}
	if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/heartbeat", "", "service-secret"); response.Code != http.StatusOK {
		t.Fatalf("host heartbeat returned %d: %s", response.Code, response.Body.String())
	}
	if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/turns/claim", "", "service-secret"); response.Code != http.StatusOK {
		t.Fatalf("turn claim returned %d: %s", response.Code, response.Body.String())
	}
	delivery := `{"fencing_token":1,"request_id":"REQUEST-DIRECT-RECOVERY"}`
	if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/turns/TURN-DIRECT-RECOVERY/delivery", delivery, "service-secret"); response.Code != http.StatusOK {
		t.Fatalf("turn delivery returned %d: %s", response.Code, response.Body.String())
	}
	for cursor, kind := range []string{"INPUT_ACCEPTED", "TURN_STARTED"} {
		event := fmt.Sprintf(`{"fencing_token":1,"cursor":%d,"request_id":"REQUEST-DIRECT-RECOVERY","kind":%q}`, cursor+1, kind)
		if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/turns/TURN-DIRECT-RECOVERY/events", event, "service-secret"); response.Code != http.StatusOK {
			t.Fatalf("runtime event %s returned %d: %s", kind, response.Code, response.Body.String())
		}
	}
	diagnosticCall := `{"session_id":"SESSION-DIRECT-RECOVERY","turn_id":"TURN-DIRECT-RECOVERY","fencing_token":1,"tool_call_id":"CALL-DIRECT-DIAGNOSTIC","command_id":"COMMAND-DIRECT-DIAGNOSTIC","idempotency_key":"CALL-DIRECT-DIAGNOSTIC","input_digest":"sha256:direct-diagnostic","tool":"archive.work.diagnostics.get.v1","input":{}}`
	diagnosticResponse := mcpRequest(runtime.MCPHandler(), http.MethodPost, "/mcp/v1/call", diagnosticCall, "service-secret")
	var diagnostic struct {
		Data struct {
			Fingerprint     string                           `json:"fingerprint"`
			RecoveryActions []workdiagnostics.RecoveryAction `json:"recovery_actions"`
		} `json:"data"`
	}
	if diagnosticResponse.Code != http.StatusOK || json.Unmarshal(diagnosticResponse.Body.Bytes(), &diagnostic) != nil ||
		len(diagnostic.Data.RecoveryActions) != 1 {
		t.Fatalf("expired execution diagnostic did not expose one recovery: %d %s", diagnosticResponse.Code, diagnosticResponse.Body.String())
	}
	action := diagnostic.Data.RecoveryActions[0]
	input, _ := json.Marshal(map[string]any{
		"action_code": action.ActionCode, "job_id": action.JobID, "expected_job_revision": action.JobRevision,
		"step_id": action.StepID, "reservation_id": action.ReservationID, "fencing_token": action.FencingToken,
		"diagnostic_fingerprint": diagnostic.Data.Fingerprint, "basis": action.Basis,
		"reason": "recover the expired execution from current persisted evidence",
	})
	recoveryCall, _ := json.Marshal(map[string]any{
		"session_id": "SESSION-DIRECT-RECOVERY", "turn_id": "TURN-DIRECT-RECOVERY", "fencing_token": 1,
		"tool_call_id": "CALL-DIRECT-RECOVERY", "command_id": "COMMAND-DIRECT-RECOVERY",
		"idempotency_key": "CALL-DIRECT-RECOVERY", "input_digest": "sha256:direct-recovery",
		"tool": "archive.work.recovery.apply.v1", "input": json.RawMessage(input),
	})
	recoveryResponse := mcpRequest(runtime.MCPHandler(), http.MethodPost, "/mcp/v1/call", string(recoveryCall), "service-secret")
	if recoveryResponse.Code != http.StatusOK || !strings.Contains(recoveryResponse.Body.String(), `"evidence_id":"EVD-RECOVERY-`) ||
		!strings.Contains(recoveryResponse.Body.String(), `"resulting_state":"QUEUED"`) {
		t.Fatalf("Archivist direct recovery failed: %d %s", recoveryResponse.Code, recoveryResponse.Body.String())
	}
	detail := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs/JOB-DIRECT-RECOVERY", "", nil, "", "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"step_state":"QUEUED"`) {
		t.Fatalf("recovered Job was not projected: %d %s", detail.Code, detail.Body.String())
	}
	ledger := openLedger(t, configuration.DatabasePath)
	defer ledger.Close()
	var reservations, leases, recoveryEvents int
	ledger.QueryRow(`SELECT COUNT(*) FROM resource_reservations WHERE job_id = ? AND released_at IS NULL`, "JOB-DIRECT-RECOVERY").Scan(&reservations)
	ledger.QueryRow(`SELECT COUNT(*) FROM execution_leases WHERE step_id LIKE ?`, "JOB-DIRECT-RECOVERY/%").Scan(&leases)
	ledger.QueryRow(`SELECT COUNT(*) FROM domain_events WHERE target_id = ? AND CAST(payload AS TEXT) LIKE '%StepRecoveryApplied%'`, "JOB-DIRECT-RECOVERY").Scan(&recoveryEvents)
	if reservations != 0 || leases != 0 || recoveryEvents != 1 {
		t.Fatalf("vertical recovery did not settle its execution boundary: reservations=%d leases=%d events=%d", reservations, leases, recoveryEvents)
	}
}

// Requirement: COL-001
func TestHumanAndArchivistCommandsShareOneLedgerAndWorkspaceReadModel(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("runtime-service-secret"), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, token := runtimeSession(t, runtime.Handler())
	origin := "http://127.0.0.1:8080"
	human := `{"job_id":"JOB-HUMAN-1","command_id":"COMMAND-HUMAN-1","idempotency_key":"JOB-HUMAN-1:create","reason":"operator starts shared work","template_name":"occasion-recording","target_title":"사람 시작 작업","expected_revision":0}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs", human, cookie, token, origin); response.Code != http.StatusCreated {
		t.Fatalf("human command returned %d: %s", response.Code, response.Body.String())
	}
	setupRunningArchivistTurn(t, runtime, "SESSION-COLLAB-1", "TURN-COLLAB-1", "REQUEST-COLLAB-1")
	read := `{"session_id":"SESSION-COLLAB-1","turn_id":"TURN-COLLAB-1","fencing_token":1,"tool_call_id":"CALL-READ-1","command_id":"COMMAND-READ-1","idempotency_key":"CALL-READ-1","input_digest":"sha256:read","tool":"archive.jobs.get.v1","input":{"job_id":"JOB-HUMAN-1"}}`
	if response := mcpRequest(runtime.MCPHandler(), http.MethodPost, "/mcp/v1/call", read, "runtime-service-secret"); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "사람 시작 작업") {
		t.Fatalf("archivist could not read human work context: %d %s", response.Code, response.Body.String())
	}
	archivist := `{"session_id":"SESSION-COLLAB-1","turn_id":"TURN-COLLAB-1","fencing_token":1,"tool_call_id":"CALL-CREATE-1","command_id":"COMMAND-ARCHIVIST-1","idempotency_key":"CALL-CREATE-1","input_digest":"sha256:create","tool":"archive.jobs.create.v1","input":{"job_id":"JOB-ARCHIVIST-1","template_name":"final-edit","target_title":"Archivist 시작 작업","reason":"archivist starts allowed work"}}`
	if response := mcpRequest(runtime.MCPHandler(), http.MethodPost, "/mcp/v1/call", archivist, "runtime-service-secret"); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"revision":1`) {
		t.Fatalf("archivist command returned %d: %s", response.Code, response.Body.String())
	}
	humanContinuation := `{"command_id":"COMMAND-HUMAN-CONTINUE-1","idempotency_key":"JOB-ARCHIVIST-1:human-continue","reason":"operator continues after Archivist unavailable","expected_revision":1}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs/JOB-ARCHIVIST-1/cancel", humanContinuation, cookie, token, origin); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"revision":2`) {
		t.Fatalf("human could not continue Archivist work: %d %s", response.Code, response.Body.String())
	}
	workspace := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs", "", nil, "", "")
	if workspace.Code != http.StatusOK || !strings.Contains(workspace.Body.String(), "JOB-HUMAN-1") || !strings.Contains(workspace.Body.String(), "JOB-ARCHIVIST-1") {
		t.Fatalf("shared workspace projection=%d %s", workspace.Code, workspace.Body.String())
	}
	ledger, err := sql.Open("sqlite", configuration.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	rows, err := ledger.Query(`SELECT payload FROM domain_events ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var combined strings.Builder
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		combined.WriteString(payload)
	}
	if !strings.Contains(combined.String(), `"type":"HUMAN"`) || !strings.Contains(combined.String(), `"type":"ARCHIVIST"`) ||
		!strings.Contains(combined.String(), `"requested_via":"archive_workspace"`) || !strings.Contains(combined.String(), `"requested_via":"mcp"`) {
		t.Fatalf("commands did not share the domain ledger: %s", combined.String())
	}
}

// Requirement: DSH-005
func TestExplicitActionCardExecutionRunsProposedCommandAndUpdatesProjection(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("runtime-service-secret"), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	setupRunningArchivistTurn(t, runtime, "SESSION-ACTION-1", "TURN-ACTION-1", "REQUEST-ACTION-1")
	seedJobActionCard(t, configuration.DatabasePath, "ACTION-CALL-ACTION-1", "COMMAND-ACTION-1", "JOB-ACTION-1", "카드 실행 작업")
	before := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/archivist-sessions", "", nil, "", "")
	if !strings.Contains(before.Body.String(), `"action_card_id":"ACTION-CALL-ACTION-1"`) || !strings.Contains(before.Body.String(), `"state":"PENDING"`) {
		t.Fatalf("pending ActionCard not projected: %s", before.Body.String())
	}
	cookie, token := runtimeSession(t, runtime.Handler())
	executed := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/action-cards/ACTION-CALL-ACTION-1/execute", `{}`, cookie, token, "http://127.0.0.1:8080")
	if executed.Code != http.StatusOK || !strings.Contains(executed.Body.String(), `"target_id":"JOB-ACTION-1"`) {
		t.Fatalf("explicit card execution returned %d: %s", executed.Code, executed.Body.String())
	}
	after := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/archivist-sessions", "", nil, "", "")
	job := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs/JOB-ACTION-1", "", nil, "", "")
	if !strings.Contains(after.Body.String(), `"state":"COMPLETED"`) || job.Code != http.StatusOK || !strings.Contains(job.Body.String(), "카드 실행 작업") {
		t.Fatalf("completed ActionCard or Job projection missing: sessions=%s job=%s", after.Body.String(), job.Body.String())
	}

	seedJobActionCard(t, configuration.DatabasePath, "ACTION-CALL-ACTION-REJECT-1", "COMMAND-ACTION-REJECT-1", "JOB-ACTION-REJECT-1", "거절할 카드 작업")
	rejected := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/action-cards/ACTION-CALL-ACTION-REJECT-1/reject", `{"reason":"담당자 범위와 다름"}`, cookie, token, "http://127.0.0.1:8080")
	if rejected.Code != http.StatusOK || !strings.Contains(rejected.Body.String(), `"state":"REJECTED"`) {
		t.Fatalf("explicit card rejection returned %d: %s", rejected.Code, rejected.Body.String())
	}
	afterReject := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/archivist-sessions", "", nil, "", "")
	rejectedJob := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs/JOB-ACTION-REJECT-1", "", nil, "", "")
	refusedExecution := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/action-cards/ACTION-CALL-ACTION-REJECT-1/execute", `{}`, cookie, token, "http://127.0.0.1:8080")
	if !strings.Contains(afterReject.Body.String(), `"state":"REJECTED"`) ||
		!strings.Contains(afterReject.Body.String(), `"rejection_reason":"담당자 범위와 다름"`) ||
		rejectedJob.Code != http.StatusNotFound || refusedExecution.Code == http.StatusOK {
		t.Fatalf("rejected card changed domain state: sessions=%s job=%d execute=%d %s", afterReject.Body.String(), rejectedJob.Code, refusedExecution.Code, refusedExecution.Body.String())
	}

	seedJobActionCard(t, configuration.DatabasePath, "ACTION-CALL-ACTION-RACE-1", "COMMAND-ACTION-RACE-1", "JOB-ACTION-RACE-1", "경합 카드 작업")
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	var wait sync.WaitGroup
	for _, request := range []struct {
		path string
		body string
	}{
		{path: "/api/v1/action-cards/ACTION-CALL-ACTION-RACE-1/execute", body: `{}`},
		{path: "/api/v1/action-cards/ACTION-CALL-ACTION-RACE-1/reject", body: `{"reason":"경합 거절"}`},
	} {
		request := request
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			responses <- runtimeRequest(runtime.Handler(), http.MethodPost, request.path, request.body, cookie, token, "http://127.0.0.1:8080")
		}()
	}
	close(start)
	wait.Wait()
	close(responses)
	successes := 0
	for response := range responses {
		if response.Code == http.StatusOK {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("execute and reject were not mutually exclusive: successes=%d", successes)
	}
	raceProjection := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/archivist-sessions", "", nil, "", "")
	raceJob := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs/JOB-ACTION-RACE-1", "", nil, "", "")
	completed := strings.Contains(raceProjection.Body.String(), `"target_id":"JOB-ACTION-RACE-1","expected_revision":0,"state":"COMPLETED"`)
	rejectedState := strings.Contains(raceProjection.Body.String(), `"target_id":"JOB-ACTION-RACE-1","expected_revision":0,"state":"REJECTED"`)
	if completed == rejectedState || (completed && raceJob.Code != http.StatusOK) || (rejectedState && raceJob.Code != http.StatusNotFound) {
		t.Fatalf("ActionCard race produced inconsistent effect: sessions=%s job=%d", raceProjection.Body.String(), raceJob.Code)
	}
}

type runtimeSecret string

func seedJobActionCard(t *testing.T, databasePath, cardID, commandID, jobID, title string) {
	t.Helper()
	card := archivistcontrol.ActionCardProjection{
		ID: cardID, ArchivistTurnID: "TURN-ACTION-1", CommandType: job.CreateCommand,
		TargetID: jobID, ExpectedRevision: 0, State: "PENDING", CommandID: commandID,
		IdempotencyKey: cardID, Reason: "approval-gated proposal", Basis: []string{"policy: GATED"},
		Parameters: map[string]any{"template_name": "final-edit", "target_title": title},
	}
	payload, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`INSERT INTO archivist_action_cards(action_card_id, session_id, turn_id, payload) VALUES (?, ?, ?, ?)`,
		card.ID, "SESSION-ACTION-1", card.ArchivistTurnID, payload); err != nil {
		t.Fatal(err)
	}
}

func (secret runtimeSecret) Resolve(string) (string, error) { return string(secret), nil }

// Requirement: WRK-005
func TestWorkerHeartbeatAPIAuthenticatesAndReportsLiveRegistryStatus(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	now := time.Date(2026, 10, 26, 11, 30, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("service-secret"), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	payload := `{"worker_id":"worker-main","snapshot_id":1,"observed_at":"2026-10-26T11:30:00Z","cpu_average_5m":12.5,"gpu_average_5m":3,"idle_stable_millis":360000,"available_memory_bytes":17179869184,"available_vram_bytes":4294967296,"scratch_free_bytes":107374182400,"heavy_running":0,"light_running":1,"busy_physical_device_ids":[],"accepting_work":true,"accessible_storage_ids":["scratch-main"]}`
	if response := workerHeartbeatRequest(runtime.Handler(), payload, "wrong-secret"); response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid Worker credential returned %d: %s", response.Code, response.Body.String())
	}
	if response := workerHeartbeatRequest(runtime.Handler(), payload, "service-secret"); response.Code != http.StatusOK {
		t.Fatalf("valid Worker heartbeat returned %d: %s", response.Code, response.Body.String())
	}
	statuses := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/workers", "", nil, "", "")
	body := statuses.Body.String()
	if statuses.Code != http.StatusOK || !strings.Contains(body, `"worker_id":"worker-main"`) ||
		!strings.Contains(body, `"state":"ONLINE"`) || !strings.Contains(body, `"accessible_storage_ids":["scratch-main"]`) ||
		strings.Contains(body, "service-secret") || strings.Contains(body, "wincred://") {
		t.Fatalf("Worker status response=%d %s", statuses.Code, body)
	}
}

// Requirement: WRK-009
func TestWorkerClaimsReservedStepAndAtomicallySettlesResultReservationAndProjection(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("service-secret"), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, token := runtimeSession(t, runtime.Handler())
	create := `{"job_id":"JOB-WORKER-LOOP-1","command_id":"COMMAND-WORKER-LOOP-1","idempotency_key":"JOB-WORKER-LOOP-1:create","reason":"worker protocol integration","template_name":"large-file-intake","target_title":"대용량 입고","expected_revision":0}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs", create, cookie, token, "http://127.0.0.1:8080"); response.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %s", response.Code, response.Body.String())
	}
	attachWorkerTestSpecs(t, configuration.DatabasePath, "JOB-WORKER-LOOP-1")
	heartbeat := `{"worker_id":"worker-main","snapshot_id":1,"observed_at":"2026-10-29T12:00:00Z","cpu_average_5m":2,"gpu_average_5m":0,"idle_stable_millis":360000,"available_memory_bytes":34359738368,"available_vram_bytes":8589934592,"scratch_free_bytes":536870912000,"heavy_running":0,"light_running":0,"busy_physical_device_ids":[],"accepting_work":true,"accessible_storage_ids":["scratch-main"]}`
	if response := workerHeartbeatRequest(runtime.Handler(), heartbeat, "service-secret"); response.Code != http.StatusOK {
		t.Fatalf("heartbeat/dispatch returned %d: %s", response.Code, response.Body.String())
	}
	claimed := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/claim", "", "service-secret")
	if claimed.Code != http.StatusOK || !strings.Contains(claimed.Body.String(), `"job_id":"JOB-WORKER-LOOP-1"`) ||
		!strings.Contains(claimed.Body.String(), `"step_id":"inspect_inputs"`) || !strings.Contains(claimed.Body.String(), `"fencing_token":1`) {
		t.Fatalf("claim returned %d: %s", claimed.Code, claimed.Body.String())
	}
	result := workerSuccessResult(t, configuration.DatabasePath, "JOB-WORKER-LOOP-1", "inspect_inputs", 1, "evidence://worker-loop")
	settled := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/result", result, "service-secret")
	if settled.Code != http.StatusOK {
		t.Fatalf("result returned %d: %s", settled.Code, settled.Body.String())
	}
	if replayed := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/result", result, "service-secret"); replayed.Code != http.StatusOK {
		t.Fatalf("identical result replay returned %d: %s", replayed.Code, replayed.Body.String())
	}
	changedResult := strings.Replace(result, "evidence://worker-loop", "evidence://changed", 1)
	if stale := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/result", changedResult, "service-secret"); stale.Code != http.StatusConflict {
		t.Fatalf("changed result replay returned %d: %s", stale.Code, stale.Body.String())
	}
	detail := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs/JOB-WORKER-LOOP-1", "", nil, "", "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"id":"inspect_inputs","state":"COMPLETED"`) ||
		!strings.Contains(detail.Body.String(), `"log_reference":"evidence://worker-loop"`) {
		t.Fatalf("settled projection=%d %s", detail.Code, detail.Body.String())
	}
	ledger, err := sql.Open("sqlite", configuration.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	var activeReservations, activeLeases int
	if err := ledger.QueryRow(`SELECT COUNT(*) FROM resource_reservations WHERE released_at IS NULL`).Scan(&activeReservations); err != nil {
		t.Fatal(err)
	}
	if err := ledger.QueryRow(`SELECT COUNT(*) FROM execution_leases`).Scan(&activeLeases); err != nil {
		t.Fatal(err)
	}
	if activeReservations != 0 || activeLeases != 0 {
		t.Fatalf("settlement leaked reservation=%d lease=%d", activeReservations, activeLeases)
	}
}

// Requirement: CAN-001
func TestReservedJobCancellationAtomicallyFencesLeaseAndReleasesReservation(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	now := time.Date(2026, 10, 29, 12, 30, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("service-secret"), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, token := runtimeSession(t, runtime.Handler())
	create := `{"job_id":"JOB-RESERVED-CANCEL-1","command_id":"COMMAND-RESERVED-CREATE","idempotency_key":"JOB-RESERVED-CANCEL-1:create","reason":"reserved cancellation integration","template_name":"large-file-intake","target_title":"예약 취소","expected_revision":0}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs", create, cookie, token, "http://127.0.0.1:8080"); response.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %s", response.Code, response.Body.String())
	}
	heartbeat := `{"worker_id":"worker-main","snapshot_id":1,"observed_at":"2026-10-29T12:30:00Z","cpu_average_5m":2,"gpu_average_5m":0,"idle_stable_millis":360000,"available_memory_bytes":34359738368,"available_vram_bytes":8589934592,"scratch_free_bytes":536870912000,"heavy_running":0,"light_running":0,"busy_physical_device_ids":[],"accepting_work":true,"accessible_storage_ids":["scratch-main"]}`
	if response := workerHeartbeatRequest(runtime.Handler(), heartbeat, "service-secret"); response.Code != http.StatusOK {
		t.Fatalf("dispatch returned %d: %s", response.Code, response.Body.String())
	}
	cancel := `{"command_id":"COMMAND-RESERVED-CANCEL","idempotency_key":"JOB-RESERVED-CANCEL-1:cancel","reason":"operator cancelled before process start","expected_revision":2}`
	response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs/JOB-RESERVED-CANCEL-1/cancel", cancel, cookie, token, "http://127.0.0.1:8080")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"revision":3`) {
		t.Fatalf("reserved cancellation returned %d: %s", response.Code, response.Body.String())
	}
	if claim := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/claim", "", "service-secret"); claim.Code != http.StatusNoContent {
		t.Fatalf("cancelled reservation remained claimable: %d %s", claim.Code, claim.Body.String())
	}
	detail := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs/JOB-RESERVED-CANCEL-1", "", nil, "", "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"step_state":"CANCELLED"`) {
		t.Fatalf("cancelled projection=%d %s", detail.Code, detail.Body.String())
	}
	ledger, err := sql.Open("sqlite", configuration.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	var activeReservations, leases int
	ledger.QueryRow(`SELECT COUNT(*) FROM resource_reservations WHERE released_at IS NULL`).Scan(&activeReservations)
	ledger.QueryRow(`SELECT COUNT(*) FROM execution_leases`).Scan(&leases)
	if activeReservations != 0 || leases != 0 {
		t.Fatalf("cancel leaked reservation=%d lease=%d", activeReservations, leases)
	}
}

// Requirement: CAN-002
func TestRunningCancellationPersistsOneRequestFencesLeaseAndHoldsReservation(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	now := time.Date(2026, 10, 29, 13, 0, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("service-secret"), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, token := runtimeSession(t, runtime.Handler())
	createRunningWorkerJob(t, runtime.Handler(), configuration.DatabasePath, cookie, token, "JOB-RUNNING-CANCEL-1", 1, now)
	cancel := `{"command_id":"COMMAND-RUNNING-CANCEL","idempotency_key":"JOB-RUNNING-CANCEL-1:cancel","reason":"operator requested running stop","expected_revision":3}`
	first := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs/JOB-RUNNING-CANCEL-1/cancel", cancel, cookie, token, "http://127.0.0.1:8080")
	replay := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs/JOB-RUNNING-CANCEL-1/cancel", cancel, cookie, token, "http://127.0.0.1:8080")
	if first.Code != http.StatusOK || replay.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"revision":4`) ||
		!strings.Contains(replay.Body.String(), `"command_id":"COMMAND-RUNNING-CANCEL"`) {
		t.Fatalf("running cancellation or replay failed: first=%d %s replay=%d %s", first.Code, first.Body.String(), replay.Code, replay.Body.String())
	}
	pending := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/cancellation", `{"job_id":"JOB-RUNNING-CANCEL-1","step_id":"inspect_inputs","fencing_token":1}`, "service-secret")
	if pending.Code != http.StatusOK || !strings.Contains(pending.Body.String(), `"cancellation_id":"COMMAND-RUNNING-CANCEL:inspect_inputs"`) {
		t.Fatalf("Worker did not receive cancellation request: %d %s", pending.Code, pending.Body.String())
	}
	lateResult := workerSuccessResult(t, configuration.DatabasePath, "JOB-RUNNING-CANCEL-1", "inspect_inputs", 1, "evidence://late")
	if response := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/result", lateResult, "service-secret"); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "STALE_FENCING_TOKEN") {
		t.Fatalf("fenced completion was accepted: %d %s", response.Code, response.Body.String())
	}
	detail := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs/JOB-RUNNING-CANCEL-1", "", nil, "", "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"step_state":"RUNNING"`) {
		t.Fatalf("running state changed before cleanup confirmation: %d %s", detail.Code, detail.Body.String())
	}
	ledger := openLedger(t, configuration.DatabasePath)
	defer ledger.Close()
	var requests, receipts, activeReservations, leases int
	ledger.QueryRow(`SELECT COUNT(*) FROM worker_cancellation_requests WHERE job_id = ?`, "JOB-RUNNING-CANCEL-1").Scan(&requests)
	ledger.QueryRow(`SELECT COUNT(*) FROM idempotency_receipts WHERE idempotency_key = ?`, "JOB-RUNNING-CANCEL-1:cancel").Scan(&receipts)
	ledger.QueryRow(`SELECT COUNT(*) FROM resource_reservations WHERE job_id = ? AND released_at IS NULL`, "JOB-RUNNING-CANCEL-1").Scan(&activeReservations)
	ledger.QueryRow(`SELECT COUNT(*) FROM execution_leases WHERE step_id = ?`, "JOB-RUNNING-CANCEL-1/inspect_inputs").Scan(&leases)
	if requests != 1 || receipts != 1 || activeReservations != 1 || leases != 0 {
		t.Fatalf("cancellation boundary requests=%d receipts=%d reservations=%d leases=%d", requests, receipts, activeReservations, leases)
	}
}

// Requirement: CAN-003
func TestWorkerCancellationStopAndCleanupSettlesJobRemainderAtomically(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	now := time.Date(2026, 10, 29, 13, 30, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("service-secret"), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	server := httptest.NewServer(runtime.Handler())
	defer server.Close()
	controlURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := workerclient.NewWithExecutor(workerconfig.Configuration{
		SchemaVersion: 1, ID: "worker-main", ControlURL: controlURL,
		CredentialReference: "wincred://worker/main", HeartbeatInterval: 10 * time.Second, ClaimInterval: 10 * time.Millisecond,
		AccessibleStorageIDs: []string{"scratch-main"},
	}, runtimeSecret("service-secret"), server.Client(), runtimeWorkerProbe{now: now}, func() time.Time { return now }, &completeThenCancelExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	cookie, token := runtimeSession(t, runtime.Handler())
	create := `{"job_id":"JOB-CANCEL-LOOP-1","command_id":"COMMAND-CANCEL-LOOP-CREATE","idempotency_key":"JOB-CANCEL-LOOP-1:create","reason":"Worker cancellation loop","template_name":"occasion-recording","target_title":"실행 중 정리 취소","expected_revision":0}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs", create, cookie, token, "http://127.0.0.1:8080"); response.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %s", response.Code, response.Body.String())
	}
	attachWorkerTestSpecs(t, configuration.DatabasePath, "JOB-CANCEL-LOOP-1")
	workerContext, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	errorsSeen := make(chan error, 16)
	go client.Run(workerContext, func(err error) { errorsSeen <- err })
	revision := waitForCurrentJobStep(t, runtime.Handler(), "JOB-CANCEL-LOOP-1", "analyze", "RUNNING", 3*time.Second)
	cancel := fmt.Sprintf(`{"command_id":"COMMAND-CANCEL-LOOP","idempotency_key":"JOB-CANCEL-LOOP-1:cancel","reason":"stop fixture and clean temporary output","expected_revision":%d}`, revision)
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs/JOB-CANCEL-LOOP-1/cancel", cancel, cookie, token, "http://127.0.0.1:8080"); response.Code != http.StatusOK {
		t.Fatalf("cancel returned %d: %s", response.Code, response.Body.String())
	}
	waitForJobStateOrWorkerError(t, runtime.Handler(), "JOB-CANCEL-LOOP-1", "CANCELLED", 3*time.Second, errorsSeen)
	stopWorker()
	detail := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs/JOB-CANCEL-LOOP-1", "", nil, "", "")
	if !strings.Contains(detail.Body.String(), `"id":"intake","state":"COMPLETED"`) ||
		!strings.Contains(detail.Body.String(), `"id":"analyze","state":"CANCELLED"`) ||
		!strings.Contains(detail.Body.String(), `"id":"proxy","state":"CANCELLED"`) ||
		!strings.Contains(detail.Body.String(), `"log_reference":"fixture://worker-cancelled-clean"`) {
		t.Fatalf("Job remainder was not settled: %s", detail.Body.String())
	}
	ledger := openLedger(t, configuration.DatabasePath)
	defer ledger.Close()
	var activeReservations, leases, settled int
	ledger.QueryRow(`SELECT COUNT(*) FROM resource_reservations WHERE job_id = ? AND released_at IS NULL`, "JOB-CANCEL-LOOP-1").Scan(&activeReservations)
	ledger.QueryRow(`SELECT COUNT(*) FROM execution_leases WHERE step_id = ?`, "JOB-CANCEL-LOOP-1/analyze").Scan(&leases)
	ledger.QueryRow(`SELECT COUNT(*) FROM worker_cancellation_events WHERE event_type = 'CancellationSettled'`).Scan(&settled)
	if activeReservations != 0 || leases != 0 || settled != 1 {
		t.Fatalf("cancellation settlement reservations=%d leases=%d events=%d", activeReservations, leases, settled)
	}
	select {
	case runErr := <-errorsSeen:
		t.Fatalf("Worker cancellation loop reported error: %v", runErr)
	default:
	}
}

// Requirement: CAN-005
func TestCancellationPartialReleasesStoppedProcessAndUncertainProcessQuarantinesWorker(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	now := time.Date(2026, 10, 29, 13, 45, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("service-secret"), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, token := runtimeSession(t, runtime.Handler())

	createRunningWorkerJob(t, runtime.Handler(), configuration.DatabasePath, cookie, token, "JOB-CANCEL-PARTIAL", 1, now)
	partialCancel := `{"command_id":"COMMAND-CANCEL-PARTIAL","idempotency_key":"JOB-CANCEL-PARTIAL:cancel","reason":"stop with known published effect","expected_revision":3}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs/JOB-CANCEL-PARTIAL/cancel", partialCancel, cookie, token, "http://127.0.0.1:8080"); response.Code != http.StatusOK {
		t.Fatalf("partial cancellation request returned %d: %s", response.Code, response.Body.String())
	}
	partialResult := `{"cancellation_id":"COMMAND-CANCEL-PARTIAL:inspect_inputs","job_id":"JOB-CANCEL-PARTIAL","step_id":"inspect_inputs","fencing_token":1,"process_stopped":true,"cleanup_confirmed":false,"external_effect_references":["scratch-main:staging/published.mp4"],"log_reference":"worker://partial"}`
	if response := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/cancellation-result", partialResult, "service-secret"); response.Code != http.StatusOK {
		t.Fatalf("partial cancellation result returned %d: %s", response.Code, response.Body.String())
	}
	if response := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/cancellation-result", partialResult, "service-secret"); response.Code != http.StatusOK {
		t.Fatalf("identical cancellation result replay returned %d: %s", response.Code, response.Body.String())
	}
	partialDetail := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs/JOB-CANCEL-PARTIAL", "", nil, "", "")
	if !strings.Contains(partialDetail.Body.String(), `"state":"CANCELLED"`) ||
		!strings.Contains(partialDetail.Body.String(), `"error_class":"CANCELLATION_PARTIAL"`) {
		t.Fatalf("known remaining effect was not projected as partial cancellation: %s", partialDetail.Body.String())
	}

	createRunningWorkerJob(t, runtime.Handler(), configuration.DatabasePath, cookie, token, "JOB-CANCEL-UNCERTAIN", 2, now.Add(time.Minute))
	uncertainCancel := `{"command_id":"COMMAND-CANCEL-UNCERTAIN","idempotency_key":"JOB-CANCEL-UNCERTAIN:cancel","reason":"process state unknown","expected_revision":3}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs/JOB-CANCEL-UNCERTAIN/cancel", uncertainCancel, cookie, token, "http://127.0.0.1:8080"); response.Code != http.StatusOK {
		t.Fatalf("uncertain cancellation request returned %d: %s", response.Code, response.Body.String())
	}
	uncertainResult := `{"cancellation_id":"COMMAND-CANCEL-UNCERTAIN:inspect_inputs","job_id":"JOB-CANCEL-UNCERTAIN","step_id":"inspect_inputs","fencing_token":1,"process_stopped":false,"cleanup_confirmed":false,"external_effect_references":[],"log_reference":"worker://uncertain"}`
	if response := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/cancellation-result", uncertainResult, "service-secret"); response.Code != http.StatusOK {
		t.Fatalf("uncertain cancellation result returned %d: %s", response.Code, response.Body.String())
	}
	queuedCreate := `{"job_id":"JOB-AFTER-UNCERTAIN","command_id":"COMMAND-AFTER-UNCERTAIN","idempotency_key":"JOB-AFTER-UNCERTAIN:create","reason":"must remain queued while Worker is isolated","template_name":"large-file-intake","target_title":"격리 뒤 작업","expected_revision":0}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs", queuedCreate, cookie, token, "http://127.0.0.1:8080"); response.Code != http.StatusCreated {
		t.Fatalf("queued job creation returned %d: %s", response.Code, response.Body.String())
	}
	if claim := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/claim", "", "service-secret"); claim.Code != http.StatusNoContent {
		t.Fatalf("quarantined Worker received new execution: %d %s", claim.Code, claim.Body.String())
	}
	ledger := openLedger(t, configuration.DatabasePath)
	defer ledger.Close()
	var partialReservations, uncertainReservations, partialEvents, uncertainEvents, partialCompletions int
	ledger.QueryRow(`SELECT COUNT(*) FROM resource_reservations WHERE job_id = 'JOB-CANCEL-PARTIAL' AND released_at IS NULL`).Scan(&partialReservations)
	ledger.QueryRow(`SELECT COUNT(*) FROM resource_reservations WHERE job_id = 'JOB-CANCEL-UNCERTAIN' AND released_at IS NULL`).Scan(&uncertainReservations)
	ledger.QueryRow(`SELECT COUNT(*) FROM domain_events WHERE CAST(payload AS TEXT) LIKE '%StepCancellationPartiallySettled%'`).Scan(&partialEvents)
	ledger.QueryRow(`SELECT COUNT(*) FROM worker_cancellation_events WHERE cancellation_id = 'COMMAND-CANCEL-UNCERTAIN:inspect_inputs' AND event_type = 'ExternalEffectUncertain'`).Scan(&uncertainEvents)
	ledger.QueryRow(`SELECT COUNT(*) FROM worker_completion_outbox WHERE job_id = 'JOB-CANCEL-PARTIAL'`).Scan(&partialCompletions)
	if partialReservations != 0 || uncertainReservations != 1 || partialEvents != 1 || uncertainEvents != 1 || partialCompletions != 1 {
		t.Fatalf("cancellation reconciliation partial_reservations=%d uncertain_reservations=%d partial_events=%d uncertain_events=%d completions=%d", partialReservations, uncertainReservations, partialEvents, uncertainEvents, partialCompletions)
	}
}

// Requirement: CAN-004
func TestWorkerCompletionAndCancellationRaceAcceptsOnlyFirstCommittedOutcome(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	now := time.Date(2026, 10, 29, 14, 0, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("service-secret"), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, token := runtimeSession(t, runtime.Handler())
	createRunningWorkerJob(t, runtime.Handler(), configuration.DatabasePath, cookie, token, "JOB-RACE-COMPLETE", 1, now)
	success := workerSuccessResult(t, configuration.DatabasePath, "JOB-RACE-COMPLETE", "inspect_inputs", 1, "evidence://winner")
	if response := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/result", success, "service-secret"); response.Code != http.StatusOK {
		t.Fatalf("winning completion returned %d: %s", response.Code, response.Body.String())
	}
	staleCancel := `{"command_id":"COMMAND-RACE-LATE-CANCEL","idempotency_key":"JOB-RACE-COMPLETE:late-cancel","reason":"late cancellation","expected_revision":3}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs/JOB-RACE-COMPLETE/cancel", staleCancel, cookie, token, "http://127.0.0.1:8080"); response.Code != http.StatusConflict {
		t.Fatalf("late cancellation returned %d: %s", response.Code, response.Body.String())
	}

	createRunningWorkerJob(t, runtime.Handler(), configuration.DatabasePath, cookie, token, "JOB-RACE-CANCEL", 2, now)
	cancel := `{"command_id":"COMMAND-RACE-CANCEL","idempotency_key":"JOB-RACE-CANCEL:cancel","reason":"cancellation wins","expected_revision":3}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs/JOB-RACE-CANCEL/cancel", cancel, cookie, token, "http://127.0.0.1:8080"); response.Code != http.StatusOK {
		t.Fatalf("winning cancellation returned %d: %s", response.Code, response.Body.String())
	}
	lateSuccess := workerSuccessResult(t, configuration.DatabasePath, "JOB-RACE-CANCEL", "inspect_inputs", 1, "evidence://late")
	if response := workerProtocolRequest(runtime.Handler(), "/worker/v1/steps/result", lateSuccess, "service-secret"); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "STALE_FENCING_TOKEN") {
		t.Fatalf("late completion returned %d: %s", response.Code, response.Body.String())
	}
	completed := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs/JOB-RACE-COMPLETE", "", nil, "", "")
	cancelling := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs/JOB-RACE-CANCEL", "", nil, "", "")
	if !strings.Contains(completed.Body.String(), `"id":"inspect_inputs","state":"COMPLETED"`) ||
		!strings.Contains(cancelling.Body.String(), `"id":"inspect_inputs","state":"RUNNING"`) {
		t.Fatalf("race outcomes diverged: completed=%s cancelling=%s", completed.Body.String(), cancelling.Body.String())
	}
}

type runtimeWorkerProbe struct{ now time.Time }

func (probe runtimeWorkerProbe) Snapshot(context.Context) (worker.Metrics, error) {
	return worker.Metrics{
		ObservedAt: probe.now, AverageCPU5m: 2, IdleStableFor: time.Hour,
		AvailableMemoryBytes: 32 << 30, AvailableVRAMBytes: 8 << 30, ScratchFreeBytes: 500 << 30,
		BusyPhysicalDeviceIDs: map[string]bool{}, AcceptingWork: true,
	}, nil
}

type completeThenCancelExecutor struct {
	mu    sync.Mutex
	calls int
}

func settleClaimedFixtureStep(t *testing.T, handler http.Handler, databasePath string, claim *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Data struct {
			JobID        string `json:"job_id"`
			StepID       string `json:"step_id"`
			FencingToken uint64 `json:"fencing_token"`
		} `json:"data"`
	}
	if claim.Code != http.StatusOK || json.Unmarshal(claim.Body.Bytes(), &payload) != nil ||
		payload.Data.JobID == "" || payload.Data.StepID == "" || payload.Data.FencingToken == 0 {
		t.Fatalf("invalid Worker fixture claim: %d %s", claim.Code, claim.Body.String())
	}
	result := workerSuccessResult(
		t, databasePath, payload.Data.JobID, payload.Data.StepID, payload.Data.FencingToken,
		"fixture://"+payload.Data.StepID+"-verified",
	)
	response := workerProtocolRequest(handler, "/worker/v1/steps/result", result, "service-secret")
	if response.Code != http.StatusOK {
		t.Fatalf("settle %s/%s returned %d: %s", payload.Data.JobID, payload.Data.StepID, response.Code, response.Body.String())
	}
	return payload.Data.StepID
}

func (executor *completeThenCancelExecutor) Execute(ctx context.Context, assignment workerclient.Assignment) workerclient.ExecutionResult {
	executor.mu.Lock()
	executor.calls++
	call := executor.calls
	executor.mu.Unlock()
	if call == 1 {
		outputs := make([]execution.StepOutput, 0, len(assignment.ExecutionSpec.Outputs))
		for _, target := range assignment.ExecutionSpec.Outputs {
			outputs = append(outputs, execution.StepOutput{
				ID: target.ID, Kind: target.Kind, StorageID: target.StorageID,
				RelativePath: target.RelativePath, Checksum: strings.Repeat("e", 64),
			})
		}
		return workerclient.ExecutionResult{
			Success: true, LogReference: "fixture://worker-completed", Outputs: outputs,
			VerificationReferences: []string{"fixture://worker-output-verified"},
		}
	}
	<-ctx.Done()
	return workerclient.ExecutionResult{Cancellation: &workerclient.CancellationExecutionResult{
		ProcessStopped: true, CleanupConfirmed: true, LogReference: "fixture://worker-cancelled-clean",
	}}
}

func workerSuccessResult(t *testing.T, databasePath, jobID, stepID string, fencingToken uint64, logReference string) string {
	t.Helper()
	store, err := sqlitecommand.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot, err := store.Load(jobID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := job.Decode(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var spec *execution.StepExecutionSpec
	for index := range state.Steps {
		if state.Steps[index].Workflow.ID == stepID {
			spec = state.Steps[index].ExecutionSpec
			break
		}
	}
	if spec == nil {
		t.Fatalf("missing fixture spec for %s/%s", jobID, stepID)
	}
	outputs := make([]execution.StepOutput, 0, len(spec.Outputs))
	for _, target := range spec.Outputs {
		output := execution.StepOutput{
			ID: target.ID, Kind: target.Kind, StorageID: target.StorageID,
			RelativePath: target.RelativePath, Checksum: strings.Repeat("d", 64),
		}
		if target.Kind == execution.OutputTranscript {
			output.TranscriptQuality = &transcription.QualityAssessment{
				PolicyReference: transcription.QualityPolicyReference, State: transcription.QualityPassed,
				DurationMillis: 1_000, SegmentCount: 1, TextCharacterCount: 5,
			}
		}
		outputs = append(outputs, output)
	}
	record, err := execution.NewStepOutputRecord(
		*spec, fencingToken, "worker-main", time.Date(2026, 10, 29, 12, 0, 1, 0, time.UTC),
		outputs, []string{"evidence://output-verified"},
	)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"job_id": jobID, "step_id": stepID, "fencing_token": fencingToken, "success": true,
		"failure_class": "", "wait_reason": "", "error_class": "", "log_reference": logReference,
		"output_record": record,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func createRunningWorkerJob(t *testing.T, handler http.Handler, databasePath string, cookie *http.Cookie, token, jobID string, snapshotID uint64, observedAt time.Time) {
	t.Helper()
	create := fmt.Sprintf(`{"job_id":%q,"command_id":%q,"idempotency_key":%q,"reason":"race setup","template_name":"large-file-intake","target_title":"Worker 실행","expected_revision":0}`, jobID, "COMMAND-CREATE-"+jobID, jobID+":create")
	if response := runtimeRequest(handler, http.MethodPost, "/api/v1/jobs", create, cookie, token, "http://127.0.0.1:8080"); response.Code != http.StatusCreated {
		t.Fatalf("create %s returned %d: %s", jobID, response.Code, response.Body.String())
	}
	attachWorkerTestSpecs(t, databasePath, jobID)
	heartbeat := fmt.Sprintf(`{"worker_id":"worker-main","snapshot_id":%d,"observed_at":%q,"cpu_average_5m":2,"gpu_average_5m":0,"idle_stable_millis":360000,"available_memory_bytes":34359738368,"available_vram_bytes":8589934592,"scratch_free_bytes":536870912000,"heavy_running":0,"light_running":0,"busy_physical_device_ids":[],"accepting_work":true,"accessible_storage_ids":["scratch-main"]}`, snapshotID, observedAt.UTC().Format(time.RFC3339Nano))
	if response := workerHeartbeatRequest(handler, heartbeat, "service-secret"); response.Code != http.StatusOK {
		t.Fatalf("heartbeat %s returned %d: %s", jobID, response.Code, response.Body.String())
	}
	if response := workerProtocolRequest(handler, "/worker/v1/steps/claim", "", "service-secret"); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), fmt.Sprintf(`"job_id":%q`, jobID)) {
		t.Fatalf("claim %s returned %d: %s", jobID, response.Code, response.Body.String())
	}
}

func attachWorkerTestSpecs(t *testing.T, databasePath, jobID string) {
	t.Helper()
	store, err := sqlitecommand.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot, err := store.Load(jobID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := job.Decode(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for index := range state.Steps {
		step := &state.Steps[index]
		if step.ExecutorKind != vocabulary.ExecutorWorker || len(step.RequiredProcessingKinds) == 0 {
			continue
		}
		operation, outputKind := testExecutionOperation(step.RequiredProcessingKinds[0])
		profile := ""
		if operation == execution.OperationRenderRecording {
			profile = "fixture-render@v1"
		}
		spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
			JobID: state.ID, StepID: step.Workflow.ID, TemplateName: state.TemplateName, Operation: operation,
			Inputs: []execution.InputReference{{
				OriginalAssetID: "ASSET-FIXTURE", InspectionID: "INSPECTION-FIXTURE", StorageID: "scratch-main",
				RelativePath: "fixture/source.mp4", Checksum: strings.Repeat("a", 64),
			}},
			Outputs: []execution.OutputTarget{{
				ID: "OUTPUT-" + step.Workflow.ID, Kind: outputKind, StorageID: "scratch-main",
				RelativePath: "fixture/" + state.ID + "/" + step.Workflow.ID + ".output", OriginalAssetID: "ASSET-FIXTURE",
			}},
			ProfileReference: profile,
			Parameters: execution.StepExecutionParameters{
				OrganizationDecisionID: "DECISION-FIXTURE", WorkspaceDriveID: "volume-1", ArchiveStorageID: "scratch-main", InputRevision: 1,
			},
		})
		if err != nil {
			t.Fatalf("build fixture spec for %s: %v", step.Workflow.ID, err)
		}
		step.ExecutionSpec = &spec
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Seed(context.Background(), command.Snapshot{TargetID: state.ID, Revision: state.Revision, State: encoded}); err != nil {
		t.Fatal(err)
	}
}

func testExecutionOperation(kind vocabulary.ProcessingKind) (execution.Operation, execution.OutputKind) {
	switch kind {
	case vocabulary.ProcessingInspect:
		return execution.OperationInspectMedia, execution.OutputMediaInspection
	case vocabulary.ProcessingRender:
		return execution.OperationRenderRecording, execution.OutputRendition
	case vocabulary.ProcessingThumbnail:
		return execution.OperationCreateThumbnail, execution.OutputThumbnail
	case vocabulary.ProcessingAudioExtract:
		return execution.OperationExtractSpeechAudio, execution.OutputAudio
	case vocabulary.ProcessingSTT:
		return execution.OperationTranscribeAudio, execution.OutputTranscript
	case vocabulary.ProcessingQC:
		return execution.OperationQCRendition, execution.OutputQCAssessment
	case vocabulary.ProcessingChecksum:
		return execution.OperationVerifyChecksum, execution.OutputChecksum
	case vocabulary.ProcessingDevice:
		return execution.OperationDeviceIO, execution.OutputDeviceResult
	default:
		return execution.OperationOrganizeMedia, execution.OutputOrganizedMedia
	}
}

func waitForJobState(t *testing.T, handler http.Handler, jobID, state string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response := runtimeRequest(handler, http.MethodGet, "/api/v1/jobs/"+jobID, "", nil, "", "")
		if response.Code == http.StatusOK && strings.Contains(response.Body.String(), `"step_state":"`+state+`"`) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	response := runtimeRequest(handler, http.MethodGet, "/api/v1/jobs/"+jobID, "", nil, "", "")
	t.Fatalf("Job %s did not reach %s: %d %s", jobID, state, response.Code, response.Body.String())
}

func waitForJobStateOrWorkerError(t *testing.T, handler http.Handler, jobID, state string, timeout time.Duration, errorsSeen <-chan error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-errorsSeen:
			t.Fatalf("Worker failed while waiting for %s: %v", state, err)
		default:
		}
		response := runtimeRequest(handler, http.MethodGet, "/api/v1/jobs/"+jobID, "", nil, "", "")
		if response.Code == http.StatusOK && strings.Contains(response.Body.String(), `"step_state":"`+state+`"`) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	response := runtimeRequest(handler, http.MethodGet, "/api/v1/jobs/"+jobID, "", nil, "", "")
	t.Fatalf("Job %s did not reach %s: %d %s", jobID, state, response.Code, response.Body.String())
}

func waitForCurrentJobStep(t *testing.T, handler http.Handler, jobID, stepID, state string, timeout time.Duration) uint64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response := runtimeRequest(handler, http.MethodGet, "/api/v1/jobs/"+jobID, "", nil, "", "")
		var payload struct {
			Data struct {
				Summary struct {
					Revision      uint64 `json:"revision"`
					CurrentStepID string `json:"current_step_id"`
					StepState     string `json:"step_state"`
				} `json:"summary"`
			} `json:"data"`
		}
		if response.Code == http.StatusOK && json.Unmarshal(response.Body.Bytes(), &payload) == nil &&
			payload.Data.Summary.CurrentStepID == stepID && payload.Data.Summary.StepState == state {
			return payload.Data.Summary.Revision
		}
		time.Sleep(10 * time.Millisecond)
	}
	response := runtimeRequest(handler, http.MethodGet, "/api/v1/jobs/"+jobID, "", nil, "", "")
	t.Fatalf("Job %s did not reach %s/%s: %d %s", jobID, stepID, state, response.Code, response.Body.String())
	return 0
}

func openLedger(t *testing.T, path string) *sql.DB {
	t.Helper()
	ledger, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func runtimeTestConfiguration(t *testing.T) bootconfig.Configuration {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	payload := `schema_version: 1
database: {path: data/control.db}
server:
  listen_address: 127.0.0.1:0
  allowed_cidrs: [127.0.0.0/8]
  allowed_origins: [http://127.0.0.1:8080]
  trusted_proxies: []
security: {auth_mode: disabled, mcp_listen_address: 127.0.0.1:8081}
features: {}
provider_accounts: []
workers:
  heartbeat_ttl: 30s
  definitions:
    - id: worker-main
      enabled: true
      credential_ref: wincred://worker/main
      processing_kinds: [INSPECT, RENDER]
      allowed_storage_ids: [scratch-main]
archivist_hosts:
  heartbeat_ttl: 30s
  turn_lease_ttl: 2m
  definitions:
    - id: archivist-windows
      enabled: true
      credential_ref: wincred://archive/archivists/archivist-windows
      allowed_backend_ids: [EMBEDDED_CORE]
      allowed_cli_ids: [codex]
      allowed_workspace_root_ids: [video-project]
      windows_task_name: Archive Archivist Host
storage:
  - id: scratch-main
    role: SCRATCH
    physical_device_id: drive-1
    partition_id: partition-1
    volume_id: volume-1
    root: storage/scratch
`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := bootconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return configuration
}

func runtimeSession(t *testing.T, handler http.Handler) (*http.Cookie, string) {
	t.Helper()
	response := runtimeRequest(handler, http.MethodGet, "/api/v1/session", "", nil, "", "")
	if response.Code != http.StatusOK || len(response.Result().Cookies()) != 1 {
		t.Fatalf("session returned %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Data struct {
			Token string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload.Data.Token == "" {
		t.Fatalf("invalid session response: %s err=%v", response.Body.String(), err)
	}
	return response.Result().Cookies()[0], payload.Data.Token
}

func runtimeRequest(handler http.Handler, method, path, body string, cookie *http.Cookie, token, origin string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.RemoteAddr = "127.0.0.1:45000"
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	if token != "" {
		request.Header.Set("X-CSRF-Token", token)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	handler.ServeHTTP(recorder, request)
	return recorder
}

func workerHeartbeatRequest(handler http.Handler, body, credential string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/heartbeat", bytes.NewBufferString(body))
	request.RemoteAddr = "127.0.0.1:45000"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential)
	handler.ServeHTTP(recorder, request)
	return recorder
}

func workerProtocolRequest(handler http.Handler, path, body, credential string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	request.RemoteAddr = "127.0.0.1:45000"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("X-Worker-ID", "worker-main")
	handler.ServeHTTP(recorder, request)
	return recorder
}

func archivistHostRequest(handler http.Handler, method, path, body, credential string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.RemoteAddr = "127.0.0.1:45000"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("X-Archivist-Host-ID", "archivist-windows")
	handler.ServeHTTP(recorder, request)
	return recorder
}

func mcpRequest(handler http.Handler, method, path, body, credential string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.RemoteAddr = "127.0.0.1:45000"
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("X-Archivist-Host-ID", "archivist-windows")
	handler.ServeHTTP(recorder, request)
	return recorder
}

func setupRunningArchivistTurn(t *testing.T, runtime *controlruntime.Runtime, sessionID, turnID, requestID string) {
	t.Helper()
	cookie, token := runtimeSession(t, runtime.Handler())
	origin := "http://127.0.0.1:8080"
	session := fmt.Sprintf(`{"session_id":%q,"name":"Test Archivist","archivist_host_id":"archivist-windows","cli_id":"codex"}`, sessionID)
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions", session, cookie, token, origin); response.Code != http.StatusCreated {
		t.Fatalf("session create returned %d: %s", response.Code, response.Body.String())
	}
	turn := fmt.Sprintf(`{"turn_id":%q,"request_id":%q,"prompt":"shared work"}`, turnID, requestID)
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions/"+sessionID+"/turns", turn, cookie, token, origin); response.Code != http.StatusCreated {
		t.Fatalf("turn create returned %d: %s", response.Code, response.Body.String())
	}
	if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/heartbeat", "", "runtime-service-secret"); response.Code != http.StatusOK {
		t.Fatalf("heartbeat returned %d: %s", response.Code, response.Body.String())
	}
	if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/turns/claim", "", "runtime-service-secret"); response.Code != http.StatusOK {
		t.Fatalf("claim returned %d: %s", response.Code, response.Body.String())
	}
	delivery := fmt.Sprintf(`{"fencing_token":1,"request_id":%q}`, requestID)
	if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/turns/"+turnID+"/delivery", delivery, "runtime-service-secret"); response.Code != http.StatusOK {
		t.Fatalf("delivery returned %d: %s", response.Code, response.Body.String())
	}
	for cursor, kind := range []string{"INPUT_ACCEPTED", "TURN_STARTED"} {
		event := fmt.Sprintf(`{"fencing_token":1,"cursor":%d,"request_id":%q,"kind":%q}`, cursor+1, requestID, kind)
		if response := archivistHostRequest(runtime.Handler(), http.MethodPost, "/archivist/v1/turns/"+turnID+"/events", event, "runtime-service-secret"); response.Code != http.StatusOK {
			t.Fatalf("runtime event %s returned %d: %s", kind, response.Code, response.Body.String())
		}
	}
}

func ledgerTargetColumn(table string) string {
	if table == "idempotency_receipts" {
		return "idempotency_key"
	}
	return "target_id"
}

func ledgerTargetValue(table string) string {
	if table == "idempotency_receipts" {
		return "JOB-HTTP-1:create"
	}
	return "JOB-HTTP-1"
}
