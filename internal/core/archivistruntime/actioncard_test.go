package archivistruntime

import (
	"errors"
	"testing"
	"time"

	"videoarchive/internal/core/command"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: DSH-003
func TestActionCardRequiresExplicitCurrentRevisionHumanDecision(t *testing.T) {
	card := ActionCard{
		ID: "CARD-1", ArchivistTurnID: "TURN-1", CommandID: "COMMAND-1", CommandType: "archive.commit",
		TargetID: "PACKAGE-1", ExpectedRevision: 3, IdempotencyKey: "card-1", Reason: "QC passed",
		Basis: []string{"QC-1"}, ApprovalRequired: true,
	}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	chat := ActionCardRequest{
		Source: ActionFromChat, Text: "승인", ActorType: vocabulary.ActorHuman, ActorID: "operator-1",
		CurrentRevision: 3, ApprovalID: "APPROVAL-1", ApprovalSequence: 9, OccurredAt: now,
	}
	if _, err := ResolveActionCard(card, chat); !errors.Is(err, ErrExplicitActionRequired) {
		t.Fatalf("chat text became approval: %v", err)
	}
	stale := chat
	stale.Source = ActionFromCard
	stale.CurrentRevision = 4
	if _, err := ResolveActionCard(card, stale); !errors.Is(err, command.ErrRevisionConflict) {
		t.Fatalf("stale card was not rejected: %v", err)
	}
	explicit := chat
	explicit.Source = ActionFromCard
	decision, err := ResolveActionCard(card, explicit)
	if err != nil || decision.Approval == nil || decision.Command != nil {
		t.Fatalf("explicit current card did not stage approval: decision=%+v err=%v", decision, err)
	}
	if decision.Approval.ActorType != vocabulary.ActorHuman || decision.Approval.TargetRevision != 3 ||
		decision.Approval.Reason != "QC passed" || len(decision.Approval.Basis) != 1 || decision.Approval.Basis[0] != "QC-1" {
		t.Fatalf("approval lost immutable card fields: %+v", decision.Approval)
	}
	card.Basis[0] = "MUTATED"
	if decision.Approval.Basis[0] != "QC-1" {
		t.Fatalf("approval retained mutable ActionCard basis: %+v", decision.Approval)
	}
	commandCard := card
	commandCard.Basis = []string{"QC-2"}
	commandCard.ApprovalRequired = false
	commandDecision, err := ResolveActionCard(commandCard, explicit)
	if err != nil || commandDecision.Command == nil || commandDecision.Approval != nil ||
		commandDecision.Command.ExpectedRevision != 3 || commandDecision.Command.Actor.Type != command.HumanActor {
		t.Fatalf("explicit non-gated card did not stage command: decision=%+v err=%v", commandDecision, err)
	}
}
