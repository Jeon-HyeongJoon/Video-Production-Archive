package confirmation

import (
	"time"

	"videoarchive/internal/core/vocabulary"
)

// Record proves that a human re-entered the identity of a destructive target.
// It replaces unrelated approval/confirmation booleans at policy boundaries.
type Record struct {
	ID            string
	ActorType     vocabulary.ActorType
	ActorID       string
	TargetID      string
	ExpectedLabel string
	EnteredLabel  string
	OccurredAt    time.Time
}

func (record Record) ValidFor(targetID string) bool {
	return record.ID != "" && record.ActorType == vocabulary.ActorHuman && record.ActorID != "" &&
		record.TargetID == targetID && record.ExpectedLabel != "" &&
		record.EnteredLabel == record.ExpectedLabel && !record.OccurredAt.IsZero()
}
