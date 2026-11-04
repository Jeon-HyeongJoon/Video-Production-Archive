package archiverun

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"videoarchive/internal/core/command"
)

// Requirement: ORG-012
func TestArchiveRunPersistsSelectionsFolderScopeAndStageOwnershipSeparatelyFromRunReceipt(t *testing.T) {
	createdAt := time.Date(2026, 10, 31, 14, 30, 0, 0, time.FixedZone("KST", 9*60*60))
	handler, err := NewHandler(func() time.Time { return createdAt })
	if err != nil {
		t.Fatal(err)
	}
	store := command.NewMemoryStore(nil)
	executor := command.NewAuthorizedCreatingExecutor(store, func(command.Envelope) error { return nil }, handler.Handle)
	envelope := command.Envelope{
		CommandID:        "COMMAND-CREATE-ARCHIVE-RUN-1",
		CommandType:      CreateCommand,
		Actor:            command.Actor{Type: command.ArchivistActor, ID: "ARCHIVIST-1"},
		RequestedVia:     "mcp",
		TargetID:         "ARCHIVE-RUN-1",
		ExpectedRevision: 0,
		IdempotencyKey:   "ARCHIVE-RUN-1:create:v1",
		Parameters: map[string]any{
			"worker_id":                 "WORKER-WINDOWS-1",
			"source_storage_id":         "SOURCE-STORAGE-1",
			"source_volume_id":          "VOLUME-WINDOWS-1",
			"source_physical_device_id": "PHYSICAL-WINDOWS-1",
			"archive_storage_id":        "ARCHIVE-STORAGE-1",
			"folder_discovery_revision": 17,
			"folder_ids":                []string{"FOLDER-C", "FOLDER-A", "FOLDER-B"},
			"selected_by": map[string]any{
				"type": command.HumanActor,
				"id":   "OPERATOR-1",
			},
		},
		Reason: "start the selected archive run",
	}

	first, err := executor.Execute(envelope)
	if err != nil {
		t.Fatal(err)
	}
	replay := envelope
	replay.CommandID = "COMMAND-CREATE-ARCHIVE-RUN-REPLAY"
	replay.RequestedVia = "archive_workspace"
	replay.Reason = "retry the same selected archive run"
	second, err := executor.Execute(replay)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("idempotent replay returned a different receipt: first=%+v second=%+v", first, second)
	}

	snapshot, records, events, found := store.Snapshot("ARCHIVE-RUN-1")
	if !found {
		t.Fatal("ArchiveRun aggregate was not persisted")
	}
	if bytes.Contains(snapshot.State, []byte(`"run`)) {
		t.Fatalf("command run data leaked into ArchiveRun identity or state: %s", snapshot.State)
	}
	state, err := Decode(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	wantFolders := []string{"FOLDER-A", "FOLDER-B", "FOLDER-C"}
	wantStages := []StageOwnership{{Kind: OrganizationStage}, {Kind: TranscriptionStage}}
	if state.ID != "ARCHIVE-RUN-1" || state.Revision != 1 || state.WorkerID != "WORKER-WINDOWS-1" ||
		state.SourceStorageID != "SOURCE-STORAGE-1" || state.ArchiveStorageID != "ARCHIVE-STORAGE-1" ||
		state.SourceVolumeID != "VOLUME-WINDOWS-1" || state.SourcePhysicalDeviceID != "PHYSICAL-WINDOWS-1" ||
		state.FolderDiscoveryRevision != 17 || !reflect.DeepEqual(state.FolderIDs, wantFolders) ||
		state.SelectedBy != (command.Actor{Type: command.HumanActor, ID: "OPERATOR-1"}) ||
		!reflect.DeepEqual(state.StageOwnerships, wantStages) || !state.CreatedAt.Equal(createdAt.UTC()) {
		t.Fatalf("persisted ArchiveRun lost fixed ownership data: %+v", state)
	}
	if len(records) != 2 || !records[0].Accepted || !records[1].Accepted || len(events) != 1 ||
		events[0].EventType != "ArchiveRunCreated" || events[0].TargetID != state.ID || events[0].AfterRevision != 1 {
		t.Fatalf("state, records and event were not stored through the command unit of work: records=%+v events=%+v", records, events)
	}
	receipt, receiptFound, err := store.FindReceipt(envelope.IdempotencyKey)
	if err != nil || !receiptFound || receipt.Result.TargetID != state.ID || receipt.Result.Revision != state.Revision {
		t.Fatalf("creation receipt was not persisted separately from aggregate state: receipt=%+v found=%v err=%v", receipt, receiptFound, err)
	}
}
