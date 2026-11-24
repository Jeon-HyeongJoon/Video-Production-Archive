//go:build windows

package controlruntime_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/adapter/archivebundle"
	"videoarchive/internal/adapter/bootconfig"
	"videoarchive/internal/adapter/controlruntime"
	"videoarchive/internal/adapter/ossecret"
	"videoarchive/internal/adapter/sqlitecommand"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

// Requirement: APP-005
func TestWindowsArchiveRunCompletesSourceSequenceAcrossWorkerRestart(t *testing.T) {
	if os.Getenv("ARCHIVE_WINDOWS_DRIVE_ACCEPTANCE") != "1" {
		t.Skip("set ARCHIVE_WINDOWS_DRIVE_ACCEPTANCE=1 on the isolated Windows acceptance host")
	}
	mediaExecutable := requireWindowsAcceptanceExecutable(t, "ARCHIVE_MEDIA_PATH")
	ffmpegExecutable := requireWindowsAcceptanceExecutable(t, "ARCHIVE_FFMPEG_PATH")
	ffprobeExecutable := requireWindowsAcceptanceExecutable(t, "ARCHIVE_FFPROBE_PATH")
	directory := t.TempDir()
	sourceRoot := filepath.Join(directory, "source")
	workingRoot := filepath.Join(directory, "working")
	const folderID = "261124-office/VJ"
	sourceFolder := filepath.Join(sourceRoot, filepath.FromSlash(folderID))
	for _, path := range []string{sourceFolder, workingRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	originalChecksums := make(map[string]string)
	for index := 1; index <= 3; index++ {
		name := fmt.Sprintf("C%04d.mp4", index)
		filePath := filepath.Join(sourceFolder, name)
		command := exec.Command(
			ffmpegExecutable, "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
			"-f", "lavfi", "-i", "testsrc=size=640x360:rate=30",
			"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:sample_rate=48000", 400+index*100),
			"-t", "2", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", filePath,
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("create Windows source fixture %s: %v: %s", name, err, output)
		}
	}
	sourceCapability, err := archive.OpenStorageRoot(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 3; index++ {
		name := fmt.Sprintf("%s/C%04d.mp4", folderID, index)
		_, checksum, err := sourceCapability.FileSHA256(name)
		if err != nil {
			t.Fatal(err)
		}
		originalChecksums[name] = checksum
	}
	sourceCapability.Close()

	controlPath := filepath.Join(directory, "control.yaml")
	controlDocument := fmt.Sprintf(`schema_version: 1
database: {path: %s}
server:
  listen_address: 127.0.0.1:0
  allowed_cidrs: [127.0.0.0/8]
  allowed_origins: [http://127.0.0.1:8080]
  trusted_proxies: []
security: {auth_mode: disabled, mcp_listen_address: 127.0.0.1:0}
features: {}
provider_accounts: []
workers:
  heartbeat_ttl: 5s
  definitions:
    - id: worker-windows-drive
      enabled: true
      credential_ref: wincred://worker/windows-drive
      processing_kinds: [INSPECT, RENDER, QC, ORGANIZE]
      allowed_storage_ids: [source-windows, archive-windows]
storage:
  - id: source-windows
    role: SOURCE_MEDIA
    physical_device_id: source-drive
    partition_id: source-partition
    volume_id: source-volume
    root: %s
  - id: archive-windows
    role: ARCHIVE_PRIMARY
    physical_device_id: working-drive
    partition_id: working-partition
    volume_id: working-volume
    root: %s
archive_organization:
  ffprobe:
    executable: %s
    version: ffprobe-windows-acceptance
`, yamlScalar(filepath.Join(directory, "control.db")), yamlScalar(sourceRoot), yamlScalar(workingRoot), yamlScalar(ffprobeExecutable))
	if err := os.WriteFile(controlPath, []byte(controlDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := bootconfig.Load(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	credential := "windows-drive-acceptance-secret"
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "windows-drive-acceptance", controlruntime.Dependencies{
		Secrets: runtimeSecret(credential), Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	controlServer := httptest.NewServer(runtime.Handler())
	defer controlServer.Close()

	credentialReference := "dpapi://" + filepath.ToSlash(filepath.Join(directory, "worker.dpapi"))
	if err := ossecret.ProvisionDPAPI(credentialReference, credential); err != nil {
		t.Fatal(err)
	}
	processLogRoot := filepath.Join(directory, "process-logs")
	if err := os.Mkdir(processLogRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	workerPath := filepath.Join(directory, "worker.yaml")
	workerDocument := fmt.Sprintf(`schema_version: 1
worker:
  id: worker-windows-drive
  control_url: %s
  credential_ref: %s
  heartbeat_interval: 1s
  claim_interval: 2s
  process_logs:
    root: %s
    max_file_bytes: 4194304
    tail_bytes: 12288
    retention: 336h
    max_total_bytes: 268435456
  storage_roots:
    - storage_id: source-windows
      root: %s
    - storage_id: archive-windows
      root: %s
  tools:
    ffmpeg:
      executable: %s
      version: ffmpeg-windows-acceptance
    ffprobe:
      executable: %s
      version: ffprobe-windows-acceptance
`, yamlScalar(controlServer.URL), yamlScalar(credentialReference), yamlScalar(processLogRoot), yamlScalar(sourceRoot), yamlScalar(workingRoot),
		yamlScalar(ffmpegExecutable), yamlScalar(ffprobeExecutable))
	if err := os.WriteFile(workerPath, []byte(workerDocument), 0o600); err != nil {
		t.Fatal(err)
	}

	firstWorker := startWindowsAcceptanceProcess(t, mediaExecutable, "runner", "-config", workerPath)
	defer firstWorker.Stop(t)
	waitForWindowsAcceptance(t, 15*time.Second, "production Worker ONLINE", func() bool {
		response := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/workers", "", nil, "", "")
		return response.Code == http.StatusOK && strings.Contains(response.Body.String(), `"worker_id":"worker-windows-drive"`) &&
			(strings.Contains(response.Body.String(), `"state":"ONLINE"`) || strings.Contains(response.Body.String(), `"state":"BUSY"`))
	})

	cookie, csrfToken := runtimeSession(t, runtime.Handler())
	var candidateRevision, inputRevision uint64
	waitForWindowsAcceptance(t, 15*time.Second, "published source folder inventory", func() bool {
		response := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/archive-intake", "", nil, "", "")
		var inventory struct {
			Data []struct {
				Volume            string `json:"source_volume_id"`
				CandidateRevision uint64 `json:"candidate_revision"`
				DiscoveryRevision uint64 `json:"folder_discovery_revision"`
				State             string `json:"folder_discovery_state"`
			} `json:"data"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &inventory) != nil {
			return false
		}
		for _, drive := range inventory.Data {
			if drive.Volume == "source-volume" && drive.State == "READY" && drive.CandidateRevision != 0 && drive.DiscoveryRevision != 0 {
				candidateRevision, inputRevision = drive.CandidateRevision, drive.DiscoveryRevision
				return true
			}
		}
		return false
	})
	startBody := fmt.Sprintf(`{"archive_run_id":"RUN-WINDOWS-DRIVE","idempotency_key":"windows-drive:start","session_id":"SESSION-WINDOWS-DRIVE","source_volume_id":"source-volume","expected_candidate_revision":%d,"archive_storage_id":"archive-windows","input_revision":%d,"folder_ids":[%q]}`, candidateRevision, inputRevision, folderID)
	start := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archive-intake/runs", startBody, cookie, csrfToken, "http://127.0.0.1:8080")
	if start.Code != http.StatusCreated {
		t.Fatalf("start Windows Archive Organization returned %d: %s", start.Code, start.Body.String())
	}
	jobID := waitForDriveJobID(t, runtime.Handler(), "source-volume", 10*time.Second)
	waitForWindowsAcceptance(t, 90*time.Second, "sequence render completion before restart", func() bool {
		detail := readDriveJob(t, runtime.Handler(), jobID)
		return detail.stepState("sequence_render") == workflow.Completed && detail.stepState("qc") != workflow.Running
	})
	firstWorker.Stop(t)
	secondWorker := startWindowsAcceptanceProcess(t, mediaExecutable, "runner", "-config", workerPath)
	defer secondWorker.Stop(t)
	waitForWindowsAcceptance(t, 120*time.Second, "Archive Organization Job completion after Worker restart", func() bool {
		detail := readDriveJob(t, runtime.Handler(), jobID)
		return detail.stepState("sequence_render") == workflow.Completed && detail.stepState("qc") == workflow.Completed &&
			detail.stepState("organize") == workflow.Completed && detail.stepState("archive_catalog") == workflow.Completed
	})

	store, err := sqlitecommand.Open(configuration.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load(jobID)
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	state, err := job.Decode(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	outputPaths := make(map[string][]string)
	for _, step := range state.Steps {
		if step.ExecutorKind != vocabulary.ExecutorWorker {
			continue
		}
		if step.OutputRecord == nil {
			t.Fatalf("completed Windows Step has no output record: %s", step.Workflow.ID)
		}
		for _, output := range step.OutputRecord.Outputs {
			outputPaths[step.Workflow.ID] = append(outputPaths[step.Workflow.ID], output.RelativePath)
		}
	}
	if len(outputPaths["sequence_render"]) != 2 || len(outputPaths["qc"]) != 1 || len(outputPaths["organize"]) != 1 || len(outputPaths["archive_catalog"]) != 1 {
		t.Fatalf("Windows output records are incomplete: %+v", outputPaths)
	}
	workingCapability, err := archive.OpenStorageRoot(workingRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer workingCapability.Close()
	qcPayload, err := workingCapability.ReadFile(outputPaths["qc"][0])
	if err != nil || !strings.Contains(string(qcPayload), `"outcome":"PASSED"`) {
		t.Fatalf("Windows QC evidence is incomplete: %s err=%v", qcPayload, err)
	}
	metadataPayload, err := workingCapability.ReadFile(outputPaths["organize"][0])
	var metadata archivebundle.Metadata
	if err != nil || json.Unmarshal(metadataPayload, &metadata) != nil || metadata.Disposition != vocabulary.OrganizationCompileSourceSequence {
		t.Fatalf("Windows organized bundle is incomplete: %s err=%v", metadataPayload, err)
	}
	catalogPayload, err := workingCapability.ReadFile(outputPaths["archive_catalog"][0])
	if err != nil {
		t.Fatal(err)
	}
	packages := windowsCSVRows(t, catalogPayload, 1)
	if packages[0]["archive_package_id"] != metadata.PackageID || packages[0]["result_video_path"] != metadata.VideoPath || packages[0]["source_count"] != "3" {
		t.Fatalf("catalog does not identify the sequence package: %+v", packages[0])
	}
	info, err := workingCapability.ReadFile(metadata.VideoInfoPath)
	if err != nil {
		t.Fatal(err)
	}
	details := windowsCSVRows(t, info, len(originalChecksums))
	seen := make(map[string]bool)
	for _, row := range details {
		name := row["source_relative_path"]
		before, known := originalChecksums[name]
		if !known || seen[name] || row["source_name"] != filepath.Base(name) || row["source_checksum"] != before {
			t.Fatalf("unexpected source detail: %+v", row)
		}
		seen[name] = true
	}
	sourceCapability, err = archive.OpenStorageRoot(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceCapability.Close()
	for relativePath, before := range originalChecksums {
		_, after, err := sourceCapability.FileSHA256(relativePath)
		if err != nil || after != before {
			t.Fatalf("source changed across Windows run: %s before=%s after=%s err=%v", relativePath, before, after, err)
		}
	}
	hostname, _ := os.Hostname()
	evidence, _ := json.Marshal(map[string]any{
		"requirement_id": "APP-005", "host": hostname, "job_id": jobID,
		"worker_restarted": true, "sequence_output_count": len(outputPaths["sequence_render"]),
		"qc": "PASSED", "catalog_rows": len(packages), "source_detail_rows": len(details), "source_checksum_unchanged": true,
	})
	t.Logf("acceptance_evidence=%s", evidence)
}

type driveJobView struct {
	Data struct {
		Steps []struct {
			ID              string                    `json:"id"`
			State           workflow.StepState        `json:"state"`
			FailureEvidence []job.StepFailureEvidence `json:"failure_evidence"`
		} `json:"steps"`
	} `json:"data"`
}

func (view driveJobView) stepState(stepID string) workflow.StepState {
	for _, step := range view.Data.Steps {
		if step.ID == stepID {
			return step.State
		}
	}
	return ""
}

func readDriveJob(t *testing.T, handler http.Handler, jobID string) driveJobView {
	t.Helper()
	response := runtimeRequest(handler, http.MethodGet, "/api/v1/jobs/"+jobID, "", nil, "", "")
	var view driveJobView
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &view) != nil {
		return driveJobView{}
	}
	for _, step := range view.Data.Steps {
		if step.State == workflow.Failed {
			if n := len(step.FailureEvidence); n > 0 {
				last := step.FailureEvidence[n-1]
				t.Fatalf("Windows Job %s Step %s failed: cause=%s evidence=%s log=%s", jobID, step.ID, last.CauseCode, last.EvidenceID, last.LogReference)
			}
			t.Fatalf("Windows Job %s Step %s failed without failure evidence", jobID, step.ID)
		}
	}
	return view
}

func waitForDriveJobID(t *testing.T, handler http.Handler, workspaceDriveID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	lastStatus := 0
	lastBody := ""
	for time.Now().Before(deadline) {
		response := runtimeRequest(handler, http.MethodGet, "/api/v1/archive-intake", "", nil, "", "")
		lastStatus = response.Code
		lastBody = response.Body.String()
		var payload struct {
			Data []struct {
				WorkspaceDriveID string `json:"workspace_drive_id"`
				Folders          []struct {
					JobID string `json:"job_id"`
				} `json:"folders"`
			} `json:"data"`
		}
		if response.Code == http.StatusOK && json.Unmarshal(response.Body.Bytes(), &payload) == nil {
			for _, drive := range payload.Data {
				if drive.WorkspaceDriveID == workspaceDriveID && len(drive.Folders) == 1 && drive.Folders[0].JobID != "" {
					return drive.Folders[0].JobID
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for Archive Organization Job ID: status=%d body=%s", lastStatus, lastBody)
	return ""
}

func windowsCSVRows(t *testing.T, payload []byte, count int) []map[string]string {
	t.Helper()
	records, err := csv.NewReader(bytes.NewReader(payload)).ReadAll()
	if err != nil || len(records) != count+1 {
		t.Fatalf("CSV expected %d data rows, got %d records including header: %v", count, len(records), err)
	}
	var rows []map[string]string
	for _, record := range records[1:] {
		fields := make(map[string]string, len(record))
		for column, header := range records[0] {
			fields[header] = record[column]
		}
		rows = append(rows, fields)
	}
	return rows
}
