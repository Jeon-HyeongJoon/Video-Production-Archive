package httpmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"videoarchive/internal/adapter/archivebundle"
	"videoarchive/internal/adapter/archivecatalog"
	"videoarchive/internal/adapter/sqlitecommand"
	"videoarchive/internal/adapter/sqlitestorage"
	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/systemstatus"
	"videoarchive/internal/application/workerexecutor"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/storage"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

// Requirement: MCP-014
func TestDestinationStatusReturnsScopedVerificationEvidenceWithoutRun(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	db := filepath.Join(t.TempDir(), "control.db")
	records, err := sqlitecommand.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer records.Close()
	storages, err := sqlitestorage.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer storages.Close()
	rootPaths := map[string]string{"PRIMARY": t.TempDir(), "SECOND": t.TempDir()}
	for id, rootPath := range rootPaths {
		if err := storages.Register(ctx, storage.Topology{StorageID: id, VolumeID: "volume-" + id, PhysicalDeviceID: "disk-" + id, PartitionID: "partition-" + id, Role: vocabulary.StorageArchivePrimary, Root: rootPath}); err != nil {
			t.Fatal(err)
		}
	}
	spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{JobID: "J", StepID: "verify", TemplateName: "archive-catalog-maintenance", Operation: execution.OperationVerifyArchiveCatalog, Outputs: []execution.OutputTarget{{ID: "VERIFY-7", Kind: execution.OutputArchiveCatalogVerification, StorageID: "REPORTS", RelativePath: "J.json"}}, Parameters: execution.StepExecutionParameters{ArchiveStorageID: "PRIMARY", WorkspaceDriveID: "volume-PRIMARY", InputRevision: 1}})
	if err != nil {
		t.Fatal(err)
	}
	// Publish one indexed and one still-unindexed package. The Worker must report
	// both from disk before the Archivist can explain this destination.
	tool := workerconfig.LocalTool{Executable: os.Args[0], Version: "unused-by-catalog"}
	cfg := workerconfig.Configuration{ID: "worker", AccessibleStorageIDs: []string{"PRIMARY", "REPORTS"}, Tools: workerconfig.MediaTools{FFmpeg: tool, FFprobe: tool},
		StorageRoots: []workerconfig.StorageRootConfiguration{{StorageID: "PRIMARY", Root: rootPaths["PRIMARY"]}, {StorageID: "REPORTS", Root: t.TempDir()}}}
	roots, err := workerconfig.OpenStorageRoots(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	primary := roots.MustRoot("PRIMARY")
	source, err := archive.OpenStorageRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	publisher, err := archivebundle.NewPublisher(source, primary)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"indexed", "pending"} {
		name := id + ".mp4"
		if err := source.WriteFile(name, []byte(id), 0o600); err != nil {
			t.Fatal(err)
		}
		_, hash, err := source.FileSHA256(name)
		if err != nil {
			t.Fatal(err)
		}
		_, err = publisher.PublishInitialCompact(archivebundle.InitialRequest{
			LayoutVersion: archive.PackageLayoutCompactResultFolder, PackageID: "VP-20261124-" + id, DecisionID: id, WorkspaceDriveID: "source-volume",
			MediaFolder: id, Disposition: vocabulary.OrganizationAsIs, ProfileReference: "archive-1080p-6mbps@v1", CreatedAt: now,
			Sources: []archivebundle.SourceReference{{OriginalAssetID: id, OriginalName: name, RelativePath: name, Checksum: hash}},
			Video:   archivebundle.FileSource{Root: archivebundle.RootSource, Path: name, Checksum: hash},
		})
		if err != nil {
			t.Fatal(err)
		}
		if id == "indexed" {
			reconciler, err := archivecatalog.NewReconciler(primary)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reconciler.Rebuild(); err != nil {
				t.Fatal(err)
			}
		}
	}
	registry, err := workerexecutor.NewProduction(cfg, roots)
	if err != nil {
		t.Fatal(err)
	}
	result := registry.ExecuteOperation(ctx, spec, 1)
	if !result.Success {
		t.Fatalf("destination verification failed: %s %s", result.CauseCode, result.ErrorSummary)
	}
	reports := roots.MustRoot("REPORTS")
	raw, err := reports.ReadFile("J.json")
	if err != nil {
		t.Fatal(err)
	}
	var report archivecatalog.Verification
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Current() || report.Coverage.CandidateVideos != 2 || len(report.Entries) != 2 || report.ObservedAt.IsZero() {
		t.Fatalf("unexpected verification report: %+v", report)
	}
	_, sum, err := reports.FileSHA256("J.json")
	if err != nil {
		t.Fatal(err)
	}
	_, catalogHash, err := primary.FileSHA256("archive-catalog.csv")
	if err != nil {
		t.Fatal(err)
	}
	completedAt := time.Now().UTC()
	record, err := execution.NewStepOutputRecord(spec, 1, "worker", completedAt, result.Outputs, result.VerificationReferences)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(job.State{ID: "J", Revision: 1, Steps: []job.Step{{Workflow: workflow.Step{ID: "verify", State: workflow.Completed}, ExecutionSpec: &spec, OutputRecord: &record, LastEventAt: completedAt}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := records.Seed(ctx, command.Snapshot{TargetID: "J", Revision: 1, State: payload}); err != nil {
		t.Fatal(err)
	}
	workers := systemStatusWorkers{{WorkerID: "worker", State: "ONLINE", Metrics: worker.Metrics{AccessibleStorageIDs: map[string]bool{"PRIMARY": true}}}}
	status, err := systemstatus.New(systemStatusWorks{}, workers, systemStatusHosts{}, systemStatusDiagnostics{}, records, func() time.Time { return completedAt })
	if err != nil {
		t.Fatal(err)
	}
	status.SetDestinationReaders(storages, records)
	handler, err := New(Configuration{Archivists: fixtureArchivistRuntime{selectionErr: true}, Catalog: fixtureCatalog{}, ArchiveWorkspace: fixtureArchiveWorkspace{}, Commands: fixtureCommands{}, SystemStatus: status})
	if err != nil {
		t.Fatal(err)
	}
	resp := mcpTestRequest(handler, http.MethodPost, "/mcp/v1/call", archiveToolCall("DESTINATION-STATUS", ToolSystemStatusGet, `{}`), "secret")
	var reply struct {
		Data struct {
			Catalogs []map[string]any `json:"destination_catalogs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &reply); err != nil || resp.Code != http.StatusOK {
		t.Fatalf("system status %d %v", resp.Code, err)
	}
	if len(reply.Data.Catalogs) != 2 {
		t.Fatalf("expected two destination observations, got %d", len(reply.Data.Catalogs))
	}
	got := map[string]map[string]any{}
	for _, c := range reply.Data.Catalogs {
		id, _ := c["archive_storage_id"].(string)
		got[id] = c
	}
	a, b := got["PRIMARY"], got["SECOND"]
	if a["state"] != "MISMATCH" || a["report_id"] != "VERIFY-7" ||
		a["report_sha256"] != sum || a["current_packages"] != 1.0 || a["mismatched_packages"] != 1.0 ||
		a["catalog_sha256"] != catalogHash || a["observed_at"] != report.ObservedAt.Format(time.RFC3339Nano) ||
		a["next_check"] != "REVIEW_CATALOG_DIFFERENCES" {
		t.Fatalf("missing primary verification evidence: %+v", a)
	}
	if b["state"] != "NOT_OBSERVED" || b["report_id"] != nil || b["next_check"] != "VERIFY_DESTINATION" {
		t.Fatalf("unobserved destination inherited verification evidence: %+v", b)
	}
	workers[0].State = "OFFLINE"
	stale, err := status.Snapshot(ctx)
	if err != nil || len(stale.DestinationCatalogs) != 2 || stale.DestinationCatalogs[0].State != "STALE" || stale.DestinationCatalogs[0].ReportSHA256 != sum {
		t.Fatalf("offline report: %+v %v", stale.DestinationCatalogs, err)
	}
}
