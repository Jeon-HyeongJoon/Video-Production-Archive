package workerexecutor

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/adapter/sqlitearchiveorganization"
	"videoarchive/internal/adapter/sqlitearchiveworkspace"
	"videoarchive/internal/adapter/sqlitecatalog"
	"videoarchive/internal/adapter/sqlitecommand"
	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/archiveintake"
	"videoarchive/internal/application/archiveorganization"
	"videoarchive/internal/application/dispatchcoordinator"
	"videoarchive/internal/application/workercontrol"
	"videoarchive/internal/core/archive"
	coreintake "videoarchive/internal/core/archiveintake"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/qc"
	"videoarchive/internal/core/resource"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

func renderTestOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mediaCommand(t *testing.T, tool string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(tool, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v: %s", tool, err, stderr.String())
	}
	return data
}

type renderFixture struct {
	registry *Registry
	dest     *archive.StorageRoot
	spec     execution.StepExecutionSpec
	dbPath   string
	probe    string
}

// Create MP4 packets with known frame positions. Damage only the selected
// video packets; the source index, audio and all duration metadata stay intact.
func recordingFixture(t *testing.T, videoSeconds string, intactFrames int) renderFixture {
	t.Helper()
	ctx := context.Background()
	encoder, err := exec.LookPath("ffmpeg")
	renderTestOK(t, err)
	probe, err := exec.LookPath("ffprobe")
	renderTestOK(t, err)
	const folder = "370105-행사_촬영-본당"
	src, dst, logs := t.TempDir(), t.TempDir(), t.TempDir()
	renderTestOK(t, os.Mkdir(filepath.Join(src, folder), 0700))
	source := filepath.Join(src, folder, "행사 녹화.mp4")
	mediaCommand(t, encoder, "-v", "error", "-nostdin", "-f", "lavfi", "-i", "testsrc2=s=160x90:r=12:d="+videoSeconds,
		"-f", "lavfi", "-i", "sine=sample_rate=48000:duration=6", "-c:v", "libx264", "-g", "12", "-bf", "0", "-threads", "1", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", source)
	if intactFrames < 72 {
		var packets struct{ Packets []struct{ Pos string } }
		renderTestOK(t, json.Unmarshal(mediaCommand(t, probe, "-v", "error", "-select_streams", "v:0", "-show_packets", "-show_entries", "packet=pos", "-of", "json", source), &packets))
		if len(packets.Packets) != 72 {
			t.Fatal("expected 72 source video packets", len(packets.Packets))
		}
		data, err := os.ReadFile(source)
		renderTestOK(t, err)
		for _, p := range packets.Packets[intactFrames:] {
			pos, err := strconv.Atoi(p.Pos)
			renderTestOK(t, err)
			copy(data[pos:pos+4], []byte{0x7f, 0xff, 0xff, 0xff})
		}
		renderTestOK(t, os.WriteFile(source, data, 0600))
	}
	config := workerconfig.Configuration{
		ID: "worker", AccessibleStorageIDs: []string{"src", "dest"},
		StorageRoots: []workerconfig.StorageRootConfiguration{{StorageID: "src", Root: src}, {StorageID: "dest", Root: dst}},
		Tools: workerconfig.MediaTools{
			FFmpeg:  workerconfig.LocalTool{Executable: encoder, Version: "test"},
			FFprobe: workerconfig.LocalTool{Executable: probe, Version: "test"},
		},
		ProcessLogs: &workerconfig.ProcessLogConfiguration{
			Root: logs, MaxFileBytes: 1 << 20, TailBytes: 4096, Retention: time.Hour, MaxTotalBytes: 4 << 20,
		},
	}
	roots, err := workerconfig.OpenStorageRoots(config)
	renderTestOK(t, err)
	t.Cleanup(func() { roots.Close() })
	registry, err := NewProduction(config, roots)
	renderTestOK(t, err)
	now := time.Now().UTC()
	inspect, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: "inspection", StepID: "inspect", TemplateName: "archive-intake-inspection", Operation: execution.OperationInspectMedia,
		Outputs: []execution.OutputTarget{{ID: "observations", Kind: execution.OutputMediaInspection, StorageID: "dest", RelativePath: "staging/inspection.json"}},
		Parameters: execution.StepExecutionParameters{
			ArchiveRunID: "run", ArchiveIntakeSessionID: "session", StartIdempotencyKey: "start",
			SourceStorageID: "src", ArchiveStorageID: "dest", WorkspaceDriveID: "vol",
			InputRevision: 1, FolderIDs: []string{folder}, RequestedAt: now, RequestedBy: "operator",
		},
	})
	renderTestOK(t, err)
	result := registry.ExecuteDirect(ctx, inspect, 1)
	if !result.Success || len(result.InspectedMedia) != 1 {
		t.Fatal("inspect", result)
	}
	item := result.InspectedMedia[0]
	t.Cleanup(func() {
		_, sum, err := roots.MustRoot("src").FileSHA256(item.Observation.RelativePath)
		if err != nil || sum != item.Inspection.OriginalChecksum {
			t.Error("source changed", sum, err)
		}
	})
	dbPath := filepath.Join(t.TempDir(), "control.db")
	store, err := sqlitecommand.Open(dbPath)
	renderTestOK(t, err)
	defer store.Close()
	catalog, err := sqlitecatalog.Open(dbPath)
	renderTestOK(t, err)
	defer catalog.Close()
	receipts, err := sqlitearchiveorganization.Open(dbPath)
	renderTestOK(t, err)
	defer receipts.Close()
	handler, err := job.NewHandler(func() time.Time { return now })
	renderTestOK(t, err)
	bus := command.NewAuthorizedCreatingExecutor(store, func(command.Envelope) error { return nil }, handler.Handle)
	coordinator, err := archiveorganization.NewInspectedCoordinator(catalog, receipts, bus)
	renderTestOK(t, err)
	scope, err := organization.FixFolderScope(organization.FixFolderScopeRequest{DiscoveryRevision: 1, DiscoveredFolderIDs: []string{folder}, SelectAll: true})
	renderTestOK(t, err)
	run, err := coordinator.ExecuteInspected(ctx, archiveorganization.Request{
		ArchiveRunID: "run", IdempotencyKey: "start", InputRevision: 1, ActorID: "operator", StartedAt: now,
		ExecutionPriority: resource.Immediate, ArchiveStorageID: "dest", FolderScope: scope, PolicyVersion: organization.DrivePolicyVersion,
		Session: &archiveintake.SessionAccess{Session: coreintake.Session{
			ID: "session", StorageID: "src", WorkerID: "worker", WorkspaceDriveID: "vol", PhysicalDeviceID: "disk",
		}},
		Contexts: map[string]organization.EventContext{folder: {InputRevision: 1, EventFolder: folder}},
	}, result.InspectedMedia)
	renderTestOK(t, err)
	snapshot, err := store.Load(run.Receipt.Folders[0].JobID)
	renderTestOK(t, err)
	state, err := job.Decode(snapshot)
	renderTestOK(t, err)
	for _, step := range state.Steps {
		if step.ExecutionSpec != nil && step.ExecutionSpec.Operation == execution.OperationRenderRecording {
			spec := *step.ExecutionSpec
			sourceVideo := item.Inspection.VideoStreams[0].DurationMillis
			if sourceVideo <= 0 || spec.Parameters.SourceInputs[0].VideoDurationMillis != sourceVideo {
				t.Fatal("inspected video track duration lost before render", sourceVideo, spec.Parameters.SourceInputs)
			}
			return renderFixture{registry, roots.MustRoot("dest"), spec, dbPath, probe}
		}
	}
	t.Fatal("missing recording step")
	return renderFixture{}
}

func testRenderedVideoDuration(t *testing.T) {
	for _, tc := range []struct {
		name, videoSeconds string
		intact, frames     int
		expect             string
		actualMillis       int
		pass               bool
	}{
		{"healthy", "6", 72, 72, "6000±1500ms", 6000, true},
		{"loss within tolerance", "6", 71, 71, "6000±1500ms", 5917, true},
		{"audio masks lost video", "6", 30, 30, "6000±1500ms", 2500, false},
		{"short video in original", "2.5", 72, 30, "2500±1500ms", 2500, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := recordingFixture(t, tc.videoSeconds, tc.intact)
			ctx := context.Background()
			spec := f.spec
			var completed []execution.CompletedStep
			video := spec.Outputs[0].RelativePath
			for _, step := range []string{"render", "qc", "organize", "archive_catalog"} {
				if step != "render" {
					var err error
					spec, err = execution.MaterializeDependentStepExecutionSpec(execution.MaterializeRequest{JobID: spec.JobID, TemplateName: spec.TemplateName, StepID: step, Completed: completed})
					renderTestOK(t, err)
				}
				result := f.registry.ExecuteDirect(ctx, spec, 1)
				if step == "qc" {
					var checks []qc.Check
					if tc.pass {
						if !result.Success {
							t.Fatal(result)
						}
						data, err := f.dest.ReadFile(spec.Outputs[0].RelativePath)
						renderTestOK(t, err)
						var report qc.QCAssessment
						renderTestOK(t, json.Unmarshal(data, &report))
						checks = report.Checks
					} else {
						if result.Success || result.CauseCode != "QC_CHECK_FAILED" || result.QCFailure == nil || len(result.QCFailure.Checks) != 1 || result.QCFailure.SourceInspectionID != f.spec.Inputs[0].InspectionID {
							t.Fatalf("expected measured video loss: %+v", result)
						}
						checks = result.QCFailure.Checks
						legacy := spec
						legacy.Parameters.SourceInputs = slices.Clone(spec.Parameters.SourceInputs)
						legacy.Parameters.SourceInputs[0].VideoDurationMillis = 0
						legacy, err := execution.NewStepExecutionSpec(legacy)
						renderTestOK(t, err)
						again := f.registry.ExecuteDirect(ctx, legacy, 2)
						if again.CauseCode != "QC_CHECK_FAILED" || again.QCFailure == nil || !slices.Equal(again.QCFailure.Checks, checks) {
							t.Fatal("legacy source was accepted or measured differently", again)
						}
						for _, file := range []string{spec.Outputs[0].RelativePath, "archive-catalog.csv"} {
							exists, err := f.dest.Exists(file)
							if err != nil || exists {
								t.Fatal("unexpected publication", file, exists, err)
							}
						}
					}
					i := slices.IndexFunc(checks, func(c qc.Check) bool { return c.Kind == "VIDEO_DURATION" })
					if i < 0 || checks[i].Passed != tc.pass || checks[i].Expected != tc.expect {
						t.Fatal(checks)
					}
					// MP4 timescale rounding differs by a millisecond between muxers.
					got, err := strconv.Atoi(strings.TrimSuffix(checks[i].Actual, "ms"))
					if err != nil || !strings.HasSuffix(checks[i].Actual, "ms") || got < tc.actualMillis-1 || got > tc.actualMillis+1 {
						t.Fatal(checks[i])
					}
					if !tc.pass {
						break
					}
				}
				if !result.Success {
					t.Fatal(step, result)
				}
				record, err := execution.NewStepOutputRecord(spec, 1, "worker", time.Now().UTC(), result.Outputs, result.VerificationReferences)
				renderTestOK(t, err)
				completed = append(completed, execution.CompletedStep{StepID: step, Spec: spec, OutputRecord: record})
			}
			if tc.pass {
				raw, err := f.dest.ReadFile("archive-catalog.csv")
				renderTestOK(t, err)
				rows, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
				renderTestOK(t, err)
				if len(rows) != 2 || slices.Index(rows[0], "result_video_path") < 0 {
					t.Fatal("catalog", rows)
				}
				video = rows[1][slices.Index(rows[0], "result_video_path")]
			}
			resolved, err := f.dest.ExternalProcessPath(video)
			renderTestOK(t, err)
			n := mediaCommand(t, f.probe, "-v", "error", "-select_streams", "v:0", "-count_frames", "-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", resolved)
			if strings.TrimSpace(string(n)) != strconv.Itoa(tc.frames) {
				t.Fatalf("%s: expected %d frames, observed %s", video, tc.frames, n)
			}
		})
	}
}

type renderWorkerCredential struct{}

func (renderWorkerCredential) Verify(id, ref, value string) bool {
	return id == "worker" && ref == value
}

func testTerminalDecode(t *testing.T) {
	for _, intact := range []int{12, 0} {
		t.Run(strconv.Itoa(intact)+" intact frames", func(t *testing.T) { testTerminalRender(t, intact) })
	}
}

func testTerminalRender(t *testing.T, intact int) {
	f := recordingFixture(t, "6", intact)
	ctx := context.Background()
	store, err := sqlitecommand.Open(f.dbPath)
	renderTestOK(t, err)
	// Reopen a separate control connection after the first failure.
	defer func() { store.Close() }()
	projection, err := sqlitearchiveworkspace.Open(filepath.Join(t.TempDir(), "projection.db"))
	renderTestOK(t, err)
	defer projection.Close()
	now := time.Now().UTC()
	storage := map[string]bool{"src": true, "dest": true}
	workers := worker.NewRegistry([]worker.Definition{{
		ID: "worker", Enabled: true, CredentialRef: "test", AllowedStorageIDs: storage,
		ProcessingKinds: map[vocabulary.ProcessingKind]bool{vocabulary.ProcessingRender: true},
	}}, renderWorkerCredential{}, time.Hour)
	renderTestOK(t, workers.Connect("worker", "test", now, worker.Metrics{
		SnapshotID: 1, ObservedAt: now, AvailableMemoryBytes: 64 << 30, ScratchFreeBytes: 128 << 30, AcceptingWork: true,
		IdleStableFor: time.Hour, BusyPhysicalDeviceIDs: map[string]bool{}, AccessibleStorageIDs: storage,
	}))
	dispatch := func() bool {
		d, err := dispatchcoordinator.New(store, workers, projection, func() time.Time { return now }, time.Hour, time.Hour)
		renderTestOK(t, err)
		ok, err := d.DispatchOnce(ctx)
		renderTestOK(t, err)
		return ok
	}
	if !dispatch() {
		t.Fatal("initial render was not dispatched")
	}
	assignment, _, err := store.ClaimWorkerStep(ctx, "worker", now)
	renderTestOK(t, err)
	var pos execution.ProgressUpdate
	ctx = execution.WithProgressReporter(ctx, func(_ context.Context, u execution.ProgressUpdate) error {
		if u.Status == execution.ProgressPosition {
			pos = u
		}
		return nil
	})
	res := f.registry.ExecuteDirect(ctx, assignment.ExecutionSpec, assignment.FencingToken)
	if res.Success && intact == 12 {
		// FFmpeg 6.1 can exit successfully after emitting the first second.
		// That is a QC failure, whereas the zero-frame case below exits 69.
		record, err := execution.NewStepOutputRecord(f.spec, assignment.FencingToken, "worker", now, res.Outputs, res.VerificationReferences)
		renderTestOK(t, err)
		verify, err := execution.MaterializeDependentStepExecutionSpec(execution.MaterializeRequest{
			JobID: f.spec.JobID, TemplateName: f.spec.TemplateName, StepID: "qc",
			Completed: []execution.CompletedStep{{StepID: f.spec.StepID, Spec: f.spec, OutputRecord: record}},
		})
		renderTestOK(t, err)
		failed := f.registry.ExecuteDirect(ctx, verify, 2)
		if failed.CauseCode != "QC_CHECK_FAILED" || failed.QCFailure == nil || len(failed.QCFailure.Checks) != 1 || failed.QCFailure.Checks[0].Kind != qc.CheckVideoDuration || failed.QCFailure.Checks[0].Actual != "1000ms" {
			t.Fatal(failed)
		}
		return
	}
	if res.Success || res.CauseCode != "SOURCE_DECODE_FAILED" || res.FailureClass != workflow.FailureInput || res.ProcessExitCode == nil || *res.ProcessExitCode != 69 || res.ProcessLog == nil ||
		(intact > 0 && (pos.OutputMillis <= 0 || pos.InputIndex != 1 || pos.InputCount != 1)) {
		t.Fatalf("render %+v, progress %+v", res, pos)
	}
	_, err = store.SettleWorkerStep(ctx, workercontrol.Result{
		WorkerID: "worker", JobID: assignment.JobID, StepID: assignment.StepID, FencingToken: assignment.FencingToken,
		FailureClass: res.FailureClass, ErrorClass: res.ErrorClass, CauseCode: res.CauseCode, ErrorSummary: res.ErrorSummary,
		ProcessExitCode: res.ProcessExitCode, LogReference: res.LogReference, ProcessLog: res.ProcessLog,
	}, now)
	renderTestOK(t, err)
	renderTestOK(t, store.Close())
	store, err = sqlitecommand.Open(f.dbPath)
	renderTestOK(t, err)
	snapshot, err := store.Load(f.spec.JobID)
	renderTestOK(t, err)
	state, err := job.Decode(snapshot)
	renderTestOK(t, err)
	for _, step := range state.Steps {
		if step.Workflow.ID != assignment.StepID {
			continue
		}
		if step.Workflow.State != workflow.Failed || step.Workflow.Attempt != 1 || len(step.FailureEvidence) != 1 {
			t.Fatal(step)
		}
		e := step.FailureEvidence[0]
		if e.CauseCode != "SOURCE_DECODE_FAILED" || e.ProcessExitCode == nil || *e.ProcessExitCode != 69 || e.LogReference != res.ProcessLog.Reference || e.Summary != res.ErrorSummary ||
			!strings.Contains(e.Summary, assignment.ExecutionSpec.Inputs[0].OriginalAssetID) || !strings.Contains(e.Summary, strconv.FormatInt(pos.OutputMillis, 10)+"ms") {
			t.Fatal(e)
		}
	}
	if dispatch() {
		t.Fatal("terminal render dispatched a second time")
	}
	if _, _, err = store.ClaimWorkerStep(ctx, "worker", now); !errors.Is(err, workercontrol.ErrNoWork) {
		t.Fatalf("second claim: %v", err)
	}
	for _, file := range []string{f.spec.Outputs[0].RelativePath, temporaryOutputPathForToken(f.spec.Outputs[0].RelativePath, assignment.FencingToken), "archive-catalog.csv"} {
		exists, err := f.dest.Exists(file)
		if err != nil || exists {
			t.Fatal("failed render left output", file, err)
		}
	}
}

func testRenderOutputFailure(t *testing.T) {
	f := recordingFixture(t, "6", 72)
	// An occupied output directory fails on Windows and Unix, including as root.
	file := temporaryOutputPathForToken(f.spec.Outputs[0].RelativePath, 1)
	renderTestOK(t, f.dest.MkdirAll(file, 0700))
	renderTestOK(t, f.dest.WriteFile(file+"/keep", []byte("previous"), 0600))
	r := f.registry.ExecuteDirect(context.Background(), f.spec, 1)
	if r.Success || r.FailureClass == workflow.FailureInput || r.ProcessExitCode == nil {
		t.Fatal(r)
	}
	prior, err := f.dest.ReadFile(file + "/keep")
	if err != nil || string(prior) != "previous" {
		t.Fatal("existing device-side entry lost", err)
	}
}

func testCancelledRecording(t *testing.T) {
	f := recordingFixture(t, "2.5", 72)
	// Two recordings with identical input bytes have distinct asset identities.
	spec := f.spec
	second := spec.Inputs[0]
	second.OriginalAssetID = "ASSET-2"
	second.InspectionID = "INSPECTION-2"
	spec.Inputs = append(spec.Inputs, second)
	spec.Parameters.SourceInputs = append(spec.Parameters.SourceInputs, second)
	target := spec.Outputs[0]
	target.ID, target.OriginalAssetID = "RENDITION-2", second.OriginalAssetID
	target.RelativePath = filepath.ToSlash(filepath.Join(filepath.Dir(target.RelativePath), "second.mp4"))
	spec.Outputs = append(spec.Outputs, target)
	spec, err := execution.NewStepExecutionSpec(spec)
	renderTestOK(t, err)
	first := spec.Outputs[0].RelativePath
	other := temporaryOutputPathForToken(target.RelativePath, 7)
	renderTestOK(t, f.dest.MkdirAll(filepath.ToSlash(filepath.Dir(other)), 0700))
	renderTestOK(t, f.dest.WriteFile(other, []byte("other attempt"), 0600))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var verified string
	ctx = execution.WithProgressReporter(ctx, func(_ context.Context, u execution.ProgressUpdate) error {
		if u.Status == execution.ProgressPosition && u.InputIndex == 2 && verified == "" {
			_, verified, err = f.dest.FileSHA256(first)
			cancel()
		}
		return err
	})
	result := f.registry.ExecuteDirect(ctx, spec, 1)
	renderTestOK(t, err)
	if verified == "" || result.Cancellation == nil || !result.Cancellation.ProcessStopped || result.Cancellation.CleanupConfirmed ||
		!slices.Contains(result.Cancellation.ExternalEffectReferences, "dest:"+first) {
		t.Fatal(result)
	}
	for _, file := range []string{target.RelativePath, temporaryOutputPathForToken(target.RelativePath, 1)} {
		exists, err := f.dest.Exists(file)
		if err != nil || exists {
			t.Fatal("cancelled render left an output", file, err)
		}
	}
	bytes, err := f.dest.ReadFile(other)
	if err != nil || string(bytes) != "other attempt" {
		t.Fatal("other attempt changed", err)
	}
	_, sum, err := f.dest.FileSHA256(first)
	if err != nil || sum != verified {
		t.Fatal("verified sibling changed", sum, err)
	}
}
