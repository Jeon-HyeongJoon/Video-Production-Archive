package approval

import (
	"time"

	"videoarchive/internal/core/vocabulary"
)

// Record is an immutable human decision. Sequence is assigned by the event
// store and resolves later decisions without relying on wall-clock ordering.
type Record struct {
	ID             string
	Sequence       uint64
	ActorType      vocabulary.ActorType
	ActorID        string
	TargetID       string
	TargetRevision uint64
	TargetDigest   string
	Decision       vocabulary.ApprovalDecision
	Reason         string
	Basis          []string
	OccurredAt     time.Time
}

func (record Record) ValidFor(targetID string, targetRevision uint64, targetDigest string) bool {
	return record.ID != "" && record.Sequence > 0 && record.ActorType == vocabulary.ActorHuman && record.ActorID != "" &&
		record.TargetID == targetID && record.TargetRevision == targetRevision &&
		(targetDigest == "" || record.TargetDigest == targetDigest) && record.Decision.Valid() &&
		record.Reason != "" && len(record.Basis) > 0 && !record.OccurredAt.IsZero()
}

// Latest returns the decision with the greatest event-store sequence. A
// duplicate sequence is treated as corrupt evidence instead of choosing by
// slice order or timestamp.
func Latest(records []Record, targetID string, targetRevision uint64, targetDigest string) (Record, bool) {
	var latest Record
	found := false
	seenSequences := make(map[uint64]bool, len(records))
	for _, record := range records {
		if !record.ValidFor(targetID, targetRevision, targetDigest) {
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

func Approved(records []Record, targetID string, targetRevision uint64, targetDigest string) bool {
	record, ok := Latest(records, targetID, targetRevision, targetDigest)
	return ok && record.Decision == vocabulary.ApprovalApproved
}
