package controlruntime_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"videoarchive/internal/adapter/bootconfig"
	"videoarchive/internal/adapter/controlruntime"
	"videoarchive/internal/adapter/sqlitearchivist"
	"videoarchive/internal/adapter/storageinventory"
	"videoarchive/internal/application/archivistcontrol"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/vocabulary"
)

// The restart scenario starts at a prepared advisory Turn, immediately before
// proposing the ArchiveRun. MCP-010 separately exercises discovering and pinning.
func advisoryTestConfiguration(t *testing.T) (bootconfig.Configuration, uint64) {
	t.Helper()
	dir := t.TempDir()
	source, destination := filepath.Join(dir, "source"), filepath.Join(dir, "destination")
	for _, path := range []string{filepath.Join(source, "370105-event/PGM"), destination} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "370105-event/PGM", "master.mov"), []byte("synthetic recording"), 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := registeredMediaConfiguration(t, dir, source, destination, exe)
	host := runtimeTestConfiguration(t)
	cfg.ArchivistHosts, cfg.RuntimeHeartbeatTTL, cfg.ArchivistTurnLeaseTTL = host.ArchivistHosts, host.RuntimeHeartbeatTTL, host.ArchivistTurnLeaseTTL
	root, err := archive.OpenReadOnlyStorageRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	inventory, err := storageinventory.ScanSource(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, inventory.DiscoveryRevision
}

func queueScopedAdvisory(t *testing.T, rt *controlruntime.Runtime, db, sessionID, turnID, secret string, discovery uint64) {
	t.Helper()
	beat := fmt.Sprintf(`{"worker_id":"worker-main","snapshot_id":1,"observed_at":%q,"idle_stable_millis":60000,"available_memory_bytes":34359738368,"available_vram_bytes":8589934592,"scratch_free_bytes":536870912000,"accepting_work":true,"accessible_storage_ids":["source-main","archive-main"]}`, time.Now().UTC().Format(time.RFC3339Nano))
	if r := workerHeartbeatRequest(rt.Handler(), beat, secret); r.Code != http.StatusOK {
		t.Fatal(r.Body)
	}
	turn, err := archivistruntime.NewAdvisorySystemTurn(turnID, sessionID, "REQUEST-"+turnID, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := archivistruntime.NewDriveSelection(archivistruntime.DriveSelection{WorkerID: "worker-main", WorkspaceDriveID: "source-volume", IntakeSessionID: "INTAKE-" + turnID, SourceVolumeID: "source-volume", SourceStorageID: "source-main", ArchiveStorageID: "archive-main", CandidateRevision: 1, FolderDiscoveryRevision: discovery, FolderIDs: []string{"370105-event/PGM"}, SelectedByType: vocabulary.ActorArchivist, SelectedBy: "archivist:" + sessionID})
	if err != nil {
		t.Fatal(err)
	}
	turn.DriveSelection = &selection
	store, err := sqlitearchivist.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.CreateTurn(context.Background(), archivistcontrol.TurnRecord{Turn: turn, Prompt: "선택한 폴더의 보관을 제안하고 담당자의 결정을 확인해 주세요.", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
}
