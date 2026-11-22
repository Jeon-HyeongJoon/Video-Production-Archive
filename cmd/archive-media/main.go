package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"time"

	"videoarchive/internal/adapter/hostresource"
	"videoarchive/internal/adapter/ossecret"
	"videoarchive/internal/adapter/storageinventory"
	"videoarchive/internal/adapter/windowsvolume"
	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/workerclient"
	mediaexecutor "videoarchive/internal/application/workerexecutor"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

var version = "dev"

var operationCommands = map[string]execution.Operation{
	"inspect":         execution.OperationInspectMedia,
	"render":          execution.OperationRenderRecording,
	"sequence":        execution.OperationRenderSourceSequence,
	"qc":              execution.OperationQCRendition,
	"package":         execution.OperationOrganizeMedia,
	"audio":           execution.OperationExtractSpeechAudio,
	"transcribe":      execution.OperationTranscribeAudio,
	"subtitle":        execution.OperationWriteSubtitle,
	"transcript-text": execution.OperationWriteTranscriptText,
	"package-revise":  execution.OperationReviseArchivePackage,
	"catalog":         execution.OperationWriteArchiveCatalog,
	"catalog-verify":  execution.OperationVerifyArchiveCatalog,
	"catalog-rebuild": execution.OperationRebuildArchiveCatalog,
}

type commandResult struct {
	Operation              execution.Operation                       `json:"operation"`
	Success                bool                                      `json:"success"`
	FailureClass           workflow.FailureClass                     `json:"failure_class,omitempty"`
	WaitReason             vocabulary.WaitReason                     `json:"wait_reason,omitempty"`
	WaitTargetStorageID    string                                    `json:"wait_target_storage_id,omitempty"`
	ErrorClass             string                                    `json:"error_class,omitempty"`
	CauseCode              string                                    `json:"cause_code,omitempty"`
	ErrorSummary           string                                    `json:"error_summary,omitempty"`
	ProgressStreamFailed   bool                                      `json:"progress_stream_failed,omitempty"`
	ProcessExitCode        *int                                      `json:"process_exit_code,omitempty"`
	ProcessLog             *execution.ProcessLogArtifact             `json:"process_log,omitempty"`
	LogReference           string                                    `json:"log_reference,omitempty"`
	Outputs                []execution.StepOutput                    `json:"outputs,omitempty"`
	VerificationReferences []string                                  `json:"verification_references,omitempty"`
	InspectedMedia         []execution.InspectedMedia                `json:"inspected_media,omitempty"`
	Cancellation           *workerclient.CancellationExecutionResult `json:"cancellation,omitempty"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(arguments) == 0 {
		writeUsage(stderr)
		return 2
	}
	command := strings.TrimSpace(arguments[0])
	switch command {
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, version)
		return 0
	case "operations":
		return writeOperations(stdout)
	case "check":
		return runMediaCheck(arguments[1:], stdout, stderr)
	case "runner":
		return runManagedRunner(ctx, arguments[1:], stdout, stderr)
	case "execute":
		return runOperation(ctx, arguments[1:], stdin, stdout, stderr, "")
	default:
		if command == "catalog" && len(arguments) > 1 {
			switch strings.TrimSpace(arguments[1]) {
			case "verify":
				return runOperation(ctx, arguments[2:], stdin, stdout, stderr, execution.OperationVerifyArchiveCatalog)
			case "rebuild":
				return runOperation(ctx, arguments[2:], stdin, stdout, stderr, execution.OperationRebuildArchiveCatalog)
			case "publish":
				return runOperation(ctx, arguments[2:], stdin, stdout, stderr, execution.OperationWriteArchiveCatalog)
			}
		}
		expected, exists := operationCommands[command]
		if !exists {
			fmt.Fprintf(stderr, "unknown archive-media command %q\n", command)
			writeUsage(stderr)
			return 2
		}
		return runOperation(ctx, arguments[1:], stdin, stdout, stderr, expected)
	}
}

func runOperation(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer, expected execution.Operation) int {
	flags := flag.NewFlagSet("archive-media operation", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configurationPath := flags.String("config", "media.yaml", "standalone media configuration")
	specificationPath := flags.String("spec", "-", "typed operation specification JSON path or - for stdin")
	executionToken := flags.Uint64("execution-token", 0, "non-zero execution token; generated when omitted")
	dryRun := flags.Bool("dry-run", false, "validate configuration and specification without changing files")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return 2
	}
	configuration, err := workerconfig.LoadMedia(*configurationPath)
	if err != nil {
		writeCommandError(stderr, "MEDIA_CONFIGURATION_INVALID", err)
		return 2
	}
	spec, err := readSpecification(*specificationPath, stdin)
	if err != nil {
		writeCommandError(stderr, "MEDIA_SPEC_INVALID", err)
		return 2
	}
	if expected != "" && spec.Operation != expected {
		writeCommandError(stderr, "MEDIA_OPERATION_MISMATCH", fmt.Errorf("spec operation %s does not match command %s", spec.Operation, expected))
		return 2
	}
	storageRoots, err := workerconfig.OpenStorageRoots(configuration)
	if err != nil {
		writeCommandError(stderr, "MEDIA_STORAGE_UNAVAILABLE", err)
		return 1
	}
	defer storageRoots.Close()
	executor, err := mediaexecutor.NewProduction(configuration, storageRoots)
	if err != nil {
		writeCommandError(stderr, "MEDIA_EXECUTOR_INVALID", err)
		return 2
	}
	if *dryRun {
		return writeJSON(stdout, map[string]any{
			"operation": spec.Operation, "valid": true, "spec_digest": spec.Digest,
			"storage_count": len(configuration.StorageRoots),
		})
	}
	token := *executionToken
	if token == 0 {
		token, err = randomExecutionToken()
		if err != nil {
			writeCommandError(stderr, "MEDIA_EXECUTION_TOKEN_UNAVAILABLE", err)
			return 1
		}
	}
	encoder := json.NewEncoder(stderr)
	var evidenceErr error
	var sequence uint64
	ctx = execution.WithProgressReporter(ctx, func(_ context.Context, update execution.ProgressUpdate) error {
		sequence++
		if evidenceErr == nil {
			evidenceErr = encoder.Encode(map[string]any{"job_id": spec.JobID, "step_id": spec.StepID, "execution_token": token, "report_sequence": sequence, "observed_at": time.Now().UTC(), "update": update})
		}
		return evidenceErr
	})
	result := executor.ExecuteOperation(ctx, spec, token)
	response := commandResult{
		ProgressStreamFailed: evidenceErr != nil, Operation: spec.Operation, Success: result.Success, FailureClass: result.FailureClass,
		WaitReason: result.WaitReason, WaitTargetStorageID: result.WaitTargetStorageID,
		ErrorClass: result.ErrorClass, LogReference: result.LogReference, CauseCode: result.CauseCode, ErrorSummary: result.ErrorSummary, ProcessExitCode: result.ProcessExitCode, ProcessLog: result.ProcessLog,
		Outputs: result.Outputs, VerificationReferences: result.VerificationReferences,
		InspectedMedia: result.InspectedMedia, Cancellation: result.Cancellation,
	}
	if writeJSON(stdout, response) != 0 {
		return 1
	}
	if !result.Success {
		return 1
	}
	return 0
}

func readSpecification(filePath string, stdin io.Reader) (execution.StepExecutionSpec, error) {
	var reader io.Reader = stdin
	var file *os.File
	if strings.TrimSpace(filePath) != "-" {
		var err error
		file, err = os.Open(strings.TrimSpace(filePath))
		if err != nil {
			return execution.StepExecutionSpec{}, err
		}
		defer file.Close()
		reader = file
	}
	payload, err := io.ReadAll(io.LimitReader(reader, (16<<20)+1))
	if err != nil || len(payload) == 0 || len(payload) > 16<<20 {
		return execution.StepExecutionSpec{}, errors.New("operation specification is unavailable or too large")
	}
	var source execution.StepExecutionSpec
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&source); err != nil {
		return execution.StepExecutionSpec{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return execution.StepExecutionSpec{}, errors.New("operation specification must contain exactly one JSON value")
	}
	if source.Digest != "" {
		if err := source.Validate(); err != nil {
			return execution.StepExecutionSpec{}, err
		}
		return source, nil
	}
	return execution.NewStepExecutionSpec(source)
}

func runMediaCheck(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("archive-media check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configurationPath := flags.String("config", "media.yaml", "standalone media configuration")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return 2
	}
	configuration, err := workerconfig.LoadMedia(*configurationPath)
	if err != nil {
		writeCommandError(stderr, "MEDIA_CONFIGURATION_INVALID", err)
		return 2
	}
	return writeJSON(stdout, map[string]any{"valid": true, "media_id": configuration.ID, "storage_count": len(configuration.StorageRoots)})
}

func runManagedRunner(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("archive-media runner", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configurationPath := flags.String("config", "worker.yaml", "managed runner boot configuration")
	check := flags.Bool("check-config", false, "validate configuration and exit")
	probeAttachedVolumes := flags.Bool("probe-attached-volumes", false, "print safe attached-volume observations and exit")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return 2
	}
	configuration, err := workerconfig.Load(*configurationPath)
	if err != nil {
		writeCommandError(stderr, "RUNNER_CONFIGURATION_INVALID", err)
		return 2
	}
	if *probeAttachedVolumes {
		return probeVolumes(ctx, configuration, stdout, stderr)
	}
	storageRoots, err := workerconfig.OpenStorageRoots(configuration)
	if err != nil {
		writeCommandError(stderr, "RUNNER_STORAGE_UNAVAILABLE", err)
		return 1
	}
	defer storageRoots.Close()
	if *check {
		return writeJSON(stdout, map[string]any{"valid": true, "runner_id": configuration.ID})
	}
	executor, err := mediaexecutor.NewProduction(configuration, storageRoots)
	if err != nil {
		writeCommandError(stderr, "RUNNER_EXECUTOR_INVALID", err)
		return 2
	}
	storageProbe, err := storageinventory.New(configuration, storageRoots)
	if err != nil {
		writeCommandError(stderr, "RUNNER_STORAGE_PROBE_INVALID", err)
		return 2
	}
	var volumeProbe workerclient.VolumeProbe
	if runtime.GOOS == "windows" {
		volumeProbe, err = windowsvolume.NewSystemObserver()
		if err != nil {
			writeCommandError(stderr, "RUNNER_VOLUME_PROBE_INVALID", err)
			return 2
		}
	}
	client, err := workerclient.NewWithExecutorAndInventoryProbes(
		configuration, ossecret.New(), &http.Client{Timeout: 15 * time.Second},
		hostresource.New(os.TempDir(), time.Now), time.Now, executor, storageProbe, volumeProbe, storageinventory.NewAttachedProbe(), storageRoots,
	)
	if err != nil {
		writeCommandError(stderr, "RUNNER_CLIENT_INVALID", err)
		return 2
	}
	slog.Info("Media runner started", "runner_id", configuration.ID, "control_url", configuration.ControlURL.Redacted(), "version", version)
	if err := client.Run(ctx, func(runErr error) { slog.Warn("Media runner heartbeat failed", "error", runErr) }); err != nil {
		writeCommandError(stderr, "RUNNER_STOPPED", err)
		return 1
	}
	return 0
}

func probeVolumes(ctx context.Context, configuration workerconfig.Configuration, stdout, stderr io.Writer) int {
	if runtime.GOOS != "windows" {
		writeCommandError(stderr, "WINDOWS_REQUIRED", errors.New("attached volume probe requires Windows"))
		return 1
	}
	observer, err := windowsvolume.NewSystemObserver()
	if err != nil {
		writeCommandError(stderr, "VOLUME_PROBE_INVALID", err)
		return 1
	}
	if _, err = observer.Poll(ctx, configuration.ID, time.Now().UTC()); err != nil {
		writeCommandError(stderr, "VOLUME_PROBE_FAILED", err)
		return 1
	}
	safe := make([]map[string]any, 0)
	for _, volume := range observer.Connected() {
		safe = append(safe, map[string]any{
			"physical_device_id": volume.PhysicalDeviceID, "volume_id": volume.VolumeID,
			"storage_id": volume.StorageID, "archive_storage_id": volume.ArchiveStorageID,
			"model": volume.Model, "label": volume.Label, "file_system": volume.FileSystem,
			"capacity_bytes": volume.CapacityBytes, "free_bytes": volume.FreeBytes,
		})
	}
	return writeJSON(stdout, map[string]any{"attached_volumes": safe})
}

func writeOperations(writer io.Writer) int {
	names := make([]string, 0, len(operationCommands))
	for name := range operationCommands {
		names = append(names, name)
	}
	sort.Strings(names)
	operations := make([]map[string]string, 0, len(names))
	for _, name := range names {
		operations = append(operations, map[string]string{"command": name, "operation": string(operationCommands[name])})
	}
	return writeJSON(writer, map[string]any{"operations": operations})
}

func randomExecutionToken() (uint64, error) {
	var payload [8]byte
	if _, err := cryptorand.Read(payload[:]); err != nil {
		return 0, err
	}
	value := binary.LittleEndian.Uint64(payload[:])
	if value == 0 {
		value = 1
	}
	return value, nil
}

func writeJSON(writer io.Writer, value any) int {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return 1
	}
	return 0
}

func writeCommandError(writer io.Writer, code string, err error) {
	_ = json.NewEncoder(writer).Encode(map[string]string{"error": code, "detail": err.Error()})
}

func writeUsage(writer io.Writer) {
	fmt.Fprintln(writer, "usage: archive-media <operations|check|execute|inspect|render|sequence|qc|package|audio|transcribe|subtitle|transcript-text|package-revise|catalog [publish|verify|rebuild]|runner|version>")
}
