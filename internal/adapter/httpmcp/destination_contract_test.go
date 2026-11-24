package httpmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"videoarchive/internal/adapter/sqlitecommand"
	"videoarchive/internal/adapter/sqlitestorage"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/storage"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"

	"videoarchive/internal/application/systemstatus"
)

// Requirement: MCP-014
func TestDestinationStatusReturnsScopedVerificationEvidenceWithoutRun(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
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
	for _, id := range []string{"PRIMARY", "SECOND"} {
		if err := storages.Register(ctx, storage.Topology{StorageID: id, VolumeID: "volume-" + id, PhysicalDeviceID: "disk-" + id, PartitionID: "partition-" + id, Role: vocabulary.StorageArchivePrimary, Root: t.TempDir()}); err != nil {
			t.Fatal(err)
		}
	}
	spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{JobID: "J", StepID: "verify", TemplateName: "archive-catalog-maintenance", Operation: execution.OperationVerifyArchiveCatalog, Outputs: []execution.OutputTarget{{ID: "VERIFY-7", Kind: execution.OutputArchiveCatalogVerification, StorageID: "REPORTS", RelativePath: "J.json"}}, Parameters: execution.StepExecutionParameters{ArchiveStorageID: "PRIMARY", WorkspaceDriveID: "volume-PRIMARY", InputRevision: 1}})
	if err != nil {
		t.Fatal(err)
	}
	sum := strings.Repeat("b", 64)
	output := execution.StepOutput{ID: "VERIFY-7", Kind: execution.OutputArchiveCatalogVerification, StorageID: "REPORTS", RelativePath: "J.json", Checksum: sum, CatalogVerification: &execution.CatalogVerification{
		ArchiveStorageID: "PRIMARY", State: "MISMATCH", ReportID: "VERIFY-7", ReportSHA256: sum, ObservedAt: now, CatalogSHA256: strings.Repeat("a", 64), CurrentPackages: 15, MismatchedPackages: 1, NextCheck: "REVIEW_CATALOG_DIFFERENCES"}}
	record, err := execution.NewStepOutputRecord(spec, 1, "worker", now, []execution.StepOutput{output}, []string{"verification"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(job.State{ID: "J", Revision: 1, Steps: []job.Step{{Workflow: workflow.Step{ID: "verify", State: workflow.Completed}, ExecutionSpec: &spec, OutputRecord: &record, LastEventAt: now}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := records.Seed(ctx, command.Snapshot{TargetID: "J", Revision: 1, State: payload}); err != nil {
		t.Fatal(err)
	}
	workers := systemStatusWorkers{{WorkerID: "worker", State: "ONLINE", Metrics: worker.Metrics{AccessibleStorageIDs: map[string]bool{"PRIMARY": true}}}}
	status, err := systemstatus.New(systemStatusWorks{}, workers, systemStatusHosts{}, systemStatusDiagnostics{}, records, func() time.Time { return now })
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
		a["current_packages"] != 15.0 || a["mismatched_packages"] != 1.0 ||
		a["catalog_sha256"] != strings.Repeat("a", 64) || a["observed_at"] != "2026-11-24T00:00:00Z" ||
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
