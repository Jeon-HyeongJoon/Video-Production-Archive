package archivework

import (
	"testing"

	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/workflow"
)

// Requirement: DSH-055
func TestWorkCountsCurrentAttemptMediaAndCompletedChildren(t *testing.T) {
	op := execution.OperationRenderRecording
	s := job.Step{Workflow: workflow.Step{State: workflow.Running, Attempt: 1, LastFencingToken: 2}, ExecutionSpec: &execution.StepExecutionSpec{
		Operation: op, Inputs: []execution.InputReference{{OriginalAssetID: "B", OriginalName: "second.mov", DurationMillis: 16000}, {OriginalAssetID: "A"}},
		Outputs: []execution.OutputTarget{{OriginalAssetID: "A"}, {OriginalAssetID: "B"}},
	}}
	sample := job.StepProgressEvidence{FencingToken: 1, Update: execution.ProgressUpdate{Operation: op, Status: execution.ProgressPosition, OutputMillis: 16000, InputIndex: 2, InputCount: 2, Progress: 1}}
	s.ProgressEvidence = []job.StepProgressEvidence{sample}
	if p := archiveworkspace.ObservedItems(s); p.Current != 0 || p.Completed != 0 || p.Total != 2 {
		t.Fatal(p)
	}
	sample.FencingToken = 2
	s.ProgressEvidence = append(s.ProgressEvidence, sample)
	p := archiveworkspace.ObservedItems(s)
	if p.Current != 2 || p.Completed != 1 || p.Total != 2 || p.DurationMillis != 16000 || p.PositionMillis != 16000 || p.Name != "second.mov" || p.Attempt != 2 {
		t.Fatal(p)
	}
	children := []ChildJob{{Job: archiveworkspace.JobSummary{StepState: workflow.Completed}}, {Job: archiveworkspace.JobSummary{StepState: workflow.Running, Items: p}}}
	var summary Summary
	measureChildren(&summary, children)
	if summary.EffectiveJobs != 2 || summary.CompletedJobs != 1 || summary.Items != p {
		t.Fatal(summary)
	}
	multi := Summary{}
	measureChildren(&multi, append(children, children[1]))
	if multi.EffectiveJobs != 3 || multi.CompletedJobs != 1 || multi.Items != nil {
		t.Fatal(multi)
	}
	s.Workflow.State = workflow.Cancelled
	if p := archiveworkspace.ObservedItems(s); p.Attempt != 2 || p.Current != 2 || p.Completed != 1 {
		t.Fatal(p)
	}
	s.Workflow.State = workflow.Completed
	if p := archiveworkspace.ObservedItems(s); p.Completed != 2 || p.Current != 0 {
		t.Fatal(p)
	}
	s.ExecutionSpec.Operation = execution.OperationRenderSourceSequence
	if p := archiveworkspace.ObservedItems(s); p.Total != 1 || p.Completed != 1 {
		t.Fatal(p)
	}
	s.ExecutionSpec = nil
	if archiveworkspace.ObservedItems(s) != nil {
		t.Fatal("invented a count for an unmeasured legacy step")
	}
}
