package sqlitearchivist

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"videoarchive/internal/application/archivistcontrol"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: LRH-012
func TestArchiveRunRecallQueuesOneSystemTurnPerTriggerWhileHostIsOffline(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	store, err := Open(filepath.Join(t.TempDir(), "archivist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hosts := archivistruntime.NewHostRegistry([]archivistruntime.HostDefinition{{
		ID: "HOST-1", Enabled: true, CredentialRef: "secret://host",
		AllowedBackends: map[vocabulary.RuntimeBackend]bool{vocabulary.RuntimeEmbeddedCore: true},
		AllowedCLIIDs:   map[string]bool{"codex": true}, AllowedWorkspaceRootIDs: map[string]bool{"video-project": true},
	}}, recallVerifier{}, time.Minute)
	service, err := archivistcontrol.New(store, hosts, func() time.Time { return now }, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateSession(ctx, archivistruntime.SessionSpec{
		ID: "SESSION-1", ArchivistHostID: "HOST-1", Backend: vocabulary.RuntimeEmbeddedCore,
		CLIID: "codex", WorkspaceRootID: "video-project",
	}, ""); err != nil {
		t.Fatal(err)
	}
	selection, err := archivistruntime.NewDriveSelection(archivistruntime.DriveSelection{
		WorkerID: "WORKER-1", WorkspaceDriveID: "VOLUME-1",
		IntakeSessionID: "INTAKE-1", SourceVolumeID: "VOLUME-1", SourceStorageID: "SOURCE-1",
		ArchiveStorageID: "ARCHIVE-1", CandidateRevision: 4, FolderDiscoveryRevision: 7,
		FolderIDs: []string{"recordings"}, SelectedBy: "operator-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := archivistruntime.NewTurnWithDriveSelection("TURN-START-1", "SESSION-1", "REQUEST-START-1", &selection)
	if err != nil {
		t.Fatal(err)
	}
	turn.State = vocabulary.ArchivistTurnCompleted
	if err := store.CreateTurn(ctx, archivistcontrol.TurnRecord{Turn: turn, Prompt: "start archive run", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := service.BindArchiveRun(ctx, "SESSION-1", turn.ID, "ARCHIVE-RUN-1", 1); err != nil {
		t.Fatal(err)
	}
	for _, projection := range []struct {
		state string
		holds int
	}{
		{state: "STAGE_ONE_RUNNING", holds: 2},
		{state: "TRANSCRIPTION_READY", holds: 2},
		{state: "COMPLETED", holds: 2},
	} {
		for attempt := 0; attempt < 2; attempt++ {
			if err := service.RecallArchiveRunProjection(ctx, "ARCHIVE-RUN-1", 1, projection.state, projection.holds); err != nil {
				t.Fatalf("projection recall failed: state=%s attempt=%d err=%v", projection.state, attempt, err)
			}
		}
	}
	turns, err := store.ListTurns(ctx, "SESSION-1")
	if err != nil || len(turns) != 4 {
		t.Fatalf("expected one Turn per distinct trigger: turns=%+v err=%v", turns, err)
	}
	systemTurns := 0
	triggers := map[archivistruntime.ArchiveRunWorkTrigger]bool{}
	for _, stored := range turns {
		if stored.Turn.WorkContext == nil || stored.Turn.WorkContext.WorkID != "ARCHIVE-RUN-1" ||
			stored.Turn.WorkContext.OriginTurnID != turn.ID {
			t.Fatalf("Turn was not joined by structured ArchiveRun work context: %+v", stored.Turn)
		}
		if stored.Turn.WorkReference == nil || stored.Turn.WorkReference.Kind != archivistruntime.WorkArchiveRun ||
			stored.Turn.WorkReference.WorkID != "ARCHIVE-RUN-1" || stored.Turn.WorkReference.Revision != 1 {
			t.Fatalf("Turn did not retain the persistent ArchiveRun handoff reference: %+v", stored.Turn)
		}
		triggers[stored.Turn.WorkContext.Trigger] = true
		if stored.Turn.SubmittedBy == vocabulary.ActorSystem {
			systemTurns++
		}
	}
	if systemTurns != 3 {
		t.Fatalf("projection triggers were not deduplicated: turns=%+v", turns)
	}
	for _, trigger := range []archivistruntime.ArchiveRunWorkTrigger{
		archivistruntime.WorkArchiveRunStarted, archivistruntime.WorkNewHold,
		archivistruntime.WorkStageOneComplete, archivistruntime.WorkArchiveRunComplete,
	} {
		if !triggers[trigger] {
			t.Fatalf("structured recall trigger %s is missing: %+v", trigger, turns)
		}
	}
	if _, err := service.Claim(ctx, "HOST-1"); !errors.Is(err, archivistruntime.ErrArchivistHostOffline) {
		t.Fatalf("offline Host unexpectedly claimed a recall: %v", err)
	}
	if err := service.Heartbeat("HOST-1", "host-secret"); err != nil {
		t.Fatal(err)
	}
	claimed, err := service.Claim(ctx, "HOST-1")
	if err != nil || claimed.Turn.SubmittedBy != vocabulary.ActorSystem || claimed.Turn.DriveSelection == nil ||
		claimed.Turn.DriveSelection.ArchiveRunID != "ARCHIVE-RUN-1" {
		t.Fatalf("reconnected Host did not receive the same queued recall: %+v err=%v", claimed, err)
	}
}

type recallVerifier struct{}

func (recallVerifier) Verify(archivistHostID, credentialRef, presentedCredential string) bool {
	return archivistHostID == "HOST-1" && credentialRef == "secret://host" && presentedCredential == "host-secret"
}
