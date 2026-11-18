// Package systemstatus projects the current Archive-wide operating status for
// read-only collaborators. It deliberately composes existing read models
// instead of creating another source of truth.
package systemstatus

import (
	"context"
	"errors"
	"sort"
	"time"

	"videoarchive/internal/application/archivework"
	"videoarchive/internal/application/workdiagnostics"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
)

var ErrInvalidConfiguration = errors.New("system status configuration is invalid")

const maximumOpenIssues = 50

type WorkReader interface {
	Inbox(context.Context) ([]archivework.Summary, error)
}

type WorkerReader interface {
	Statuses(context.Context) ([]worker.Status, error)
}

type ArchivistHostReader interface {
	HostStatuses() []archivistruntime.HostStatus
}

type DiagnosticReader interface {
	Diagnose(context.Context, archivistruntime.WorkReference) (workdiagnostics.Snapshot, error)
}

type WorkerSnapshot struct {
	WorkerID               string                `json:"worker_id"`
	State                  string                `json:"state"`
	LastHeartbeat          time.Time             `json:"last_heartbeat,omitempty"`
	ObservedAt             time.Time             `json:"observed_at,omitempty"`
	CPUAverage5m           float64               `json:"cpu_average_5m"`
	GPUAverage5m           float64               `json:"gpu_average_5m"`
	AvailableMemoryBytes   uint64                `json:"available_memory_bytes"`
	ScratchFreeBytes       uint64                `json:"scratch_free_bytes"`
	AcceptingWork          bool                  `json:"accepting_work"`
	HeavySlotsUsed         int                   `json:"heavy_slots_used"`
	HeavySlotsMaximum      int                   `json:"heavy_slots_maximum"`
	LightSlotsUsed         int                   `json:"light_slots_used"`
	LightSlotsMaximum      int                   `json:"light_slots_maximum"`
	AdditionalWorkPossible bool                  `json:"additional_work_possible"`
	HeavyWaitReason        vocabulary.WaitReason `json:"heavy_wait_reason,omitempty"`
	LightWaitReason        vocabulary.WaitReason `json:"light_wait_reason,omitempty"`
}

type ArchivistHostSnapshot struct {
	ArchivistHostID string    `json:"archivist_host_id"`
	State           string    `json:"state"`
	LastHeartbeat   time.Time `json:"last_heartbeat,omitempty"`
}

type WorkCounts struct {
	Total     int `json:"total"`
	Active    int `json:"active"`
	Queued    int `json:"queued"`
	Attention int `json:"attention"`
	Failed    int `json:"failed"`
	Completed int `json:"completed"`
}

type OpenIssue struct {
	WorkKind    archivistruntime.WorkKind `json:"work_kind"`
	WorkID      string                    `json:"work_id"`
	Revision    uint64                    `json:"revision"`
	State       archivework.State         `json:"state"`
	WorkerID    string                    `json:"worker_id,omitempty"`
	HoldCount   int                       `json:"hold_count"`
	LastEventAt time.Time                 `json:"last_event_at"`
	Diagnostic  *workdiagnostics.Snapshot `json:"diagnostic,omitempty"`
}

type Snapshot struct {
	ObservedAt        time.Time               `json:"observed_at"`
	Workers           []WorkerSnapshot        `json:"workers"`
	ArchivistHosts    []ArchivistHostSnapshot `json:"archivist_hosts"`
	WorkCounts        WorkCounts              `json:"work_counts"`
	OpenIssues        []OpenIssue             `json:"open_issues"`
	OmittedIssueCount int                     `json:"omitted_issue_count"`
}

type Service struct {
	works       WorkReader
	workers     WorkerReader
	hosts       ArchivistHostReader
	diagnostics DiagnosticReader
	now         func() time.Time
}

func New(works WorkReader, workers WorkerReader, hosts ArchivistHostReader, diagnostics DiagnosticReader, now func() time.Time) (*Service, error) {
	if works == nil || workers == nil || hosts == nil || diagnostics == nil || now == nil {
		return nil, ErrInvalidConfiguration
	}
	return &Service{works: works, workers: workers, hosts: hosts, diagnostics: diagnostics, now: now}, nil
}

func (service *Service) Snapshot(ctx context.Context) (Snapshot, error) {
	now := service.now().UTC()
	workerStatuses, err := service.workers.Statuses(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	works, err := service.works.Inbox(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	result := Snapshot{ObservedAt: now}
	for _, status := range workerStatuses {
		result.Workers = append(result.Workers, WorkerSnapshot{
			WorkerID: status.WorkerID, State: status.State, LastHeartbeat: status.LastHeartbeat,
			ObservedAt: status.Metrics.ObservedAt, CPUAverage5m: status.Metrics.AverageCPU5m,
			GPUAverage5m: status.Metrics.AverageGPU5m, AvailableMemoryBytes: status.Capacity.AvailableMemoryBytes,
			ScratchFreeBytes: status.Capacity.ScratchFreeBytes, AcceptingWork: status.Metrics.AcceptingWork,
			HeavySlotsUsed: status.Capacity.HeavySlotsUsed, HeavySlotsMaximum: status.Capacity.HeavySlotsMaximum,
			LightSlotsUsed: status.Capacity.LightSlotsUsed, LightSlotsMaximum: status.Capacity.LightSlotsMaximum,
			AdditionalWorkPossible: status.Capacity.AdditionalWorkPossible,
			HeavyWaitReason:        status.Capacity.HeavyAdmission.WaitReason, LightWaitReason: status.Capacity.LightAdmission.WaitReason,
		})
	}
	sort.Slice(result.Workers, func(i, j int) bool { return result.Workers[i].WorkerID < result.Workers[j].WorkerID })
	for _, status := range service.hosts.HostStatuses() {
		result.ArchivistHosts = append(result.ArchivistHosts, ArchivistHostSnapshot{
			ArchivistHostID: status.ArchivistHostID, State: string(status.State), LastHeartbeat: status.LastHeartbeat,
		})
	}
	sort.Slice(result.ArchivistHosts, func(i, j int) bool {
		return result.ArchivistHosts[i].ArchivistHostID < result.ArchivistHosts[j].ArchivistHostID
	})
	for _, work := range works {
		state := effectiveState(work)
		incrementCount(&result.WorkCounts, state)
		if work.ReplacementWorkID != "" || state == archivework.StateCompleted || state == archivework.StateQueued {
			continue
		}
		reference := archivistruntime.WorkReference{Kind: workKind(work.Kind), WorkID: work.WorkID, Revision: work.Revision}
		if reference.Revision == 0 {
			reference.Revision = 1
		}
		diagnostic, diagnosticErr := service.diagnostics.Diagnose(ctx, reference)
		include := state == archivework.StateFailed || state == archivework.StateAttention
		if diagnosticErr == nil && diagnostic.ShouldReport {
			include = true
		}
		if !include {
			continue
		}
		issue := OpenIssue{
			WorkKind: reference.Kind, WorkID: work.WorkID, Revision: reference.Revision, State: state,
			WorkerID: work.WorkerID, HoldCount: work.HoldCount, LastEventAt: work.LastEventAt,
		}
		if diagnosticErr == nil {
			sanitized := workdiagnostics.Sanitize(diagnostic)
			issue.Diagnostic = &sanitized
		}
		result.OpenIssues = append(result.OpenIssues, issue)
	}
	sort.SliceStable(result.OpenIssues, func(i, j int) bool {
		left, right := issueRank(result.OpenIssues[i].State), issueRank(result.OpenIssues[j].State)
		if left != right {
			return left > right
		}
		if !result.OpenIssues[i].LastEventAt.Equal(result.OpenIssues[j].LastEventAt) {
			return result.OpenIssues[i].LastEventAt.After(result.OpenIssues[j].LastEventAt)
		}
		return result.OpenIssues[i].WorkID < result.OpenIssues[j].WorkID
	})
	if len(result.OpenIssues) > maximumOpenIssues {
		result.OmittedIssueCount = len(result.OpenIssues) - maximumOpenIssues
		result.OpenIssues = result.OpenIssues[:maximumOpenIssues]
	}
	return result, nil
}

func effectiveState(work archivework.Summary) archivework.State {
	if work.ReplacementWorkID != "" {
		return archivework.StateCompleted
	}
	return work.State
}

func incrementCount(counts *WorkCounts, state archivework.State) {
	counts.Total++
	switch state {
	case archivework.StateActive:
		counts.Active++
	case archivework.StateQueued:
		counts.Queued++
	case archivework.StateAttention:
		counts.Attention++
	case archivework.StateFailed:
		counts.Failed++
	case archivework.StateCompleted:
		counts.Completed++
	}
}

func workKind(kind archivework.Kind) archivistruntime.WorkKind {
	if kind == archivework.ArchiveRunWork {
		return archivistruntime.WorkArchiveRun
	}
	return archivistruntime.WorkJob
}

func issueRank(state archivework.State) int {
	switch state {
	case archivework.StateFailed:
		return 3
	case archivework.StateAttention:
		return 2
	case archivework.StateActive:
		return 1
	default:
		return 0
	}
}
