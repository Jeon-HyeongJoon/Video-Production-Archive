package sqlitearchivist

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"videoarchive/internal/application/archivistcontrol"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/vocabulary"
)

func TestInvalidActionCardResolutionReturnsClaimToPending(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
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
	turn, err := archivistruntime.NewTurn("TURN-1", "SESSION-1", "REQUEST-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTurn(ctx, archivistcontrol.TurnRecord{Turn: turn, Prompt: "invalid proposal", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := service.RecordActionCard(ctx, "SESSION-1", archivistcontrol.ActionCardProjection{
		ID: "CARD-1", ArchivistTurnID: turn.ID, CommandType: "unsupported.command", TargetID: "TARGET-1",
		ExpectedRevision: 1, State: "PENDING", CommandID: "COMMAND-1", IdempotencyKey: "CARD-1", Reason: "invalid fixture",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareActionCard(ctx, "CARD-1", "operator-1"); err == nil {
		t.Fatal("unsupported ActionCard unexpectedly resolved")
	}
	record, err := store.ActionCard(ctx, "CARD-1")
	if err != nil || record.Card.State != "PENDING" {
		t.Fatalf("failed ActionCard claim was not released: card=%+v err=%v", record.Card, err)
	}
}
