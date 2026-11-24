//go:build destination_catalog_contract

package httpmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"videoarchive/internal/application/systemstatus"
)

type destinationStatusSnapshot struct{ snapshot systemstatus.Snapshot }

func (s destinationStatusSnapshot) Snapshot(context.Context) (systemstatus.Snapshot, error) {
	return s.snapshot, nil
}

// Requirement: MCP-014
func TestDestinationStatusReturnsScopedVerificationEvidenceWithoutRun(t *testing.T) {
	const projection = `
{
  "destination_catalogs": [
    {
      "archive_storage_id": "PRIMARY",
      "state": "MISMATCH",
      "report_id": "VERIFY-7",
      "observed_at": "2026-11-24T00:00:00Z",
      "catalog_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "current_packages": 15,
      "mismatched_packages": 1,
      "unresolved_videos": 0,
      "next_check": "REVIEW_CATALOG_DIFFERENCES"
    },
    {
      "archive_storage_id": "SECOND",
      "state": "NOT_OBSERVED",
      "next_check": "VERIFY_DESTINATION"
    }
  ]
}
`
	var snapshot systemstatus.Snapshot
	if err := json.Unmarshal([]byte(projection), &snapshot); err != nil {
		t.Fatal(err)
	}
	handler, err := New(Configuration{Archivists: fixtureArchivistRuntime{selectionErr: true}, Catalog: fixtureCatalog{}, ArchiveWorkspace: fixtureArchiveWorkspace{}, Commands: fixtureCommands{}, SystemStatus: destinationStatusSnapshot{snapshot}})
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
}
