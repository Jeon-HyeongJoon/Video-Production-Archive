package archiveintakeworkspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"sort"
	"strings"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/archiveintake"
	"videoarchive/internal/core/archiverun"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/storage"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

var ErrInvalidConfiguration = errors.New("archive intake workspace configuration is invalid")

type projectionDriveState struct {
	view              DriveView
	folders           map[string]FolderView
	discoveredFolders map[string]FolderView
}

type Store interface {
	Receipts(context.Context) ([]organization.ArchiveOrganizationReceipt, error)
	LatestDecisionForArchiveRun(context.Context, string, string) (organization.OrganizationDecision, error)
	FindOverrideReceipt(context.Context, string) (organization.OrganizationDecision, string, bool, error)
	AppendOverride(context.Context, string, string, string, organization.OrganizationDecision) error
}

type SnapshotLoader interface {
	Load(string) (command.Snapshot, error)
}

type SnapshotLister interface {
	ListSnapshots() ([]command.Snapshot, error)
}

type ArchiveStateReader interface {
	PackageState(context.Context, string, string) (archive.PackageState, error)
}

type IntakeSessionReader interface {
	Session(context.Context, string) (archiveintake.Session, error)
	ListSessions(context.Context) ([]archiveintake.Session, error)
}

type CandidateLister interface {
	ListCandidates(context.Context) ([]archiveintake.Candidate, error)
}

type StorageLister interface {
	ListAccess(context.Context) ([]storage.Access, error)
}

type WorkerStatusReader interface {
	Statuses(time.Time) []worker.Status
}

type Option func(*Service)

func WithArchiveStateReader(reader ArchiveStateReader) Option {
	return func(service *Service) { service.archiveStates = reader }
}

func WithIntakeSessions(reader IntakeSessionReader) Option {
	return func(service *Service) { service.intakeSessions = reader }
}

func WithInventory(candidates CandidateLister, storages StorageLister, workers WorkerStatusReader, now func() time.Time) Option {
	return func(service *Service) {
		service.candidates = candidates
		service.storages = storages
		service.workers = workers
		service.now = now
	}
}

func WithResolvedFolderStarter(starter ResolvedFolderStarter) Option {
	return func(service *Service) { service.resolvedFolders = starter }
}

type Service struct {
	store           Store
	jobs            SnapshotLoader
	archiveStates   ArchiveStateReader
	intakeSessions  IntakeSessionReader
	candidates      CandidateLister
	storages        StorageLister
	workers         WorkerStatusReader
	resolvedFolders ResolvedFolderStarter
	now             func() time.Time
}

func New(store Store, jobs SnapshotLoader, options ...Option) (*Service, error) {
	if store == nil || jobs == nil {
		return nil, ErrInvalidConfiguration
	}
	service := &Service{store: store, jobs: jobs}
	for _, option := range options {
		if option != nil {
			option(service)
		}
	}
	return service, nil
}

func (service *Service) Projection(ctx context.Context) ([]DriveView, error) {
	receipts, err := service.store.Receipts(ctx)
	if err != nil {
		return nil, err
	}
	drives := make(map[string]*projectionDriveState)
	receiptsByRunID := make(map[string]organization.ArchiveOrganizationReceipt, len(receipts))
	for _, receipt := range receipts {
		receiptsByRunID[receipt.ID] = receipt
	}
	archiveRunsByDrive := make(map[string]archiverun.State)
	if snapshots, listErr := listSnapshots(service.jobs); listErr != nil {
		return nil, listErr
	} else {
		for _, snapshot := range snapshots {
			run, decodeErr := archiverun.Decode(snapshot)
			if decodeErr != nil {
				continue
			}
			driveID := run.SourceVolumeID
			current, found := archiveRunsByDrive[driveID]
			if !found || run.CreatedAt.After(current.CreatedAt) ||
				(run.CreatedAt.Equal(current.CreatedAt) && run.ID > current.ID) {
				archiveRunsByDrive[driveID] = run
			}
		}
	}
	selectedReceipts := make([]organization.ArchiveOrganizationReceipt, 0, len(archiveRunsByDrive))
	selectedDrives := make(map[string]bool, len(archiveRunsByDrive))
	for driveID, run := range archiveRunsByDrive {
		selectedDrives[driveID] = true
		if receipt, found := receiptsByRunID[run.ID]; found {
			selectedReceipts = append(selectedReceipts, receipt)
		}
	}
	for _, receipt := range latestArchiveRunReceipts(receipts) {
		if !selectedDrives[receipt.WorkspaceDriveID] {
			selectedReceipts = append(selectedReceipts, receipt)
		}
	}
	sort.Slice(selectedReceipts, func(i, j int) bool {
		return selectedReceipts[i].WorkspaceDriveID < selectedReceipts[j].WorkspaceDriveID
	})
	transcriptionJobs, err := service.transcriptionJobs()
	if err != nil {
		return nil, err
	}
	organizationJobs, err := service.organizationJobs()
	if err != nil {
		return nil, err
	}
	completedByDrive, err := service.completedWork(ctx, receipts, organizationJobs)
	if err != nil {
		return nil, err
	}
	for _, receipt := range selectedReceipts {
		state := drives[receipt.WorkspaceDriveID]
		if state == nil {
			state = &projectionDriveState{view: DriveView{
				WorkspaceDriveID: receipt.WorkspaceDriveID, ArchiveRunID: receipt.ID, SessionID: receipt.SessionID,
				WorkerID: receipt.WorkerID, State: "IN_PROGRESS", InputRevision: receipt.InputRevision,
				FileCount: len(receipt.Files), CatalogRowCount: len(receipt.Files), CatalogUpdatedAt: receipt.CompletedAt,
			}, folders: make(map[string]FolderView), discoveredFolders: make(map[string]FolderView)}
			drives[receipt.WorkspaceDriveID] = state
		} else if receipt.InputRevision >= state.view.InputRevision {
			state.view.ArchiveRunID = receipt.ID
			state.view.SessionID = receipt.SessionID
			state.view.WorkerID = receipt.WorkerID
			state.view.InputRevision = receipt.InputRevision
			state.view.FileCount = len(receipt.Files)
			state.view.CatalogRowCount = len(receipt.Files)
			state.view.CatalogUpdatedAt = receipt.CompletedAt
		}
		service.projectArchiveRun(&state.view, receipt)
		if service.intakeSessions != nil {
			intakeSession, sessionErr := service.intakeSessions.Session(ctx, receipt.SessionID)
			if sessionErr != nil {
				return nil, sessionErr
			}
			state.view.SourceVolumeID = intakeSession.VolumeID
			if state.view.SourceStorageID == "" {
				state.view.SourceStorageID = intakeSession.StorageID
			}
		}
		for _, coordinated := range receipt.Folders {
			decision, err := service.store.LatestDecisionForArchiveRun(ctx, receipt.ID, coordinated.Folder)
			if err != nil {
				return nil, err
			}
			folderJobID := coordinated.JobID
			if folderJobID == "" {
				if resolved, found := organizationJobs[receipt.ID+"\x00"+coordinated.Folder]; found {
					folderJobID = resolved.ID
				}
			}
			view := FolderView{
				Folder: coordinated.Folder, DecisionID: decision.ID, InputRevision: decision.InputRevision,
				DecisionSequence: decision.Sequence, Disposition: decision.Disposition,
				ProfileReference:     decision.ProfileReference,
				HoldReasons:          append([]vocabulary.OrganizationHoldReason(nil), decision.HoldReasons...),
				WorkOptions:          workOptions(decision),
				ClassificationAssets: classificationAssets(decision),
				VideoCount:           folderVideoCount(receipt.Files, coordinated.Folder),
				EstimatedOutputBytes: estimatedOutputBytes(decision),
				JobID:                folderJobID,
			}
			classificationState := workflow.Completed
			classificationWaitReason := vocabulary.WaitReason("")
			if decision.Disposition == vocabulary.OrganizationHold {
				classificationState = workflow.Waiting
				classificationWaitReason = vocabulary.WaitInput
			}
			view.Stages = append(view.Stages, StageView{
				ID: "classification", State: classificationState, WaitReason: classificationWaitReason,
				CompletedCount: completedItemCount(classificationState, 0, view.VideoCount), TotalCount: view.VideoCount,
			})
			if view.JobID != "" {
				snapshot, err := service.jobs.Load(view.JobID)
				if err != nil {
					return nil, err
				}
				jobState, err := job.Decode(snapshot)
				if err != nil {
					return nil, err
				}
				projectCatalogUpdatedAt(&state.view, jobState)
				view.CurrentStepID = jobState.CurrentStepID
				view.StepState = currentStepState(jobState)
				view.Stages = append(view.Stages, projectStageOne(jobState, view.VideoCount)...)
				view.ArchivePackageID, view.ArchiveState = storedPackage(jobState)
				projectArchivePresentation(&view, decision)
				if view.ArchivePackageID != "" && service.archiveStates != nil {
					packageState, stateErr := service.archiveStates.PackageState(ctx, state.view.ArchiveStorageID, view.ArchivePackageID)
					if stateErr != nil {
						return nil, stateErr
					}
					if packageState.Valid() {
						view.ArchiveState = packageState
					}
				}
				view.CanAddTranscription = view.ArchiveState == archive.PackageStored && organization.TranscriptionEligible(decision, true)
			} else if decision.Disposition == vocabulary.OrganizationHold {
				view.CurrentStepID = "resolve_classification"
				view.StepState = workflow.Waiting
			} else {
				view.CurrentStepID = "job_pending"
				view.StepState = workflow.Queued
				view.Stages = append(view.Stages, StageView{ID: "job_pending", State: workflow.Queued, TotalCount: view.VideoCount})
			}
			if stageTwo, ok := transcriptionJobs[receipt.ID+"\x00"+coordinated.Folder]; ok {
				for _, stageTwoJob := range stageTwo {
					projectCatalogUpdatedAt(&state.view, stageTwoJob)
					view.TranscriptQualities = append(view.TranscriptQualities, transcriptQualities(stageTwoJob)...)
				}
				view.TranscriptionJobID = stageTwo[0].ID
				view.CurrentStepID, view.StepState = aggregateStageTwoState(stageTwo)
				view.Stages = append(view.Stages, projectStageTwoJobs(stageTwo)...)
				sort.Slice(view.TranscriptQualities, func(i, j int) bool {
					return view.TranscriptQualities[i].TranscriptID < view.TranscriptQualities[j].TranscriptID
				})
				view.CanAddTranscription = false
			}
			view.Stages = orderedStages(view.Stages)
			previous, exists := state.folders[coordinated.Folder]
			if !exists || view.InputRevision > previous.InputRevision ||
				(view.InputRevision == previous.InputRevision && view.DecisionSequence > previous.DecisionSequence) {
				state.folders[coordinated.Folder] = view
			}
		}
	}
	for driveID, run := range archiveRunsByDrive {
		if _, found := receiptsByRunID[run.ID]; found {
			continue
		}
		state := drives[driveID]
		if state == nil {
			state = &projectionDriveState{folders: make(map[string]FolderView), discoveredFolders: make(map[string]FolderView)}
			drives[driveID] = state
		} else {
			state.folders = make(map[string]FolderView)
		}
		state.view.WorkspaceDriveID = driveID
		state.view.ArchiveRunID = run.ID
		state.view.ArchiveRunRevision = run.Revision
		state.view.WorkerID = run.WorkerID
		state.view.SourceVolumeID = driveID
		state.view.SourceStorageID = run.SourceStorageID
		state.view.ArchiveStorageID = run.ArchiveStorageID
		state.view.InputRevision = run.FolderDiscoveryRevision
		state.view.State = "IN_PROGRESS"
		for _, folderID := range run.FolderIDs {
			state.folders[folderID] = FolderView{Folder: folderID, InputRevision: run.FolderDiscoveryRevision}
		}
	}
	if err := service.mergeInventory(ctx, drives); err != nil {
		return nil, err
	}
	result := make([]DriveView, 0, len(drives))
	for _, state := range drives {
		if !state.view.ConnectionState.Valid() {
			state.view.ConnectionState = vocabulary.VolumeUnknown
		}
		if state.view.ConnectionWorkerID == "" {
			state.view.ConnectionWorkerID = state.view.WorkerID
		}
		state.view.CompletedWork = append(state.view.CompletedWork, completedByDrive[state.view.WorkspaceDriveID]...)
		discoveredFolderNames := make([]string, 0, len(state.discoveredFolders))
		for folder := range state.discoveredFolders {
			discoveredFolderNames = append(discoveredFolderNames, folder)
		}
		sort.Strings(discoveredFolderNames)
		for _, folder := range discoveredFolderNames {
			state.view.DiscoveredFolders = append(state.view.DiscoveredFolders, state.discoveredFolders[folder])
		}
		state.view.DiscoveredFolderCount = len(state.view.DiscoveredFolders)
		folderNames := make([]string, 0, len(state.folders))
		for folder := range state.folders {
			folderNames = append(folderNames, folder)
		}
		sort.Strings(folderNames)
		for _, folder := range folderNames {
			state.view.Folders = append(state.view.Folders, state.folders[folder])
		}
		state.view.FolderCount = len(state.view.Folders)
		state.view.StageOneRemaining, state.view.StageTwoRemaining, state.view.HoldCount,
			state.view.EligibleTranscriptionCount = summarizeFolders(state.view.Folders)
		if state.view.ArchiveRunID != "" {
			state.view.ArchiveRunState = archiveRunState(state.view)
			state.view.State = driveStateName(state.view)
			if state.view.FolderDiscoveryRevision != 0 && state.view.FolderDiscoveryRevision != state.view.InputRevision {
				state.view.State = "CONTENT_CHANGED"
			}
		} else {
			state.view.State = "UNCLASSIFIED"
		}
		result = append(result, state.view)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].WorkspaceDriveID < result[j].WorkspaceDriveID })
	return result, nil
}

func (service *Service) completedWork(
	ctx context.Context,
	receipts []organization.ArchiveOrganizationReceipt,
	organizationJobs map[string]job.State,
) (map[string][]CompletedWorkView, error) {
	result := make(map[string][]CompletedWorkView)
	seenPackages := make(map[string]bool)
	for _, receipt := range receipts {
		for _, coordinated := range receipt.Folders {
			decision, err := service.store.LatestDecisionForArchiveRun(ctx, receipt.ID, coordinated.Folder)
			if err != nil {
				return nil, err
			}
			jobState, found := organizationJobs[receipt.ID+"\x00"+coordinated.Folder]
			if coordinated.JobID != "" && (!found || jobState.ID != coordinated.JobID) {
				snapshot, loadErr := service.jobs.Load(coordinated.JobID)
				if loadErr != nil {
					return nil, loadErr
				}
				jobState, loadErr = job.Decode(snapshot)
				if loadErr != nil {
					return nil, loadErr
				}
				found = true
			}
			if !found || !completedJob(jobState) {
				continue
			}
			for _, step := range jobState.Steps {
				if step.Workflow.ID != "organize" || step.Workflow.State != workflow.Completed || step.ExecutionSpec == nil || step.OutputRecord == nil {
					continue
				}
				targets := make(map[string]execution.OutputTarget, len(step.ExecutionSpec.Outputs))
				for _, target := range step.ExecutionSpec.Outputs {
					targets[target.ID] = target
				}
				for _, output := range step.OutputRecord.Outputs {
					if output.Kind != execution.OutputOrganizedMedia || output.ID == "" || seenPackages[output.ID] {
						continue
					}
					originalNames, sourceCount := completedWorkSources(decision, targets[output.ID].OriginalAssetID)
					presentation, presentationErr := archive.PresentPackage(output.ID, decision.Folder, originalNames)
					if presentationErr != nil {
						continue
					}
					packageState := archive.PackageStored
					if service.archiveStates != nil {
						storedState, stateErr := service.archiveStates.PackageState(ctx, output.StorageID, output.ID)
						if stateErr != nil {
							return nil, stateErr
						}
						if storedState.Valid() {
							packageState = storedState
						}
					}
					seenPackages[output.ID] = true
					result[receipt.WorkspaceDriveID] = append(result[receipt.WorkspaceDriveID], CompletedWorkView{
						ArchiveRunID: receipt.ID, Folder: decision.Folder, JobID: jobState.ID,
						Disposition: decision.Disposition, ProfileReference: decision.ProfileReference,
						SourceVideoCount: sourceCount, ArchiveStorageID: output.StorageID,
						ArchivePackageID: output.ID, ArchiveDate: presentation.ArchiveDate,
						ArchiveDisplayName: presentation.DisplayName, ArchiveVideoFileName: presentation.VideoFileName,
						ArchiveRelativePath: completedWorkRelativePath(output, targets[output.ID]),
						ArchiveState:        packageState, CompletedAt: step.OutputRecord.RecordedAt,
					})
				}
			}
		}
	}
	for driveID := range result {
		sort.Slice(result[driveID], func(i, j int) bool {
			left, right := result[driveID][i], result[driveID][j]
			if !left.CompletedAt.Equal(right.CompletedAt) {
				return left.CompletedAt.After(right.CompletedAt)
			}
			return left.ArchivePackageID < right.ArchivePackageID
		})
	}
	return result, nil
}

func completedWorkRelativePath(output execution.StepOutput, target execution.OutputTarget) string {
	relativePath := strings.TrimSpace(output.RelativePath)
	if relativePath == "" {
		relativePath = strings.TrimSpace(target.RelativePath)
	}
	if relativePath == "" {
		return ""
	}
	clean := path.Clean(strings.ReplaceAll(relativePath, `\`, "/"))
	if clean == "." || clean == ".." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") {
		return ""
	}
	directory := path.Dir(clean)
	if directory == "." {
		return ""
	}
	return directory
}

func completedWorkSources(decision organization.OrganizationDecision, originalAssetID string) ([]string, int) {
	if originalAssetID != "" {
		for _, input := range decision.Inputs {
			if input.OriginalAssetID == originalAssetID {
				return []string{path.Base(input.RelativePath)}, 1
			}
		}
	}
	names := make([]string, 0, len(decision.Inputs))
	for _, input := range decision.Inputs {
		names = append(names, path.Base(input.RelativePath))
	}
	return names, len(names)
}

func workOptions(decision organization.OrganizationDecision) []WorkOptionView {
	if decision.Disposition != vocabulary.OrganizationHold {
		return nil
	}
	options := organization.AvailableWorkOptions(decision)
	views := make([]WorkOptionView, 0, len(options))
	for _, option := range options {
		views = append(views, WorkOptionView{
			Disposition: option.Disposition, TargetVideoBitRateKbps: option.TargetVideoBitRateKbps,
		})
	}
	return views
}

func latestArchiveRunReceipts(receipts []organization.ArchiveOrganizationReceipt) []organization.ArchiveOrganizationReceipt {
	byDrive := make(map[string]organization.ArchiveOrganizationReceipt)
	for _, receipt := range receipts {
		current, found := byDrive[receipt.WorkspaceDriveID]
		if !found || receipt.CompletedAt.After(current.CompletedAt) ||
			(receipt.CompletedAt.Equal(current.CompletedAt) && receipt.ID > current.ID) {
			byDrive[receipt.WorkspaceDriveID] = receipt
		}
	}
	latest := make([]organization.ArchiveOrganizationReceipt, 0, len(byDrive))
	for _, receipt := range byDrive {
		latest = append(latest, receipt)
	}
	sort.Slice(latest, func(i, j int) bool { return latest[i].WorkspaceDriveID < latest[j].WorkspaceDriveID })
	return latest
}

func (service *Service) mergeInventory(ctx context.Context, drives map[string]*projectionDriveState) error {
	if service.candidates == nil || service.storages == nil || service.workers == nil || service.now == nil {
		return nil
	}
	candidates, err := service.candidates.ListCandidates(ctx)
	if err != nil {
		return err
	}
	accesses, err := service.storages.ListAccess(ctx)
	if err != nil {
		return err
	}
	projectionTime := service.now().UTC()
	statuses := service.workers.Statuses(projectionTime)
	previewDate := projectionTime.Format("2006-01-02")
	var sessions []archiveintake.Session
	if service.intakeSessions != nil {
		sessions, err = service.intakeSessions.ListSessions(ctx)
		if err != nil {
			return err
		}
	}
	statusByWorker := make(map[string]worker.Status, len(statuses))
	candidateByVolume := make(map[string]archiveintake.Candidate, len(candidates))
	observations := make(map[string]worker.StorageObservation)
	attachedVolumes := make(map[string]worker.AttachedVolumeObservation)
	for _, status := range statuses {
		statusByWorker[status.WorkerID] = status
		for _, observation := range status.Metrics.StorageObservations {
			observations[status.WorkerID+"\x00"+observation.StorageID] = observation
		}
		for _, volume := range status.Metrics.AttachedVolumes {
			attachedVolumes[status.WorkerID+"\x00"+volume.StorageID] = volume
		}
	}
	for _, candidate := range candidates {
		candidateByVolume[candidate.VolumeID] = candidate
		state := drives[candidate.VolumeID]
		if state == nil {
			state = &projectionDriveState{view: DriveView{
				WorkspaceDriveID: candidate.VolumeID, WorkerID: candidate.WorkerID,
				SourceVolumeID: candidate.VolumeID, SourceStorageID: candidate.StorageID,
				CandidateRevision: candidate.Revision, State: "UNCLASSIFIED",
			}, folders: make(map[string]FolderView), discoveredFolders: make(map[string]FolderView)}
			drives[candidate.VolumeID] = state
		}
		state.view.CandidateRevision = candidate.Revision
		var selectionSession archiveintake.Session
		for _, session := range sessions {
			if session.WorkspaceDriveID != candidate.VolumeID || session.VolumeID != candidate.VolumeID ||
				session.StorageID != candidate.StorageID || session.WorkerID != candidate.WorkerID ||
				session.CandidateRevision != candidate.Revision {
				continue
			}
			if selectionSession.ID == "" || session.SelectedAt.After(selectionSession.SelectedAt) ||
				(session.SelectedAt.Equal(selectionSession.SelectedAt) && session.ID > selectionSession.ID) {
				selectionSession = session
			}
		}
		if selectionSession.ID != "" {
			session := selectionSession
			state.view.SelectionSessionID = session.ID
			if state.view.ArchiveRunID == "" {
				state.view.SessionID = session.ID
			}
		}
		if state.view.SourceVolumeID == "" {
			state.view.SourceVolumeID = candidate.VolumeID
		}
		if state.view.SourceStorageID == "" {
			state.view.SourceStorageID = candidate.StorageID
		}
		observation, found := observations[candidate.WorkerID+"\x00"+candidate.StorageID]
		if !found {
			continue
		}
		state.view.FolderDiscoveryState = observation.DiscoveryState
		state.view.FolderDiscoveryRevision = observation.DiscoveryRevision
		state.view.DiscoveredFileCount = 0
		for _, folder := range observation.Folders {
			videos := make([]SourceVideoView, 0, len(folder.Videos))
			originalNames := make([]string, 0, len(folder.Videos))
			for _, video := range folder.Videos {
				videos = append(videos, SourceVideoView{
					RelativePath: video.RelativePath, FileName: video.FileName,
					SizeBytes: video.SizeBytes, ModifiedAt: video.ModifiedAt,
				})
				originalNames = append(originalNames, video.FileName)
			}
			view := FolderView{
				Folder: folder.Folder, InputRevision: observation.DiscoveryRevision,
				VideoCount: folder.VideoCount, EstimatedOutputBytes: folder.InputBytes, Videos: videos,
			}
			if preview, previewErr := archive.PreviewFlatPackage(previewDate, folder.Folder, originalNames, folder.VideoCount); previewErr == nil {
				view.ExpectedResult = preview
			}
			state.discoveredFolders[folder.Folder] = view
			if scoped, exists := state.folders[folder.Folder]; exists && state.view.ArchiveRunID != "" {
				scoped.VideoCount = folder.VideoCount
				scoped.EstimatedOutputBytes = folder.InputBytes
				scoped.Videos = videos
				state.folders[folder.Folder] = scoped
			}
			state.view.DiscoveredFileCount += folder.VideoCount
			if state.view.ArchiveRunID == "" {
				state.folders[folder.Folder] = view
			}
		}
		if state.view.ArchiveRunID == "" {
			state.view.InputRevision = observation.DiscoveryRevision
			state.view.FileCount = state.view.DiscoveredFileCount
		}
	}
	for _, state := range drives {
		connectionWorkerID := state.view.WorkerID
		candidate, hasCandidate := candidateByVolume[state.view.WorkspaceDriveID]
		if hasCandidate {
			connectionWorkerID = candidate.WorkerID
		}
		state.view.ConnectionWorkerID = connectionWorkerID
		state.view.ConnectionState = vocabulary.VolumeUnknown
		status, found := statusByWorker[connectionWorkerID]
		workerLive := found && liveWorkerState(status.State)
		if found {
			state.view.ConnectionObservedAt = status.Metrics.ObservedAt
			if state.view.ConnectionObservedAt.IsZero() {
				state.view.ConnectionObservedAt = status.LastHeartbeat
			}
		}
		source, sourceObserved := attachedVolumes[connectionWorkerID+"\x00"+state.view.SourceStorageID]
		sourceMatches := sourceObserved && source.VolumeID == state.view.WorkspaceDriveID
		if workerLive {
			if sourceMatches && (!hasCandidate || candidate.Connected) {
				state.view.ConnectionState = vocabulary.VolumeConnected
			} else {
				state.view.ConnectionState = vocabulary.VolumeDisconnected
			}
		}
		seenStorageIDs := make(map[string]bool)
		if sourceObserved {
			state.view.StorageCandidates = append(state.view.StorageCandidates, StorageView{
				StorageID: source.StorageID, Role: vocabulary.StorageSourceMedia,
				WorkspaceDriveID: source.VolumeID, AccessKind: vocabulary.StorageAccessAttachedVolume,
				Accessible: state.view.ConnectionState == vocabulary.VolumeConnected,
				Model:      source.Model, Label: source.Label, FileSystem: source.FileSystem,
				CapacityBytes: source.CapacityBytes, FreeBytes: source.FreeBytes,
			})
			seenStorageIDs[source.StorageID] = true
		} else if state.view.SourceStorageID != "" {
			state.view.StorageCandidates = append(state.view.StorageCandidates, StorageView{
				StorageID: state.view.SourceStorageID, Role: vocabulary.StorageSourceMedia,
				WorkspaceDriveID: state.view.WorkspaceDriveID, AccessKind: vocabulary.StorageAccessAttachedVolume,
				Accessible: false,
			})
			seenStorageIDs[state.view.SourceStorageID] = true
		}
		// Every other attached data volume is also an explicit archive target
		// candidate. The source logical volume itself is excluded by identity;
		// choosing a candidate still belongs to the human start command.
		for _, attached := range status.Metrics.AttachedVolumes {
			if seenStorageIDs[attached.ArchiveStorageID] || attached.VolumeID == state.view.WorkspaceDriveID {
				continue
			}
			state.view.StorageCandidates = append(state.view.StorageCandidates, StorageView{
				StorageID: attached.ArchiveStorageID, Role: vocabulary.StorageArchivePrimary,
				WorkspaceDriveID: attached.VolumeID, AccessKind: vocabulary.StorageAccessAttachedVolume, Accessible: workerLive,
				Model: attached.Model, Label: attached.Label, FileSystem: attached.FileSystem,
				CapacityBytes: attached.CapacityBytes, FreeBytes: attached.FreeBytes,
			})
			seenStorageIDs[attached.ArchiveStorageID] = true
		}
		for _, access := range accesses {
			if !status.Metrics.AccessibleStorageIDs[access.StorageID] ||
				seenStorageIDs[access.StorageID] ||
				(access.Role != vocabulary.StorageSourceMedia && access.Role != vocabulary.StorageArchivePrimary) {
				continue
			}
			observation, observed := observations[connectionWorkerID+"\x00"+access.StorageID]
			accessKind := observation.AccessKind
			if !accessKind.Valid() {
				accessKind = vocabulary.StorageAccessLocal
			}
			state.view.StorageCandidates = append(state.view.StorageCandidates, StorageView{
				StorageID: access.StorageID, Role: access.Role, WorkspaceDriveID: access.WorkspaceDriveID,
				AccessKind: accessKind, Accessible: workerLive && observed && observation.Connected,
				CapacityBytes: observation.CapacityBytes, FreeBytes: observation.FreeBytes,
			})
			seenStorageIDs[access.StorageID] = true
		}
		sort.Slice(state.view.StorageCandidates, func(i, j int) bool {
			return state.view.StorageCandidates[i].StorageID < state.view.StorageCandidates[j].StorageID
		})
	}
	return nil
}

func liveWorkerState(state string) bool {
	switch state {
	case "ONLINE", "BUSY", "DRAINING":
		return true
	default:
		return false
	}
}

func listSnapshots(loader SnapshotLoader) ([]command.Snapshot, error) {
	lister, ok := loader.(SnapshotLister)
	if !ok {
		return nil, nil
	}
	return lister.ListSnapshots()
}

func folderVideoCount(files []organization.CoordinatedFile, folder string) int {
	count := 0
	for _, file := range files {
		fileFolder := path.Dir(file.RelativePath)
		if fileFolder == "." {
			fileFolder = "root"
		}
		if fileFolder == folder {
			count++
		}
	}
	return count
}

func estimatedOutputBytes(decision organization.OrganizationDecision) uint64 {
	var total uint64
	if decision.Disposition == vocabulary.OrganizationAsIs {
		for _, input := range decision.Inputs {
			if input.Inspection.SizeBytes > 0 {
				total += uint64(input.Inspection.SizeBytes)
			}
		}
		return total
	}
	if decision.Disposition != vocabulary.OrganizationRenderRecordings && decision.Disposition != vocabulary.OrganizationCompileSourceSequence {
		return 0
	}
	bitRate := int64(6_000_000)
	if decision.Disposition == vocabulary.OrganizationCompileSourceSequence || strings.Contains(strings.ToLower(decision.ProfileReference), "20mbps") {
		bitRate = 20_000_000
	}
	for _, input := range decision.Inputs {
		if input.Inspection.DurationMillis > 0 {
			total += uint64(input.Inspection.DurationMillis * bitRate / 8 / 1000)
		}
	}
	return total
}

func classificationAssets(decision organization.OrganizationDecision) []ClassificationAssetView {
	assets := make([]ClassificationAssetView, 0, len(decision.Inputs))
	for _, input := range decision.Inputs {
		inspection := input.Inspection
		assets = append(assets, ClassificationAssetView{
			OriginalAssetID: input.OriginalAssetID, RelativePath: input.RelativePath,
			Checksum: inspection.OriginalChecksum, DurationMillis: inspection.DurationMillis,
			SizeBytes: inspection.SizeBytes, BitRate: inspection.BitRate,
			Containers:   append([]string(nil), inspection.Containers...),
			VideoStreams: append([]mediaanalysis.VideoStream(nil), inspection.VideoStreams...),
			AudioStreams: append([]mediaanalysis.AudioStream(nil), inspection.AudioStreams...),
		})
	}
	return assets
}

func projectArchivePresentation(view *FolderView, decision organization.OrganizationDecision) {
	if view == nil || view.ArchivePackageID == "" || len(decision.Inputs) == 0 {
		return
	}
	originalNames := make([]string, 0, len(decision.Inputs))
	for _, input := range decision.Inputs {
		originalNames = append(originalNames, path.Base(input.RelativePath))
	}
	presentation, err := archive.PresentPackage(view.ArchivePackageID, decision.Folder, originalNames)
	if err != nil {
		return
	}
	view.ArchiveDate = presentation.ArchiveDate
	view.ArchiveDisplayName = presentation.DisplayName
	view.ArchiveVideoFileName = presentation.VideoFileName
}

func (service *Service) projectArchiveRun(view *DriveView, receipt organization.ArchiveOrganizationReceipt) {
	if view == nil || receipt.ID == "" || receipt.ID != view.ArchiveRunID {
		return
	}
	view.ArchiveRunRevision = 1
	snapshot, err := service.jobs.Load(receipt.ID)
	if err != nil {
		return
	}
	archiveRun, err := archiverun.Decode(snapshot)
	if err != nil {
		return
	}
	view.ArchiveRunRevision = archiveRun.Revision
	view.SourceStorageID = archiveRun.SourceStorageID
	view.ArchiveStorageID = archiveRun.ArchiveStorageID
}

func (service *Service) transcriptionJobs() (map[string][]job.State, error) {
	result := make(map[string][]job.State)
	lister, ok := service.jobs.(SnapshotLister)
	if !ok {
		return result, nil
	}
	snapshots, err := lister.ListSnapshots()
	if err != nil {
		return nil, err
	}
	for _, snapshot := range snapshots {
		state, err := job.Decode(snapshot)
		if err != nil || state.TemplateName != "archive-transcription" {
			continue
		}
		archiveRunID, folder := jobArchiveRunAndFolder(state)
		if archiveRunID == "" || folder == "" {
			continue
		}
		key := archiveRunID + "\x00" + folder
		result[key] = append(result[key], state)
	}
	for key := range result {
		sort.Slice(result[key], func(i, j int) bool { return result[key][i].ID < result[key][j].ID })
	}
	return result, nil
}

func (service *Service) organizationJobs() (map[string]job.State, error) {
	result := make(map[string]job.State)
	lister, ok := service.jobs.(SnapshotLister)
	if !ok {
		return result, nil
	}
	snapshots, err := lister.ListSnapshots()
	if err != nil {
		return nil, err
	}
	for _, snapshot := range snapshots {
		state, err := job.Decode(snapshot)
		if err != nil || !strings.HasPrefix(state.TemplateName, "archive-intake-") {
			continue
		}
		archiveRunID, folder := jobArchiveRunAndFolder(state)
		if archiveRunID == "" || folder == "" {
			continue
		}
		key := archiveRunID + "\x00" + folder
		previous, found := result[key]
		if !found || state.Revision > previous.Revision {
			result[key] = state
		}
	}
	return result, nil
}

func jobArchiveRunAndFolder(state job.State) (string, string) {
	for _, step := range state.Steps {
		if step.ExecutionSpec == nil {
			continue
		}
		parameters := step.ExecutionSpec.Parameters
		if parameters.ArchiveRunID != "" && parameters.MediaFolder != "" {
			return parameters.ArchiveRunID, parameters.MediaFolder
		}
	}
	return "", ""
}

func currentStepState(state job.State) workflow.StepState {
	for _, step := range state.Steps {
		if step.Workflow.ID == state.CurrentStepID {
			return step.Workflow.State
		}
	}
	if completedJob(state) {
		return workflow.Completed
	}
	return workflow.Queued
}

func completedJob(state job.State) bool {
	if len(state.Steps) == 0 {
		return false
	}
	for _, step := range state.Steps {
		if step.Workflow.State != workflow.Completed {
			return false
		}
	}
	return true
}

func projectCatalogUpdatedAt(view *DriveView, state job.State) {
	for _, step := range state.Steps {
		if step.Workflow.ID == "archive_catalog" && step.Workflow.State == workflow.Completed &&
			step.LastEventAt.After(view.CatalogUpdatedAt) {
			view.CatalogUpdatedAt = step.LastEventAt
		}
	}
}

func projectStageOne(state job.State, total int) []StageView {
	stages := make([]StageView, 0, len(state.Steps))
	for _, step := range state.Steps {
		id := ""
		switch step.Workflow.ID {
		case "source_timeline":
			id = "markers"
		case "render", "sequence_render":
			id = "render"
		case "qc":
			id = "qc"
		case "organize":
			id = "archive"
		}
		if id != "" {
			stages = append(stages, stageView(id, step, total))
		}
	}
	return stages
}

func projectStageTwo(state job.State, total int) []StageView {
	stages := make([]StageView, 0, len(state.Steps))
	for _, step := range state.Steps {
		id := ""
		switch step.Workflow.ID {
		case "audio", "transcript", "subtitle", "transcript_text":
			id = step.Workflow.ID
		case "archive_package_revision":
			id = "add"
		}
		if id != "" {
			stages = append(stages, stageView(id, step, total))
		}
	}
	return stages
}

func projectStageTwoJobs(states []job.State) []StageView {
	byID := make(map[string][]StageView)
	for _, state := range states {
		for _, stage := range projectStageTwo(state, 1) {
			byID[stage.ID] = append(byID[stage.ID], stage)
		}
	}
	result := make([]StageView, 0, len(byID))
	for id, values := range byID {
		aggregate := StageView{ID: id, State: workflow.Completed, TotalCount: len(states)}
		for _, value := range values {
			aggregate.CompletedCount += value.CompletedCount
		}
		aggregate.State, aggregate.WaitReason, aggregate.WaitTargetStorageID = aggregateStageViews(values)
		result = append(result, aggregate)
	}
	return orderedStages(result)
}

func aggregateStageViews(values []StageView) (workflow.StepState, vocabulary.WaitReason, string) {
	for _, desired := range []workflow.StepState{
		workflow.Failed, workflow.Waiting, workflow.Running, workflow.Verifying,
		workflow.Reserved, workflow.Paused, workflow.Queued, workflow.Cancelled,
	} {
		var selected *StageView
		for index := range values {
			if values[index].State == desired {
				selected = &values[index]
				break
			}
		}
		if selected == nil {
			continue
		}
		if desired != workflow.Waiting {
			return desired, "", ""
		}
		reason, target := selected.WaitReason, selected.WaitTargetStorageID
		for _, value := range values {
			if value.State == workflow.Waiting && (value.WaitReason != reason || value.WaitTargetStorageID != target) {
				return desired, "", ""
			}
		}
		return desired, reason, target
	}
	return workflow.Completed, "", ""
}

func aggregateStageTwoState(states []job.State) (string, workflow.StepState) {
	stages := projectStageTwoJobs(states)
	for _, desired := range []workflow.StepState{
		workflow.Failed, workflow.Waiting, workflow.Running, workflow.Verifying,
		workflow.Reserved, workflow.Paused, workflow.Queued, workflow.Cancelled,
	} {
		for _, stage := range stages {
			if stage.State == desired {
				return stage.ID, desired
			}
		}
	}
	return "archive_catalog", workflow.Completed
}

func transcriptQualities(state job.State) []TranscriptQualityView {
	for _, step := range state.Steps {
		if step.Workflow.ID != "transcript" || step.Workflow.State != workflow.Completed || step.ExecutionSpec == nil || step.OutputRecord == nil {
			continue
		}
		originals := make(map[string]string, len(step.ExecutionSpec.Outputs))
		for _, target := range step.ExecutionSpec.Outputs {
			originals[target.ID] = target.OriginalAssetID
		}
		result := make([]TranscriptQualityView, 0, len(step.OutputRecord.Outputs))
		for _, output := range step.OutputRecord.Outputs {
			if output.Kind != execution.OutputTranscript || output.TranscriptQuality == nil || output.TranscriptQuality.Validate() != nil {
				continue
			}
			quality := output.TranscriptQuality
			result = append(result, TranscriptQualityView{
				TranscriptID: output.ID, OriginalAssetID: originals[output.ID], State: quality.State,
				Reasons:        append([]transcription.QualityReason(nil), quality.Reasons...),
				DurationMillis: quality.DurationMillis, SegmentCount: quality.SegmentCount,
				TextCharacterCount: quality.TextCharacterCount,
			})
		}
		sort.Slice(result, func(i, j int) bool { return result[i].TranscriptID < result[j].TranscriptID })
		return result
	}
	return nil
}

func stageView(id string, step job.Step, total int) StageView {
	waitTargetStorageID := ""
	if step.Workflow.State == workflow.Waiting && step.Workflow.WaitReason == vocabulary.WaitDevice {
		waitTargetStorageID = step.WaitTargetStorageID
	}
	return StageView{
		ID: id, State: step.Workflow.State, WaitReason: step.Workflow.WaitReason,
		WaitTargetStorageID: waitTargetStorageID,
		CompletedCount:      completedItemCount(step.Workflow.State, step.Progress, total), TotalCount: total,
	}
}

func completedItemCount(state workflow.StepState, progress float64, total int) int {
	if total <= 0 {
		return 0
	}
	if state == workflow.Completed {
		return total
	}
	if progress <= 0 {
		return 0
	}
	if progress >= 1 {
		return total
	}
	completed := int(progress * float64(total))
	if completed < 0 {
		return 0
	}
	if completed > total {
		return total
	}
	return completed
}

var stageOrder = map[string]int{
	"classification": 0, "markers": 1, "render": 2, "qc": 3, "archive": 4,
	"audio": 5, "transcript": 6, "subtitle": 7, "transcript_text": 8, "add": 9,
	"job_pending": 10,
}

func orderedStages(stages []StageView) []StageView {
	result := append([]StageView(nil), stages...)
	sort.SliceStable(result, func(i, j int) bool {
		left, leftFound := stageOrder[result[i].ID]
		right, rightFound := stageOrder[result[j].ID]
		if !leftFound {
			left = len(stageOrder) + 1
		}
		if !rightFound {
			right = len(stageOrder) + 1
		}
		return left < right
	})
	return result
}

func storedPackage(state job.State) (string, archive.PackageState) {
	for _, step := range state.Steps {
		if step.Workflow.ID != "organize" || step.Workflow.State != workflow.Completed || step.OutputRecord == nil {
			continue
		}
		for _, output := range step.OutputRecord.Outputs {
			if output.Kind == execution.OutputOrganizedMedia && output.ID != "" {
				return output.ID, archive.PackageStored
			}
		}
	}
	return "", ""
}

func summarizeFolders(folders []FolderView) (stageOneRemaining, stageTwoRemaining, holds, eligible int) {
	for _, folder := range folders {
		if folder.Disposition == vocabulary.OrganizationHold {
			holds++
			continue
		}
		stageOneTerminal := folder.ArchiveState.Valid() || folder.StepState == workflow.Failed || folder.StepState == workflow.Cancelled
		if folder.TranscriptionJobID == "" && !stageOneTerminal {
			stageOneRemaining++
		}
		if folder.CanAddTranscription {
			eligible++
		}
		if folder.TranscriptionJobID != "" && folder.StepState != workflow.Completed &&
			folder.StepState != workflow.Failed && folder.StepState != workflow.Cancelled {
			stageTwoRemaining++
		}
	}
	return
}

func archiveRunState(view DriveView) string {
	if view.StageOneRemaining > 0 {
		return "STAGE_ONE_RUNNING"
	}
	if view.StageTwoRemaining > 0 {
		return "STAGE_TWO_RUNNING"
	}
	for _, folder := range view.Folders {
		if folder.CanAddTranscription && folder.TranscriptionJobID == "" {
			return "TRANSCRIPTION_READY"
		}
	}
	return "COMPLETED"
}

func driveStateName(view DriveView) string {
	if view.StageOneRemaining > 0 || view.StageTwoRemaining > 0 {
		return "IN_PROGRESS"
	}
	if view.HoldCount > 0 {
		return "HAS_HOLD"
	}
	return "COMPLETED"
}

func (service *Service) Override(ctx context.Context, request OverrideCommand) (organization.OrganizationDecision, error) {
	if strings.TrimSpace(request.CommandID) == "" || strings.TrimSpace(request.DecisionID) == "" ||
		strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.ArchiveRunID) == "" || strings.TrimSpace(request.WorkspaceDriveID) == "" ||
		strings.TrimSpace(request.Folder) == "" || request.ExpectedInputRevision == 0 ||
		(request.Actor.Type != vocabulary.ActorHuman && request.Actor.Type != vocabulary.ActorArchivist) ||
		strings.TrimSpace(request.Actor.ID) == "" || request.OccurredAt.IsZero() || !request.Disposition.Valid() {
		return organization.OrganizationDecision{}, organization.ErrInvalidDecision
	}
	digest, err := overrideDigest(request)
	if err != nil {
		return organization.OrganizationDecision{}, err
	}
	stored, storedDigest, found, err := service.store.FindOverrideReceipt(ctx, request.IdempotencyKey)
	if err != nil {
		return organization.OrganizationDecision{}, err
	}
	if found {
		if storedDigest != digest {
			return organization.OrganizationDecision{}, command.ErrIdempotencyConflict
		}
		if err := service.continueResolvedFolder(ctx, request.ArchiveRunID, request.IdempotencyKey, stored, request.OccurredAt); err != nil {
			return organization.OrganizationDecision{}, err
		}
		return stored, nil
	}
	latest, err := service.store.LatestDecisionForArchiveRun(ctx, request.ArchiveRunID, request.Folder)
	if err != nil {
		return organization.OrganizationDecision{}, err
	}
	history, err := organization.NewDecisionHistory(latest)
	if err != nil {
		return organization.OrganizationDecision{}, err
	}
	reason := strings.TrimSpace(request.Reason)
	basis := append([]string(nil), request.Basis...)
	if request.Actor.Type == vocabulary.ActorHuman {
		if reason == "" {
			reason = "explicit operator classification"
		}
		if len(basis) == 0 {
			basis = []string{"previous-decision:" + latest.ID}
		}
	}
	decision, err := history.RecordOverride(organization.OverrideRequest{
		DecisionID: request.DecisionID, ExpectedInputRevision: request.ExpectedInputRevision,
		Disposition: request.Disposition, TargetVideoBitRateKbps: request.TargetVideoBitRateKbps,
		DecidedByType: request.Actor.Type, DecidedBy: request.Actor.ID,
		Reason: reason, Basis: basis, DecidedAt: request.OccurredAt,
	})
	if err != nil {
		return organization.OrganizationDecision{}, err
	}
	if err := service.store.AppendOverride(ctx, request.IdempotencyKey, digest, latest.ID, decision); err != nil {
		return organization.OrganizationDecision{}, err
	}
	if err := service.continueResolvedFolder(ctx, request.ArchiveRunID, request.IdempotencyKey, decision, request.OccurredAt); err != nil {
		return organization.OrganizationDecision{}, err
	}
	return decision, nil
}

func (service *Service) continueResolvedFolder(ctx context.Context, archiveRunID, idempotencyKey string, decision organization.OrganizationDecision, occurredAt time.Time) error {
	if decision.Disposition == vocabulary.OrganizationHold {
		return nil
	}
	if service.resolvedFolders == nil {
		return ErrInvalidConfiguration
	}
	receipts, err := service.store.Receipts(ctx)
	if err != nil {
		return err
	}
	for _, receipt := range receipts {
		if receipt.ID != archiveRunID || receipt.WorkspaceDriveID != decision.WorkspaceDriveID || receipt.InputRevision != decision.InputRevision {
			continue
		}
		containsFolder := false
		for _, folder := range receipt.Folders {
			if folder.Folder == decision.Folder {
				containsFolder = true
				break
			}
		}
		if !containsFolder {
			continue
		}
		snapshot, err := service.jobs.Load(receipt.ID)
		if err != nil {
			return err
		}
		archiveRun, err := archiverun.Decode(snapshot)
		if err != nil {
			return err
		}
		_, err = service.resolvedFolders.StartResolvedFolder(ctx, receipt.SessionID, idempotencyKey, archiveRun, decision, occurredAt)
		return err
	}
	return command.ErrTargetNotFound
}

func overrideDigest(request OverrideCommand) (string, error) {
	payload, err := json.Marshal(struct {
		DecisionID             string                             `json:"decision_id"`
		ArchiveRunID           string                             `json:"archive_run_id"`
		WorkspaceDriveID       string                             `json:"workspace_drive_id"`
		Folder                 string                             `json:"folder"`
		ExpectedInputRevision  uint64                             `json:"expected_input_revision"`
		Disposition            vocabulary.OrganizationDisposition `json:"disposition"`
		TargetVideoBitRateKbps int                                `json:"target_video_bitrate_kbps"`
		Actor                  command.Actor                      `json:"actor"`
		Reason                 string                             `json:"reason"`
		Basis                  []string                           `json:"basis"`
	}{
		DecisionID: request.DecisionID, ArchiveRunID: request.ArchiveRunID, WorkspaceDriveID: request.WorkspaceDriveID, Folder: request.Folder,
		ExpectedInputRevision: request.ExpectedInputRevision, Disposition: request.Disposition,
		TargetVideoBitRateKbps: request.TargetVideoBitRateKbps, Actor: request.Actor,
		Reason: strings.TrimSpace(request.Reason), Basis: append([]string(nil), request.Basis...),
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}
