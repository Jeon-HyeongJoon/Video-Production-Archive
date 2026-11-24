//go:build windows

package controlruntime_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
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
	"videoarchive/internal/core/execution"
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
	// Keep the native whisper JSON above MAX_PATH even with a short TEMP root.
	workingRoot := filepath.Join(directory, "working")
	for len(workingRoot) < 152 {
		workingRoot = filepath.Join(workingRoot, "recording-archive")
	}
	const folderID = "261124-office/PGM"
	const sourceRelative = folderID + "/event-master.mov"
	sourceFolder := filepath.Join(sourceRoot, filepath.FromSlash(folderID))
	for _, value := range []string{sourceFolder, workingRoot} {
		if err := os.MkdirAll(value, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sourcePath := filepath.Join(sourceFolder, "event-master.mov")
	fixture := exec.Command(
		ffmpegExecutable, "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc=size=640x360:rate=30", "-i", koreanAudio, "-t", "8",
		"-c:v", "mpeg4", "-q:v", "3", "-c:a", "pcm_s16le", "-shortest", sourcePath,
	)
	if output, err := fixture.CombinedOutput(); err != nil {
		t.Fatalf("create Windows recording fixture: %v: %s", err, output)
	}
	sourceCapability, err := archive.OpenReadOnlyStorageRoot(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	_, originalChecksum, err := sourceCapability.FileSHA256(sourceRelative)
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
	workingCapability, err := archive.OpenStorageRoot(workingRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer workingCapability.Close()
	// Both revisions publish to the same paths. Observe the first publication
	// at its result boundary, before Control can dispatch the next revision.
	firstPublication := map[string]chan []byte{"organize": make(chan []byte, 1), "archive_catalog": make(chan []byte, 1)}
	controlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/worker/v1/steps/result" {
			raw, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var result struct {
				StepID string                      `json:"step_id"`
				Output *execution.StepOutputRecord `json:"output_record"`
			}
			if json.Unmarshal(raw, &result) == nil && firstPublication[result.StepID] != nil && result.Output != nil && len(result.Output.Outputs) == 1 {
				out := result.Output.Outputs[0]
				payload, err := workingCapability.ReadFile(out.RelativePath)
				if err != nil || fmt.Sprintf("%x", sha256.Sum256(payload)) != out.Checksum {
					t.Errorf("published %s differs from Worker receipt: %v", result.StepID, err)
				}
				select {
				case firstPublication[result.StepID] <- payload:
				default:
				}
			}
		}
		runtime.Handler().ServeHTTP(w, r)
	}))
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
  id: worker-windows-recording
  control_url: %s
  credential_ref: %s
  heartbeat_interval: 1s
  claim_interval: 1s
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
`, yamlScalar(controlServer.URL), yamlScalar(credentialReference), yamlScalar(processLogRoot), yamlScalar(sourceRoot), yamlScalar(workingRoot),
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
	candidateRevision, inputRevision := windowsInventoryRevisions(t, runtime.Handler(), "source-volume")
	startBody := fmt.Sprintf(`{"archive_run_id":"RUN-WINDOWS-RECORDING","idempotency_key":"windows-recording:start","session_id":"SESSION-WINDOWS-RECORDING","source_volume_id":"source-volume","expected_candidate_revision":%d,"archive_storage_id":"archive-windows","input_revision":%d,"folder_ids":[%q]}`, candidateRevision, inputRevision, folderID)
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
	var transcriptionJobID string
	waitForWindowsAcceptance(t, 20*time.Second, "Control automatic recording transcription continuation", func() bool {
		jobs := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs", "", nil, "", "")
		if jobs.Code != http.StatusOK {
			return false
		}
		var projection struct {
			Data []struct {
				JobID        string `json:"job_id"`
				TemplateName string `json:"template_name"`
			} `json:"data"`
		}
		if json.Unmarshal(jobs.Body.Bytes(), &projection) != nil {
			return false
		}
		for _, projected := range projection.Data {
			if projected.TemplateName == "archive-transcription" {
				transcriptionJobID = projected.JobID
				return transcriptionJobID != ""
			}
		}
		return false
	})
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
	captured := func(stepID string) []byte {
		t.Helper()
		select {
		case payload := <-firstPublication[stepID]:
			return payload
		default:
			t.Fatalf("missing first publication for %s", stepID)
			return nil
		}
	}
	var initial, bundle archivebundle.Metadata
	if err := json.Unmarshal(captured("organize"), &initial); err != nil || initial.Revision != 1 || initial.PackageID == "" || initial.VideoPath == "" ||
		initial.TranscriptPath != "" || initial.TranscriptTextPath != "" || initial.SubtitlePath != "" {
		t.Fatalf("initial package metadata: %+v err=%v", initial, err)
	}
	firstRow := windowsCSVRows(t, captured("archive_catalog"), 1)[0]
	if firstRow["package_revision"] != "1" || firstRow["archive_package_id"] != initial.PackageID || firstRow["result_video_path"] != initial.VideoPath ||
		firstRow["transcript_path"] != "" || firstRow["transcript_text_path"] != "" || firstRow["subtitle_path"] != "" {
		t.Fatalf("initial catalog: %+v", firstRow)
	}
	revisionMetadataPath := outputPaths["archive_package_revision"][0]
	metadataPayload, err := workingCapability.ReadFile(revisionMetadataPath)
	if err != nil || json.Unmarshal(metadataPayload, &bundle) != nil || bundle.Revision != 2 || bundle.PackageID != initial.PackageID || bundle.VideoPath != initial.VideoPath ||
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
	rows := windowsCSVRows(t, catalogPayload, 1)
	finalRow := rows[0]
	if finalRow["package_revision"] != "2" || finalRow["archive_package_id"] != bundle.PackageID || finalRow["result_video_path"] != bundle.VideoPath ||
		finalRow["transcript_path"] != bundle.TranscriptPath || finalRow["transcript_text_path"] != bundle.TranscriptTextPath || finalRow["subtitle_path"] != bundle.SubtitlePath {
		t.Fatalf("revised catalog: %+v", finalRow)
	}
	assertWindowsCatalogCurrent(t, workingCapability, bundle)
	sourceCapability, err = archive.OpenReadOnlyStorageRoot(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceCapability.Close()
	_, finalChecksum, err := sourceCapability.FileSHA256(sourceRelative)
	if err != nil || finalChecksum != originalChecksum {
		t.Fatalf("Windows recording source changed: before=%s after=%s err=%v", originalChecksum, finalChecksum, err)
	}
	hostname, _ := os.Hostname()
	evidence, _ := json.Marshal(map[string]any{
		"requirement_id": "APP-007", "host": hostname, "stage_one_job_id": jobID, "stage_two_job_id": transcriptionJobID,
		"profile": "archive-1080p-6mbps@v1", "qc": "PASSED", "transcript_language": transcript.Content.Language,
		"transcript_segments": len(transcript.Content.Segments), "initial_package_revision": initial.Revision, "package_revision": bundle.Revision, "bundle_file_count": 5,
		"catalog_rows": len(rows), "worker_step_output_records": workerOutputRecordCount, "source_checksum_unchanged": true,
	})
	t.Logf("acceptance_evidence=%s", evidence)
}
