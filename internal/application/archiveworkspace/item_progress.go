package archiveworkspace

import (
	"path"
	"strings"

	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/workflow"
)

// ItemProgress counts sequential outputs in a single execution attempt.
// A clock at the end of a video is still provisional until the output verifies.
type ItemProgress struct {
	Operation      execution.Operation `json:"operation"`
	Total          int                 `json:"total"`
	Completed      int                 `json:"completed"`
	Current        int                 `json:"current,omitempty"`
	Name           string              `json:"name,omitempty"`
	PositionMillis int64               `json:"position_millis,omitempty"`
	DurationMillis int64               `json:"duration_millis,omitempty"`
	Attempt        int                 `json:"attempt,omitempty"`
}

func ObservedItems(s job.Step) *ItemProgress {
	spec := s.ExecutionSpec
	if spec == nil || len(spec.Outputs) == 0 {
		return nil
	}
	p := ItemProgress{Operation: spec.Operation, Total: len(spec.Outputs), Attempt: s.Workflow.Attempt}
	switch spec.Operation {
	case execution.OperationRenderSourceSequence:
		p.Total = 1 // The second output is a marker artifact.
	case execution.OperationRenderRecording, execution.OperationExtractSpeechAudio, execution.OperationTranscribeAudio:
	default:
		return nil
	}
	switch s.Workflow.State {
	case workflow.Running, workflow.Verifying, workflow.Completed:
		p.Attempt++ // Workflow.Attempt counts failed attempts.
	}
	if s.Workflow.State == workflow.Completed {
		p.Completed = p.Total
		return &p
	}
	if s.Workflow.State == workflow.Queued || s.Workflow.State == workflow.Waiting || s.Workflow.State == workflow.Reserved {
		return &p
	}
	for i := len(s.ProgressEvidence) - 1; i >= 0; i-- {
		e := s.ProgressEvidence[i]
		u := e.Update
		if e.FencingToken != s.Workflow.LastFencingToken || u.Operation != spec.Operation {
			continue
		}
		if u.Status == execution.ProgressCheckingOutputs {
			p.Completed = p.Total
			return &p
		}
		if u.InputIndex < 1 || int(u.InputCount) != p.Total || u.InputIndex > u.InputCount {
			continue
		}
		p.Current, p.Completed, p.PositionMillis = int(u.InputIndex), int(u.InputIndex)-1, u.OutputMillis
		if spec.Operation == execution.OperationRenderSourceSequence {
			if timeline := spec.Parameters.SourceTimeline; timeline != nil {
				p.DurationMillis = timeline.DurationMillis()
			}
			break
		}
		id := spec.Outputs[p.Current-1].OriginalAssetID
		for _, input := range spec.Inputs {
			if id != "" && input.OriginalAssetID == id {
				name := input.OriginalName
				if name == "" && input.Kind == "" {
					name = input.RelativePath
				}
				p.Name = path.Base(strings.ReplaceAll(name, "\\", "/"))
				if p.Name == "." {
					p.Name = ""
				}
				p.DurationMillis = input.DurationMillis
				break
			}
		}
		break
	}
	return &p
}
