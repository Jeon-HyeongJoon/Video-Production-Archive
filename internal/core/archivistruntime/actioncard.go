package archivistruntime

import (
	"errors"
	"strings"
	"time"

	"videoarchive/internal/core/approval"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidActionCard      = errors.New("invalid action card")
	ErrExplicitActionRequired = errors.New("EXPLICIT_ACTION_REQUIRED")
)

type ActionCard struct {
	ID               string
	ArchivistTurnID  string
	CommandID        string
	CommandType      string
	TargetID         string
	ExpectedRevision uint64
	IdempotencyKey   string
	Reason           string
	Basis            []string
	ApprovalRequired bool
	Parameters       map[string]any
}

type ActionSource string

const (
	ActionFromChat ActionSource = "CHAT"
	ActionFromCard ActionSource = "ACTION_CARD"
)

type ActionCardRequest struct {
	Source           ActionSource
	Text             string
	ActorType        vocabulary.ActorType
	ActorID          string
	CurrentRevision  uint64
	ApprovalID       string
	ApprovalSequence uint64
	OccurredAt       time.Time
}

type ActionCardDecision struct {
	Approval *approval.Record
	Command  *command.Envelope
}

func ResolveActionCard(card ActionCard, request ActionCardRequest) (ActionCardDecision, error) {
	if !validActionCard(card) {
		return ActionCardDecision{}, ErrInvalidActionCard
	}
	if request.Source != ActionFromCard {
		return ActionCardDecision{}, ErrExplicitActionRequired
	}
	if request.ActorType != vocabulary.ActorHuman || strings.TrimSpace(request.ActorID) == "" {
		return ActionCardDecision{}, ErrExplicitActionRequired
	}
	if request.CurrentRevision != card.ExpectedRevision {
		return ActionCardDecision{}, command.ErrRevisionConflict
	}
	if card.ApprovalRequired {
		if strings.TrimSpace(request.ApprovalID) == "" || request.ApprovalSequence == 0 || request.OccurredAt.IsZero() {
			return ActionCardDecision{}, ErrInvalidActionCard
		}
		record := approval.Record{
			ID: strings.TrimSpace(request.ApprovalID), Sequence: request.ApprovalSequence,
			ActorType: request.ActorType, ActorID: strings.TrimSpace(request.ActorID),
			TargetID: card.TargetID, TargetRevision: card.ExpectedRevision,
			Decision: vocabulary.ApprovalApproved, Reason: card.Reason,
			Basis: append([]string(nil), card.Basis...), OccurredAt: request.OccurredAt,
		}
		return ActionCardDecision{Approval: &record}, nil
	}
	parameters := make(map[string]any, len(card.Parameters)+3)
	for key, value := range card.Parameters {
		parameters[key] = value
	}
	parameters["archivist_turn_id"] = card.ArchivistTurnID
	parameters["action_card_id"] = card.ID
	parameters["basis"] = append([]string(nil), card.Basis...)
	envelope := command.Envelope{
		CommandID: card.CommandID, CommandType: card.CommandType,
		Actor:        command.Actor{Type: command.HumanActor, ID: strings.TrimSpace(request.ActorID)},
		RequestedVia: "archive_workspace_action_card", TargetID: card.TargetID, ExpectedRevision: card.ExpectedRevision,
		IdempotencyKey: card.IdempotencyKey, Parameters: parameters, Reason: card.Reason,
	}
	if err := envelope.Validate(); err != nil {
		return ActionCardDecision{}, ErrInvalidActionCard
	}
	return ActionCardDecision{Command: &envelope}, nil
}

func validActionCard(card ActionCard) bool {
	if strings.TrimSpace(card.ID) == "" || strings.TrimSpace(card.ArchivistTurnID) == "" ||
		strings.TrimSpace(card.CommandID) == "" || strings.TrimSpace(card.CommandType) == "" ||
		strings.TrimSpace(card.TargetID) == "" || strings.TrimSpace(card.IdempotencyKey) == "" ||
		strings.TrimSpace(card.Reason) == "" || len(card.Basis) == 0 {
		return false
	}
	for _, basis := range card.Basis {
		if strings.TrimSpace(basis) == "" {
			return false
		}
	}
	return true
}
