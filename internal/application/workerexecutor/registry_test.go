package workerexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"videoarchive/internal/adapter/archivebundle"
	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/workerclient"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

// Requirement: WRK-012
func TestProductionExecutorRoutesOneTypedOperationThroughRegisteredStorageRoots(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source")
	workingPath := filepath.Join(directory, "working")
	for _, root := range []string{sourcePath, workingPath} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sourcePayload := []byte("source-fixture")
	if err := os.WriteFile(filepath.Join(sourcePath, "clip.mov"), sourcePayload, 0o600); err != nil {
		t.Fatal(err)
	}
	configuration := workerconfig.Configuration{
		StorageRoots:         []workerconfig.StorageRootConfiguration{{StorageID: "source", Root: sourcePath}, {StorageID: "working", Root: workingPath}},
		AccessibleStorageIDs: []string{"source", "working"},
	}
	roots, err := workerconfig.OpenStorageRoots(configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	_, checksum, err := roots.MustRoot("source").FileSHA256("clip.mov")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: "JOB-1", StepID: "organize", TemplateName: "archive-intake-as-is", Operation: execution.OperationOrganizeMedia,
		Inputs: []execution.InputReference{{
			OriginalAssetID: "ASSET-1", OriginalName: "clip.mov", InspectionID: "INSPECTION-1", StorageID: "source",
			RelativePath: "clip.mov", Checksum: checksum, DurationMillis: 10_000, VideoCodec: "h264",
			Width: 1920, Height: 1080, VideoBitRateBPS: 6_000_000, ProfileReference: "archive-1080p-6mbps@v1",
		}},
		Outputs: []execution.OutputTarget{{
			ID: "VP-20261031-0001", Kind: execution.OutputOrganizedMedia, StorageID: "working",
			RelativePath: "2026-10-31_clip__0001/.archive-package.json", OriginalAssetID: "ASSET-1",
		}},
		ProfileReference: "archive-1080p-6mbps@v1",
		Parameters: execution.StepExecutionParameters{
			OrganizationDecisionID: "DECISION-1", WorkspaceDriveID: "DRIVE-1", MediaFolder: "recordings", InputRevision: 1,
			ArchiveStorageID: "working", ArchiveDate: "20261031",
			Disposition: vocabulary.OrganizationAsIs, SelectedProfileReference: "archive-1080p-6mbps@v1",
			SourceInputs: []execution.InputReference{{
				OriginalAssetID: "ASSET-1", OriginalName: "clip.mov", InspectionID: "INSPECTION-1", StorageID: "source",
				RelativePath: "clip.mov", Checksum: checksum, DurationMillis: 10_000, VideoCodec: "h264",
				Width: 1920, Height: 1080, VideoBitRateBPS: 6_000_000, ProfileReference: "archive-1080p-6mbps@v1",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	productionConfiguration := workerconfig.Configuration{
		ID: "worker-production", Tools: workerconfig.MediaTools{
			FFmpeg:  workerconfig.LocalTool{Executable: executable, Version: "ffmpeg-fixture-v1"},
			FFprobe: workerconfig.LocalTool{Executable: executable, Version: "ffprobe-fixture-v1"},
		},
	}
	registry, err := NewProduction(productionConfiguration, roots)
	if err != nil {
		t.Fatal(err)
	}
	result := registry.Execute(context.Background(), workerclient.Assignment{
		JobID: spec.JobID, StepID: spec.StepID, FencingToken: 1, LeaseExpiresAt: time.Now().Add(time.Minute), ExecutionSpec: spec,
	})
	if !result.Success || len(result.Outputs) != 1 || result.Outputs[0].ID != "VP-20261031-0001" ||
		len(result.VerificationReferences) != 1 {
		t.Fatalf("supported operation was not executed exactly once: result=%+v", result)
	}
	published, err := roots.MustRoot("working").ReadFile("2026-10-31_clip__0001/clip.mp4")
	if err != nil || string(published) != string(sourcePayload) {
		t.Fatalf("verified output was not published through StorageRoot: %q err=%v", published, err)
	}

	unsupported := spec
	unsupported.Operation = execution.OperationCreateThumbnail
	unsupported.StepID = "thumbnail"
	unsupported.Outputs = []execution.OutputTarget{{
		ID: "THUMBNAIL-2", Kind: execution.OutputThumbnail, StorageID: "working", RelativePath: "staging/thumbnail.jpg",
	}}
	unsupported, err = execution.NewStepExecutionSpec(unsupported)
	if err != nil {
		t.Fatal(err)
	}
	rejected := registry.Execute(context.Background(), workerclient.Assignment{
		JobID: unsupported.JobID, StepID: unsupported.StepID, FencingToken: 2, ExecutionSpec: unsupported,
	})
	if rejected.Success || rejected.ErrorClass != "UNSUPPORTED_STEP_OPERATION" {
		t.Fatalf("unsupported operation reached an Adapter: result=%+v", rejected)
	}
	if _, err := roots.MustRoot("working").ReadFile("staging/thumbnail.jpg"); err == nil {
		t.Fatal("unsupported operation changed the filesystem")
	}

	unregistered := spec
	unregistered.Inputs = append([]execution.InputReference(nil), spec.Inputs...)
	unregistered.Inputs[0].StorageID = "unknown"
	unregistered, err = execution.NewStepExecutionSpec(unregistered)
	if err != nil {
		t.Fatal(err)
	}
	rejected = registry.Execute(context.Background(), workerclient.Assignment{
		JobID: unregistered.JobID, StepID: unregistered.StepID, FencingToken: 3, ExecutionSpec: unregistered,
	})
	if rejected.Success || rejected.ErrorClass != "UNREGISTERED_STORAGE" {
		t.Fatalf("unregistered Storage reached an Adapter: result=%+v", rejected)
	}
	handler := &copyHandler{}
	if _, err := New(roots, handler, &copyHandler{}); !errors.Is(err, ErrDuplicateOperation) {
		t.Fatalf("duplicate operation was accepted: %v", err)
	}

	cancelSpec := spec
	cancelSpec.JobID = "JOB-CANCEL"
	cancelSpec.StepID = "organize-cancel"
	cancelSpec.Outputs = []execution.OutputTarget{{
		ID: "VP-CANCEL", Kind: execution.OutputOrganizedMedia, StorageID: "working",
		RelativePath: "staging/cancelled.mp4", OriginalAssetID: "ASSET-1",
	}}
	cancelSpec, err = execution.NewStepExecutionSpec(cancelSpec)
	if err != nil {
		t.Fatal(err)
	}
	cancelExecutor, err := New(roots, &blockingPartialHandler{})
	if err != nil {
		t.Fatal(err)
	}
	cancelContext, cancel := context.WithCancel(context.Background())
	resultChannel := make(chan workerclient.ExecutionResult, 1)
	go func() {
		resultChannel <- cancelExecutor.Execute(cancelContext, workerclient.Assignment{
			JobID: cancelSpec.JobID, StepID: cancelSpec.StepID, FencingToken: 4, ExecutionSpec: cancelSpec,
		})
	}()
	partial := temporaryOutputPathForToken(cancelSpec.Outputs[0].RelativePath, 4)
	for attempt := 0; attempt < 100; attempt++ {
		if exists, _ := roots.MustRoot("working").Exists(partial); exists {
			break
		}
		time.Sleep(time.Millisecond)
	}
	otherAttemptPartial := temporaryOutputPathForToken(cancelSpec.Outputs[0].RelativePath, 5)
	if err := roots.MustRoot("working").WriteFile(otherAttemptPartial, []byte("new-attempt"), 0o600); err != nil {
		t.Fatal(err)
	}
	cancel()
	cancelled := <-resultChannel
	if cancelled.Cancellation == nil || !cancelled.Cancellation.ProcessStopped || !cancelled.Cancellation.CleanupConfirmed {
		t.Fatalf("cancelled production operation did not prove stop and cleanup: %+v", cancelled)
	}
	if exists, _ := roots.MustRoot("working").Exists(partial); exists {
		t.Fatal("cancelled production operation left its temporary output")
	}
	if payload, err := roots.MustRoot("working").ReadFile(otherAttemptPartial); err != nil || string(payload) != "new-attempt" {
		t.Fatalf("cancelled operation changed another fencing token's temporary output: payload=%q err=%v", payload, err)
	}

	t.Run("actual ffmpeg source sequence", testActualSourceSequence)
}

// Requirement: APP-010
func TestDirectCLIAndManagedRunnerSerializeSameOutputPath(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source")
	archivePath := filepath.Join(directory, "archive")
	for _, rootPath := range []string{sourcePath, archivePath} {
		if err := os.Mkdir(rootPath, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "clip.mov"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	roots, err := workerconfig.OpenStorageRoots(workerconfig.Configuration{
		StorageRoots: []workerconfig.StorageRootConfiguration{
			{StorageID: "source", Root: sourcePath},
			{StorageID: "archive", Root: archivePath},
		},
		AccessibleStorageIDs: []string{"source", "archive"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	_, checksum, err := roots.MustRoot("source").FileSHA256("clip.mov")
	if err != nil {
		t.Fatal(err)
	}

	entered := make(chan string, 3)
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer release()
	registry, err := New(roots, &lockingProbeHandler{
		blockedOutput: "results/a.json",
		entered:       entered,
		release:       releaseFirst,
	})
	if err != nil {
		t.Fatal(err)
	}
	specification := func(jobID, outputPath string) execution.StepExecutionSpec {
		t.Helper()
		spec, specErr := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
			JobID: jobID, StepID: "organize", TemplateName: "media-operation-lock-test",
			Operation: execution.OperationOrganizeMedia,
			Inputs: []execution.InputReference{{
				OriginalAssetID: "ASSET-1", OriginalName: "clip.mov", InspectionID: "INSPECTION-1",
				StorageID: "source", RelativePath: "clip.mov", Checksum: checksum, DurationMillis: 1,
			}},
			Outputs: []execution.OutputTarget{{
				ID: "OUTPUT-" + jobID, Kind: execution.OutputOrganizedMedia, StorageID: "archive",
				RelativePath: outputPath, OriginalAssetID: "ASSET-1",
			}},
			ProfileReference: "archive-1080p-6mbps@v1",
			Parameters: execution.StepExecutionParameters{
				OrganizationDecisionID: "DECISION-1", WorkspaceDriveID: "VOLUME-1",
				ArchiveStorageID: "archive", InputRevision: 1,
			},
		})
		if specErr != nil {
			t.Fatal(specErr)
		}
		return spec
	}

	firstSpec := specification("DIRECT-A", "results/a.json")
	firstResult := make(chan workerclient.ExecutionResult, 1)
	go func() { firstResult <- registry.ExecuteDirect(context.Background(), firstSpec, 11) }()
	if path := <-entered; path != "results/a.json" {
		t.Fatalf("direct execution entered the wrong output: %s", path)
	}
	helperSpec, err := json.Marshal(firstSpec)
	if err != nil {
		t.Fatal(err)
	}
	helperReady := filepath.Join(directory, "cross-process.ready")
	helperResult := filepath.Join(directory, "cross-process.result")
	helper := osexec.Command(os.Args[0], "-test.run=^TestMediaOperationCrossProcessHelper$")
	helper.Env = append(os.Environ(),
		"ARCHIVE_MEDIA_LOCK_HELPER=1",
		"ARCHIVE_MEDIA_LOCK_SOURCE="+sourcePath,
		"ARCHIVE_MEDIA_LOCK_ARCHIVE="+archivePath,
		"ARCHIVE_MEDIA_LOCK_SPEC="+string(helperSpec),
		"ARCHIVE_MEDIA_LOCK_READY="+helperReady,
		"ARCHIVE_MEDIA_LOCK_RESULT="+helperResult,
	)
	helperOutput := &strings.Builder{}
	helper.Stdout = helperOutput
	helper.Stderr = helperOutput
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	helperDone := make(chan error, 1)
	go func() { helperDone <- helper.Wait() }()
	for attempt := 0; attempt < 200; attempt++ {
		if _, err := os.Stat(helperReady); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Stat(helperReady); err != nil {
		t.Fatalf("cross-process Runner did not start: %v: %s", err, helperOutput.String())
	}
	select {
	case err := <-helperDone:
		t.Fatalf("cross-process Runner bypassed the output lock: %v: %s", err, helperOutput.String())
	case <-time.After(100 * time.Millisecond):
	}

	sameContext, cancelSame := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelSame()
	sameResult := make(chan workerclient.ExecutionResult, 1)
	go func() {
		sameResult <- registry.Execute(sameContext, workerclient.Assignment{
			JobID: firstSpec.JobID, StepID: firstSpec.StepID, FencingToken: 12, ExecutionSpec: firstSpec,
		})
	}()

	differentSpec := specification("RUNNER-B", "results/b.json")
	different := registry.Execute(context.Background(), workerclient.Assignment{
		JobID: differentSpec.JobID, StepID: differentSpec.StepID, FencingToken: 13, ExecutionSpec: differentSpec,
	})
	if !different.Success {
		t.Fatalf("different output did not execute in parallel: %+v", different)
	}
	if path := <-entered; path != "results/b.json" {
		t.Fatalf("managed execution entered the wrong output: %s", path)
	}

	serialized := <-sameResult
	if serialized.Cancellation == nil || serialized.Success {
		t.Fatalf("same output entered instead of waiting for the shared lock: %+v", serialized)
	}
	select {
	case unexpected := <-entered:
		t.Fatalf("same output handler executed while the direct operation held the lock: %s", unexpected)
	default:
	}
	release()
	if completed := <-firstResult; !completed.Success {
		t.Fatalf("direct execution did not complete after releasing its output: %+v", completed)
	}
	select {
	case err := <-helperDone:
		if err != nil {
			t.Fatalf("cross-process Runner failed after lock release: %v: %s", err, helperOutput.String())
		}
	case <-time.After(5 * time.Second):
		_ = helper.Process.Kill()
		t.Fatal("cross-process Runner did not continue after lock release")
	}
	if payload, err := os.ReadFile(helperResult); err != nil || string(payload) != "success" {
		t.Fatalf("cross-process Runner did not publish its serialized result: %q err=%v", payload, err)
	}
}

func TestMediaOperationCrossProcessHelper(t *testing.T) {
	if os.Getenv("ARCHIVE_MEDIA_LOCK_HELPER") != "1" {
		t.Skip("cross-process lock helper")
	}
	if err := os.WriteFile(os.Getenv("ARCHIVE_MEDIA_LOCK_READY"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	var spec execution.StepExecutionSpec
	if err := json.Unmarshal([]byte(os.Getenv("ARCHIVE_MEDIA_LOCK_SPEC")), &spec); err != nil {
		t.Fatal(err)
	}
	roots, err := workerconfig.OpenStorageRoots(workerconfig.Configuration{
		StorageRoots: []workerconfig.StorageRootConfiguration{
			{StorageID: "source", Root: os.Getenv("ARCHIVE_MEDIA_LOCK_SOURCE")},
			{StorageID: "archive", Root: os.Getenv("ARCHIVE_MEDIA_LOCK_ARCHIVE")},
		},
		AccessibleStorageIDs: []string{"source", "archive"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	registry, err := New(roots, &crossProcessProbeHandler{})
	if err != nil {
		t.Fatal(err)
	}
	result := registry.Execute(context.Background(), workerclient.Assignment{
		JobID: spec.JobID, StepID: spec.StepID, FencingToken: 14, ExecutionSpec: spec,
	})
	if !result.Success {
		t.Fatalf("managed Runner result failed: %+v", result)
	}
	if err := os.WriteFile(os.Getenv("ARCHIVE_MEDIA_LOCK_RESULT"), []byte("success"), 0o600); err != nil {
		t.Fatal(err)
	}
}

type lockingProbeHandler struct {
	blockedOutput string
	entered       chan<- string
	release       <-chan struct{}
}

type crossProcessProbeHandler struct{}

func (*crossProcessProbeHandler) Operation() execution.Operation {
	return execution.OperationOrganizeMedia
}

func (*crossProcessProbeHandler) Execute(ctx context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	target := spec.Outputs[0]
	checksum, err := publishPayload(ctx, roots.MustRoot(target.StorageID), target.RelativePath, []byte("cross-process-runner"))
	if err != nil {
		return nil, nil, err
	}
	outputs := []execution.StepOutput{stepOutput(target, checksum)}
	return outputs, verificationReferences(spec.Operation, outputs), nil
}

func (*lockingProbeHandler) Operation() execution.Operation { return execution.OperationOrganizeMedia }

func (handler *lockingProbeHandler) Execute(ctx context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	target := spec.Outputs[0]
	handler.entered <- target.RelativePath
	if target.RelativePath == handler.blockedOutput {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-handler.release:
		}
	}
	checksum, err := publishPayload(ctx, roots.MustRoot(target.StorageID), target.RelativePath, []byte(spec.JobID))
	if err != nil {
		return nil, nil, err
	}
	outputs := []execution.StepOutput{stepOutput(target, checksum)}
	return outputs, verificationReferences(spec.Operation, outputs), nil
}

type blockingPartialHandler struct{}

func (*blockingPartialHandler) Operation() execution.Operation {
	return execution.OperationOrganizeMedia
}

// Requirement: WRK-015
func TestProductionExecutorClassifiesInputTransientAndStorageFailures(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source")
	archivePath := filepath.Join(directory, "archive")
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(archivePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "clip.mov"), []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	roots, err := workerconfig.OpenStorageRoots(workerconfig.Configuration{
		StorageRoots:         []workerconfig.StorageRootConfiguration{{StorageID: "source", Root: sourcePath}, {StorageID: "archive", Root: archivePath}},
		AccessibleStorageIDs: []string{"source", "archive"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	_, checksum, err := roots.MustRoot("source").FileSHA256("clip.mov")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: "JOB-FAILURES", StepID: "organize", TemplateName: "archive-intake-as-is", Operation: execution.OperationOrganizeMedia,
		Inputs: []execution.InputReference{{
			OriginalAssetID: "ASSET-1", OriginalName: "clip.mov", InspectionID: "INSPECTION-1", StorageID: "source",
			RelativePath: "clip.mov", Checksum: checksum, DurationMillis: 1_000,
		}},
		Outputs: []execution.OutputTarget{{
			ID: "OUTPUT-1", Kind: execution.OutputOrganizedMedia, StorageID: "archive",
			RelativePath: "packages/output.mp4", OriginalAssetID: "ASSET-1",
		}},
		ProfileReference: "archive-1080p-6mbps@v1",
		Parameters: execution.StepExecutionParameters{
			OrganizationDecisionID: "DECISION-1", WorkspaceDriveID: "VOLUME-1", ArchiveStorageID: "archive", InputRevision: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	transientRegistry, err := New(roots, errorHandler{err: errors.Join(ErrProductionExecution, archivebundle.ErrPackageExists)})
	if err != nil {
		t.Fatal(err)
	}
	assignment := workerclient.Assignment{JobID: spec.JobID, StepID: spec.StepID, FencingToken: 1, ExecutionSpec: spec}
	transient := transientRegistry.Execute(context.Background(), assignment)
	if transient.FailureClass != workflow.FailureTransient || transient.ErrorClass != "STEP_EXECUTION_FAILED" ||
		transient.CauseCode != "ARCHIVE_PACKAGE_ALREADY_EXISTS" || transient.ErrorSummary != "The Archive Package already exists" {
		t.Fatalf("temporary Adapter error was not retryable: %+v", transient)
	}
	badChecksum := spec
	badChecksum.Inputs = append([]execution.InputReference(nil), spec.Inputs...)
	badChecksum.Inputs[0].Checksum = strings.Repeat("f", 64)
	badChecksum, err = execution.NewStepExecutionSpec(badChecksum)
	if err != nil {
		t.Fatal(err)
	}
	assignment.ExecutionSpec = badChecksum
	permanent := transientRegistry.Execute(context.Background(), assignment)
	if permanent.FailureClass != workflow.FailureInput || permanent.ErrorClass != "INPUT_CHECKSUM_MISMATCH" ||
		permanent.CauseCode != "INPUT_CHECKSUM_MISMATCH" || permanent.ErrorSummary == "" {
		t.Fatalf("invalid input was not permanent: %+v", permanent)
	}
	if err := os.Remove(filepath.Join(sourcePath, "clip.mov")); err != nil {
		t.Fatal(err)
	}
	assignment.ExecutionSpec = spec
	deviceWait := transientRegistry.Execute(context.Background(), assignment)
	if deviceWait.FailureClass != workflow.FailureResource || deviceWait.WaitReason != vocabulary.WaitDevice ||
		deviceWait.WaitTargetStorageID != "source" || deviceWait.CauseCode != "STORAGE_UNAVAILABLE" || deviceWait.ErrorSummary == "" {
		t.Fatalf("unavailable Storage did not wait without consuming an attempt: %+v", deviceWait)
	}
	diskWait, classified := storageFailure(errors.Join(archive.ErrStorageFull), "archive")
	if !classified || diskWait.FailureClass != workflow.FailureResource || diskWait.WaitReason != vocabulary.WaitDevice ||
		diskWait.WaitTargetStorageID != "archive" || diskWait.CauseCode != "STORAGE_FULL" || diskWait.ErrorSummary == "" {
		t.Fatalf("full Storage did not become a targeted device wait: %+v classified=%v", diskWait, classified)
	}
}

type errorHandler struct{ err error }

func (errorHandler) Operation() execution.Operation { return execution.OperationOrganizeMedia }

func (handler errorHandler) Execute(context.Context, execution.StepExecutionSpec, *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	return nil, nil, handler.err
}

func (*blockingPartialHandler) Execute(ctx context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	root := roots.MustRoot(spec.Outputs[0].StorageID)
	if err := root.MkdirAll(filepath.Dir(spec.Outputs[0].RelativePath), 0o700); err != nil {
		return nil, nil, err
	}
	if err := root.WriteFile(temporaryOutputPath(ctx, spec.Outputs[0].RelativePath), []byte("partial"), 0o600); err != nil {
		return nil, nil, err
	}
	<-ctx.Done()
	return nil, nil, ctx.Err()
}

// Requirement: CFG-011
func TestWorkerArchiveIntakeInspectionUsesLocalFFprobeAndReturnsOnlyNormalizedPaths(t *testing.T) {
	ffmpegPath, ffmpegErr := osexec.LookPath("ffmpeg")
	ffprobePath, ffprobeErr := osexec.LookPath("ffprobe")
	if ffmpegErr != nil || ffprobeErr != nil {
		t.Skip("actual FFmpeg toolchain is not installed")
	}
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source")
	archivePath := filepath.Join(directory, "archive")
	if err := os.MkdirAll(filepath.Join(sourcePath, "event-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archivePath, 0o700); err != nil {
		t.Fatal(err)
	}
	clipPath := filepath.Join(sourcePath, "event-a", "clip.mp4")
	command := osexec.Command(
		ffmpegPath, "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x180:rate=30", "-t", "1",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", clipPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create inspection fixture: %v: %s", err, output)
	}
	configuration := workerconfig.Configuration{
		ID: "worker-inspection", StorageRoots: []workerconfig.StorageRootConfiguration{
			{StorageID: "source", Root: sourcePath}, {StorageID: "archive", Root: archivePath},
		},
		AccessibleStorageIDs: []string{"source", "archive"},
		Tools: workerconfig.MediaTools{
			FFmpeg:  workerconfig.LocalTool{Executable: ffmpegPath, Version: "ffmpeg-actual"},
			FFprobe: workerconfig.LocalTool{Executable: ffprobePath, Version: "ffprobe-actual"},
		},
	}
	roots, err := workerconfig.OpenStorageRoots(configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	sourceSize, sourceChecksum, err := roots.MustRoot("source").FileSHA256("event-a/clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	requestedAt := time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC)
	spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: "JOB-INSPECTION-1", StepID: "inspect", TemplateName: "archive-intake-inspection",
		Operation: execution.OperationInspectMedia,
		Outputs: []execution.OutputTarget{{
			ID: "MEDIA-INSPECTION-1", Kind: execution.OutputMediaInspection, StorageID: "archive",
			RelativePath: "staging/JOB-INSPECTION-1/inspection/result.json",
		}},
		Parameters: execution.StepExecutionParameters{
			ArchiveRunID: "RUN-1", ArchiveIntakeSessionID: "SESSION-1", StartIdempotencyKey: "start:1",
			SourceStorageID: "source", ArchiveStorageID: "archive", WorkspaceDriveID: "VOLUME-1",
			InputRevision: 7, FolderIDs: []string{"event-a"}, RequestedBy: "operator-1", RequestedAt: requestedAt,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewProduction(configuration, roots)
	if err != nil {
		t.Fatal(err)
	}
	result := registry.Execute(context.Background(), workerclient.Assignment{
		JobID: spec.JobID, StepID: spec.StepID, FencingToken: 1, LeaseExpiresAt: time.Now().Add(time.Minute), ExecutionSpec: spec,
	})
	if !result.Success || len(result.Outputs) != 1 || len(result.InspectedMedia) != 1 ||
		result.InspectedMedia[0].Folder != "event-a" || result.InspectedMedia[0].Observation.RelativePath != "event-a/clip.mp4" ||
		result.InspectedMedia[0].Inspection.ProcessorVersion != "ffprobe-actual" {
		t.Fatalf("Worker-side Archive Intake inspection failed: %+v", result)
	}
	observation := result.InspectedMedia[0].Observation
	if observation.StorageID != "source" || observation.WorkspaceDriveID != "VOLUME-1" || observation.OriginalName != "clip.mp4" ||
		observation.SizeBytes != sourceSize || observation.Checksum != sourceChecksum || observation.ObservedBy != "worker-inspection" ||
		!observation.ObservedAt.Equal(requestedAt) {
		t.Fatalf("production inspection did not create the complete input observation: %+v", observation)
	}
	afterSize, afterChecksum, err := roots.MustRoot("source").FileSHA256("event-a/clip.mp4")
	if err != nil || afterSize != sourceSize || afterChecksum != sourceChecksum {
		t.Fatalf("production inspection changed the source: before=%d/%s after=%d/%s err=%v", sourceSize, sourceChecksum, afterSize, afterChecksum, err)
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), sourcePath) || strings.Contains(string(payload), archivePath) || strings.Contains(string(payload), ffprobePath) {
		t.Fatalf("Worker-local source or executable path crossed the result protocol: %s", payload)
	}
	if size, _, err := roots.MustRoot("archive").FileSHA256("staging/JOB-INSPECTION-1/inspection/result.json"); err != nil || size <= 0 {
		t.Fatalf("normalized inspection evidence was not published: size=%d err=%v", size, err)
	}
}

func TestProductionInspectionFailureReturnsBoundedProcessLogArtifact(t *testing.T) {
	ffmpegPath, ffmpegErr := osexec.LookPath("ffmpeg")
	ffprobePath, ffprobeErr := osexec.LookPath("ffprobe")
	if ffmpegErr != nil || ffprobeErr != nil {
		t.Skip("actual FFmpeg toolchain is not installed")
	}
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source")
	archivePath := filepath.Join(directory, "archive")
	processLogPath := filepath.Join(directory, "process-logs")
	for _, root := range []string{sourcePath, archivePath, processLogPath} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(sourcePath, "broken"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "broken", "not-video.mp4"), []byte("not a media container"), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration := workerconfig.Configuration{
		ID: "worker-failure-evidence", StorageRoots: []workerconfig.StorageRootConfiguration{
			{StorageID: "source", Root: sourcePath}, {StorageID: "archive", Root: archivePath},
		},
		AccessibleStorageIDs: []string{"source", "archive"},
		Tools: workerconfig.MediaTools{
			FFmpeg:  workerconfig.LocalTool{Executable: ffmpegPath, Version: "ffmpeg-actual"},
			FFprobe: workerconfig.LocalTool{Executable: ffprobePath, Version: "ffprobe-actual"},
		},
		ProcessLogs: &workerconfig.ProcessLogConfiguration{
			Root: processLogPath, MaxFileBytes: 4096, TailBytes: 2048,
			Retention: time.Hour, MaxTotalBytes: 8192,
		},
	}
	roots, err := workerconfig.OpenStorageRoots(configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: "JOB-INSPECTION-FAILURE", StepID: "inspect", TemplateName: "archive-intake-inspection",
		Operation: execution.OperationInspectMedia,
		Outputs: []execution.OutputTarget{{
			ID: "MEDIA-INSPECTION-FAILURE", Kind: execution.OutputMediaInspection, StorageID: "archive",
			RelativePath: "staging/JOB-INSPECTION-FAILURE/inspection/result.json",
		}},
		Parameters: execution.StepExecutionParameters{
			ArchiveRunID: "RUN-FAILURE", ArchiveIntakeSessionID: "SESSION-FAILURE", StartIdempotencyKey: "start:failure",
			SourceStorageID: "source", ArchiveStorageID: "archive", WorkspaceDriveID: "VOLUME-FAILURE",
			InputRevision: 1, FolderIDs: []string{"broken"}, RequestedBy: "operator-1",
			RequestedAt: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewProduction(configuration, roots)
	if err != nil {
		t.Fatal(err)
	}
	result := registry.Execute(context.Background(), workerclient.Assignment{
		JobID: spec.JobID, StepID: spec.StepID, FencingToken: 3,
		LeaseExpiresAt: time.Now().Add(time.Minute), ExecutionSpec: spec,
	})
	if result.Success || result.ProcessLog == nil || result.ProcessLog.State != execution.ProcessLogAvailable ||
		result.LogReference != result.ProcessLog.Reference || !strings.HasPrefix(result.LogReference, "evidence://process-log/PLOG-") ||
		strings.Contains(result.ProcessLog.SanitizedTail, sourcePath) {
		t.Fatalf("inspection failure evidence mismatch: %+v", result)
	}
	info, err := os.Stat(filepath.Join(processLogPath, "process", result.ProcessLog.ID+".log"))
	if err != nil || info.Size() <= 0 || info.Size() > configuration.ProcessLogs.MaxFileBytes {
		t.Fatalf("bounded Worker-local process log mismatch: info=%v err=%v", info, err)
	}
}

// Requirement: INT-001
func TestProductionInputInspectionHashesRegisteredStorageFileReadOnly(t *testing.T) {
	TestWorkerArchiveIntakeInspectionUsesLocalFFprobeAndReturnsOnlyNormalizedPaths(t)
}

// Requirement: QCT-004
func TestQCRenditionUsesEachReadyInputProfileForMultiFileAsIsFolder(t *testing.T) {
	ffmpegPath, ffmpegErr := osexec.LookPath("ffmpeg")
	ffprobePath, ffprobeErr := osexec.LookPath("ffprobe")
	if ffmpegErr != nil || ffprobeErr != nil {
		t.Skip("actual FFmpeg toolchain is not installed")
	}
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source")
	archivePath := filepath.Join(directory, "archive")
	for _, root := range []string{sourcePath, archivePath} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	type readyInput struct {
		assetID string
		name    string
		profile string
		bitrate int64
	}
	ready := []readyInput{
		{assetID: "ASSET-6", name: "ready-6.mp4", profile: "archive-1080p-6mbps@v1", bitrate: 6_000_000},
		{assetID: "ASSET-20", name: "ready-20.mp4", profile: "archive-1080p-20mbps@v1", bitrate: 20_000_000},
	}
	for index, input := range ready {
		command := osexec.Command(
			ffmpegPath, "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
			"-f", "lavfi", "-i", fmt.Sprintf("color=c=black:size=320x180:rate=%d", 24+index*6),
			"-t", "1", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-an", filepath.Join(sourcePath, input.name),
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("create ready input %s: %v: %s", input.name, err, output)
		}
	}
	configuration := workerconfig.Configuration{
		ID: "worker-multi-qc", StorageRoots: []workerconfig.StorageRootConfiguration{
			{StorageID: "source", Root: sourcePath}, {StorageID: "archive", Root: archivePath},
		},
		AccessibleStorageIDs: []string{"source", "archive"},
		Tools: workerconfig.MediaTools{
			FFmpeg:  workerconfig.LocalTool{Executable: ffmpegPath, Version: "ffmpeg-actual"},
			FFprobe: workerconfig.LocalTool{Executable: ffprobePath, Version: "ffprobe-actual"},
		},
	}
	roots, err := workerconfig.OpenStorageRoots(configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	inputs := make([]execution.InputReference, 0, len(ready))
	outputs := make([]execution.OutputTarget, 0, len(ready))
	for _, item := range ready {
		_, checksum, err := roots.MustRoot("source").FileSHA256(item.name)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, execution.InputReference{
			OriginalAssetID: item.assetID, OriginalName: item.name, InspectionID: "INSPECTION-" + item.assetID,
			StorageID: "source", RelativePath: item.name, Checksum: checksum, DurationMillis: 1_000,
			VideoCodec: "h264", Width: 320, Height: 180, VideoBitRateBPS: item.bitrate, ProfileReference: item.profile,
		})
		outputs = append(outputs, execution.OutputTarget{
			ID: "QC-" + item.assetID, Kind: execution.OutputQCAssessment, StorageID: "archive",
			RelativePath: "staging/JOB-MULTI-QC/qc/" + item.assetID + ".json", OriginalAssetID: item.assetID,
		})
	}
	spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: "JOB-MULTI-QC", StepID: "qc", TemplateName: "archive-intake-as-is",
		Operation: execution.OperationQCRendition, Inputs: inputs, Outputs: outputs,
		Parameters: execution.StepExecutionParameters{
			OrganizationDecisionID: "DECISION-MULTI-QC", WorkspaceDriveID: "DRIVE-1", MediaFolder: "ready",
			ArchiveStorageID: "archive", InputRevision: 1, Disposition: vocabulary.OrganizationAsIs,
			SourceInputs: inputs,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewProduction(configuration, roots)
	if err != nil {
		t.Fatal(err)
	}
	result := registry.Execute(context.Background(), workerclient.Assignment{
		JobID: spec.JobID, StepID: spec.StepID, FencingToken: 1,
		LeaseExpiresAt: time.Now().Add(time.Minute), ExecutionSpec: spec,
	})
	if !result.Success || len(result.Outputs) != len(ready) {
		t.Fatalf("multi-file as-is QC did not use the persisted per-input profiles: %+v", result)
	}
	for _, item := range ready {
		payload, err := roots.MustRoot("archive").ReadFile("staging/JOB-MULTI-QC/qc/" + item.assetID + ".json")
		if err != nil || !strings.Contains(string(payload), `"profile_reference":"`+item.profile+`"`) ||
			!strings.Contains(string(payload), `"outcome":"PASSED"`) {
			t.Fatalf("QC evidence for %s does not preserve its ready profile: %s err=%v", item.assetID, payload, err)
		}
	}
}

// Requirement: ARC-020
func TestMultiFileAsIsPackagesPreserveEachReadyInputProfile(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source")
	archivePath := filepath.Join(directory, "archive")
	for _, root := range []string{sourcePath, archivePath} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	type readyInput struct {
		assetID   string
		name      string
		packageID string
		profile   string
	}
	ready := []readyInput{
		{assetID: "ASSET-6", name: "ready-6.mp4", packageID: "VP-20261110-0001", profile: "archive-1080p-6mbps@v1"},
		{assetID: "ASSET-20", name: "ready-20.mp4", packageID: "VP-20261110-0002", profile: "archive-1080p-20mbps@v1"},
	}
	configuration := workerconfig.Configuration{
		ID: "worker-multi-package", StorageRoots: []workerconfig.StorageRootConfiguration{
			{StorageID: "source", Root: sourcePath}, {StorageID: "archive", Root: archivePath},
		},
		AccessibleStorageIDs: []string{"source", "archive"},
		Tools: workerconfig.MediaTools{
			FFmpeg:  workerconfig.LocalTool{Executable: os.Args[0], Version: "unused"},
			FFprobe: workerconfig.LocalTool{Executable: os.Args[0], Version: "unused"},
		},
	}
	roots, err := workerconfig.OpenStorageRoots(configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	inputs := make([]execution.InputReference, 0, len(ready))
	outputs := make([]execution.OutputTarget, 0, len(ready))
	for _, item := range ready {
		payload := []byte("fixture-" + item.assetID)
		if err := os.WriteFile(filepath.Join(sourcePath, item.name), payload, 0o600); err != nil {
			t.Fatal(err)
		}
		_, checksum, err := roots.MustRoot("source").FileSHA256(item.name)
		if err != nil {
			t.Fatal(err)
		}
		input := execution.InputReference{
			OriginalAssetID: item.assetID, OriginalName: item.name, InspectionID: "INSPECTION-" + item.assetID,
			StorageID: "source", RelativePath: item.name, Checksum: checksum, DurationMillis: 1_000,
			SourceSizeBytes: int64(len(payload)), SourceContainer: "mp4", VideoCodec: "h264",
			Width: 1920, Height: 1080, VideoBitRateBPS: 6_000_000, ProfileReference: item.profile,
		}
		inputs = append(inputs, input)
		presentation, err := archive.PresentFlatPackage(item.packageID, "ready", []string{item.name})
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, execution.OutputTarget{
			ID: item.packageID, Kind: execution.OutputOrganizedMedia, StorageID: "archive",
			RelativePath: filepath.ToSlash(filepath.Join(presentation.PackageDirectory, archivebundle.CompactPackageDocumentName)), OriginalAssetID: item.assetID,
		})
	}
	spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: "JOB-MULTI-PACKAGE", StepID: "organize", TemplateName: "archive-intake-as-is",
		Operation: execution.OperationOrganizeMedia, Inputs: inputs, Outputs: outputs,
		Parameters: execution.StepExecutionParameters{
			OrganizationDecisionID: "DECISION-MULTI-PACKAGE", WorkspaceDriveID: "DRIVE-1", MediaFolder: "ready",
			ArchiveStorageID: "archive", InputRevision: 1, Disposition: vocabulary.OrganizationAsIs,
			SourceInputs: inputs,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewProduction(configuration, roots)
	if err != nil {
		t.Fatal(err)
	}
	result := registry.Execute(context.Background(), workerclient.Assignment{
		JobID: spec.JobID, StepID: spec.StepID, FencingToken: 1,
		LeaseExpiresAt: time.Now().Add(time.Minute), ExecutionSpec: spec,
	})
	if !result.Success || len(result.Outputs) != len(ready) {
		t.Fatalf("multi-file as-is packaging failed: %+v", result)
	}
	for index, item := range ready {
		payload, err := roots.MustRoot("archive").ReadFile(outputs[index].RelativePath)
		var metadata struct {
			ProfileReference string `json:"profile_reference"`
		}
		if err != nil || json.Unmarshal(payload, &metadata) != nil || metadata.ProfileReference != item.profile {
			t.Fatalf("package %s lost its input profile: metadata=%+v err=%v", item.packageID, metadata, err)
		}
	}
}

// Requirement: WRK-018
func TestArchivePublicationRecoveryReconcilesPackageAndCatalogIdempotently(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source")
	archivePath := filepath.Join(directory, "archive")
	for _, root := range []string{sourcePath, archivePath} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configuration := workerconfig.Configuration{
		ID: "worker-recovery", StorageRoots: []workerconfig.StorageRootConfiguration{
			{StorageID: "source", Root: sourcePath}, {StorageID: "archive", Root: archivePath},
		},
		AccessibleStorageIDs: []string{"source", "archive"},
		Tools: workerconfig.MediaTools{
			FFmpeg:  workerconfig.LocalTool{Executable: os.Args[0], Version: "unused"},
			FFprobe: workerconfig.LocalTool{Executable: os.Args[0], Version: "unused"},
		},
	}
	roots, err := workerconfig.OpenStorageRoots(configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	if err := roots.MustRoot("source").WriteFile("recording.mp4", []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, videoChecksum, err := roots.MustRoot("source").FileSHA256("recording.mp4")
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := archivebundle.NewPublisher(roots.MustRoot("source"), roots.MustRoot("archive"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 19, 17, 0, 0, 0, time.UTC)
	packageID := "VP-20261119-RECOVERY"
	stored, err := publisher.PublishInitialCompact(archivebundle.InitialRequest{
		LayoutVersion: archive.PackageLayoutCompactResultFolder, PackageID: packageID,
		WorkspaceDriveID: "VOLUME-1", DecisionID: "DECISION-1", MediaFolder: "recordings",
		Disposition: vocabulary.OrganizationAsIs, ProfileReference: "archive-1080p-6mbps@v1", CreatedAt: now,
		Video:   archivebundle.FileSource{Root: archivebundle.RootSource, Path: "recording.mp4", Checksum: videoChecksum},
		Sources: []archivebundle.SourceReference{{OriginalAssetID: "ASSET-1", OriginalName: "recording.mp4", RelativePath: "recording.mp4", Checksum: videoChecksum}},
	})
	if err != nil {
		t.Fatal(err)
	}
	staging := []struct {
		path string
		kind execution.OutputKind
		id   string
		body []byte
	}{
		{"staging/recovery/transcript.json", execution.OutputTranscript, "TRANSCRIPT-1", []byte(`{"schema_version":1,"content":{}}`)},
		{"staging/recovery/transcript.txt", execution.OutputTranscriptText, "TEXT-1", []byte("[00:00:00.000 --> 00:00:01.000] test\n")},
		{"staging/recovery/subtitle.srt", execution.OutputSubtitle, "SUBTITLE-1", []byte("1\n00:00:00,000 --> 00:00:01,000\ntest\n")},
	}
	inputs := make([]execution.InputReference, 0, len(staging))
	for _, item := range staging {
		if err := roots.MustRoot("archive").MkdirAll(filepath.ToSlash(filepath.Dir(item.path)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := roots.MustRoot("archive").WriteFile(item.path, item.body, 0o600); err != nil {
			t.Fatal(err)
		}
		_, checksum, err := roots.MustRoot("archive").FileSHA256(item.path)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, execution.InputReference{OriginalAssetID: "ASSET-1", ArtifactID: item.id, StorageID: "archive", RelativePath: item.path, Checksum: checksum, Kind: item.kind})
	}
	spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: "JOB-RECOVERY-PACKAGE", StepID: "reconcile_archive_publication", TemplateName: "archive-publication-recovery",
		Operation: execution.OperationReconcileArchivePublication, Inputs: inputs,
		Outputs: []execution.OutputTarget{{ID: "CATALOG-RECOVERY", Kind: execution.OutputArchiveCatalog, StorageID: "archive", RelativePath: "archive-catalog.csv"}},
		Parameters: execution.StepExecutionParameters{
			ArchiveRunID: "RUN-1", OrganizationDecisionID: "DECISION-1", WorkspaceDriveID: "VOLUME-1",
			ArchiveStorageID: "archive", InputRevision: 1, ArchivePackageID: packageID, ArchivePackageRevision: stored.Revision,
			MediaFolder: "recordings", Disposition: vocabulary.OrganizationAsIs, SelectedProfileReference: "archive-1080p-6mbps@v1",
			SourceInputs: []execution.InputReference{{OriginalAssetID: "ASSET-1", OriginalName: "recording.mp4", StorageID: "source", RelativePath: "recording.mp4", Checksum: videoChecksum, ProfileReference: "archive-1080p-6mbps@v1"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewProduction(configuration, roots)
	if err != nil {
		t.Fatal(err)
	}
	for token := uint64(1); token <= 2; token++ {
		result := registry.Execute(context.Background(), workerclient.Assignment{JobID: spec.JobID, StepID: spec.StepID, FencingToken: token, LeaseExpiresAt: time.Now().Add(time.Minute), ExecutionSpec: spec})
		if !result.Success || len(result.Outputs) != 1 || result.Outputs[0].Kind != execution.OutputArchiveCatalog {
			t.Fatalf("archive publication recovery failed on attempt %d: %+v", token, result)
		}
	}
	current, err := publisher.Current(packageID)
	if err != nil || current.Revision != stored.Revision+1 || current.TranscriptTextPath == "" || current.SubtitlePath == "" {
		t.Fatalf("recovery did not publish exactly one package revision: before=%+v current=%+v err=%v", stored, current, err)
	}
	if payload, err := roots.MustRoot("archive").ReadFile("archive-catalog.csv"); err != nil || !strings.Contains(string(payload), packageID) ||
		!strings.Contains(string(payload), "transcript.txt") || !strings.Contains(string(payload), "subtitle.srt") {
		t.Fatalf("recovery did not publish the completed catalog row: %s err=%v", payload, err)
	}
}

func testActualSourceSequence(t *testing.T) {
	ffmpegPath, ffmpegErr := osexec.LookPath("ffmpeg")
	ffprobePath, ffprobeErr := osexec.LookPath("ffprobe")
	if ffmpegErr != nil || ffprobeErr != nil {
		t.Skip("actual FFmpeg toolchain is not installed")
	}
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source")
	workingPath := filepath.Join(directory, "working")
	for _, root := range []string{sourcePath, workingPath} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for index := 1; index <= 3; index++ {
		outputPath := filepath.Join(sourcePath, fmt.Sprintf("C%04d.mp4", index))
		frameRate := 30
		if index == 3 {
			frameRate = 120
		}
		command := osexec.Command(
			ffmpegPath, "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
			"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=320x180:rate=%d", frameRate),
			"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:sample_rate=48000", 400+index*100),
			"-t", "1", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", outputPath,
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("create actual clip %d: %v: %s", index, err, output)
		}
	}
	configuration := workerconfig.Configuration{
		ID: "worker-actual", StorageRoots: []workerconfig.StorageRootConfiguration{
			{StorageID: "source", Root: sourcePath}, {StorageID: "working", Root: workingPath},
		},
		AccessibleStorageIDs: []string{"source", "working"},
		Tools: workerconfig.MediaTools{
			FFmpeg:  workerconfig.LocalTool{Executable: ffmpegPath, Version: "ffmpeg-actual"},
			FFprobe: workerconfig.LocalTool{Executable: ffprobePath, Version: "ffprobe-actual"},
		},
	}
	roots, err := workerconfig.OpenStorageRoots(configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	inputs := make([]execution.InputReference, 0, 3)
	clips := make([]organization.SourceClip, 0, 3)
	markers := make([]organization.CutMarker, 0, 2)
	for index := 1; index <= 3; index++ {
		assetID := fmt.Sprintf("ASSET-%d", index)
		name := fmt.Sprintf("C%04d.mp4", index)
		_, checksum, err := roots.MustRoot("source").FileSHA256(name)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, execution.InputReference{
			OriginalAssetID: assetID, OriginalName: name, InspectionID: fmt.Sprintf("INSPECTION-%d", index),
			StorageID: "source", RelativePath: name, Checksum: checksum, DurationMillis: 1_000,
			VideoCodec: "h264", Width: 320, Height: 180, VideoBitRateBPS: 500_000, HasAudio: true,
		})
		clips = append(clips, organization.SourceClip{Sequence: uint32(index), OriginalAssetID: assetID, InMillis: 0, OutMillis: 1_000})
		if index > 1 {
			markers = append(markers, organization.CutMarker{
				BeforeClipID: fmt.Sprintf("ASSET-%d", index-1), AfterClipID: assetID, AtMillis: int64(index-1) * 1_000,
			})
		}
	}
	timeline := organization.SourceTimeline{
		ID: "TIMELINE-1", Revision: 1, DecisionID: "DECISION-1", Clips: clips, Markers: markers,
	}
	spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: "JOB-ACTUAL", StepID: "sequence_render", TemplateName: "archive-intake-source-sequence",
		Operation: execution.OperationRenderSourceSequence, Inputs: inputs,
		Outputs: []execution.OutputTarget{
			{ID: "RENDITION-1", Kind: execution.OutputRendition, StorageID: "working", RelativePath: "staging/JOB-ACTUAL/sequence.mp4"},
			{ID: "MARKERS-1", Kind: execution.OutputCutMarkers, StorageID: "working", RelativePath: "staging/JOB-ACTUAL/sequence.markers.json"},
		},
		ProfileReference: "archive-1080p-20mbps@v1",
		Parameters: execution.StepExecutionParameters{
			OrganizationDecisionID: "DECISION-1", WorkspaceDriveID: "DRIVE-1", MediaFolder: "source",
			ArchiveStorageID: "working", InputRevision: 1, Disposition: vocabulary.OrganizationCompileSourceSequence,
			SelectedProfileReference: "archive-1080p-20mbps@v1", SourceInputs: inputs, SourceTimeline: &timeline,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewProduction(configuration, roots)
	if err != nil {
		t.Fatal(err)
	}
	result := registry.Execute(context.Background(), workerclient.Assignment{
		JobID: spec.JobID, StepID: spec.StepID, FencingToken: 1, LeaseExpiresAt: time.Now().Add(time.Minute), ExecutionSpec: spec,
	})
	if !result.Success || len(result.Outputs) != 2 {
		t.Fatalf("actual source sequence failed: %+v", result)
	}
	for _, relativePath := range []string{"staging/JOB-ACTUAL/sequence.mp4", "staging/JOB-ACTUAL/sequence.markers.json"} {
		if size, _, err := roots.MustRoot("working").FileSHA256(relativePath); err != nil || size <= 0 {
			t.Fatalf("actual source sequence output %s is missing: size=%d err=%v", relativePath, size, err)
		}
	}
}

type copyHandler struct{ calls int }

func (handler *copyHandler) Operation() execution.Operation { return execution.OperationOrganizeMedia }

func (handler *copyHandler) Execute(context.Context, execution.StepExecutionSpec, *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	handler.calls++
	return nil, nil, errors.New("duplicate fixture must not execute")
}
