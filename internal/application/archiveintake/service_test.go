package archiveintake

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"videoarchive/internal/adapter/sqlitearchiveintake"
	coreintake "videoarchive/internal/core/archiveintake"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: DEV-008
func TestAuthorizedSelectionOpensOnlyCurrentCandidateAsReadOnlySessionPinnedToLogicalVolume(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	selectedRoot := filepath.Join(directory, "selected")
	otherRoot := filepath.Join(directory, "other")
	workingRoot := filepath.Join(directory, "working")
	for _, root := range []string{selectedRoot, otherRoot, workingRoot} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(selectedRoot, "selected.mp4"), []byte("selected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherRoot, "other.mp4"), []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}

	repository, err := sqlitearchiveintake.Open(filepath.Join(directory, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	now := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	for _, candidate := range []coreintake.Candidate{
		{VolumeID: "VOLUME-1", Revision: 4, PhysicalDeviceID: "PHYSICAL-1", WorkerID: "WORKER-1", StorageID: "SOURCE-1", Root: selectedRoot, Connected: true, ObservedAt: now},
		{VolumeID: "VOLUME-2", Revision: 2, PhysicalDeviceID: "PHYSICAL-2", WorkerID: "WORKER-1", StorageID: "SOURCE-2", Root: otherRoot, Connected: true, ObservedAt: now},
		{VolumeID: "VOLUME-REMOTE", Revision: 1, PhysicalDeviceID: "PHYSICAL-REMOTE", WorkerID: "WORKER-1", StorageID: "SOURCE-REMOTE", Connected: true, ObservedAt: now},
	} {
		if err := repository.SaveCandidate(ctx, candidate); err != nil {
			t.Fatal(err)
		}
	}
	if sessions, err := repository.ListSessions(ctx); err != nil || len(sessions) != 0 {
		t.Fatalf("candidate observation created a session: sessions=%+v err=%v", sessions, err)
	}
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	access, err := service.Select(ctx, SelectRequest{
		SessionID: "INTAKE-1", VolumeID: "VOLUME-1", ExpectedCandidateRevision: 4,
		Actor: command.Actor{Type: vocabulary.ActorHuman, ID: "operator-1"}, SelectedAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer access.Close()
	payload, err := access.Root.ReadFile("selected.mp4")
	if err != nil || string(payload) != "selected" {
		t.Fatalf("selected root was not readable: payload=%q err=%v", payload, err)
	}
	if _, err := access.Root.ReadFile(filepath.Join("..", "other", "other.mp4")); err == nil {
		t.Fatal("unselected volume was reachable through the session root")
	}
	if access.Session.WorkspaceDriveID != "VOLUME-1" || access.Session.VolumeID != "VOLUME-1" || access.Session.PhysicalDeviceID != "PHYSICAL-1" ||
		access.Session.WorkerID != "WORKER-1" || len(access.ReservationPhysicalDeviceIDs()) != 1 ||
		access.ReservationPhysicalDeviceIDs()[0] != "PHYSICAL-1" {
		t.Fatalf("session was not pinned to its logical volume and auxiliary physical guard: %+v", access.Session)
	}
	if sessions, err := repository.ListSessions(ctx); err != nil || len(sessions) != 1 || sessions[0].ID != "INTAKE-1" {
		t.Fatalf("selected session was not stored once: sessions=%+v err=%v", sessions, err)
	}
	if _, err := service.Select(ctx, SelectRequest{
		SessionID: "INTAKE-STALE", VolumeID: "VOLUME-1", ExpectedCandidateRevision: 3,
		Actor: command.Actor{Type: vocabulary.ActorHuman, ID: "operator-1"}, SelectedAt: now.Add(2 * time.Minute),
	}); !errors.Is(err, coreintake.ErrStaleCandidate) {
		t.Fatalf("stale selection was accepted: %v", err)
	}
	archivistAccess, err := service.Select(ctx, SelectRequest{
		SessionID: "INTAKE-ARCHIVIST", VolumeID: "VOLUME-2", ExpectedCandidateRevision: 2,
		Actor: command.Actor{Type: vocabulary.ActorArchivist, ID: "archivist-1"}, SelectedAt: now.Add(3 * time.Minute),
	})
	if err != nil || archivistAccess.Session.SelectedByType != vocabulary.ActorArchivist || archivistAccess.Session.SelectedBy != "archivist-1" {
		t.Fatalf("authenticated Archivist selection provenance was not stored: access=%+v err=%v", archivistAccess, err)
	}
	defer archivistAccess.Close()
	remote, err := service.Select(ctx, SelectRequest{
		SessionID: "INTAKE-REMOTE", VolumeID: "VOLUME-REMOTE", ExpectedCandidateRevision: 1,
		Actor: command.Actor{Type: vocabulary.ActorHuman, ID: "operator-1"}, SelectedAt: now.Add(4 * time.Minute),
	})
	if err != nil || remote.Root != nil || remote.Session.StorageID != "SOURCE-REMOTE" {
		t.Fatalf("remote Worker selection did not remain an ID-only session: access=%+v err=%v", remote, err)
	}
	inventory, err := NewInventoryService(repository)
	if err != nil {
		t.Fatal(err)
	}
	selectedStorageIDs, err := inventory.SelectedStorageIDs(ctx, "WORKER-1")
	if err != nil || len(selectedStorageIDs) != 3 || selectedStorageIDs[0] != "SOURCE-1" || selectedStorageIDs[1] != "SOURCE-2" || selectedStorageIDs[2] != "SOURCE-REMOTE" {
		t.Fatalf("Worker did not receive only the current connected selections: ids=%v err=%v", selectedStorageIDs, err)
	}
}
