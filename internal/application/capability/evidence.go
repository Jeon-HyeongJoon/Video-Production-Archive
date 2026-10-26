package capability

import (
	"context"
	"errors"
	"time"

	"videoarchive/internal/core/command"
)

var (
	ErrInvalidEvidence    = errors.New("invalid capability evidence")
	ErrInvalidCorrelation = errors.New("invalid capability correlation")
)

type EvidencePhase string

const (
	EvidenceRequested EvidencePhase = "CAPABILITY_REQUESTED"
	EvidenceStarted   EvidencePhase = "CAPABILITY_STARTED"
	EvidenceCompleted EvidencePhase = "CAPABILITY_COMPLETED"
	EvidenceFailed    EvidencePhase = "CAPABILITY_FAILED"
	EvidenceCancelled EvidencePhase = "CAPABILITY_CANCELLED"
)

type Evidence struct {
	RunID                  string        `json:"run_id"`
	Phase                  EvidencePhase `json:"phase"`
	CapabilityName         string        `json:"capability_name"`
	CapabilityVersion      uint32        `json:"capability_version"`
	CommandID              string        `json:"command_id"`
	TargetID               string        `json:"target_id"`
	Actor                  command.Actor `json:"actor"`
	RequestedVia           string        `json:"requested_via"`
	JobID                  string        `json:"job_id,omitempty"`
	StepID                 string        `json:"step_id,omitempty"`
	WorkerID               string        `json:"worker_id,omitempty"`
	WorkspaceDriveIDs      []string      `json:"workspace_drive_ids,omitempty"`
	InputIDs               []string      `json:"input_ids,omitempty"`
	ResultIDs              []string      `json:"result_ids,omitempty"`
	VerificationReferences []string      `json:"verification_references,omitempty"`
	StartedAt              time.Time     `json:"started_at,omitempty"`
	ObservedAt             time.Time     `json:"observed_at"`
	FinishedAt             time.Time     `json:"finished_at,omitempty"`
	Outcome                string        `json:"outcome,omitempty"`
	ErrorClass             string        `json:"error_class,omitempty"`
}

func (evidence Evidence) Validate() error {
	if evidence.RunID == "" || evidence.CapabilityName == "" || evidence.CapabilityVersion == 0 ||
		evidence.CommandID == "" || evidence.TargetID == "" || !evidence.Actor.Type.Valid() ||
		evidence.Actor.ID == "" || evidence.RequestedVia == "" || evidence.ObservedAt.IsZero() {
		return ErrInvalidEvidence
	}
	switch evidence.Phase {
	case EvidenceRequested:
		if !evidence.StartedAt.IsZero() || !evidence.FinishedAt.IsZero() || evidence.Outcome != "" || evidence.ErrorClass != "" {
			return ErrInvalidEvidence
		}
	case EvidenceStarted:
		if evidence.StartedAt.IsZero() || !evidence.FinishedAt.IsZero() || evidence.Outcome != "" || evidence.ErrorClass != "" {
			return ErrInvalidEvidence
		}
	case EvidenceCompleted:
		if evidence.StartedAt.IsZero() || evidence.FinishedAt.IsZero() || evidence.Outcome != "SUCCEEDED" || evidence.ErrorClass != "" {
			return ErrInvalidEvidence
		}
	case EvidenceFailed:
		if evidence.StartedAt.IsZero() || evidence.FinishedAt.IsZero() || evidence.Outcome != "FAILED" || evidence.ErrorClass == "" {
			return ErrInvalidEvidence
		}
	case EvidenceCancelled:
		if evidence.StartedAt.IsZero() || evidence.FinishedAt.IsZero() || evidence.Outcome != "CANCELLED" || evidence.ErrorClass != "CANCELLED" {
			return ErrInvalidEvidence
		}
	default:
		return ErrInvalidEvidence
	}
	return nil
}

type EvidenceSink interface {
	Append(context.Context, Evidence) error
}

type ObservedPort struct {
	next Port
	sink EvidenceSink
	now  func() time.Time
}

var _ Port = (*ObservedPort)(nil)

func NewObservedPort(next Port, sink EvidenceSink) (*ObservedPort, error) {
	if next == nil || sink == nil {
		return nil, ErrInvalidEvidence
	}
	return &ObservedPort{next: next, sink: sink, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (port *ObservedPort) Execute(ctx context.Context, input Input) (Receipt, error) {
	if input.Correlation.RunID == "" {
		return Receipt{}, ErrInvalidCorrelation
	}

	evidenceCtx := context.WithoutCancel(ctx)
	requestedAt := port.now()
	base := Evidence{
		RunID: input.Correlation.RunID, Phase: EvidenceRequested,
		CapabilityName: input.Name, CapabilityVersion: input.Version,
		CommandID: input.Command.CommandID, TargetID: input.Command.TargetID,
		Actor: input.Command.Actor, RequestedVia: input.Command.RequestedVia,
		JobID: input.Correlation.JobID, StepID: input.Correlation.StepID,
		WorkerID:          input.Correlation.WorkerID,
		WorkspaceDriveIDs: append([]string(nil), input.Correlation.WorkspaceDriveIDs...),
		InputIDs:          append([]string(nil), input.Correlation.InputIDs...),
		ObservedAt:        requestedAt,
	}
	if err := port.sink.Append(evidenceCtx, base); err != nil {
		return Receipt{}, err
	}

	startedAt := port.now()
	started := base
	started.Phase = EvidenceStarted
	started.StartedAt = startedAt
	started.ObservedAt = startedAt
	if err := port.sink.Append(evidenceCtx, started); err != nil {
		return Receipt{}, err
	}

	receipt, executionErr := port.next.Execute(ctx, input)
	finishedAt := port.now()
	terminal := started
	terminal.ObservedAt = finishedAt
	terminal.FinishedAt = finishedAt
	if executionErr != nil {
		terminal.Phase, terminal.Outcome, terminal.ErrorClass = classifyFailure(executionErr)
		if evidenceErr := port.sink.Append(evidenceCtx, terminal); evidenceErr != nil {
			return Receipt{}, errors.Join(executionErr, evidenceErr)
		}
		return Receipt{}, executionErr
	}

	terminal.Phase = EvidenceCompleted
	terminal.Outcome = "SUCCEEDED"
	if receipt.Reference.JobID != "" {
		terminal.JobID = receipt.Reference.JobID
	}
	if receipt.Reference.StepID != "" {
		terminal.StepID = receipt.Reference.StepID
	}
	if receipt.Reference.ResultID != "" {
		terminal.ResultIDs = []string{receipt.Reference.ResultID}
	}
	terminal.VerificationReferences = append([]string(nil), receipt.Reference.VerificationReferences...)
	if evidenceErr := port.sink.Append(evidenceCtx, terminal); evidenceErr != nil {
		return receipt, evidenceErr
	}
	return receipt, nil
}

func classifyFailure(err error) (EvidencePhase, string, string) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return EvidenceCancelled, "CANCELLED", "CANCELLED"
	}
	return EvidenceFailed, "FAILED", "CAPABILITY_EXECUTION"
}
