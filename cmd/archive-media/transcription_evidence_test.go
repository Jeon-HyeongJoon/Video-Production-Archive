package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/core/execution"
)

func TestMain(m *testing.M) {
	mode := os.Getenv("ARCHIVE_TEST_STT_ENGINE")
	if mode == "" {
		os.Exit(m.Run())
	}
	var prefix string
	for i := 1; i+1 < len(os.Args); i++ {
		if os.Args[i] == "-of" {
			prefix = os.Args[i+1]
		}
	}
	if prefix == "" || os.Args[len(os.Args)-1] != "-pp" {
		os.Exit(3)
	}
	fmt.Fprintln(os.Stdout, "private-transcript-sentinel")
	fmt.Fprintln(os.Stderr, "private-diagnostic-sentinel")
	fmt.Fprintln(os.Stderr, "whisper_print_progress_callback: progress =  20%")
	fmt.Fprintln(os.Stderr, "whisper_print_progress_callback: progress =  21%")
	fmt.Fprint(os.Stderr, "whisper_print_progress_callback: progress =  80%\r")
	switch mode {
	case "output-open", "other-output":
		name := prefix + ".json"
		if mode == "other-output" {
			name += ".unrelated"
		} else if err := os.Mkdir(name, 0700); err != nil {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "open: failed to open '%s' for writing\n", name)
		os.Exit(11)
	case "bad-model":
		fmt.Fprintln(os.Stderr, "whisper_model_load: invalid model data (bad magic)")
		os.Exit(3)
	}
	if mode == "exit" {
		os.Exit(17)
	}
	if mode == "missing" {
		os.Exit(0)
	}
	payload := `{"result":{"language":"ko"},"transcription":[{"offsets":{"from":0,"to":500},"text":"private-transcript-sentinel"}]}`
	if mode == "korean" {
		payload = `{"result":{"language":"ko"},"transcription":[{"offsets":{"from":0,"to":1600},"text":"행사 녹화"},{"offsets":{"from":1700,"to":2000},"text":"범위 밖"}]}`
	}
	if mode == "bad-utf8" {
		payload = strings.Replace(payload, "private-transcript-sentinel", "깨진 "+string([]byte{0xff}), 1)
	}
	if mode == "invalid" {
		payload = "{"
	}
	if err := os.WriteFile(prefix+".json", []byte(payload), 0600); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

type cliSample struct {
	JobID    string                   `json:"job_id"`
	StepID   string                   `json:"step_id"`
	Token    uint64                   `json:"execution_token"`
	Sequence uint64                   `json:"report_sequence"`
	At       time.Time                `json:"observed_at"`
	Update   execution.ProgressUpdate `json:"update"`
}

func runTranscriptionFixture(t *testing.T, mode string) (int, commandResult, []cliSample) {
	t.Helper()
	root, logs, source := t.TempDir(), t.TempDir(), t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`schema_version: 1
media:
  id: test-media
  storage_roots: [{storage_id: media, root: %s}, {storage_id: source, root: %s}]
  process_logs: {root: %s, max_file_bytes: 4096, tail_bytes: 1024, retention: 1h, max_total_bytes: 16384}
  tools:
    ffmpeg: {executable: %s, version: fixture}
    ffprobe: {executable: %s, version: fixture}
    whispercpp:
      executable: %s
      version: fixture
      model_storage_id: media
      model_relative_path: model.bin
      profile: {id: ko, version: 1, model_id: model, language: ko, thread_count: 1}
`, strconv.Quote(root), strconv.Quote(source), strconv.Quote(logs), strconv.Quote(exe), strconv.Quote(exe), strconv.Quote(exe))
	write := func(name string, payload []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), payload, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("media.yaml", []byte(config))
	write("model.bin", []byte("fixture"))
	wav := make([]byte, 44+32000)
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], 16000)
	binary.LittleEndian.PutUint32(wav[28:], 32000)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], 32000)
	if mode == "bad-audio" {
		wav = []byte("invalid WAV")
	}
	write("source.wav", wav)
	spec := execution.StepExecutionSpec{JobID: "JOB-STT", StepID: "transcript", TemplateName: "recording-transcription", Operation: execution.OperationTranscribeAudio, ProfileReference: "ko@v1", Parameters: execution.StepExecutionParameters{OrganizationDecisionID: "D1", WorkspaceDriveID: "V1", InputRevision: 1}}
	for i := 1; i <= 2; i++ {
		asset := fmt.Sprintf("ASSET-%d", i)
		spec.Inputs = append(spec.Inputs, execution.InputReference{OriginalAssetID: asset, ArtifactID: "AUDIO-" + asset, Kind: execution.OutputAudio, StorageID: "media", RelativePath: "source.wav", Checksum: fmt.Sprintf("%x", sha256.Sum256(wav))})
		spec.Outputs = append(spec.Outputs, execution.OutputTarget{OriginalAssetID: asset, ID: "TEXT-" + asset, Kind: execution.OutputTranscript, StorageID: "media", RelativePath: "out/" + asset + ".json"})
	}
	if mode == "output-open" || mode == "other-output" {
		spec.Outputs[0].RelativePath = "staging/" + strings.Repeat("a", 100) + "/" + strings.Repeat("b", 100) + "/transcript.json"
	}
	var verifyPackage func(commandResult)
	if mode == "bad-utf8" || mode == "korean" {
		spec.Inputs, spec.Outputs = spec.Inputs[:1], spec.Outputs[:1]
		verifyPackage = prepareSTTPackage(t, root, source, spec.Inputs[0], spec.Outputs[0])
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARCHIVE_TEST_STT_ENGINE", mode)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"transcribe", "--config", filepath.Join(root, "media.yaml"), "--execution-token", "19"}, bytes.NewReader(data), &stdout, &stderr)
	for _, secret := range []string{"private-transcript-sentinel", "private-diagnostic-sentinel", root} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Fatal("operation stream disclosed private data")
		}
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("invalid result: %v, stderr=%s", err, stderr.String())
	}
	var samples []cliSample
	for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		var s cliSample
		if err := json.Unmarshal([]byte(line), &s); err != nil || s.JobID != spec.JobID || s.StepID != spec.StepID || s.Token != 19 || s.Sequence != uint64(len(samples)+1) || s.At.IsZero() || s.Update.Validate() != nil {
			t.Fatalf("invalid observation %s: %v", line, err)
		}
		samples = append(samples, s)
	}
	if result.ProcessLog != nil {
		raw, err := os.ReadFile(filepath.Join(logs, "process", result.ProcessLog.ID+".log"))
		if err != nil || bytes.Contains(raw, []byte("private-transcript-sentinel")) || !bytes.Contains(raw, []byte("private-diagnostic-sentinel")) || result.ProcessLog.SanitizedTail != "" {
			t.Fatalf("unexpected process evidence: %v %+v", err, result.ProcessLog)
		}
	}
	if verifyPackage != nil {
		verifyPackage(result)
	}
	return code, result, samples
}

// Requirement: TRN-008
func TestTranscriptionReportsObservedEngineProgressForEachInput(t *testing.T) {
	code, r, samples := runTranscriptionFixture(t, "success")
	if code != 0 || !r.Success || len(r.VerificationReferences) != 2 {
		t.Fatalf("transcription result: %+v", r)
	}
	var engines []string
	var last float64
	for _, s := range samples {
		u := s.Update
		if u.Status.IsPhase() {
			continue
		}
		if u.InputCount != 2 || u.InputIndex == 0 || u.SourceEndMillis != 1000 || u.StepFraction() < last {
			t.Fatalf("invalid input observation: %+v", u)
		}
		last = u.StepFraction()
		if u.Status == execution.ProgressProcessing {
			engines = append(engines, fmt.Sprintf("%d:%d", u.InputIndex, u.ProcessPercent))
		}
	}
	if strings.Join(engines, ",") != "1:20,1:80,2:20,2:80" || last != 1 {
		t.Fatalf("engine observations %v, final fraction %f", engines, last)
	}
}

// Requirement: TRN-009
func TestTranscriptionFailureIdentifiesLastObservedBoundaryAndSafeCause(t *testing.T) {
	t.Run("valid Korean publishes one revision", func(t *testing.T) {
		code, result, _ := runTranscriptionFixture(t, "korean")
		if code != 0 || !result.Success || len(result.Outputs) != 1 {
			t.Fatalf("Korean transcription: %+v", result)
		}
	})
	openCause := "WHISPER_OUTPUT_OPEN_FAILED"
	if runtime.GOOS == "windows" {
		openCause = "WHISPER_LONG_OUTPUT_OPEN_FAILED"
	}
	for _, tc := range []struct{ mode, cause string }{{"output-open", openCause}, {"other-output", "PROCESS_EXIT_NONZERO"}, {"bad-model", "WHISPER_MODEL_INVALID"}, {"bad-audio", "WHISPER_CHUNK_PLAN_FAILED"}, {"exit", "PROCESS_EXIT_NONZERO"}, {"invalid", "WHISPER_OUTPUT_INVALID"}, {"bad-utf8", "WHISPER_OUTPUT_INVALID"}, {"missing", "WHISPER_OUTPUT_UNAVAILABLE"}} {
		t.Run(tc.mode, func(t *testing.T) {
			code, r, samples := runTranscriptionFixture(t, tc.mode)
			if code != 1 || r.Success || r.CauseCode != tc.cause || len(r.VerificationReferences) != 0 {
				t.Fatalf("failure result: %+v", r)
			}
			if tc.mode == "output-open" && (!strings.Contains(r.ErrorSummary, "target was a directory") || (runtime.GOOS == "windows" && !strings.Contains(r.ErrorSummary, "extended path used: true"))) {
				t.Fatalf("missing output observations: %+v", r)
			}
			last := samples[len(samples)-1].Update
			if tc.mode == "bad-audio" {
				if last.Status != execution.ProgressExecuting {
					t.Fatal(last)
				}
				return
			}
			if last.Status != execution.ProgressFailed || last.InputIndex != 1 || last.ProcessPercent != 80 {
				t.Fatalf("lost failure position: %+v", last)
			}
			if tc.mode == "output-open" || tc.mode == "other-output" || tc.mode == "bad-model" {
				want := 11
				if tc.mode == "bad-model" {
					want = 3
				}
				if r.ProcessExitCode == nil || *r.ProcessExitCode != want || r.ProcessLog == nil || r.ProcessLog.State != execution.ProcessLogAvailable {
					t.Fatalf("lost failure evidence: %+v", r)
				}
			}
			if tc.mode == "exit" && (r.ProcessExitCode == nil || *r.ProcessExitCode != 17 || r.ProcessLog == nil || r.ProcessLog.State != execution.ProcessLogAvailable) {
				t.Fatalf("lost process failure: %+v", r)
			}
		})
	}
}
