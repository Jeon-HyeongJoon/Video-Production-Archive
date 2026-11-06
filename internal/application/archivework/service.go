// Package archivework projects the user's persistent work units. ArchiveRun
// is the parent work unit for archive intake; its inspection, folder and STT
// Jobs remain independently scheduled child executions. Jobs that do not
// belong to an ArchiveRun remain standalone work units.
package archivework

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/core/archiverun"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

var (
	ErrInvalidService = errors.New("archive work service is invalid")
	ErrWorkNotFound   = errors.New("archive work was not found")
)

type Kind string

const (
	ArchiveRunWork Kind = "ARCHIVE_RUN"
	StandaloneJob  Kind = "JOB"
)

type State string

const (
	StateFailed    State = "FAILED"
	StateAttention State = "ATTENTION"
	StateActive    State = "ACTIVE"
	StateQueued    State = "QUEUED"
	StateCompleted State = "COMPLETED"
)

type ChildRole string

const (
	ChildInspection    ChildRole = "INSPECTION"
	ChildFolder        ChildRole = "FOLDER"
	ChildTranscription ChildRole = "TRANSCRIPTION"
)

type Summary struct {
	WorkID           string
	Kind             Kind
	State            State
	CreatedAt        time.Time
	LastEventAt      time.Time
	WorkerID         string
	SourceVolumeID   string
	SourceStorageID  string
	ArchiveStorageID string
	FolderCount      int
	VideoCount       int
	HoldCount        int
	ResultCount      int
	ChildJobCount    int
	Progress         float64
	Title            string
}

type ChildJob struct {
	Role   ChildRole
	Folder string
	Job    archiveworkspace.JobSummary
}

type View struct {
	Summary       Summary
	FolderIDs     []string
	Children      []ChildJob
	CompletedWork []archiveintakeworkspace.CompletedWorkView
}

type SnapshotReader interface {
	ListSnapshots() ([]command.Snapshot, error)
}

type JobReader interface {
	Inbox(context.Context) ([]archiveworkspace.JobSummary, error)
	Detail(context.Context, string) (archiveworkspace.JobView, error)
}

type DriveReader interface {
	Projection(context.Context) ([]archiveintakeworkspace.DriveView, error)
}

type Service struct {
	ledger SnapshotReader
	jobs   JobReader
	drives DriveReader
}

func New(ledger SnapshotReader, jobs JobReader, drives DriveReader) (*Service, error) {
	if ledger == nil || jobs == nil || drives == nil {
		return nil, ErrInvalidService
	}
	return &Service{ledger: ledger, jobs: jobs, drives: drives}, nil
}

func (service *Service) Inbox(ctx context.Context) ([]Summary, error) {
	views, err := service.project(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]Summary, 0, len(views))
	for _, view := range views {
		result = append(result, view.Summary)
	}
	return result, nil
}

func (service *Service) Detail(ctx context.Context, workID string) (View, error) {
	views, err := service.project(ctx)
	if err != nil {
		return View{}, err
	}
	for _, view := range views {
		if view.Summary.WorkID == strings.TrimSpace(workID) {
			return view, nil
		}
	}
	return View{}, ErrWorkNotFound
}

func (service *Service) project(ctx context.Context) ([]View, error) {
	snapshots, err := service.ledger.ListSnapshots()
	if err != nil {
		return nil, err
	}
	jobSummaries, err := service.jobs.Inbox(ctx)
	if err != nil {
		return nil, err
	}
	drives, err := service.drives.Projection(ctx)
	if err != nil {
		return nil, err
	}

	summaryByJob := make(map[string]archiveworkspace.JobSummary, len(jobSummaries))
	for _, summary := range jobSummaries {
		summaryByJob[summary.JobID] = summary
	}
	driveByRun := make(map[string]archiveintakeworkspace.DriveView)
	completedByRun := make(map[string][]archiveintakeworkspace.CompletedWorkView)
	for _, drive := range drives {
		if drive.ArchiveRunID != "" {
			driveByRun[drive.ArchiveRunID] = drive
		}
		for _, completed := range drive.CompletedWork {
			completedByRun[completed.ArchiveRunID] = append(completedByRun[completed.ArchiveRunID], completed)
		}
	}
	runs := make(map[string]archiverun.State)
	jobsByRun := make(map[string][]ChildJob)
	ownedJobs := make(map[string]bool)
	for _, snapshot := range snapshots {
		if run, decodeErr := archiverun.Decode(snapshot); decodeErr == nil {
			runs[run.ID] = run
		}
	}
	for _, snapshot := range snapshots {
		state, decodeErr := job.Decode(snapshot)
		if decodeErr != nil {
			continue
		}
		runID := jobArchiveRunID(state)
		if runID == "" {
			continue
		}
		if _, aggregateExists := runs[runID]; !aggregateExists {
			if _, projectedExists := driveByRun[runID]; !projectedExists {
				continue
			}
		}
		summary, exists := summaryByJob[state.ID]
		if !exists {
			continue
		}
		ownedJobs[state.ID] = true
		jobsByRun[runID] = append(jobsByRun[runID], ChildJob{
			Role: childRole(state.TemplateName), Folder: childFolder(state), Job: summary,
		})
	}

	result := make([]View, 0, len(runs)+len(jobSummaries))
	for runID, run := range runs {
		children := append([]ChildJob(nil), jobsByRun[runID]...)
		sortChildren(children)
		drive, current := driveByRun[runID]
		completed := append([]archiveintakeworkspace.CompletedWorkView(nil), completedByRun[runID]...)
		sort.Slice(completed, func(i, j int) bool { return completed[i].CompletedAt.After(completed[j].CompletedAt) })
		result = append(result, View{
			Summary:   summarizeRun(run, children, drive, current, completed),
			FolderIDs: append([]string(nil), run.FolderIDs...), Children: children, CompletedWork: completed,
		})
	}
	for runID, drive := range driveByRun {
		if _, exists := runs[runID]; exists {
			continue
		}
		children := append([]ChildJob(nil), jobsByRun[runID]...)
		sortChildren(children)
		completed := append([]archiveintakeworkspace.CompletedWorkView(nil), completedByRun[runID]...)
		sort.Slice(completed, func(i, j int) bool { return completed[i].CompletedAt.After(completed[j].CompletedAt) })
		folders := driveFolderIDs(drive)
		result = append(result, View{
			Summary: summarizeProjectedRun(runID, drive, children, completed), FolderIDs: folders,
			Children: children, CompletedWork: completed,
		})
	}
	for _, summary := range jobSummaries {
		if ownedJobs[summary.JobID] {
			continue
		}
		result = append(result, View{
			Summary:  summarizeStandalone(summary),
			Children: []ChildJob{{Role: ChildFolder, Folder: summary.TargetTitle, Job: summary}},
		})
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, right := stateRank(result[i].Summary.State), stateRank(result[j].Summary.State)
		if left != right {
			return left > right
		}
		if !result[i].Summary.LastEventAt.Equal(result[j].Summary.LastEventAt) {
			return result[i].Summary.LastEventAt.After(result[j].Summary.LastEventAt)
		}
		return result[i].Summary.WorkID < result[j].Summary.WorkID
	})
	return result, nil
}

func driveFolderIDs(drive archiveintakeworkspace.DriveView) []string {
	result := make([]string, 0, len(drive.Folders))
	for _, folder := range drive.Folders {
		if strings.TrimSpace(folder.Folder) != "" {
			result = append(result, folder.Folder)
		}
	}
	return result
}

func jobArchiveRunID(state job.State) string {
	var runID string
	for _, step := range state.Steps {
		if step.ExecutionSpec == nil || strings.TrimSpace(step.ExecutionSpec.Parameters.ArchiveRunID) == "" {
			continue
		}
		candidate := strings.TrimSpace(step.ExecutionSpec.Parameters.ArchiveRunID)
		if runID != "" && runID != candidate {
			return ""
		}
		runID = candidate
	}
	return runID
}

func childRole(template string) ChildRole {
	switch template {
	case "archive-intake-inspection":
		return ChildInspection
	case "archive-transcription":
		return ChildTranscription
	default:
		return ChildFolder
	}
}

func childFolder(state job.State) string {
	if childRole(state.TemplateName) == ChildInspection {
		return ""
	}
	for _, step := range state.Steps {
		if step.ExecutionSpec != nil && strings.TrimSpace(step.ExecutionSpec.Parameters.MediaFolder) != "" {
			return step.ExecutionSpec.Parameters.MediaFolder
		}
	}
	return state.TargetTitle
}

func sortChildren(children []ChildJob) {
	rank := map[ChildRole]int{ChildInspection: 0, ChildFolder: 1, ChildTranscription: 2}
	sort.SliceStable(children, func(i, j int) bool {
		if rank[children[i].Role] != rank[children[j].Role] {
			return rank[children[i].Role] < rank[children[j].Role]
		}
		if children[i].Folder != children[j].Folder {
			return children[i].Folder < children[j].Folder
		}
		return children[i].Job.JobID < children[j].Job.JobID
	})
}

func summarizeRun(run archiverun.State, children []ChildJob, drive archiveintakeworkspace.DriveView, current bool, completed []archiveintakeworkspace.CompletedWorkView) Summary {
	summary := Summary{
		WorkID: run.ID, Kind: ArchiveRunWork, CreatedAt: run.CreatedAt, LastEventAt: run.CreatedAt,
		WorkerID: run.WorkerID, SourceVolumeID: run.SourceVolumeID, SourceStorageID: run.SourceStorageID,
		ArchiveStorageID: run.ArchiveStorageID, FolderCount: len(run.FolderIDs), ChildJobCount: len(children),
		ResultCount: len(completed),
	}
	var totalProgress float64
	for _, child := range children {
		totalProgress += child.Job.Progress
		if child.Job.LastEventAt.After(summary.LastEventAt) {
			summary.LastEventAt = child.Job.LastEventAt
		}
	}
	if len(children) > 0 {
		summary.Progress = totalProgress / float64(len(children))
	}
	if current {
		summary.HoldCount = drive.HoldCount
		for _, folder := range drive.Folders {
			summary.VideoCount += folder.VideoCount
		}
	} else {
		for _, item := range completed {
			summary.VideoCount += item.SourceVideoCount
		}
	}
	summary.State = summarizeState(children, drive, current)
	return summary
}

func summarizeProjectedRun(runID string, drive archiveintakeworkspace.DriveView, children []ChildJob, completed []archiveintakeworkspace.CompletedWorkView) Summary {
	createdAt := drive.CatalogUpdatedAt
	for _, child := range children {
		if createdAt.IsZero() || child.Job.LastEventAt.Before(createdAt) {
			createdAt = child.Job.LastEventAt
		}
	}
	summary := Summary{
		WorkID: runID, Kind: ArchiveRunWork, CreatedAt: createdAt, LastEventAt: createdAt,
		WorkerID: drive.WorkerID, SourceVolumeID: drive.SourceVolumeID, SourceStorageID: drive.SourceStorageID,
		ArchiveStorageID: drive.ArchiveStorageID, FolderCount: drive.FolderCount, HoldCount: drive.HoldCount,
		ChildJobCount: len(children), ResultCount: len(completed),
	}
	if summary.FolderCount == 0 {
		summary.FolderCount = len(drive.Folders)
	}
	var totalProgress float64
	for _, child := range children {
		totalProgress += child.Job.Progress
		if child.Job.LastEventAt.After(summary.LastEventAt) {
			summary.LastEventAt = child.Job.LastEventAt
		}
	}
	if len(children) > 0 {
		summary.Progress = totalProgress / float64(len(children))
	}
	for _, folder := range drive.Folders {
		summary.VideoCount += folder.VideoCount
	}
	summary.State = summarizeState(children, drive, true)
	return summary
}

func summarizeStandalone(job archiveworkspace.JobSummary) Summary {
	return Summary{
		WorkID: job.JobID, Kind: StandaloneJob, State: stateFromJob(job), CreatedAt: job.LastEventAt,
		LastEventAt: job.LastEventAt, WorkerID: job.WorkerID, ChildJobCount: 1, Progress: job.Progress, Title: job.TargetTitle,
	}
}

func summarizeState(children []ChildJob, drive archiveintakeworkspace.DriveView, current bool) State {
	for _, child := range children {
		if child.Job.StepState == workflow.Failed || child.Job.ErrorClass != "" {
			return StateFailed
		}
	}
	if current {
		if drive.HoldCount > 0 || drive.ArchiveRunState == "TRANSCRIPTION_READY" || driveNeedsReview(drive) {
			return StateAttention
		}
		if drive.ArchiveRunState == "COMPLETED" {
			return StateCompleted
		}
		if drive.ArchiveRunState == "STAGE_ONE_RUNNING" || drive.ArchiveRunState == "STAGE_TWO_RUNNING" {
			return StateActive
		}
	}
	state := StateCompleted
	for _, child := range children {
		candidate := stateFromJob(child.Job)
		switch candidate {
		case StateFailed:
			return StateFailed
		case StateAttention:
			state = StateAttention
		case StateActive:
			if state != StateAttention {
				state = StateActive
			}
		case StateQueued:
			if state == StateCompleted {
				state = StateQueued
			}
		}
	}
	if len(children) == 0 {
		return StateQueued
	}
	return state
}

func stateFromJob(summary archiveworkspace.JobSummary) State {
	if summary.StepState == workflow.Failed || summary.ErrorClass != "" {
		return StateFailed
	}
	if summary.NeedsHumanIntervention || (summary.StepState == workflow.Waiting && humanWait(summary.WaitReason)) {
		return StateAttention
	}
	switch summary.StepState {
	case workflow.Reserved, workflow.Running, workflow.Verifying:
		return StateActive
	case workflow.Completed, workflow.Cancelled:
		return StateCompleted
	default:
		return StateQueued
	}
}

func humanWait(reason vocabulary.WaitReason) bool {
	return reason == vocabulary.WaitInput || reason == vocabulary.WaitApproval || reason == vocabulary.WaitDevice
}

func driveNeedsReview(drive archiveintakeworkspace.DriveView) bool {
	for _, folder := range drive.Folders {
		for _, quality := range folder.TranscriptQualities {
			if quality.State == "REVIEW_RECOMMENDED" {
				return true
			}
		}
	}
	return false
}

func stateRank(state State) int {
	switch state {
	case StateFailed:
		return 5
	case StateAttention:
		return 4
	case StateActive:
		return 3
	case StateQueued:
		return 2
	case StateCompleted:
		return 1
	default:
		return 0
	}
}
