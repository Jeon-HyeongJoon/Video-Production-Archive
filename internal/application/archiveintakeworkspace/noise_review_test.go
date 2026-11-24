package archiveintakeworkspace_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"videoarchive/internal/adapter/sqlitearchiveorganization"
	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/application/archivetranscription"
	"videoarchive/internal/core/archiverun"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

// Requirement: ORG-028
func TestOnlyCurrentHumanNoiseChoiceCanExcludeFolderWithoutWork(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitearchiveorganization.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const folder = "card/M4ROOT/SUB"
	now := time.Now().UTC()
	hold := organization.OrganizationDecision{ID: "HOLD-1", Sequence: 1, WorkspaceDriveID: "V", Folder: folder, InputRevision: 7, Disposition: vocabulary.OrganizationHold,
		HoldReasons: []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldSourceRole}, DecidedByType: vocabulary.ActorSystem, DecidedBy: "SYSTEM", DecidedAt: now,
		ClassificationEvidence: &organization.ClassificationEvidence{State: "RECORDED", Files: []organization.FileEvidence{{RelativePath: folder + "/C0001S01.mp4", ReasonCode: "UNPAIRED_PROXY"}}}}
	if err := store.SaveReceipt(ctx, organization.ArchiveOrganizationReceipt{ID: "RUN", IdempotencyKey: "start", InputDigest: "digest", WorkerID: "W", WorkspaceDriveID: "V", SessionID: "I", InputRevision: 7, CompletedAt: now,
		Folders: []organization.CoordinatedFolder{{Folder: folder, Decision: hold}}, Files: []organization.CoordinatedFile{{RelativePath: folder + "/C0001S01.mp4", OriginalAssetID: "A"}}}); err != nil {
		t.Fatal(err)
	}
	jobs := workstationJobs{}
	service, err := archiveintakeworkspace.New(store, jobs)
	if err != nil {
		t.Fatal(err)
	}
	req := archiveintakeworkspace.OverrideCommand{CommandID: "C", DecisionID: "D2", ExpectedDecisionID: hold.ID, IdempotencyKey: "K", ArchiveRunID: "RUN", WorkspaceDriveID: "V", Folder: folder, ExpectedInputRevision: 7,
		Disposition: vocabulary.OrganizationExcludeFromRun, Actor: command.Actor{ID: "agent", Type: vocabulary.ActorArchivist}, OccurredAt: now, Reason: "choice", Basis: []string{hold.ID}}
	if _, err := service.Override(ctx, req); !errors.Is(err, organization.ErrInvalidDecision) {
		t.Fatalf("agent choice: %v", err)
	}
	req.Actor = command.Actor{Type: vocabulary.ActorHuman, ID: "operator"}
	req.ExpectedDecisionID = "stale"
	if _, err := service.Override(ctx, req); !errors.Is(err, organization.ErrStaleInputRevision) {
		t.Fatalf("stale decision: %v", err)
	}
	req.ExpectedDecisionID = hold.ID
	req.Disposition = vocabulary.OrganizationHold
	kept, err := service.Override(ctx, req)
	if err != nil || kept.Sequence != 2 || kept.ClassificationEvidence == nil {
		t.Fatalf("retain HOLD: %+v %v", kept, err)
	}
	req.CommandID, req.DecisionID, req.IdempotencyKey = "C2", "D3", "K2"
	req.Disposition = vocabulary.OrganizationExcludeFromRun
	if _, err := service.Override(ctx, req); !errors.Is(err, organization.ErrStaleInputRevision) {
		t.Fatalf("older choice at same input revision: %v", err)
	}
	req.ExpectedDecisionID, req.Basis, req.Reason = kept.ID, nil, ""
	for range 2 {
		result, err := service.Override(ctx, req)
		if err != nil || result.Sequence != 3 || result.DecidedBy != "operator" || result.DecidedByType != vocabulary.ActorHuman || !slices.Contains(result.DecisionBasis, "previous-decision:"+kept.ID) || result.ClassificationEvidence == nil || len(result.Actions) != 0 {
			t.Fatalf("exclude/replay: %+v %v", result, err)
		}
	}
	p, err := service.Projection(ctx)
	if err != nil || len(p) != 1 || len(p[0].Folders) != 1 {
		t.Fatalf("projection: %+v %v", p, err)
	}
	f := p[0].Folders[0]
	if f.Disposition != vocabulary.OrganizationExcludeFromRun || f.StepState != workflow.Completed || f.JobID != "" || f.ArchivePackageID != "" || p[0].HoldCount != 0 || p[0].StageOneRemaining != 0 || len(jobs) != 0 {
		t.Fatalf("excluded folder counted as work or stored: %+v", f)
	}
	stageOne, err := archivetranscription.NewJobStageOneProjection(store, jobs)
	if err != nil {
		t.Fatal(err)
	}
	progress, recordings, err := stageOne.StageOne(ctx, archiverun.State{ID: "RUN", WorkerID: "W", FolderDiscoveryRevision: 7, FolderIDs: []string{folder}})
	if err != nil || len(recordings) != 0 || len(progress) != 1 {
		t.Fatalf("STT projection: %+v, %v, %v", progress, recordings, err)
	}
	completion, err := archiverun.EvaluateCompletion(progress, false, nil)
	if err != nil || !completion.Complete || len(completion.EligibleFolderIDs) != 0 {
		t.Fatalf("unexpected downstream work: %+v %v", completion, err)
	}
}
