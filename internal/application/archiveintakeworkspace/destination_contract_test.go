//go:build destination_catalog_contract

package archiveintakeworkspace_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

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
				var record execution.StepOutputRecord
				if err := json.Unmarshal([]byte(`
{
  "outputs": [
    {
      "id": "CAT",
      "kind": "ARCHIVE_CATALOG",
      "storage_id": "ARCHIVE",
      "relative_path": "archive-catalog.csv",
      "checksum": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    }
  ],
  "catalog_publication": {
    "archive_storage_id": "ARCHIVE",
    "archive_run_id": "RUN",
    "packages": [
      {
        "package_id": "VP-20261124-SEQ",
        "revision": 1,
        "manifest_digest": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
      }
    ],
    "catalog_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  }
}
`), &record); err != nil {
					t.Fatal(err)
				}
				record.RecordedAt = at
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
