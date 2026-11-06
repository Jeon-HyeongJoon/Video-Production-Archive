package sqlitearchiveintake

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"videoarchive/internal/core/archiveintake"
)

func TestListSessionsNormalizesLegacyRowsWithoutHoldingTheOnlySQLiteCursor(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(t.TempDir() + "/intake.db")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	now := time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
	candidate := archiveintake.Candidate{
		VolumeID: "VOLUME-1", Revision: 3, PhysicalDeviceID: "DISK-1", WorkerID: "WORKER-1",
		StorageID: "SOURCE-1", Connected: true, ObservedAt: now,
	}
	if err := repository.SaveCandidate(ctx, candidate); err != nil {
		t.Fatal(err)
	}
	legacy := archiveintake.Session{
		ID: "SESSION-LEGACY", Revision: 1, CandidateRevision: 3, VolumeID: "VOLUME-1",
		WorkspaceDriveID: "DISK-1", WorkerID: "WORKER-1", StorageID: "SOURCE-1",
		SelectedBy: "operator-1", SelectedAt: now,
	}
	payload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.db.ExecContext(ctx, `
		INSERT INTO archive_intake_sessions(session_id, volume_id, payload) VALUES (?, ?, ?)
	`, legacy.ID, legacy.VolumeID, payload); err != nil {
		t.Fatal(err)
	}
	result := make(chan struct {
		sessions []archiveintake.Session
		err      error
	}, 1)
	go func() {
		sessions, listErr := repository.ListSessions(ctx)
		result <- struct {
			sessions []archiveintake.Session
			err      error
		}{sessions: sessions, err: listErr}
	}()
	select {
	case listed := <-result:
		if listed.err != nil || len(listed.sessions) != 1 || listed.sessions[0].WorkspaceDriveID != "VOLUME-1" ||
			listed.sessions[0].PhysicalDeviceID != "DISK-1" {
			t.Fatalf("legacy session normalization=%+v err=%v", listed.sessions, listed.err)
		}
	case <-time.After(time.Second):
		t.Fatal("ListSessions deadlocked while normalizing a legacy row")
	}
}
