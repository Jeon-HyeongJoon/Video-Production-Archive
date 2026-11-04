package workerexecutor

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"
	"time"

	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/workerclient"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
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
			RelativePath: "packages/2026/10/31/VP-20261031-0001/revisions/000001/metadata.json", OriginalAssetID: "ASSET-1",
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
	published, err := roots.MustRoot("working").ReadFile("packages/2026/10/31/VP-20261031-0001/revisions/000001/clip.mp4")
	if err != nil || string(published) != string(sourcePayload) {
		t.Fatalf("verified output was not published through StorageRoot: %q err=%v", published, err)
	}

	unsupported := spec
	unsupported.Operation = execution.OperationInspectMedia
	unsupported.StepID = "inspect"
	unsupported.Outputs = []execution.OutputTarget{{
		ID: "INSPECTION-2", Kind: execution.OutputMediaInspection, StorageID: "working", RelativePath: "staging/inspection.json",
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
	if _, err := roots.MustRoot("working").ReadFile("staging/inspection.json"); err == nil {
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

	t.Run("actual ffmpeg source sequence", testActualSourceSequence)
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
		command := osexec.Command(
			ffmpegPath, "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
			"-f", "lavfi", "-i", "testsrc=size=320x180:rate=30",
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
