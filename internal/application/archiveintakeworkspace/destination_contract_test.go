package archiveintakeworkspace_test

import (
	"context"
	"fmt"
	"os"
	"path"
	"testing"
	"time"
	"videoarchive/internal/adapter/archivebundle"
	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/workerexecutor"

	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

// Requirement: DSH-046
func TestDestinationRunSummaryCountsOnlyProvenPackagePublications(t *testing.T) {
	now := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	decision := organization.OrganizationDecision{ID: "DECISION", Sequence: 1, WorkspaceDriveID: "DRIVE", Folder: "clip", InputRevision: 7, Disposition: vocabulary.OrganizationCompileSourceSequence}
	receipt := organization.ArchiveOrganizationReceipt{ID: "RUN", WorkspaceDriveID: "DRIVE", InputRevision: 7, WorkerID: "WORKER", SessionID: "SESSION", CompletedAt: now,
		Folders: []organization.CoordinatedFolder{{Folder: "clip", Decision: decision, JobID: "JOB"}}}
	for i := 1; i <= 5; i++ {
		receipt.Files = append(receipt.Files, organization.CoordinatedFile{RelativePath: fmt.Sprintf("clip/C%04d.mp4", i), OriginalAssetID: fmt.Sprintf("A%d", i)})
	}
	store := workstationStore{receipts: []organization.ArchiveOrganizationReceipt{receipt}, decisions: map[string]organization.OrganizationDecision{"clip": decision}}
	for _, tc := range []struct {
		name, state string
		published   bool
		count       int
	}{
		{"failed render", "NOT_PUBLISHED", false, 0}, {"published five-cut package", "PUBLISHED", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := now.Add(time.Minute)
			spec := archiveRunSpec("RUN", "clip")
			spec.Parameters.ArchiveStorageID = "ARCHIVE"
			spec.Parameters.InputRevision = 7
			step := job.Step{Workflow: workflow.Step{ID: "render", State: workflow.Failed}, ExecutionSpec: spec, LastEventAt: at}
			if tc.published {
				catalog, record := publishDestinationSequence(t, receipt, decision, at)
				spec = &catalog
				step = job.Step{Workflow: workflow.Step{ID: "archive_catalog", State: workflow.Completed}, ExecutionSpec: spec, LastEventAt: at, OutputRecord: &record}
			}
			jobs := workstationJobs{"JOB": jobSnapshot(t, job.State{ID: "JOB", Revision: 2, TemplateName: "archive-intake-sequence", TargetTitle: "clip", CurrentStepID: step.Workflow.ID, Steps: []job.Step{step}})}
			service, err := archiveintakeworkspace.New(store, jobs)
			if err != nil {
				t.Fatal(err)
			}
			views, err := service.Projection(context.Background())
			if err != nil || len(views) != 1 {
				t.Fatalf("projection: %v (%d)", err, len(views))
			}
			v := views[0]
			wantAt := time.Time{}
			if tc.published {
				wantAt = at
			}
			if v.FileCount != 5 || v.CatalogRowCount != tc.count || !v.CatalogUpdatedAt.Equal(wantAt) {
				t.Fatalf("%s: inputs=%d catalog rows=%d at=%v, want rows=%d at=%v", tc.state, v.FileCount, v.CatalogRowCount, v.CatalogUpdatedAt, tc.count, wantAt)
			}
		})
	}
}

func publishDestinationSequence(t *testing.T, receipt organization.ArchiveOrganizationReceipt, decision organization.OrganizationDecision, at time.Time) (execution.StepExecutionSpec, execution.StepOutputRecord) {
	t.Helper()
	tool := workerconfig.LocalTool{Executable: os.Args[0], Version: "fixture"}
	cfg := workerconfig.Configuration{ID: "WORKER", AccessibleStorageIDs: []string{"ARCHIVE"}, StorageRoots: []workerconfig.StorageRootConfiguration{{StorageID: "ARCHIVE", Root: t.TempDir()}}, Tools: workerconfig.MediaTools{FFprobe: tool, FFmpeg: tool}}
	roots, err := workerconfig.OpenStorageRoots(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	root := roots.MustRoot("ARCHIVE")
	if err := root.WriteFile("render.mp4", []byte("sequence"), 0600); err != nil {
		t.Fatal(err)
	}
	_, sum, err := root.FileSHA256("render.mp4")
	if err != nil {
		t.Fatal(err)
	}
	pub, err := archivebundle.NewPublisher(root, root)
	if err != nil {
		t.Fatal(err)
	}
	initial := archivebundle.InitialRequest{LayoutVersion: archivebundle.CompactLayoutVersion, PackageID: "VP-20261124-SEQ", WorkspaceDriveID: receipt.WorkspaceDriveID, DecisionID: decision.ID, MediaFolder: decision.Folder, Disposition: decision.Disposition, ProfileReference: "archive-1080p-20mbps@v1", CreatedAt: at, Video: archivebundle.FileSource{Root: archivebundle.RootSource, Path: "render.mp4", Checksum: sum}}
	for _, file := range receipt.Files {
		initial.Sources = append(initial.Sources, archivebundle.SourceReference{OriginalAssetID: file.OriginalAssetID, OriginalName: path.Base(file.RelativePath), RelativePath: file.RelativePath, Checksum: sum})
	}
	pkg, err := pub.PublishInitialCompact(initial)
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := root.FileSHA256(pkg.MetadataPath)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: "JOB", StepID: "archive_catalog", TemplateName: "archive-intake-source-sequence", Operation: execution.OperationWriteArchiveCatalog,
		Inputs:     []execution.InputReference{{ArtifactID: pkg.PackageID, Kind: execution.OutputOrganizedMedia, StorageID: "ARCHIVE", RelativePath: pkg.MetadataPath, Checksum: hash}},
		Outputs:    []execution.OutputTarget{{ID: "CAT", Kind: execution.OutputArchiveCatalog, StorageID: "ARCHIVE", RelativePath: "archive-catalog.csv"}},
		Parameters: execution.StepExecutionParameters{ArchiveRunID: receipt.ID, ArchiveStorageID: "ARCHIVE", WorkspaceDriveID: receipt.WorkspaceDriveID, OrganizationDecisionID: decision.ID, InputRevision: receipt.InputRevision, MediaFolder: decision.Folder, Disposition: decision.Disposition},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := workerexecutor.NewProduction(cfg, roots)
	if err != nil {
		t.Fatal(err)
	}
	result := reg.ExecuteDirect(context.Background(), spec, 1)
	if !result.Success {
		t.Fatalf("publication failed: %+v", result)
	}
	record, err := execution.NewStepOutputRecord(spec, 1, cfg.ID, at, result.Outputs, result.VerificationReferences)
	if err != nil || record.ValidateFor(spec) != nil || record.CatalogPublication == nil || len(record.CatalogPublication.Packages) != 1 {
		t.Fatalf("publication receipt: %+v %v", record, err)
	}
	return spec, record
}
