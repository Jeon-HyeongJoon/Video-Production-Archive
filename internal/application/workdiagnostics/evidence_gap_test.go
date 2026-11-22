package workdiagnostics

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"videoarchive/internal/adapter/sqlitearchiveworkspace"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/workflow"
)

// Requirement: OBS-005
func TestEvidenceGapsDistinguishMissingObservationsFromKnownState(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	store, err := sqlitearchiveworkspace.Open(filepath.Join(t.TempDir(), "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	jobs, err := archiveworkspace.NewQueryService(store)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(fixedWorkReader{}, jobs, fixedWorkerReader{}, fixedExecutionReader{}, func() time.Time { return now }, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sample := func(id string, seq uint64, at time.Time) job.StepProgressEvidence {
		return job.StepProgressEvidence{EvidenceID: id, StepID: "transcript", WorkerID: "worker-1", FencingToken: 1, ReportSequence: seq, ObservedAt: at,
			Update: execution.ProgressUpdate{Operation: execution.OperationTranscribeAudio, Status: execution.ProgressCompleted, ChunkIndex: uint32(seq), ChunkCount: 8, SourceStartMillis: int64(seq-1) * 1000, SourceEndMillis: int64(seq) * 1000, Progress: float64(seq) / 8}}
	}
	last := sample("EVD-FOUR", 4, now.Add(-2*time.Second))
	t.Run("settlement detects a missing final report without inventing a failed Step", func(t *testing.T) {
		view := archiveworkspace.JobView{Summary: archiveworkspace.JobSummary{JobID: "JOB-TAIL", Revision: 9, CurrentStepID: "transcript", StepState: workflow.Completed, LastEventAt: now}, Steps: []archiveworkspace.StepStatus{{ID: "transcript", State: workflow.Completed, HasVerificationReferences: true, ProgressReportCounts: map[uint64]uint64{1: 6}, ProgressEvidence: []job.StepProgressEvidence{last}, ProgressHistoryOmitted: 3}}}
		if err := store.Replace(ctx, view); err != nil {
			t.Fatal(err)
		}
		diag, err := service.Diagnose(ctx, archivistruntime.WorkReference{Kind: archivistruntime.WorkJob, WorkID: "JOB-TAIL", Revision: 9})
		if err != nil || len(diag.Jobs) != 1 {
			t.Fatal(err)
		}
		d := diag.Jobs[0]
		if !d.LastProgressAt.Equal(last.ObservedAt) || len(d.EvidenceGaps) != 2 || d.EvidenceGaps[0].Kind != "PROGRESS_WINDOW" || d.EvidenceGaps[0].HistoryOmitted != 3 || d.EvidenceGaps[1].First != 5 || d.EvidenceGaps[1].Last != 6 || d.EvidenceGaps[1].LastKnown == nil || d.EvidenceGaps[1].LastKnown.EvidenceID != last.EvidenceID || d.StepState != workflow.Completed || len(diag.RecoveryActions) != 0 {
			t.Fatalf("unexpected settlement evidence: %+v", d)
		}
	})
	for _, tc := range []struct {
		id, kind string
		state    workflow.StepState
		samples  []job.StepProgressEvidence
	}{
		{"JOB-GAP", "PROGRESS_SEQUENCE", workflow.Running, []job.StepProgressEvidence{sample("EVD-ONE", 1, now.Add(-6*time.Second)), last}},
		{"JOB-LEGACY", "RESULT_VERIFICATION", workflow.Completed, nil},
	} {
		t.Run(tc.id, func(t *testing.T) {
			// The persisted Step transition is newer than the last actual Worker sample.
			eventAt := now.Add(-time.Second)
			view := archiveworkspace.JobView{Summary: archiveworkspace.JobSummary{JobID: tc.id, Revision: 8, CurrentStepID: "transcript", StepState: tc.state, LastEventAt: eventAt},
				Steps: []archiveworkspace.StepStatus{{ID: "transcript", State: tc.state, LastEventAt: eventAt, ProgressEvidence: tc.samples}}}
			if err := store.Replace(ctx, view); err != nil {
				t.Fatal(err)
			}
			diag, err := service.Diagnose(ctx, archivistruntime.WorkReference{Kind: archivistruntime.WorkJob, WorkID: tc.id, Revision: 8})
			if err != nil {
				t.Fatal(err)
			}
			if len(diag.Jobs) != 1 || diag.Jobs[0].StepState != tc.state || len(diag.RecoveryActions) != 0 || len(diag.Jobs[0].FailureEvidence) != 0 {
				t.Fatalf("gap changed the known outcome or authorized recovery: %+v", diag)
			}
			var stateFact Fact
			for _, fact := range diag.Facts {
				if fact.Code == "STEP_STATE_OBSERVED" {
					stateFact = fact
				}
			}
			if stateFact.EvidenceID == "" || !stateFact.ObservedAt.Equal(eventAt) {
				t.Fatalf("missing source or time for the persisted Step state: %+v", stateFact)
			}
			// Read the diagnostic contract as exposed to Archivist clients.
			payload, err := json.Marshal(diag.Jobs[0])
			if err != nil {
				t.Fatal(err)
			}
			var detail struct {
				Gaps []struct {
					Kind       string `json:"kind"`
					First      uint64 `json:"first_missing_sequence"`
					Last       uint64 `json:"last_missing_sequence"`
					Known      *Fact  `json:"last_known_fact"`
					Unobserved bool   `json:"unobserved"`
				} `json:"evidence_gaps"`
			}
			if err := json.Unmarshal(payload, &detail); err != nil {
				t.Fatal(err)
			}
			for _, gap := range detail.Gaps {
				if gap.Kind != tc.kind {
					continue
				}
				if tc.samples != nil {
					if gap.First != 2 || gap.Last != 3 || gap.Unobserved || gap.Known == nil || gap.Known.EvidenceID != last.EvidenceID || !gap.Known.ObservedAt.Equal(last.ObservedAt) {
						t.Fatalf("wrong missing range or last observed source: %+v", gap)
					}
				} else if !gap.Unobserved || gap.Known != nil {
					t.Fatalf("legacy output has no verification observation: %+v", gap)
				}
				return
			}
			t.Fatalf("diagnostic for %s omitted the %s evidence gap", tc.id, tc.kind)
		})
	}
}
