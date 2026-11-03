package archiveworkspace

import (
	"context"
	"errors"
	"sort"
	"time"

	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

var (
	ErrInvalidProjection = errors.New("invalid archive workspace projection")
	ErrJobNotFound       = errors.New("archive workspace job was not found")
)

type ToolCallStatus string

const (
	ToolCallPending   ToolCallStatus = "PENDING"
	ToolCallRunning   ToolCallStatus = "RUNNING"
	ToolCallCompleted ToolCallStatus = "COMPLETED"
	ToolCallFailed    ToolCallStatus = "FAILED"
)

type Job struct {
	ID            string
	Revision      uint64
	TemplateName  string
	TargetTitle   string
	ControlMode   workflow.ControlMode
	CurrentStepID string
}

type StepStatus struct {
	ID            string
	State         workflow.StepState
	Progress      float64
	ExecutorKind  vocabulary.ExecutorKind
	ActorID       string
	WorkerID      string
	WaitReason    vocabulary.WaitReason
	ErrorClass    string
	LogReference  string
	LastEventAt   time.Time
	DependencyIDs []string
}

type ArchivistAction struct {
	Action         string
	ToolCallID     string
	ToolCallStatus ToolCallStatus
	OccurredAt     time.Time
}

type WorkerResource struct {
	WorkerID             string
	CPUPercent           float64
	GPUPercent           float64
	AvailableMemoryBytes uint64
	AvailableVRAMBytes   uint64
	ScratchFreeBytes     uint64
	HeavyRunning         int
	AcceptingWork        bool
	AccessibleStorageIDs []string
	HeartbeatAt          time.Time
}

type TimelineItem struct {
	Kind       string
	Reference  string
	Summary    string
	OccurredAt time.Time
}

type ProjectionInput struct {
	Job              Job
	Steps            []StepStatus
	ArchivistActions []ArchivistAction
	Workers          []WorkerResource
	Timeline         []TimelineItem
}

type JobSummary struct {
	JobID                  string
	Revision               uint64
	TemplateName           string
	TargetTitle            string
	ControlMode            workflow.ControlMode
	CurrentStepID          string
	StepState              workflow.StepState
	Progress               float64
	ExecutorKind           vocabulary.ExecutorKind
	ActorID                string
	WorkerID               string
	ArchivistAction        string
	ToolCallStatus         ToolCallStatus
	WaitReason             vocabulary.WaitReason
	ErrorClass             string
	LastEventAt            time.Time
	LogReference           string
	NeedsHumanIntervention bool
	InterventionReason     string
	Worker                 *WorkerResource
}

type JobView struct {
	Summary          JobSummary
	Steps            []StepStatus
	ArchivistActions []ArchivistAction
	Workers          []WorkerResource
	Timeline         []TimelineItem
}

func Project(input ProjectionInput) (JobView, error) {
	if input.Job.ID == "" || input.Job.TemplateName == "" || input.Job.TargetTitle == "" ||
		input.Job.CurrentStepID == "" || !validControlMode(input.Job.ControlMode) {
		return JobView{}, ErrInvalidProjection
	}

	steps := append([]StepStatus(nil), input.Steps...)
	var current *StepStatus
	for index := range steps {
		if !validStep(steps[index]) {
			return JobView{}, ErrInvalidProjection
		}
		if steps[index].ID == input.Job.CurrentStepID {
			current = &steps[index]
		}
	}
	if current == nil {
		return JobView{}, ErrInvalidProjection
	}

	actions := append([]ArchivistAction(nil), input.ArchivistActions...)
	sort.SliceStable(actions, func(i, j int) bool { return actions[i].OccurredAt.Before(actions[j].OccurredAt) })
	workers := cloneWorkers(input.Workers)
	timeline := append([]TimelineItem(nil), input.Timeline...)
	sort.SliceStable(timeline, func(i, j int) bool { return timeline[i].OccurredAt.Before(timeline[j].OccurredAt) })

	summary := JobSummary{
		JobID: input.Job.ID, Revision: input.Job.Revision,
		TemplateName: input.Job.TemplateName, TargetTitle: input.Job.TargetTitle,
		ControlMode: input.Job.ControlMode, CurrentStepID: current.ID,
		StepState: current.State, Progress: current.Progress,
		ExecutorKind: current.ExecutorKind, ActorID: current.ActorID, WorkerID: current.WorkerID,
		WaitReason: current.WaitReason, ErrorClass: current.ErrorClass,
		LastEventAt: current.LastEventAt, LogReference: current.LogReference,
	}
	if len(actions) > 0 {
		latest := actions[len(actions)-1]
		summary.ArchivistAction = latest.Action
		summary.ToolCallStatus = latest.ToolCallStatus
	}
	for index := range workers {
		if workers[index].WorkerID == current.WorkerID {
			worker := workers[index]
			summary.Worker = &worker
			break
		}
	}
	summary.NeedsHumanIntervention, summary.InterventionReason = intervention(summary)

	return JobView{Summary: summary, Steps: steps, ArchivistActions: actions, Workers: workers, Timeline: timeline}, nil
}

func validControlMode(mode workflow.ControlMode) bool {
	return mode == workflow.ArchivistMode || mode == workflow.AssistedMode || mode == workflow.ManualMode
}

func validStep(step StepStatus) bool {
	if step.ID == "" || step.Progress < 0 || step.Progress > 1 || step.LastEventAt.IsZero() {
		return false
	}
	switch step.State {
	case workflow.Queued, workflow.Waiting, workflow.Reserved, workflow.Running, workflow.Verifying,
		workflow.Paused, workflow.Failed, workflow.Completed, workflow.Cancelled:
	default:
		return false
	}
	if step.WaitReason != "" && !step.WaitReason.Valid() {
		return false
	}
	return step.ExecutorKind.Valid()
}

func intervention(summary JobSummary) (bool, string) {
	if summary.StepState == workflow.Failed || summary.ErrorClass != "" {
		return true, "STEP_FAILED"
	}
	if summary.ToolCallStatus == ToolCallFailed {
		return true, "TOOL_CALL_FAILED"
	}
	if summary.StepState == workflow.Waiting {
		switch summary.WaitReason {
		case vocabulary.WaitApproval:
			return true, "APPROVAL_REQUIRED"
		case vocabulary.WaitInput:
			return true, "INPUT_REQUIRED"
		case vocabulary.WaitDevice:
			return true, "DEVICE_ATTENTION_REQUIRED"
		}
	}
	return false, ""
}

func cloneWorkers(source []WorkerResource) []WorkerResource {
	workers := append([]WorkerResource(nil), source...)
	for index := range workers {
		workers[index].AccessibleStorageIDs = append([]string(nil), workers[index].AccessibleStorageIDs...)
	}
	return workers
}

type Reader interface {
	List(context.Context) ([]JobView, error)
	Get(context.Context, string) (JobView, error)
}

type QueryService struct {
	reader Reader
}

func NewQueryService(reader Reader) (*QueryService, error) {
	if reader == nil {
		return nil, ErrInvalidProjection
	}
	return &QueryService{reader: reader}, nil
}

func (service *QueryService) Inbox(ctx context.Context) ([]JobSummary, error) {
	views, err := service.reader.List(ctx)
	if err != nil {
		return nil, err
	}
	summaries := make([]JobSummary, 0, len(views))
	for _, view := range views {
		summaries = append(summaries, view.Summary)
	}
	return summaries, nil
}

func (service *QueryService) Detail(ctx context.Context, jobID string) (JobView, error) {
	return service.reader.Get(ctx, jobID)
}
