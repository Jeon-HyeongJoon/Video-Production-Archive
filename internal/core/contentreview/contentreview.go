package contentreview

import (
	"time"

	"videoarchive/internal/core/vocabulary"
)

// Record is an append-only human review of one content revision. Current
// review status is a projection of these records, never a mutable Artifact
// provenance field.
type Record struct {
	ID             string
	Sequence       uint64
	ReviewerID     string
	ActorType      vocabulary.ActorType
	TargetID       string
	TargetRevision uint64
	Decision       vocabulary.ContentReviewDecision
	Reason         string
	Basis          []string
	OccurredAt     time.Time
}

func (record Record) ValidFor(targetID string, targetRevision uint64) bool {
	return record.ID != "" && record.Sequence > 0 && record.ReviewerID != "" &&
		record.ActorType == vocabulary.ActorHuman && record.TargetID == targetID &&
		record.TargetRevision == targetRevision && record.Decision.Valid() && record.Reason != "" &&
		len(record.Basis) > 0 && !record.OccurredAt.IsZero()
}

func Latest(records []Record, targetID string, targetRevision uint64) (Record, bool) {
	var latest Record
	found := false
	seenSequences := make(map[uint64]bool, len(records))
	for _, record := range records {
		if !record.ValidFor(targetID, targetRevision) {
			continue
		}
		if seenSequences[record.Sequence] {
			return Record{}, false
		}
		seenSequences[record.Sequence] = true
		if !found || record.Sequence > latest.Sequence {
			latest = record
			found = true
		}
	}
	return latest, found
}
