package workdiagnostics

import (
	"fmt"
	"slices"

	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

type EvidenceGap struct {
	Kind           string `json:"kind"`
	StepID         string `json:"step_id"`
	FencingToken   uint64 `json:"fencing_token,omitempty"`
	First          uint64 `json:"first_missing_sequence,omitempty"`
	Last           uint64 `json:"last_missing_sequence,omitempty"`
	HistoryOmitted uint64 `json:"history_omitted,omitempty"`
	LastKnown      *Fact  `json:"last_known_fact,omitempty"`
	Unobserved     bool   `json:"unobserved,omitempty"`
}

func progressFact(evidence job.StepProgressEvidence) Fact {
	u := evidence.Update
	summary := string(u.Operation) + " " + string(u.Status)
	if u.InputCount != 0 {
		summary += fmt.Sprintf(" input %d of %d", u.InputIndex, u.InputCount)
	}
	switch {
	case u.Status.IsPhase():
	case u.Status == execution.ProgressPosition:
		summary += fmt.Sprintf(" at output millisecond %d", u.OutputMillis)
	default:
		summary += fmt.Sprintf(" chunk %d of %d, source milliseconds %d-%d", u.ChunkIndex, u.ChunkCount, u.SourceStartMillis, u.SourceEndMillis)
		if u.ProcessPercent != 0 {
			summary += fmt.Sprintf(", engine %d%%", u.ProcessPercent)
		}
	}
	observedAt := evidence.ObservedAt
	if !u.WorkerObservedAt.IsZero() {
		observedAt = u.WorkerObservedAt
	}
	return Fact{EvidenceID: evidence.EvidenceID, Code: "WORKER_PROGRESS_RECORDED", Summary: summary, ObservedAt: observedAt}
}

func evidenceGaps(step archiveworkspace.StepStatus) []EvidenceGap {
	var gaps []EvidenceGap
	if step.State == workflow.Completed && (step.ExecutorKind == vocabulary.ExecutorWorker || step.ExecutorKind == "") && !step.HasVerificationReferences {
		gaps = append(gaps, EvidenceGap{Kind: "RESULT_VERIFICATION", StepID: step.ID, Unobserved: true})
	}
	if step.ProgressHistoryOmitted > 0 {
		gaps = append(gaps, EvidenceGap{Kind: "PROGRESS_WINDOW", StepID: step.ID, HistoryOmitted: step.ProgressHistoryOmitted})
	}
	last := make(map[uint64]job.StepProgressEvidence)
	for i, e := range step.ProgressEvidence {
		previous := last[e.FencingToken].ReportSequence
		if e.ReportSequence > previous && e.ReportSequence-previous > 1 && (i != 0 || step.ProgressHistoryOmitted == 0) {
			gaps = append(gaps, EvidenceGap{Kind: "PROGRESS_SEQUENCE", StepID: step.ID, FencingToken: e.FencingToken, First: previous + 1, Last: e.ReportSequence - 1})
		}
		last[e.FencingToken] = e
	}
	var tokens []uint64
	for token := range step.ProgressReportCounts {
		tokens = append(tokens, token)
	}
	slices.Sort(tokens)
	for _, token := range tokens {
		if step.ProgressHistoryOmitted > 0 && len(step.ProgressEvidence) > 0 && token < step.ProgressEvidence[0].FencingToken {
			continue
		}
		previous := last[token].ReportSequence
		if total := step.ProgressReportCounts[token]; total > previous {
			gaps = append(gaps, EvidenceGap{Kind: "PROGRESS_SEQUENCE", StepID: step.ID, FencingToken: token, First: previous + 1, Last: total})
		}
	}
	for i := range gaps {
		gap := &gaps[i]
		if gap.Kind != "PROGRESS_SEQUENCE" {
			continue
		}
		if e, ok := last[gap.FencingToken]; ok {
			fact := progressFact(e)
			gap.LastKnown = &fact
		} else {
			gap.Unobserved = true
		}
	}
	return gaps
}
