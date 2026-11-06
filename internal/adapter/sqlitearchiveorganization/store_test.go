package sqlitearchiveorganization

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
)

func TestLatestDecisionForArchiveRunDoesNotReadAnotherRunsDecision(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "organization.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)
	first := organization.OrganizationDecision{
		ID: "DECISION-RUN-1-1", Sequence: 1, WorkspaceDriveID: "DRIVE-1", Folder: "recordings",
		InputRevision: 1, Disposition: vocabulary.OrganizationHold, DecidedAt: now,
	}
	second := first
	second.ID = "DECISION-RUN-2-1"
	second.DecidedAt = now.Add(time.Minute)
	for _, receipt := range []organization.ArchiveOrganizationReceipt{
		receiptForDecision("RUN-1", "KEY-1", first, now),
		receiptForDecision("RUN-2", "KEY-2", second, now.Add(time.Minute)),
	} {
		if err := store.SaveReceipt(ctx, receipt); err != nil {
			t.Fatal(err)
		}
	}

	override := first
	override.ID = "DECISION-RUN-1-2"
	override.Sequence = 2
	override.Disposition = vocabulary.OrganizationRenderRecordings
	override.DecidedAt = now.Add(2 * time.Minute)
	if err := store.AppendOverride(ctx, "OVERRIDE-1", "DIGEST-OVERRIDE-1", first.ID, override); err != nil {
		t.Fatal(err)
	}

	runOne, err := store.LatestDecisionForArchiveRun(ctx, "RUN-1", "recordings")
	if err != nil || runOne.ID != override.ID || runOne.Sequence != 2 {
		t.Fatalf("run one did not retain its override: decision=%+v err=%v", runOne, err)
	}
	runTwo, err := store.LatestDecisionForArchiveRun(ctx, "RUN-2", "recordings")
	if err != nil || runTwo.ID != second.ID || runTwo.Sequence != 1 {
		t.Fatalf("run two read another ArchiveRun decision: decision=%+v err=%v", runTwo, err)
	}
}

func TestConcurrentOverridesCompareAndSwapTheLatestRunFolderDecision(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "organization.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 11, 6, 2, 0, 0, 0, time.UTC)
	initial := organization.OrganizationDecision{
		ID: "DECISION-INITIAL", Sequence: 1, WorkspaceDriveID: "DRIVE-1", Folder: "hold",
		InputRevision: 1, Disposition: vocabulary.OrganizationHold, DecidedAt: now,
	}
	if err := store.SaveReceipt(ctx, receiptForDecision("RUN-1", "KEY-1", initial, now)); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for index := 1; index <= 2; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			decision := initial
			decision.ID = "DECISION-OVERRIDE-" + string(rune('0'+index))
			decision.Sequence = 2
			decision.Disposition = vocabulary.OrganizationRenderRecordings
			decision.DecidedAt = now.Add(time.Duration(index) * time.Minute)
			results <- store.AppendOverride(ctx, "OVERRIDE-"+decision.ID, "DIGEST-"+decision.ID, initial.ID, decision)
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for result := range results {
		switch {
		case result == nil:
			succeeded++
		case errors.Is(result, ErrRecordConflict):
			conflicted++
		default:
			t.Fatalf("unexpected override result: %v", result)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent overrides did not select exactly one latest decision: success=%d conflict=%d", succeeded, conflicted)
	}
	latest, err := store.LatestDecisionForArchiveRun(ctx, "RUN-1", "hold")
	if err != nil || latest.Sequence != 2 {
		t.Fatalf("latest decision is inconsistent after override race: %+v err=%v", latest, err)
	}
}

func receiptForDecision(runID, key string, decision organization.OrganizationDecision, completedAt time.Time) organization.ArchiveOrganizationReceipt {
	return organization.ArchiveOrganizationReceipt{
		ID: runID, IdempotencyKey: key, InputDigest: "DIGEST-" + runID,
		SessionID: "INTAKE-1", InputRevision: 1, WorkspaceDriveID: "DRIVE-1", WorkerID: "WORKER-1",
		Files:   []organization.CoordinatedFile{{RelativePath: "recordings/video.mp4", ObservationID: "OBS-1", OriginalAssetID: "ASSET-1", InspectionID: "INSPECTION-1"}},
		Folders: []organization.CoordinatedFolder{{Folder: "recordings", Decision: decision}}, CompletedAt: completedAt,
	}
}
