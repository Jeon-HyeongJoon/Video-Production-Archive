package archiveorganization

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"videoarchive/internal/adapter/sqlitearchiveorganization"
	"videoarchive/internal/adapter/sqlitecatalog"
	"videoarchive/internal/adapter/sqlitecommand"
	appintake "videoarchive/internal/application/archiveintake"
	"videoarchive/internal/core/archive"
	coreintake "videoarchive/internal/core/archiveintake"
	"videoarchive/internal/core/asset"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/resource"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

// Requirement: APP-002
func TestCoordinatorIdempotentlyProcessesOnlyFixedFolderScope(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	sourceRootPath := filepath.Join(directory, "source")
	for relative, payload := range map[string]string{
		"ready/event.mp4": "ready-recording", "hold/a.mov": "raw-a", "hold/b.mov": "raw-b",
		"outside/ignored.mov": "out-of-scope",
	} {
		fullPath := filepath.Join(sourceRootPath, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := archive.OpenReadOnlyStorageRoot(sourceRootPath)
	if err != nil {
		t.Fatal(err)
	}
	access := &appintake.SessionAccess{
		Session: coreintake.Session{
			ID: "INTAKE-1", Revision: 1, CandidateRevision: 1, VolumeID: "VOLUME-1",
			WorkspaceDriveID: "VOLUME-1", PhysicalDeviceID: "PHYSICAL-1", WorkerID: "WORKER-1", StorageID: "SOURCE-1",
			SelectedBy: "operator-1", SelectedAt: time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC),
		},
		Root: root,
	}
	defer access.Close()
	catalog, err := sqlitecatalog.Open(filepath.Join(directory, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	commandStore, err := sqlitecommand.Open(filepath.Join(directory, "command.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer commandStore.Close()
	runStore, err := sqlitearchiveorganization.Open(filepath.Join(directory, "organization.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer runStore.Close()
	now := time.Date(2026, 10, 30, 17, 0, 0, 0, time.UTC)
	jobHandler, err := job.NewHandler(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	jobBus := command.NewAuthorizedCreatingExecutor(commandStore, func(command.Envelope) error { return nil }, jobHandler.Handle)
	analyzer := &fixtureAnalyzer{now: now}
	coordinator, err := NewCoordinator(catalog, runStore, jobBus, analyzer)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{
		ArchiveRunID: "ORG-RUN-1", IdempotencyKey: "drive-organize:INTAKE-1:r4", InputRevision: 4,
		FolderScope: mustFolderScope(t, 4, []string{"ready", "hold"}),
		Session:     access, ArchiveStorageID: "ARCHIVE-1", ActorID: "operator-1", StartedAt: now,
		ExecutionPriority: resource.Background,
	}
	first, err := coordinator.Execute(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || len(first.Receipt.Files) != 3 || len(first.Receipt.Folders) != 2 {
		t.Fatalf("initial coordination receipt is incomplete: %+v", first)
	}
	jobCount, holdCount := 0, 0
	for _, folder := range first.Receipt.Folders {
		if folder.Decision.Disposition == vocabulary.OrganizationHold {
			holdCount++
			if folder.JobID != "" {
				t.Fatalf("HOLD folder created a Job: %+v", folder)
			}
			continue
		}
		jobCount++
		snapshot, err := commandStore.Load(folder.JobID)
		if err != nil {
			t.Fatal(err)
		}
		state, err := job.Decode(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range state.Steps {
			if len(step.PhysicalDeviceIDs) != 1 || step.PhysicalDeviceIDs[0] != "PHYSICAL-1" ||
				len(step.RequiredStorageIDs) != 2 || step.RequiredStorageIDs[0] != "SOURCE-1" || step.RequiredStorageIDs[1] != "ARCHIVE-1" {
				t.Fatalf("folder Job lost workspace context: %+v", step)
			}
		}
	}
	if jobCount != 1 || holdCount != 1 {
		t.Fatalf("folder decisions did not branch once: jobs=%d holds=%d receipt=%+v", jobCount, holdCount, first.Receipt)
	}
	for _, file := range first.Receipt.Files {
		observation, err := catalog.Observation(ctx, file.ObservationID)
		if err != nil || observation.WorkspaceDriveID != "VOLUME-1" || observation.RelativePath != file.RelativePath {
			t.Fatalf("observation link is missing: link=%+v observation=%+v err=%v", file, observation, err)
		}
		originals, err := catalog.FindOriginalsByChecksum(ctx, observation.Checksum)
		if err != nil || len(originals) != 1 || originals[0].ID != file.OriginalAssetID {
			t.Fatalf("original link is missing: link=%+v originals=%+v err=%v", file, originals, err)
		}
		inspections, err := catalog.InspectionsByOriginal(ctx, file.OriginalAssetID)
		if err != nil || len(inspections) != 1 || inspections[0].ID != file.InspectionID {
			t.Fatalf("inspection link is missing: link=%+v inspections=%+v err=%v", file, inspections, err)
		}
	}
	second, err := coordinator.Execute(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || second.Receipt.ID != first.Receipt.ID || analyzer.calls != 3 {
		t.Fatalf("repeat execution did not return the existing receipt: first=%+v second=%+v calls=%d", first, second, analyzer.calls)
	}
	snapshots, err := commandStore.ListSnapshots()
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("repeat execution created duplicate Jobs: snapshots=%d err=%v", len(snapshots), err)
	}
	firstFiles := make(map[string]organization.CoordinatedFile, len(first.Receipt.Files))
	for _, file := range first.Receipt.Files {
		firstFiles[file.RelativePath] = file
	}
	firstFolders := make(map[string]organization.CoordinatedFolder, len(first.Receipt.Folders))
	for _, folder := range first.Receipt.Folders {
		firstFolders[folder.Folder] = folder
	}
	newRunRequest := request
	newRunRequest.ArchiveRunID = "ORG-RUN-2"
	newRunRequest.IdempotencyKey = "drive-organize:INTAKE-1:r4:second-run"
	newRunRequest.StartedAt = now.Add(time.Hour)
	analyzer.now = newRunRequest.StartedAt
	newRun, err := coordinator.Execute(ctx, newRunRequest)
	if err != nil {
		t.Fatal(err)
	}
	if newRun.Replayed || newRun.Receipt.ID == first.Receipt.ID || analyzer.calls != 6 {
		t.Fatalf("new ArchiveRun did not independently inspect the same fixed scope: first=%+v new=%+v calls=%d", first, newRun, analyzer.calls)
	}
	for _, file := range newRun.Receipt.Files {
		previous := firstFiles[file.RelativePath]
		if previous.OriginalAssetID == "" || file.OriginalAssetID != previous.OriginalAssetID || file.ObservationID != previous.ObservationID || file.InspectionID == previous.InspectionID {
			t.Fatalf("new ArchiveRun did not reuse source identity and isolate inspection: previous=%+v new=%+v", previous, file)
		}
		observation, err := catalog.Observation(ctx, file.ObservationID)
		if err != nil {
			t.Fatal(err)
		}
		originals, err := catalog.FindOriginalsByChecksum(ctx, observation.Checksum)
		if err != nil || len(originals) != 1 || originals[0].ID != file.OriginalAssetID {
			t.Fatalf("new ArchiveRun duplicated OriginalAsset identity: file=%+v originals=%+v err=%v", file, originals, err)
		}
	}
	for _, folder := range newRun.Receipt.Folders {
		previous := firstFolders[folder.Folder]
		if previous.Decision.ID == "" || folder.Decision.ID == previous.Decision.ID || (folder.JobID != "" && folder.JobID == previous.JobID) {
			t.Fatalf("new ArchiveRun reused decision or Job identity: previous=%+v new=%+v", previous, folder)
		}
	}
	replayedNewRun, err := coordinator.Execute(ctx, newRunRequest)
	if err != nil || !replayedNewRun.Replayed || replayedNewRun.Receipt.ID != newRun.Receipt.ID || analyzer.calls != 6 {
		t.Fatalf("new ArchiveRun replay was not idempotent: run=%+v replay=%+v calls=%d err=%v", newRun, replayedNewRun, analyzer.calls, err)
	}
	snapshots, err = commandStore.ListSnapshots()
	if err != nil || len(snapshots) != 2 {
		t.Fatalf("separate ArchiveRuns did not own separate Jobs: snapshots=%d err=%v", len(snapshots), err)
	}
	receipts, err := runStore.Receipts(ctx)
	if err != nil || len(receipts) != 2 {
		t.Fatalf("separate ArchiveRuns did not retain separate receipts: receipts=%d err=%v", len(receipts), err)
	}
}

// Requirement: APP-003
func TestCoordinatorCreatesDeterministicFirstWorkerStepSpecsForEveryAutomaticDisposition(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	sourceRootPath := filepath.Join(directory, "source")
	for relative, payload := range map[string]string{
		"as-is/ready.mp4": "ready", "render/raw.mov": "raw",
		"sequence/clip001.mov": "clip-1", "sequence/clip002.mov": "clip-2",
	} {
		fullPath := filepath.Join(sourceRootPath, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := archive.OpenReadOnlyStorageRoot(sourceRootPath)
	if err != nil {
		t.Fatal(err)
	}
	access := &appintake.SessionAccess{Session: coreintake.Session{
		ID: "INTAKE-SPECS", Revision: 1, CandidateRevision: 1, VolumeID: "VOLUME-1",
		WorkspaceDriveID: "VOLUME-1", PhysicalDeviceID: "PHYSICAL-1", WorkerID: "WORKER-1", StorageID: "SOURCE-1",
		SelectedBy: "operator-1", SelectedAt: time.Date(2026, 10, 30, 18, 0, 0, 0, time.UTC),
	}, Root: root}
	defer access.Close()
	catalog, err := sqlitecatalog.Open(filepath.Join(directory, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	commandStore, err := sqlitecommand.Open(filepath.Join(directory, "command.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer commandStore.Close()
	runStore, err := sqlitearchiveorganization.Open(filepath.Join(directory, "organization.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer runStore.Close()
	now := time.Date(2026, 10, 30, 18, 30, 0, 0, time.UTC)
	jobHandler, err := job.NewHandler(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	jobBus := command.NewAuthorizedCreatingExecutor(commandStore, func(command.Envelope) error { return nil }, jobHandler.Handle)
	coordinator, err := NewCoordinator(catalog, runStore, jobBus, &specificationAnalyzer{now: now})
	if err != nil {
		t.Fatal(err)
	}
	request := Request{
		ArchiveRunID: "ORG-RUN-SPECS", IdempotencyKey: "drive-organize:INTAKE-SPECS:r7", InputRevision: 7,
		FolderScope: mustFolderScope(t, 7, []string{"as-is", "render", "sequence"}),
		Session:     access, ArchiveStorageID: "ARCHIVE-1", ActorID: "operator-1", StartedAt: now,
		ExecutionPriority: resource.Background,
	}
	result, err := coordinator.Execute(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]struct {
		stepID    string
		operation execution.Operation
		inputs    int
		timeline  bool
	}{
		"as-is":    {stepID: "qc", operation: execution.OperationQCRendition, inputs: 1},
		"render":   {stepID: "render", operation: execution.OperationRenderRecording, inputs: 1},
		"sequence": {stepID: "sequence_render", operation: execution.OperationRenderSourceSequence, inputs: 2, timeline: true},
	}
	digests := make(map[string]string, len(expected))
	for _, folder := range result.Receipt.Folders {
		want, ok := expected[folder.Folder]
		if !ok || folder.JobID == "" {
			t.Fatalf("unexpected automatic folder result: %+v", folder)
		}
		snapshot, err := commandStore.Load(folder.JobID)
		if err != nil {
			t.Fatal(err)
		}
		state, err := job.Decode(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var spec *execution.StepExecutionSpec
		var sourceTimelineState workflow.StepState
		var firstWorkerState workflow.StepState
		for index := range state.Steps {
			if state.Steps[index].Workflow.ID == want.stepID {
				spec = state.Steps[index].ExecutionSpec
				firstWorkerState = state.Steps[index].Workflow.State
			}
			if state.Steps[index].Workflow.ID == "source_timeline" {
				sourceTimelineState = state.Steps[index].Workflow.State
			}
		}
		if spec == nil || spec.Validate() != nil || spec.JobID != folder.JobID || spec.StepID != want.stepID ||
			spec.TemplateName != state.TemplateName || spec.Operation != want.operation || len(spec.Inputs) != want.inputs ||
			len(spec.Outputs) == 0 || spec.Parameters.OrganizationDecisionID != folder.Decision.ID ||
			spec.Parameters.WorkspaceDriveID != "VOLUME-1" || spec.Parameters.InputRevision != 7 {
			t.Fatalf("folder %s has an invalid first Worker spec: %+v", folder.Folder, spec)
		}
		for _, input := range spec.Inputs {
			if input.StorageID != "SOURCE-1" || input.OriginalAssetID == "" || input.InspectionID == "" || input.RelativePath == "" {
				t.Fatalf("folder %s lost safe source references: %+v", folder.Folder, input)
			}
		}
		for _, output := range spec.Outputs {
			if output.StorageID != "ARCHIVE-1" || output.ID == "" || output.RelativePath == "" {
				t.Fatalf("folder %s lost stable output targets: %+v", folder.Folder, output)
			}
		}
		if (spec.Parameters.SourceTimeline != nil) != want.timeline {
			t.Fatalf("folder %s timeline mismatch: %+v", folder.Folder, spec.Parameters.SourceTimeline)
		}
		if want.timeline && (sourceTimelineState != workflow.Completed || firstWorkerState != workflow.Queued || state.CurrentStepID != want.stepID) {
			t.Fatalf("source timeline did not make sequence render ready: source=%s worker=%s current=%s", sourceTimelineState, firstWorkerState, state.CurrentStepID)
		}
		digests[folder.Folder] = spec.Digest
	}
	if len(digests) != len(expected) {
		t.Fatalf("not every automatic disposition created a Job spec: %v", digests)
	}
	replayed, err := coordinator.Execute(ctx, request)
	if err != nil || !replayed.Replayed {
		t.Fatalf("same input did not replay: %+v err=%v", replayed, err)
	}
	for _, folder := range replayed.Receipt.Folders {
		snapshot, err := commandStore.Load(folder.JobID)
		if err != nil {
			t.Fatal(err)
		}
		state, err := job.Decode(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range state.Steps {
			if step.ExecutionSpec != nil && step.ExecutionSpec.Digest != digests[folder.Folder] {
				t.Fatalf("replay changed spec digest for %s: %s", folder.Folder, step.ExecutionSpec.Digest)
			}
		}
	}
}

func mustFolderScope(t *testing.T, revision uint64, folderIDs []string) organization.FolderScope {
	t.Helper()
	scope, err := organization.FixFolderScope(organization.FixFolderScopeRequest{
		DiscoveryRevision: revision, DiscoveredFolderIDs: folderIDs, SelectAll: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

type specificationAnalyzer struct{ now time.Time }

func (analyzer *specificationAnalyzer) Inspect(_ context.Context, inspectionID string, original asset.OriginalAsset, relativePath, workerID string) (mediaanalysis.MediaInspection, error) {
	duration, bitRate, width, height, container := int64(300_000), int64(40_000_000), 3840, 2160, "mov"
	if relativePath == "as-is/ready.mp4" {
		duration, bitRate, width, height, container = 60_000, 6_000_000, 1920, 1080, "mp4"
	} else if relativePath == "sequence/clip001.mov" {
		duration, width, height = 10_000, 1920, 1080
	} else if relativePath == "sequence/clip002.mov" {
		duration, width, height = 12_000, 1920, 1080
	}
	return mediaanalysis.NewMediaInspection(mediaanalysis.MediaInspection{
		ID: inspectionID, OriginalAssetID: original.ID, OriginalChecksum: original.Checksum,
		Containers: []string{container}, DurationMillis: duration, SizeBytes: original.SizeBytes, BitRate: bitRate,
		VideoStreams: []mediaanalysis.VideoStream{{
			Index: 0, Codec: "h264", Width: width, Height: height,
			FrameRate: mediaanalysis.Rational{Numerator: 30, Denominator: 1}, BitRate: bitRate,
		}},
		AudioStreams:     []mediaanalysis.AudioStream{{Index: 1, Codec: "aac", SampleRateHz: 48_000, Channels: 2}},
		ProcessorVersion: "fixture-ffprobe-v1", ObservedBy: workerID, InspectedAt: analyzer.now,
	})
}

type fixtureAnalyzer struct {
	now   time.Time
	calls int
}

func (analyzer *fixtureAnalyzer) Inspect(_ context.Context, inspectionID string, original asset.OriginalAsset, relativePath, workerID string) (mediaanalysis.MediaInspection, error) {
	analyzer.calls++
	ready := relativePath == "ready/event.mp4"
	duration := int64(300_000)
	bitRate := int64(0)
	container := "mov"
	if ready {
		duration = 60_000
		bitRate = 6_000_000
		container = "mp4"
	}
	return mediaanalysis.NewMediaInspection(mediaanalysis.MediaInspection{
		ID: inspectionID, OriginalAssetID: original.ID, OriginalChecksum: original.Checksum,
		Containers: []string{container}, DurationMillis: duration, SizeBytes: original.SizeBytes, BitRate: bitRate,
		VideoStreams: []mediaanalysis.VideoStream{{
			Index: 0, Codec: "h264", Width: 1920, Height: 1080,
			FrameRate: mediaanalysis.Rational{Numerator: 30, Denominator: 1}, BitRate: bitRate,
		}},
		AudioStreams:     []mediaanalysis.AudioStream{{Index: 1, Codec: "aac", SampleRateHz: 48_000, Channels: 2}},
		ProcessorVersion: "fixture-ffprobe-v1", ObservedBy: workerID, InspectedAt: analyzer.now,
	})
}
