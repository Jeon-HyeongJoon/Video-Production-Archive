package httpmcp

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/adapter/archivebundle"
	"videoarchive/internal/adapter/ffprobe"
	"videoarchive/internal/adapter/sqlitearchiveorganization"
	"videoarchive/internal/adapter/sqlitecatalog"
	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/archiveintake"
	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/application/archiveorganization"
	"videoarchive/internal/application/workerexecutor"
	"videoarchive/internal/core/archive"
	coreintake "videoarchive/internal/core/archiveintake"
	"videoarchive/internal/core/asset"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/qc"
	"videoarchive/internal/core/resource"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

type eventInspector struct {
	root  *archive.ReadOnlyStorageRoot
	probe *ffprobe.Inspector
}

func (a eventInspector) Inspect(ctx context.Context, id string, original asset.OriginalAsset, relative, workerID string) (mediaanalysis.MediaInspection, error) {
	name, err := a.root.ExternalProcessPath(relative)
	if err != nil {
		return mediaanalysis.MediaInspection{}, err
	}
	return a.probe.Inspect(ctx, ffprobe.Request{InspectionID: id, OriginalAssetID: original.ID, OriginalChecksum: original.Checksum, ResolvedInputPath: name})
}

type publishedEvent struct {
	ArchivePackageID string   `json:"archive_package_id"`
	OriginalAssetIDs []string `json:"original_asset_ids"`
	Role             string   `json:"role"`
	ProfileReference string   `json:"profile_reference"`
	ArchiveRunID     string   `json:"archive_run_id"`
	InputRevision    uint64   `json:"input_revision"`
	RelativeVideo    string   `json:"relative_video"`
	PathState        string   `json:"path_state"`
	Stage            string   `json:"stage"`
	ManifestChecksum string   `json:"manifest_checksum"`
	Basis            []string `json:"basis"`
}

func checkPublishedEventOutputs(t *testing.T, folder, secondName, secondRole string) {
	ctx := t.Context()
	selection := archiveIntakeSelectionFixture(t)
	selection.FolderIDs = []string{folder}
	now := time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC)
	encoder, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	probe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Fatal(err)
	}
	src, dst := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, folder), 0700); err != nil {
		t.Fatal(err)
	}
	listed := worker.MediaFolderObservation{Folder: folder, VideoCount: 2}
	for _, f := range []struct{ name, size string }{{secondName, "160x90"}, {"C0001.MP4", "320x180"}} {
		relative := path.Join(folder, f.name)
		full := filepath.Join(src, filepath.FromSlash(relative))
		output, err := exec.CommandContext(ctx, encoder, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=s="+f.size+":r=24", "-t", "1", "-an", "-c:v", "libx264", "-threads", "1", full).CombinedOutput()
		if err != nil {
			t.Fatalf("create %s: %v %s", relative, err, output)
		}
		info, err := os.Stat(full)
		if err != nil {
			t.Fatal(err)
		}
		listed.Videos = append(listed.Videos, worker.MediaFileObservation{FileName: f.name, RelativePath: relative, SizeBytes: uint64(info.Size())})
	}
	cfg := workerconfig.Configuration{ID: selection.WorkerID, AccessibleStorageIDs: []string{selection.SourceStorageID, selection.ArchiveStorageID},
		StorageRoots: []workerconfig.StorageRootConfiguration{{StorageID: selection.SourceStorageID, Root: src}, {StorageID: selection.ArchiveStorageID, Root: dst}},
		Tools:        workerconfig.MediaTools{FFmpeg: workerconfig.LocalTool{Executable: encoder, Version: "test"}, FFprobe: workerconfig.LocalTool{Executable: probe, Version: "test"}}}
	roots, err := workerconfig.OpenStorageRoots(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	registry, err := workerexecutor.NewProduction(cfg, roots)
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := sqlitearchiveorganization.Open(filepath.Join(t.TempDir(), "receipts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer receipts.Close()
	catalog, err := sqlitecatalog.Open(filepath.Join(t.TempDir(), "sources.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	inspector, err := ffprobe.NewInspector(probe, "test", selection.WorkerID, ffprobe.OSRunner{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := archive.OpenReadOnlyStorageRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	jobs := command.NewMemoryStore(nil)
	handler, err := job.NewHandler(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	bus := command.NewAuthorizedCreatingExecutor(jobs, func(command.Envelope) error { return nil }, handler.Handle)
	coordinator, err := archiveorganization.NewCoordinator(catalog, receipts, bus, eventInspector{root, inspector})
	if err != nil {
		t.Fatal(err)
	}
	rev := selection.FolderDiscoveryRevision
	scope, err := organization.FixFolderScope(organization.FixFolderScopeRequest{DiscoveryRevision: rev, DiscoveredFolderIDs: []string{folder}, SelectAll: true})
	if err != nil {
		t.Fatal(err)
	}
	contexts := organization.InspectListing(worker.StorageObservation{DiscoveryState: vocabulary.FolderDiscoveryReady, DiscoveryRevision: rev, Folders: []worker.MediaFolderObservation{listed}})
	request := archiveorganization.Request{PolicyVersion: organization.DrivePolicyVersion, Contexts: map[string]organization.EventContext{folder: contexts[folder].EventContext}, ArchiveRunID: selection.ArchiveRunID,
		IdempotencyKey: "inspect", InputRevision: rev, FolderScope: scope, ArchiveStorageID: selection.ArchiveStorageID, ActorID: selection.SelectedBy, StartedAt: now, ExecutionPriority: resource.Immediate,
		Session: &archiveintake.SessionAccess{Root: root, Session: coreintake.Session{ID: selection.IntakeSessionID, CandidateRevision: selection.CandidateRevision,
			WorkspaceDriveID: selection.WorkspaceDriveID, StorageID: selection.SourceStorageID, WorkerID: selection.WorkerID, PhysicalDeviceID: "disk"}}}
	result, err := coordinator.Execute(ctx, request)
	if err != nil || len(result.Receipt.Folders) != 1 {
		t.Fatal(result, err)
	}
	coordinated := result.Receipt.Folders[0]
	d := coordinated.Decision
	if d.Disposition != vocabulary.OrganizationRenderRecordings || len(d.Inputs) != 2 {
		t.Fatalf("expected individual renders: %+v", d)
	}
	snapshot, err := jobs.Load(coordinated.JobID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := job.Decode(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot = publishFolderSteps(t, registry, state)
	svc, err := archiveintakeworkspace.New(receipts, command.NewMemoryStore(map[string]command.Snapshot{snapshot.TargetID: snapshot}))
	if err != nil {
		t.Fatal(err)
	}
	mcp := holdReviewMCPHandler(t, nil)
	mcp.drives = svc
	mcp.archivists = fixtureArchivistRuntime{selection: selection}
	resp := mcpTestRequest(mcp, http.MethodPost, "/mcp/v1/call", archiveToolCall("OUTPUTS", ToolArchiveRunStatus, `{}`), "secret")
	var status struct {
		Data struct {
			Outputs []publishedEvent                    `json:"event_output_evidence"`
			Folders []archiveintakeworkspace.FolderView `json:"folders"`
		} `json:"data"`
	}
	if resp.Code != http.StatusOK || json.Unmarshal(resp.Body.Bytes(), &status) != nil {
		t.Fatal(resp.Body.String())
	}
	dest := roots.MustRoot(selection.ArchiveStorageID)
	raw, err := dest.ReadFile("archive-catalog.csv")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
	if err != nil || len(rows) != 3 {
		t.Fatalf("catalog rows=%d, %v", len(rows), err)
	}
	if len(status.Data.Outputs) != 2 || len(status.Data.Folders) != 1 {
		t.Fatalf("catalog has two videos, Archivist returned %d outputs: %+v", len(status.Data.Outputs), status.Data.Outputs)
	}
	folderPackageMatched := false
	seen := map[string]bool{}
	for _, record := range rows[1:] {
		fields := map[string]string{}
		for i, k := range rows[0] {
			fields[k] = record[i]
		}
		id, video := fields["archive_package_id"], fields["result_video_path"]
		metadataPath := path.Join(path.Dir(video), ".archive-package.json")
		raw, err := dest.ReadFile(metadataPath)
		var doc archivebundle.CompactDocument
		if err != nil || json.Unmarshal(raw, &doc) != nil || len(doc.Sources) != 1 || doc.VideoPath != video || doc.PackageID != id || doc.ManifestDigest != fields["manifest_digest"] || doc.DecisionID != d.ID {
			t.Fatalf("catalog/metadata mismatch for %s: %s %v", id, raw, err)
		}
		if len(id) < 12 {
			t.Fatalf("invalid package ID %q", id)
		}
		source := doc.Sources[0]
		inputIndex := slices.IndexFunc(d.Inputs, func(v organization.Input) bool { return v.OriginalAssetID == source.OriginalAssetID })
		if inputIndex < 0 || seen[source.OriginalAssetID] {
			t.Fatalf("unexpected original in package %s: %+v", id, source)
		}
		seen[source.OriginalAssetID] = true
		input := d.Inputs[inputIndex]
		expectedRole, expectedProfile := organization.RoleSource, "archive-1080p-20mbps@v1"
		if path.Base(input.RelativePath) == secondName && secondRole == organization.RoleRecording {
			expectedRole, expectedProfile = secondRole, "archive-1080p-6mbps@v1"
		}
		_, sum, err := root.FileSHA256(input.RelativePath)
		if err != nil || sum != input.Inspection.OriginalChecksum || sum != source.Checksum || source.RelativePath != input.RelativePath ||
			input.RenderSelection == nil || input.RenderSelection.Role != expectedRole || input.RenderSelection.ProfileReference != expectedProfile || expectedProfile != fields["profile_reference"] || doc.ProfileReference != fields["profile_reference"] {
			t.Fatal(input, source, fields, err)
		}
		preview, err := archive.PreviewFlatPackage(doc.ArchiveDate, folder, []string{path.Base(input.RelativePath)}, 1)
		if err != nil {
			t.Fatal(err)
		}
		want := path.Join(strings.Replace(preview.FirstIndividualDirectoryPattern, "XXXXXXXXXXXX", id[len(id)-12:], 1), preview.FirstIndividualVideoFileName)
		size, videoSHA, err := dest.FileSHA256(video)
		if err != nil || size <= 0 || want != video || len(doc.Revisions) != 1 || !slices.ContainsFunc(doc.Revisions[0].Files, func(f archivebundle.CompactFile) bool {
			return f.Path == path.Base(video) && f.Checksum == videoSHA && f.Size == size
		}) {
			t.Fatalf("unverified published video %q (%s): %v", video, videoSHA, err)
		}
		i := slices.IndexFunc(status.Data.Outputs, func(o publishedEvent) bool { return o.ArchivePackageID == id })
		if i < 0 {
			t.Fatalf("Archivist has no package %s: %+v", id, status.Data.Outputs)
		}
		o := status.Data.Outputs[i]
		if o.ArchiveRunID != selection.ArchiveRunID || o.InputRevision != rev || o.RelativeVideo != video || o.PathState != "PUBLISHED_PACKAGE" || o.Stage != "STORED" || !slices.Equal(o.OriginalAssetIDs, []string{source.OriginalAssetID}) ||
			o.Role != input.RenderSelection.Role || o.ProfileReference != doc.ProfileReference {
			t.Fatalf("Archivist package mismatch for %s: %+v", id, o)
		}
		_, metadataSHA, err := dest.FileSHA256(metadataPath)
		publishProof := "verification://organize_media/" + id + "/" + metadataSHA
		if err != nil || o.ManifestChecksum != metadataSHA || !slices.Contains(o.Basis, publishProof) || !slices.Contains(o.Basis, metadataPath) {
			t.Fatalf("package proof: %+v %v", o, err)
		}
		var proofs int
		for _, ref := range o.Basis {
			if !strings.HasPrefix(ref, "verification://qc_rendition/") {
				continue
			}
			proofs++
			qcPath := path.Join("staging", coordinated.JobID, "qc", path.Base(path.Dir(ref))+".json")
			data, err := dest.ReadFile(qcPath)
			var proof qc.QCAssessment
			if err != nil || json.Unmarshal(data, &proof) != nil || proof.Outcome != qc.Passed || proof.TargetChecksum != videoSHA || proof.SourceInspectionID != input.Inspection.ID {
				t.Fatalf("QC proof for %s: %s %v", video, data, err)
			}
			if _, sum, err := dest.FileSHA256(qcPath); err != nil || sum != path.Base(ref) {
				t.Fatalf("QC checksum for %s: %s %v", video, sum, err)
			}
		}
		if proofs != 1 {
			t.Fatalf("expected one QC proof for %s, got %v", video, o.Basis)
		}
		t.Logf("%s -> %s / %s / %s, video SHA256 %s", source.OriginalAssetID, o.Role, id, video, videoSHA)
		f := status.Data.Folders[0]
		if f.ArchivePackageID == id {
			folderPackageMatched = true
			if f.ArchiveVideoFileName != path.Base(video) {
				t.Fatalf("folder package %s shows %s, published %s", id, f.ArchiveVideoFileName, video)
			}
		}
	}
	if !folderPackageMatched {
		t.Fatalf("folder summary references unpublished package %s", status.Data.Folders[0].ArchivePackageID)
	}
}

func publishFolderSteps(t *testing.T, registry *workerexecutor.Registry, state job.State) command.Snapshot {
	t.Helper()
	var completed []execution.CompletedStep
	for _, id := range []string{"render", "qc", "organize", "archive_catalog"} {
		i := slices.IndexFunc(state.Steps, func(step job.Step) bool { return step.Workflow.ID == id })
		if i < 0 {
			t.Fatalf("folder job is missing %s", id)
		}
		step := &state.Steps[i]
		if step.ExecutionSpec == nil {
			spec, err := execution.MaterializeDependentStepExecutionSpec(execution.MaterializeRequest{JobID: state.ID, TemplateName: state.TemplateName, StepID: step.Workflow.ID, Completed: completed})
			if err != nil {
				t.Fatal(step.Workflow.ID, err)
			}
			step.ExecutionSpec = &spec
		}
		spec := *step.ExecutionSpec
		for _, next := range []workflow.StepState{workflow.Reserved, workflow.Running} {
			if err := step.Workflow.Transition(next); err != nil {
				t.Fatal(spec.StepID, err)
			}
		}
		result := registry.ExecuteDirect(t.Context(), spec, 1)
		if !result.Success {
			t.Fatalf("%s: %+v", spec.StepID, result)
		}
		record, err := execution.NewStepOutputRecord(spec, 1, "WORKER-1", time.Now().UTC(), result.Outputs, result.VerificationReferences)
		if err != nil {
			t.Fatal(spec.StepID, err)
		}
		completed = append(completed, execution.CompletedStep{StepID: spec.StepID, Spec: spec, OutputRecord: record})
		step.OutputRecord = &record
		for _, next := range []workflow.StepState{workflow.Verifying, workflow.Completed} {
			if err := step.Workflow.Transition(next); err != nil {
				t.Fatal(spec.StepID, err)
			}
		}
		state.CurrentStepID = spec.StepID
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return command.Snapshot{TargetID: state.ID, Revision: state.Revision, State: raw}
}
