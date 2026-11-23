// Package archivework projects the user's persistent work units. ArchiveRun
// is the parent work unit for archive intake; its inspection, folder and STT
// Jobs remain independently scheduled child executions. Jobs that do not
// belong to an ArchiveRun remain standalone work units.
package archivework

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"time"

	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/core/archiverun"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/pipeline"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

var (
	ErrInvalidService = errors.New("archive work service is invalid")
	ErrWorkNotFound   = errors.New("archive work was not found")
	ErrJobNotFound    = errors.New("archive work job was not found")
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
	Revision         uint64
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
	// ReplacementWorkID is the next ArchiveRun that took over every failed
	// folder of this run. The failed outcome remains historical truth, while
	// this reference says there is no longer an unresolved handoff action.
	ReplacementWorkID string
}

type ChildJob struct {
	Role   ChildRole
	Folder string
	Job    archiveworkspace.JobSummary
}

// Failure is the parent-level failure evidence for one child Job. It keeps
// the structured class and safe log reference already stored on the Job; it
// never copies raw process output into the work projection.
type Failure struct {
	JobID        string
	Folder       string
	StepID       string
	ErrorClass   string
	LogReference string
}

type View struct {
	Summary       Summary
	FolderIDs     []string
	Children      []ChildJob
	CompletedWork []archiveintakeworkspace.CompletedWorkView
	Failures      []Failure
}

// VideoInfo is the persisted, observed source-video information for one Job.
// It deliberately contains no Storage root or mount path.
type VideoInfo struct {
	OriginalAssetID      string
	OriginalName         string
	RelativePath         string
	Checksum             string
	SourceCreatedAt      time.Time
	DurationMillis       int64
	SizeBytes            int64
	Container            string
	BitRateBPS           int64
	VideoCodec           string
	VideoProfile         string
	Width                int
	Height               int
	FrameRateNumerator   int64
	FrameRateDenominator int64
	VideoPixelFormat     string
	VideoBitRateBPS      int64
	AudioCodec           string
	AudioProfile         string
	AudioBitRateBPS      int64
	AudioSampleRateHz    int
	AudioChannels        int
	AudioChannelLayout   string
	ProfileReference     string
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

// Media returns video facts only when a caller asks for one addressable Job.
// The ordinary work list and detail projections therefore stay small even
// when an ArchiveRun contains many source videos.
func (service *Service) Media(ctx context.Context, jobID string) ([]VideoInfo, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil, ErrJobNotFound
	}
	snapshots, err := service.ledger.ListSnapshots()
	if err != nil {
		return nil, err
	}
	for _, snapshot := range snapshots {
		state, decodeErr := job.Decode(snapshot)
		if decodeErr != nil || state.ID != jobID {
			continue
		}
		media := mediaFromJob(state)
		runID := jobArchiveRunID(state)
		if runID == "" {
			return media, nil
		}
		drives, projectionErr := service.drives.Projection(ctx)
		if projectionErr != nil {
			if len(media) > 0 {
				return media, nil
			}
			return nil, projectionErr
		}
		for _, drive := range drives {
			if drive.ArchiveRunID != runID {
				continue
			}
			assets := make([]archiveintakeworkspace.ClassificationAssetView, 0)
			for _, folder := range drive.Folders {
				assets = append(assets, folder.ClassificationAssets...)
			}
			observed := mediaFromClassificationAssets(assets)
			if len(media) == 0 && state.TemplateName == "archive-intake-inspection" {
				return observed, nil
			}
			return enrichVideoInfo(media, observed), nil
		}
		return media, nil
	}
	return nil, ErrJobNotFound
}

func mediaFromJob(state job.State) []VideoInfo {
	sources := make([]execution.InputReference, 0)
	for _, step := range state.Steps {
		if step.ExecutionSpec != nil {
			sources = append(sources, step.ExecutionSpec.Parameters.SourceInputs...)
		}
	}
	if len(sources) == 0 {
		for _, step := range state.Steps {
			if step.ExecutionSpec == nil {
				continue
			}
			for _, input := range step.ExecutionSpec.Inputs {
				if input.VideoCodec != "" || input.Width > 0 || input.Height > 0 || input.DurationMillis > 0 || input.Kind == execution.OutputRendition {
					sources = append(sources, input)
				}
			}
		}
	}
	seen := make(map[string]bool, len(sources))
	result := make([]VideoInfo, 0, len(sources))
	for _, input := range sources {
		identity := strings.TrimSpace(input.OriginalAssetID)
		if identity == "" {
			identity = strings.TrimSpace(input.RelativePath)
		}
		if identity == "" || seen[identity] {
			continue
		}
		seen[identity] = true
		name := strings.TrimSpace(input.OriginalName)
		if name == "" {
			name = path.Base(input.RelativePath)
		}
		result = append(result, VideoInfo{
			OriginalAssetID: input.OriginalAssetID, OriginalName: name, RelativePath: input.RelativePath,
			Checksum: input.Checksum, SourceCreatedAt: input.SourceCreatedAt, DurationMillis: input.DurationMillis,
			SizeBytes: input.SourceSizeBytes, Container: input.SourceContainer, BitRateBPS: input.SourceBitRateBPS,
			VideoCodec: input.VideoCodec, VideoProfile: input.VideoProfile, Width: input.Width, Height: input.Height,
			FrameRateNumerator: input.FrameRateNumerator, FrameRateDenominator: input.FrameRateDenominator,
			VideoPixelFormat: input.VideoPixelFormat, VideoBitRateBPS: input.VideoBitRateBPS,
			AudioCodec: input.AudioCodec, AudioProfile: input.AudioProfile, AudioBitRateBPS: input.AudioBitRateBPS,
			AudioSampleRateHz: input.AudioSampleRateHz, AudioChannels: input.AudioChannels,
			AudioChannelLayout: input.AudioChannelLayout, ProfileReference: input.ProfileReference,
		})
	}
	sortVideoInfo(result)
	return result
}

func mediaFromClassificationAssets(assets []archiveintakeworkspace.ClassificationAssetView) []VideoInfo {
	seen := make(map[string]bool, len(assets))
	result := make([]VideoInfo, 0, len(assets))
	for _, asset := range assets {
		identity := strings.TrimSpace(asset.OriginalAssetID)
		if identity == "" {
			identity = strings.TrimSpace(asset.RelativePath)
		}
		if identity == "" || seen[identity] {
			continue
		}
		seen[identity] = true
		info := VideoInfo{
			OriginalAssetID: asset.OriginalAssetID, OriginalName: path.Base(asset.RelativePath), RelativePath: asset.RelativePath,
			Checksum: asset.Checksum, DurationMillis: asset.DurationMillis, SizeBytes: asset.SizeBytes,
			Container: strings.Join(asset.Containers, ","), BitRateBPS: asset.BitRate,
		}
		if len(asset.VideoStreams) > 0 {
			video := asset.VideoStreams[0]
			info.VideoCodec, info.VideoProfile, info.Width, info.Height = video.Codec, video.Profile, video.Width, video.Height
			info.FrameRateNumerator, info.FrameRateDenominator = video.FrameRate.Numerator, video.FrameRate.Denominator
			info.VideoPixelFormat, info.VideoBitRateBPS = video.PixelFormat, video.BitRate
		}
		if len(asset.AudioStreams) > 0 {
			audio := asset.AudioStreams[0]
			info.AudioCodec, info.AudioProfile, info.AudioBitRateBPS = audio.Codec, audio.Profile, audio.BitRate
			info.AudioSampleRateHz, info.AudioChannels, info.AudioChannelLayout = audio.SampleRateHz, audio.Channels, audio.ChannelLayout
		}
		result = append(result, info)
	}
	sortVideoInfo(result)
	return result
}

func enrichVideoInfo(media, observed []VideoInfo) []VideoInfo {
	byIdentity := make(map[string]VideoInfo, len(observed)*2)
	for _, item := range observed {
		if item.OriginalAssetID != "" {
			byIdentity["asset:"+item.OriginalAssetID] = item
		}
		if item.RelativePath != "" {
			byIdentity["path:"+item.RelativePath] = item
		}
	}
	result := append([]VideoInfo(nil), media...)
	for index, item := range result {
		match, found := byIdentity["asset:"+item.OriginalAssetID]
		if !found {
			match, found = byIdentity["path:"+item.RelativePath]
		}
		if !found {
			continue
		}
		if !item.SourceCreatedAt.IsZero() {
			match.SourceCreatedAt = item.SourceCreatedAt
		}
		if item.OriginalName != "" {
			match.OriginalName = item.OriginalName
		}
		if item.ProfileReference != "" {
			match.ProfileReference = item.ProfileReference
		}
		result[index] = match
	}
	sortVideoInfo(result)
	return result
}

func sortVideoInfo(result []VideoInfo) {
	sort.Slice(result, func(i, j int) bool {
		if result[i].RelativePath != result[j].RelativePath {
			return result[i].RelativePath < result[j].RelativePath
		}
		return result[i].OriginalAssetID < result[j].OriginalAssetID
	})
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
			Failures: failuresFromChildren(children),
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
			Children: children, CompletedWork: completed, Failures: failuresFromChildren(children),
		})
	}
	for _, summary := range jobSummaries {
		if ownedJobs[summary.JobID] {
			continue
		}
		result = append(result, View{
			Summary:  summarizeStandalone(summary),
			Children: []ChildJob{{Role: ChildFolder, Folder: summary.TargetTitle, Job: summary}},
			Failures: failuresFromChildren([]ChildJob{{Role: ChildFolder, Folder: summary.TargetTitle, Job: summary}}),
		})
	}
	markReplacementRuns(result)
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

// markReplacementRuns separates a run's immutable failed outcome from the
// question "does a person still need to do anything?". A later ArchiveRun on
// the same source volume takes over a failed run when its fixed scope contains
// every failed folder. The earliest matching run is the durable handoff link;
// if that replacement also fails it becomes its own unresolved work item.
func markReplacementRuns(views []View) {
	for index := range views {
		failed := failedFolders(views[index])
		if views[index].Summary.Kind != ArchiveRunWork || views[index].Summary.State != StateFailed || len(failed) == 0 {
			continue
		}
		candidate := -1
		for other := range views {
			if other == index || views[other].Summary.Kind != ArchiveRunWork ||
				views[other].Summary.SourceVolumeID == "" || views[other].Summary.SourceVolumeID != views[index].Summary.SourceVolumeID ||
				!views[other].Summary.CreatedAt.After(views[index].Summary.CreatedAt) || !containsFolders(views[other].FolderIDs, failed) {
				continue
			}
			if candidate < 0 || views[other].Summary.CreatedAt.Before(views[candidate].Summary.CreatedAt) {
				candidate = other
			}
		}
		if candidate >= 0 {
			views[index].Summary.ReplacementWorkID = views[candidate].Summary.WorkID
		}
	}
}

func failedFolders(view View) []string {
	set := make(map[string]bool)
	for _, child := range unresolvedChildren(view.Children) {
		if child.Job.StepState != workflow.Failed && child.Job.ErrorClass == "" {
			continue
		}
		if strings.TrimSpace(child.Folder) != "" {
			set[child.Folder] = true
			continue
		}
		// Inspection failed before a folder Job existed. A replacement must own
		// the whole fixed scope rather than merely sharing the same drive.
		for _, folder := range view.FolderIDs {
			set[folder] = true
		}
	}
	result := make([]string, 0, len(set))
	for folder := range set {
		result = append(result, folder)
	}
	sort.Strings(result)
	return result
}

func containsFolders(scope, required []string) bool {
	available := make(map[string]bool, len(scope))
	for _, folder := range scope {
		available[folder] = true
	}
	for _, folder := range required {
		if !available[folder] {
			return false
		}
	}
	return len(required) > 0
}

func failuresFromChildren(children []ChildJob) []Failure {
	result := make([]Failure, 0)
	for _, child := range unresolvedChildren(children) {
		if child.Job.StepState != workflow.Failed && child.Job.ErrorClass == "" {
			continue
		}
		result = append(result, Failure{
			JobID: child.Job.JobID, Folder: child.Folder, StepID: child.Job.CurrentStepID,
			ErrorClass: child.Job.ErrorClass, LogReference: child.Job.LogReference,
		})
	}
	return result
}

func unresolvedChildren(children []ChildJob) []ChildJob {
	recovered := make(map[string]bool)
	for _, child := range children {
		if child.Job.RecoverySource == nil || child.Job.RecoverySource.JobID == "" ||
			child.Job.StepState == workflow.Failed || child.Job.ErrorClass != "" {
			continue
		}
		recovered[child.Job.RecoverySource.JobID] = true
	}
	result := make([]ChildJob, 0, len(children))
	for _, child := range children {
		if recovered[child.Job.JobID] && (child.Job.StepState == workflow.Failed || child.Job.ErrorClass != "") {
			continue
		}
		result = append(result, child)
	}
	return result
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
	case "archive-transcription", pipeline.ArchiveTranscriptionRecoveryName:
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
	effectiveChildren := unresolvedChildren(children)
	summary := Summary{
		WorkID: run.ID, Revision: run.Revision, Kind: ArchiveRunWork, CreatedAt: run.CreatedAt, LastEventAt: run.CreatedAt,
		WorkerID: run.WorkerID, SourceVolumeID: run.SourceVolumeID, SourceStorageID: run.SourceStorageID,
		ArchiveStorageID: run.ArchiveStorageID, FolderCount: len(run.FolderIDs), ChildJobCount: len(children),
		ResultCount: len(completed),
	}
	var totalProgress float64
	for _, child := range effectiveChildren {
		totalProgress += child.Job.Progress
		if child.Job.LastEventAt.After(summary.LastEventAt) {
			summary.LastEventAt = child.Job.LastEventAt
		}
	}
	if len(effectiveChildren) > 0 {
		summary.Progress = totalProgress / float64(len(effectiveChildren))
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
	summary.State = summarizeState(effectiveChildren, drive, current)
	return summary
}

func summarizeProjectedRun(runID string, drive archiveintakeworkspace.DriveView, children []ChildJob, completed []archiveintakeworkspace.CompletedWorkView) Summary {
	effectiveChildren := unresolvedChildren(children)
	createdAt := drive.CatalogUpdatedAt
	for _, child := range effectiveChildren {
		if createdAt.IsZero() || child.Job.LastEventAt.Before(createdAt) {
			createdAt = child.Job.LastEventAt
		}
	}
	summary := Summary{
		WorkID: runID, Revision: drive.ArchiveRunRevision, Kind: ArchiveRunWork, CreatedAt: createdAt, LastEventAt: createdAt,
		WorkerID: drive.WorkerID, SourceVolumeID: drive.SourceVolumeID, SourceStorageID: drive.SourceStorageID,
		ArchiveStorageID: drive.ArchiveStorageID, FolderCount: drive.FolderCount, HoldCount: drive.HoldCount,
		ChildJobCount: len(children), ResultCount: len(completed),
	}
	if summary.FolderCount == 0 {
		summary.FolderCount = len(drive.Folders)
	}
	var totalProgress float64
	for _, child := range effectiveChildren {
		totalProgress += child.Job.Progress
		if child.Job.LastEventAt.After(summary.LastEventAt) {
			summary.LastEventAt = child.Job.LastEventAt
		}
	}
	if len(effectiveChildren) > 0 {
		summary.Progress = totalProgress / float64(len(effectiveChildren))
	}
	for _, folder := range drive.Folders {
		summary.VideoCount += folder.VideoCount
	}
	summary.State = summarizeState(effectiveChildren, drive, true)
	return summary
}

func summarizeStandalone(job archiveworkspace.JobSummary) Summary {
	return Summary{
		WorkID: job.JobID, Revision: job.Revision, Kind: StandaloneJob, State: stateFromJob(job), CreatedAt: job.LastEventAt,
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
		if drive.HoldCount > 0 || driveNeedsReview(drive) {
			return StateAttention
		}
		if drive.ArchiveRunState == "COMPLETED" {
			return StateCompleted
		}
		if drive.ArchiveRunState == "STAGE_ONE_RUNNING" || drive.ArchiveRunState == "TRANSCRIPTION_READY" || drive.ArchiveRunState == "STAGE_TWO_RUNNING" {
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
