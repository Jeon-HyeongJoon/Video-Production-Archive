//go:build windows

package controlruntime_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/adapter/bootconfig"
	"videoarchive/internal/adapter/controlruntime"
	"videoarchive/internal/adapter/ossecret"
	"videoarchive/internal/adapter/sqlitecommand"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

// Requirement: APP-007
func TestWindowsRecordingCompletesRenderingTranscriptionAndOrganization(t *testing.T) {
	if os.Getenv("ARCHIVE_WINDOWS_RECORDING_ACCEPTANCE") != "1" {
		t.Skip("set ARCHIVE_WINDOWS_RECORDING_ACCEPTANCE=1 on the isolated Windows acceptance host")
	}
	mediaExecutable := requireWindowsAcceptanceExecutable(t, "ARCHIVE_MEDIA_PATH")
	ffmpegExecutable := requireWindowsAcceptanceExecutable(t, "ARCHIVE_FFMPEG_PATH")
	ffprobeExecutable := requireWindowsAcceptanceExecutable(t, "ARCHIVE_FFPROBE_PATH")
	whisperExecutable := requireWindowsAcceptanceExecutable(t, "ARCHIVE_WHISPER_PATH")
	whisperModel := requireWindowsAcceptanceExecutable(t, "ARCHIVE_WHISPER_MODEL_PATH")
	koreanAudio := requireWindowsAcceptanceExecutable(t, "ARCHIVE_KOREAN_AUDIO_PATH")

	directory := t.TempDir()
	sourceRoot := filepath.Join(directory, "source")
	workingRoot := filepath.Join(directory, "working")
	sourceFolder := filepath.Join(sourceRoot, "recordings")
	for _, value := range []string{sourceFolder, workingRoot} {
		if err := os.MkdirAll(value, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sourcePath := filepath.Join(sourceFolder, "event-master.mov")
	fixture := exec.Command(
		ffmpegExecutable, "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc=size=1280x720:rate=30", "-i", koreanAudio,
		"-c:v", "mpeg4", "-q:v", "3", "-c:a", "pcm_s16le", "-shortest", sourcePath,
	)
	if output, err := fixture.CombinedOutput(); err != nil {
		t.Fatalf("create Windows recording fixture: %v: %s", err, output)
	}
	sourceCapability, err := archive.OpenStorageRoot(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	_, originalChecksum, err := sourceCapability.FileSHA256("recordings/event-master.mov")
	sourceCapability.Close()
	if err != nil {
		t.Fatal(err)
	}

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
    - id: worker-windows-recording
      enabled: true
      credential_ref: wincred://worker/windows-recording
      processing_kinds: [INSPECT, RENDER, QC, AUDIO_EXTRACT, STT, ORGANIZE]
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
    version: ffprobe-windows-recording-acceptance
`, yamlScalar(filepath.Join(directory, "control.db")), yamlScalar(sourceRoot), yamlScalar(workingRoot), yamlScalar(ffprobeExecutable))
	if err := os.WriteFile(controlPath, []byte(controlDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := bootconfig.Load(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	credential := "windows-recording-acceptance-secret"
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "windows-recording-acceptance", controlruntime.Dependencies{
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
	workerPath := filepath.Join(directory, "worker.yaml")
	workerDocument := fmt.Sprintf(`schema_version: 1
worker:
  id: worker-windows-recording
  control_url: %s
  credential_ref: %s
  heartbeat_interval: 1s
  claim_interval: 1s
  storage_roots:
    - storage_id: source-windows
      root: %s
    - storage_id: archive-windows
      root: %s
    - storage_id: models-windows
      root: %s
  tools:
    ffmpeg:
      executable: %s
      version: ffmpeg-windows-recording-acceptance
    ffprobe:
      executable: %s
      version: ffprobe-windows-recording-acceptance
    whispercpp:
      executable: %s
      version: whisper.cpp-windows-recording-acceptance
      model_storage_id: models-windows
      model_relative_path: %s
      profile:
        id: event-ko
        version: 1
        model_id: ggml-large-v3-turbo-q5_0
        language: ko
        thread_count: 4
`, yamlScalar(controlServer.URL), yamlScalar(credentialReference), yamlScalar(sourceRoot), yamlScalar(workingRoot),
		yamlScalar(filepath.Dir(whisperModel)), yamlScalar(ffmpegExecutable), yamlScalar(ffprobeExecutable),
		yamlScalar(whisperExecutable), yamlScalar(filepath.Base(whisperModel)))
	if err := os.WriteFile(workerPath, []byte(workerDocument), 0o600); err != nil {
		t.Fatal(err)
	}

	workerProcess := startWindowsAcceptanceProcess(t, mediaExecutable, "runner", "-config", workerPath)
	defer workerProcess.Stop(t)
	waitForWindowsAcceptance(t, 20*time.Second, "recording production Worker ONLINE", func() bool {
		response := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/workers", "", nil, "", "")
		return response.Code == http.StatusOK && strings.Contains(response.Body.String(), `"worker_id":"worker-windows-recording"`) &&
			(strings.Contains(response.Body.String(), `"state":"ONLINE"`) || strings.Contains(response.Body.String(), `"state":"BUSY"`))
	})

	cookie, csrfToken := runtimeSession(t, runtime.Handler())
	startBody := `{"archive_run_id":"RUN-WINDOWS-RECORDING","idempotency_key":"windows-recording:start","session_id":"SESSION-WINDOWS-RECORDING","source_volume_id":"source-volume","expected_candidate_revision":1,"archive_storage_id":"archive-windows","input_revision":1,"folder_ids":["recordings"]}`
	start := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archive-intake/runs", startBody, cookie, csrfToken, "http://127.0.0.1:8080")
	if start.Code != http.StatusCreated {
		t.Fatalf("start Windows recording organization returned %d: %s", start.Code, start.Body.String())
	}
	jobID := waitForDriveJobID(t, runtime.Handler(), "source-volume", 10*time.Second)
	waitForWindowsAcceptance(t, 2*time.Minute, "recording stage-one render, QC, package, and catalog completion", func() bool {
		detail := readDriveJob(t, runtime.Handler(), jobID)
		for _, stepID := range []string{"render", "qc", "organize", "archive_catalog"} {
			if detail.stepState(stepID) != workflow.Completed {
				return false
			}
		}
		return true
	})
	transcription := runtimeRequest(
		runtime.Handler(), http.MethodPost, "/api/v1/archive-runs/RUN-WINDOWS-RECORDING/transcription",
		`{"idempotency_key":"windows-recording:transcription","expected_archive_run_revision":1}`,
		cookie, csrfToken, "http://127.0.0.1:8080",
	)
	var transcriptionResult struct {
		Data struct {
			JobIDs        []string `json:"job_ids"`
			EligibleCount int      `json:"eligible_count"`
			HoldCount     int      `json:"hold_count"`
		} `json:"data"`
	}
	if transcription.Code != http.StatusCreated || json.Unmarshal(transcription.Body.Bytes(), &transcriptionResult) != nil ||
		len(transcriptionResult.Data.JobIDs) != 1 || transcriptionResult.Data.EligibleCount != 1 || transcriptionResult.Data.HoldCount != 0 {
		t.Fatalf("start separate Windows transcription returned %d: %s", transcription.Code, transcription.Body.String())
	}
	transcriptionJobID := transcriptionResult.Data.JobIDs[0]
	waitForWindowsAcceptance(t, 5*time.Minute, "recording stage-two STT, package revision, and catalog completion", func() bool {
		detail := readDriveJob(t, runtime.Handler(), transcriptionJobID)
		for _, stepID := range []string{"audio", "transcript", "subtitle", "transcript_text", "archive_package_revision", "archive_catalog"} {
			if detail.stepState(stepID) != workflow.Completed {
				return false
			}
		}
		return true
	})

	store, err := sqlitecommand.Open(configuration.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	outputPaths := make(map[string][]string)
	workerOutputRecordCount := 0
	for _, completedJobID := range []string{jobID, transcriptionJobID} {
		snapshot, loadErr := store.Load(completedJobID)
		if loadErr != nil {
			store.Close()
			t.Fatal(loadErr)
		}
		state, decodeErr := job.Decode(snapshot)
		if decodeErr != nil {
			store.Close()
			t.Fatal(decodeErr)
		}
		for _, step := range state.Steps {
			if step.ExecutorKind != vocabulary.ExecutorWorker {
				continue
			}
			if step.ExecutionSpec == nil || step.OutputRecord == nil {
				store.Close()
				t.Fatalf("completed Windows recording Step lacks execution evidence: %s", step.Workflow.ID)
			}
			if step.Workflow.ID == "render" && step.ExecutionSpec.ProfileReference != "archive-1080p-6mbps@v1" {
				store.Close()
				t.Fatalf("recording did not use the default 6Mbps Profile: %+v", step.ExecutionSpec)
			}
			workerOutputRecordCount++
			for _, output := range step.OutputRecord.Outputs {
				outputPaths[step.Workflow.ID] = append(outputPaths[step.Workflow.ID], output.RelativePath)
			}
		}
	}
	store.Close()
	for _, stepID := range []string{"render", "qc", "organize", "audio", "transcript", "subtitle", "transcript_text", "archive_package_revision"} {
		if len(outputPaths[stepID]) != 1 {
			t.Fatalf("Windows recording output record for %s is incomplete: %+v", stepID, outputPaths)
		}
	}
	if len(outputPaths["archive_catalog"]) != 2 || workerOutputRecordCount != 10 {
		t.Fatalf("Windows two-stage output records are incomplete: count=%d outputs=%+v", workerOutputRecordCount, outputPaths)
	}

	workingCapability, err := archive.OpenStorageRoot(workingRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer workingCapability.Close()
	qcPayload, err := workingCapability.ReadFile(outputPaths["qc"][0])
	if err != nil || !strings.Contains(string(qcPayload), `"outcome":"PASSED"`) ||
		!strings.Contains(string(qcPayload), `"profile_reference":"archive-1080p-6mbps@v1"`) {
		t.Fatalf("Windows recording QC evidence is incomplete: %s err=%v", qcPayload, err)
	}
	transcriptPayload, err := workingCapability.ReadFile(outputPaths["transcript"][0])
	if err != nil {
		t.Fatal(err)
	}
	var transcript struct {
		ProfileReference string `json:"profile_reference"`
		Content          struct {
			Language string `json:"language"`
			Segments []struct {
				Text string `json:"text"`
			} `json:"segments"`
		} `json:"content"`
	}
	if json.Unmarshal(transcriptPayload, &transcript) != nil || transcript.ProfileReference != "event-ko@v1" ||
		transcript.Content.Language != "ko" || len(transcript.Content.Segments) == 0 ||
		strings.TrimSpace(transcript.Content.Segments[0].Text) == "" {
		t.Fatalf("Windows recording Transcript is incomplete: %s", transcriptPayload)
	}
	subtitlePayload, err := workingCapability.ReadFile(outputPaths["subtitle"][0])
	if err != nil || !strings.Contains(string(subtitlePayload), " --> ") {
		t.Fatalf("Windows recording Subtitle is incomplete: %s err=%v", subtitlePayload, err)
	}
	stageOneMetadataPayload, err := workingCapability.ReadFile(outputPaths["organize"][0])
	if err != nil {
		t.Fatal(err)
	}
	var bundle struct {
		Revision           uint64 `json:"revision"`
		VideoPath          string `json:"video_path"`
		TranscriptPath     string `json:"transcript_path"`
		TranscriptTextPath string `json:"transcript_text_path"`
		SubtitlePath       string `json:"subtitle_path"`
	}
	if json.Unmarshal(stageOneMetadataPayload, &bundle) != nil || bundle.Revision != 1 || bundle.VideoPath == "" ||
		bundle.TranscriptPath != "" || bundle.TranscriptTextPath != "" || bundle.SubtitlePath != "" {
		t.Fatalf("Windows stage-one stored package is incomplete: %s", stageOneMetadataPayload)
	}
	revisionMetadataPath := outputPaths["archive_package_revision"][0]
	metadataPayload, err := workingCapability.ReadFile(revisionMetadataPath)
	if err != nil || json.Unmarshal(metadataPayload, &bundle) != nil || bundle.Revision != 2 ||
		path.Dir(bundle.VideoPath) != path.Dir(bundle.TranscriptPath) || path.Dir(bundle.VideoPath) != path.Dir(bundle.TranscriptTextPath) ||
		path.Dir(bundle.VideoPath) != path.Dir(bundle.SubtitlePath) || path.Dir(bundle.VideoPath) != path.Dir(revisionMetadataPath) {
		t.Fatalf("Windows recording revised package paths are incomplete: %s err=%v", metadataPayload, err)
	}
	for _, relativePath := range []string{bundle.VideoPath, bundle.TranscriptPath, bundle.TranscriptTextPath, bundle.SubtitlePath, revisionMetadataPath} {
		if size, _, err := workingCapability.FileSHA256(relativePath); err != nil || size <= 0 {
			t.Fatalf("Windows recording bundle file %s is unavailable: size=%d err=%v", relativePath, size, err)
		}
	}
	catalogPayload, err := workingCapability.ReadFile(outputPaths["archive_catalog"][1])
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(strings.NewReader(string(catalogPayload))).ReadAll()
	if err != nil || len(records) != 2 || !strings.Contains(string(catalogPayload), bundle.TranscriptPath) ||
		!strings.Contains(string(catalogPayload), bundle.TranscriptTextPath) || !strings.Contains(string(catalogPayload), bundle.SubtitlePath) {
		t.Fatalf("Windows recording ArchiveCatalog is incomplete: rows=%d err=%v payload=%s", len(records), err, catalogPayload)
	}
	sourceCapability, err = archive.OpenStorageRoot(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceCapability.Close()
	_, finalChecksum, err := sourceCapability.FileSHA256("recordings/event-master.mov")
	if err != nil || finalChecksum != originalChecksum {
		t.Fatalf("Windows recording source changed: before=%s after=%s err=%v", originalChecksum, finalChecksum, err)
	}
	hostname, _ := os.Hostname()
	evidence, _ := json.Marshal(map[string]any{
		"requirement_id": "APP-007", "host": hostname, "stage_one_job_id": jobID, "stage_two_job_id": transcriptionJobID,
		"profile": "archive-1080p-6mbps@v1", "qc": "PASSED", "transcript_language": transcript.Content.Language,
		"transcript_segments": len(transcript.Content.Segments), "package_revision": bundle.Revision, "bundle_file_count": 5,
		"catalog_rows": len(records) - 1, "worker_step_output_records": workerOutputRecordCount, "source_checksum_unchanged": true,
	})
	t.Logf("acceptance_evidence=%s", evidence)
}
