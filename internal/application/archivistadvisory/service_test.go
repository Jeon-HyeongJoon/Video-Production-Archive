package archivistadvisory

import (
	"context"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/application/archivistcontrol"
	"videoarchive/internal/application/workdiagnostics"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/vocabulary"
)

type fixedSessions struct {
	values []archivistcontrol.SessionProjection
}

func (fixture fixedSessions) Projection(context.Context) ([]archivistcontrol.SessionProjection, error) {
	return fixture.values, nil
}

type fixedDrives struct {
	values []archiveintakeworkspace.DriveView
}

func (fixture fixedDrives) Projection(context.Context) ([]archiveintakeworkspace.DriveView, error) {
	return fixture.values, nil
}

type fixedDiagnostics struct {
	values map[string]workdiagnostics.Snapshot
}

func (fixture fixedDiagnostics) Diagnose(_ context.Context, reference archivistruntime.WorkReference) (workdiagnostics.Snapshot, error) {
	return fixture.values[reference.WorkID], nil
}

type recordingTurns struct {
	seen   map[string]bool
	events []archivistcontrol.AdvisoryEvent
}

func (fixture *recordingTurns) HandleAdvisoryEvent(_ context.Context, event archivistcontrol.AdvisoryEvent) (archivistcontrol.TurnRecord, bool, error) {
	key := event.SessionID + ":" + string(event.Kind) + ":" + event.TriggerID
	if fixture.seen == nil {
		fixture.seen = make(map[string]bool)
	}
	if fixture.seen[key] {
		return archivistcontrol.TurnRecord{}, true, nil
	}
	fixture.seen[key] = true
	fixture.events = append(fixture.events, event)
	return archivistcontrol.TurnRecord{}, false, nil
}

// Requirement: LRH-023
func TestAdvisoryLoopQueuesOneRecoveryLimitedTurnForStaleReferencedWork(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	normal := archivistruntime.WorkReference{Kind: archivistruntime.WorkJob, WorkID: "JOB-NORMAL", Revision: 1}
	stale := archivistruntime.WorkReference{Kind: archivistruntime.WorkJob, WorkID: "JOB-STALE", Revision: 4}
	turns := &recordingTurns{}
	service, err := New(
		fixedSessions{values: []archivistcontrol.SessionProjection{
			{Session: archivistruntime.Session{ID: "SESSION-NORMAL"}, CreatedAt: now.Add(-time.Hour), Turns: []archivistcontrol.TurnProjection{{UpdatedAt: now.Add(-time.Minute), WorkReference: &normal}}},
			{Session: archivistruntime.Session{ID: "SESSION-STALE"}, CreatedAt: now.Add(-2 * time.Hour), Turns: []archivistcontrol.TurnProjection{{UpdatedAt: now.Add(-2 * time.Minute), WorkReference: &stale}}},
		}},
		fixedDrives{}, fixedDiagnostics{values: map[string]workdiagnostics.Snapshot{
			"JOB-NORMAL": {WorkID: "JOB-NORMAL", ShouldReport: false, Fingerprint: "normal"},
			"JOB-STALE":  {WorkID: "JOB-STALE", ShouldReport: true, Fingerprint: "stale-render-v1"},
		}}, turns,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Evaluate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Evaluate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(turns.events) != 1 || turns.events[0].SessionID != "SESSION-STALE" ||
		turns.events[0].Kind != archivistcontrol.AdvisoryWorkDiagnostic || turns.events[0].Work == nil ||
		turns.events[0].Work.WorkID != "JOB-STALE" || turns.events[0].TriggerID != "stale-render-v1" ||
		!strings.Contains(turns.events[0].Prompt, "archive.work.recovery.apply.v1") ||
		!strings.Contains(turns.events[0].Prompt, "read diagnostics again") {
		t.Fatalf("stale work advisory was not deduplicated and bound: %+v", turns.events)
	}
}

// Requirement: LRH-024
func TestAdvisoryLoopQueuesOneReadOnlyTurnForReadyNewDrive(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	turns := &recordingTurns{}
	service, err := New(
		fixedSessions{values: []archivistcontrol.SessionProjection{
			{Session: archivistruntime.Session{ID: "SESSION-OLD"}, CreatedAt: now.Add(-time.Hour)},
			{Session: archivistruntime.Session{ID: "SESSION-RECENT"}, CreatedAt: now.Add(-2 * time.Hour), Turns: []archivistcontrol.TurnProjection{{UpdatedAt: now.Add(-time.Minute)}}},
		}},
		fixedDrives{values: []archiveintakeworkspace.DriveView{
			{WorkspaceDriveID: "DISCONNECTED", ConnectionState: vocabulary.VolumeDisconnected, FolderDiscoveryState: vocabulary.FolderDiscoveryReady, CandidateRevision: 1, FolderDiscoveryRevision: 1, DiscoveredFolderCount: 1, State: "UNCLASSIFIED"},
			{WorkspaceDriveID: "SCANNING", ConnectionState: vocabulary.VolumeConnected, FolderDiscoveryState: vocabulary.FolderDiscoveryScanning, CandidateRevision: 2, DiscoveredFolderCount: 1, State: "UNCLASSIFIED"},
			{WorkspaceDriveID: "NO-ARCHIVE", ConnectionState: vocabulary.VolumeConnected, FolderDiscoveryState: vocabulary.FolderDiscoveryReady, CandidateRevision: 3, FolderDiscoveryRevision: 4, DiscoveredFolderCount: 1, State: "UNCLASSIFIED"},
			{WorkspaceDriveID: "READY-DRIVE", ConnectionState: vocabulary.VolumeConnected, FolderDiscoveryState: vocabulary.FolderDiscoveryReady, CandidateRevision: 5, FolderDiscoveryRevision: 6, DiscoveredFolderCount: 2, State: "UNCLASSIFIED", SourceStorageID: "SOURCE-1", StorageCandidates: []archiveintakeworkspace.StorageView{
				{StorageID: "SOURCE-1", Role: vocabulary.StorageSourceMedia, Accessible: true, FreeBytes: 100},
				{StorageID: "ARCHIVE-1", Role: vocabulary.StorageArchivePrimary, Accessible: true, FreeBytes: 1_000},
			}},
		}}, fixedDiagnostics{values: map[string]workdiagnostics.Snapshot{}}, turns,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Evaluate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Evaluate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(turns.events) != 1 || turns.events[0].SessionID != "SESSION-RECENT" ||
		turns.events[0].Kind != archivistcontrol.AdvisoryNewDrive || turns.events[0].Work != nil ||
		turns.events[0].TriggerID != "READY-DRIVE:5:6" {
		t.Fatalf("new drive advisory selected the wrong source or session: %+v", turns.events)
	}
}
