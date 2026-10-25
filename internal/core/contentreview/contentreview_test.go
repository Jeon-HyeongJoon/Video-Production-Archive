package contentreview

import (
	"testing"
	"time"

	"videoarchive/internal/core/vocabulary"
)

// Requirement: REV-001
func TestContentReviewUsesLatestImmutableDecisionForTargetRevision(t *testing.T) {
	now := time.Now()
	accepted := Record{
		ID: "REVIEW-1", Sequence: 1, ReviewerID: "operator-1", ActorType: vocabulary.ActorHuman,
		TargetID: "SUMMARY-1", TargetRevision: 2, Decision: vocabulary.ContentAccepted,
		Reason: "facts checked", Basis: []string{"TRANSCRIPT-1"}, OccurredAt: now,
	}
	changes := accepted
	changes.ID = "REVIEW-2"
	changes.Sequence = 2
	changes.Decision = vocabulary.ContentChangesRequested
	changes.Reason = "speaker name is wrong"
	latest, ok := Latest([]Record{accepted, changes}, "SUMMARY-1", 2)
	if !ok || latest.ID != "REVIEW-2" || latest.Decision != vocabulary.ContentChangesRequested {
		t.Fatalf("latest review projection is wrong: latest=%+v ok=%v", latest, ok)
	}
	if accepted.Decision != vocabulary.ContentAccepted {
		t.Fatalf("earlier immutable review was changed: %+v", accepted)
	}
	duplicate := accepted
	duplicate.ID = "REVIEW-corrupt"
	if _, ok := Latest([]Record{accepted, changes, duplicate}, "SUMMARY-1", 2); ok {
		t.Fatal("duplicate persisted sequence produced a review projection")
	}
}
