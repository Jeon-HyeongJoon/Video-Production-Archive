package archiveorganization

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"time"

	appintake "videoarchive/internal/application/archiveintake"
	coreintake "videoarchive/internal/core/archiveintake"
	"videoarchive/internal/core/archiverun"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/pipeline"
	"videoarchive/internal/core/resource"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

type WorkerInventory interface {
	WorkerSelector
	Statuses(time.Time) []worker.Status
}

type SnapshotLoader interface {
	Load(string) (command.Snapshot, error)
}

// RemoteStartService starts Archive Intake without opening the source on the
// Control process. The first persisted Job is a pinned Worker inspection.
type RemoteStartService struct {
	intake           *appintake.Service
	intakeRepository IntakeRepository
	storage          StorageResolver
	workers          WorkerInventory
	receipts         ReceiptStore
	jobs             JobBus
	archiveRuns      JobBus
	states           SnapshotLoader
	now              func() time.Time
}

func NewRemoteStartService(
	intakeRepository IntakeRepository,
	storageResolver StorageResolver,
	workers WorkerInventory,
	receipts ReceiptStore,
	jobs JobBus,
	archiveRuns JobBus,
	states SnapshotLoader,
	now func() time.Time,
) (*RemoteStartService, error) {
	intake, err := appintake.NewService(intakeRepository)
	if err != nil || storageResolver == nil || workers == nil || receipts == nil || jobs == nil || archiveRuns == nil || states == nil || now == nil {
		return nil, ErrStartRejected
	}
	return &RemoteStartService{
		intake: intake, intakeRepository: intakeRepository, storage: storageResolver, workers: workers,
		receipts: receipts, jobs: jobs, archiveRuns: archiveRuns, states: states, now: now,
	}, nil
}

func (service *RemoteStartService) Start(ctx context.Context, request StartRequest) (Result, error) {
	request = normalizeStartRequest(request)
	if err := validateStartRequest(request); err != nil {
		return Result{}, err
	}
	if stored, found, err := service.receipts.FindReceipt(ctx, request.IdempotencyKey); err != nil {
		return Result{}, err
	} else if found {
		return Result{Receipt: stored, Replayed: true}, nil
	}
	candidate, err := service.intakeRepository.Candidate(ctx, request.SourceVolumeID)
	if err != nil || candidate.Revision != request.ExpectedCandidateRevision || !candidate.Connected {
		return Result{}, errors.Join(ErrStartRejected, err)
	}
	if (request.ExpectedWorkerID != "" && request.ExpectedWorkerID != candidate.WorkerID) ||
		(request.ExpectedSourceStorageID != "" && request.ExpectedSourceStorageID != candidate.StorageID) ||
		(request.ExpectedWorkspaceDriveID != "" && request.ExpectedWorkspaceDriveID != candidate.VolumeID) {
		return Result{}, ErrStartRejected
	}
	archiveStorage, err := service.storage.ResolveAccess(ctx, request.ArchiveStorageID)
	if err != nil || archiveStorage.Role != vocabulary.StorageArchivePrimary || archiveStorage.StorageID == candidate.StorageID {
		return Result{}, errors.Join(ErrStartRejected, err)
	}
	definition, err := service.workers.Select(worker.Requirement{
		ProcessingKinds: []vocabulary.ProcessingKind{vocabulary.ProcessingInspect},
		StorageIDs:      []string{candidate.StorageID, archiveStorage.StorageID}, PinnedWorkerID: candidate.WorkerID,
	}, service.now().UTC())
	if err != nil || !definition.Enabled || definition.ID != candidate.WorkerID {
		return Result{}, ErrStartRejected
	}
	discovered, selectedBytes, archiveFree, err := service.validateLiveScope(candidate.WorkerID, candidate.StorageID, archiveStorage.StorageID, request.InputRevision, request.FolderIDs)
	if err != nil || selectedBytes == 0 {
		return Result{}, errors.Join(ErrStartRejected, err)
	}
	if err := ValidateArchiveCapacity(archiveStorage.StorageID, selectedBytes, archiveFree); err != nil {
		return Result{}, err
	}
	folderScope, err := organization.FixFolderScope(organization.FixFolderScopeRequest{
		DiscoveryRevision: request.InputRevision, DiscoveredFolderIDs: discovered, SelectedFolderIDs: request.FolderIDs,
	})
	if err != nil {
		return Result{}, errors.Join(ErrStartRejected, err)
	}
	storedSession, sessionErr := service.intakeRepository.Session(ctx, request.SessionID)
	if sessionErr == nil {
		if storedSession.VolumeID != candidate.VolumeID || storedSession.CandidateRevision != candidate.Revision ||
			storedSession.StorageID != candidate.StorageID || storedSession.WorkerID != candidate.WorkerID || storedSession.SelectedBy != request.SelectedBy.ID {
			return Result{}, ErrStartRejected
		}
	} else {
		if request.Actor.Type != vocabulary.ActorHuman {
			return Result{}, ErrStartRejected
		}
		access, selectErr := service.intake.Select(ctx, appintake.SelectRequest{
			SessionID: request.SessionID, VolumeID: request.SourceVolumeID,
			ExpectedCandidateRevision: request.ExpectedCandidateRevision, Actor: request.SelectedBy, SelectedAt: request.StartedAt,
		})
		if selectErr != nil {
			return Result{}, errors.Join(ErrStartRejected, selectErr)
		}
		storedSession = access.Session
		if access.Root != nil {
			_ = access.Root.Close()
		}
	}
	replayed := false
	if snapshot, loadErr := service.states.Load(request.ArchiveRunID); loadErr == nil {
		run, decodeErr := archiverun.Decode(snapshot)
		if decodeErr != nil || !sameRunSelection(run, request.IdempotencyKey, candidate.WorkerID, candidate.StorageID, candidate.VolumeID, candidate.PhysicalDeviceID, archiveStorage.StorageID, folderScope, request.SelectedBy) {
			return Result{}, command.ErrIdempotencyConflict
		}
		replayed = true
	} else if _, executeErr := service.archiveRuns.Execute(command.Envelope{
		CommandID: request.ArchiveRunID + ":create", CommandType: archiverun.CreateCommand,
		Actor: request.SelectedBy, RequestedVia: "APPLICATION", TargetID: request.ArchiveRunID, ExpectedRevision: 0,
		IdempotencyKey: request.IdempotencyKey + ":archive-run",
		Parameters: map[string]any{
			"start_idempotency_key": request.IdempotencyKey,
			"worker_id":             candidate.WorkerID, "source_storage_id": candidate.StorageID,
			"source_volume_id": candidate.VolumeID, "source_physical_device_id": candidate.PhysicalDeviceID,
			"archive_storage_id": archiveStorage.StorageID, "folder_discovery_revision": request.InputRevision,
			"folder_ids": folderScope.FolderIDs(), "selected_by": request.SelectedBy,
		}, Reason: "human selected archive run",
	}); executeErr != nil {
		return Result{}, errors.Join(ErrStartRejected, executeErr)
	}
	inspectionJobID := stableID("JOB-INSPECTION", request.ArchiveRunID)
	if replayed {
		if existing, loadErr := service.states.Load(inspectionJobID); loadErr == nil {
			state, decodeErr := job.Decode(existing)
			if decodeErr != nil || state.TemplateName != pipeline.ArchiveIntakeInspection().Name {
				return Result{}, command.ErrIdempotencyConflict
			}
			return Result{Receipt: pendingOrganizationReceipt(request, storedSession, candidate, folderScope), Replayed: true}, nil
		} else if !errors.Is(loadErr, command.ErrTargetNotFound) {
			return Result{}, loadErr
		}
	}
	template := pipeline.ArchiveIntakeInspection()
	manifestID := stableID("MEDIA-INSPECTION", request.ArchiveRunID)
	spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: inspectionJobID, StepID: "inspect", TemplateName: template.Name, Operation: execution.OperationInspectMedia,
		Outputs: []execution.OutputTarget{{
			ID: manifestID, Kind: execution.OutputMediaInspection, StorageID: archiveStorage.StorageID,
			RelativePath: path.Join("staging", inspectionJobID, "inspection", manifestID+".json"),
		}},
		Parameters: execution.StepExecutionParameters{
			ArchiveRunID: request.ArchiveRunID, ArchiveIntakeSessionID: storedSession.ID,
			StartIdempotencyKey: request.IdempotencyKey, SourceStorageID: candidate.StorageID,
			ArchiveStorageID: archiveStorage.StorageID, WorkspaceDriveID: candidate.VolumeID,
			InputRevision: request.InputRevision, FolderIDs: folderScope.FolderIDs(),
			RequestedBy: request.SelectedBy.ID, RequestedAt: request.StartedAt.UTC(),
		},
	})
	if err != nil {
		return Result{}, err
	}
	if _, err := service.jobs.Execute(command.Envelope{
		CommandID: stableID("COMMAND", inspectionJobID), CommandType: job.CreateCommand,
		Actor: command.Actor{Type: vocabulary.ActorSystem, ID: "archive-intake-coordinator"}, RequestedVia: "APPLICATION",
		TargetID: inspectionJobID, ExpectedRevision: 0, IdempotencyKey: request.IdempotencyKey + ":inspection",
		Parameters: map[string]any{
			"template_name": template.Name, "target_title": candidate.VolumeID, "control_mode": workflow.AssistedMode,
			"execution_priority":   resource.Immediate,
			"required_storage_ids": []string{candidate.StorageID, archiveStorage.StorageID},
			"physical_device_ids":  []string{candidate.PhysicalDeviceID},
			"step_execution_specs": map[string]execution.StepExecutionSpec{"inspect": spec},
		}, Reason: "inspect fixed archive intake scope on Worker",
	}); err != nil {
		return Result{}, err
	}
	pending := pendingOrganizationReceipt(request, storedSession, candidate, folderScope)
	return Result{Receipt: pending, Replayed: replayed}, nil
}

func pendingOrganizationReceipt(request StartRequest, session coreintake.Session, candidate coreintake.Candidate, scope organization.FolderScope) organization.ArchiveOrganizationReceipt {
	pending := organization.ArchiveOrganizationReceipt{
		ID: request.ArchiveRunID, IdempotencyKey: request.IdempotencyKey, SessionID: session.ID,
		InputRevision: request.InputRevision, WorkspaceDriveID: candidate.VolumeID, WorkerID: candidate.WorkerID,
	}
	for _, folderID := range scope.FolderIDs() {
		pending.Folders = append(pending.Folders, organization.CoordinatedFolder{Folder: folderID})
	}
	return pending
}

func (service *RemoteStartService) validateLiveScope(workerID, sourceStorageID, archiveStorageID string, revision uint64, selected []string) ([]string, uint64, uint64, error) {
	for _, status := range service.workers.Statuses(service.now().UTC()) {
		if status.WorkerID != workerID || (status.State != "ONLINE" && status.State != "BUSY") {
			continue
		}
		var discovered []string
		var selectedBytes, archiveFree uint64
		var archiveConnected bool
		selectedSet := make(map[string]bool, len(selected))
		for _, folderID := range selected {
			selectedSet[strings.TrimSpace(folderID)] = true
		}
		for _, observation := range status.Metrics.StorageObservations {
			switch observation.StorageID {
			case sourceStorageID:
				if observation.DiscoveryState != vocabulary.FolderDiscoveryReady || observation.DiscoveryRevision != revision {
					return nil, 0, 0, ErrStartRejected
				}
				for _, folder := range observation.Folders {
					discovered = append(discovered, folder.Folder)
					if selectedSet[folder.Folder] {
						selectedBytes += folder.InputBytes
					}
				}
			case archiveStorageID:
				if observation.Connected {
					archiveFree = observation.FreeBytes
					archiveConnected = true
				}
			}
		}
		if len(discovered) > 0 && archiveConnected {
			return discovered, selectedBytes, archiveFree, nil
		}
	}
	return nil, 0, 0, ErrStartRejected
}

func normalizeStartRequest(request StartRequest) StartRequest {
	request.ArchiveRunID = strings.TrimSpace(request.ArchiveRunID)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.SessionID = strings.TrimSpace(request.SessionID)
	request.SourceVolumeID = strings.TrimSpace(request.SourceVolumeID)
	request.ArchiveStorageID = strings.TrimSpace(request.ArchiveStorageID)
	request.ExpectedWorkerID = strings.TrimSpace(request.ExpectedWorkerID)
	request.ExpectedSourceStorageID = strings.TrimSpace(request.ExpectedSourceStorageID)
	request.ExpectedWorkspaceDriveID = strings.TrimSpace(request.ExpectedWorkspaceDriveID)
	request.Actor.ID = strings.TrimSpace(request.Actor.ID)
	request.SelectedBy.ID = strings.TrimSpace(request.SelectedBy.ID)
	if request.SelectedBy.ID == "" && request.Actor.Type == vocabulary.ActorHuman {
		request.SelectedBy = request.Actor
	}
	return request
}

func validateStartRequest(request StartRequest) error {
	if request.ArchiveRunID == "" || request.IdempotencyKey == "" || request.SessionID == "" || request.SourceVolumeID == "" ||
		request.ExpectedCandidateRevision == 0 || request.ArchiveStorageID == "" || request.InputRevision == 0 || len(request.FolderIDs) == 0 ||
		(request.Actor.Type != vocabulary.ActorHuman && request.Actor.Type != vocabulary.ActorArchivist) || request.Actor.ID == "" ||
		request.SelectedBy.Type != vocabulary.ActorHuman || request.SelectedBy.ID == "" || request.StartedAt.IsZero() {
		return ErrStartRejected
	}
	return nil
}

func sameRunSelection(run archiverun.State, startIdempotencyKey, workerID, sourceStorageID, volumeID, physicalDeviceID, archiveStorageID string, scope organization.FolderScope, selectedBy command.Actor) bool {
	return run.StartIdempotencyKey == startIdempotencyKey && run.WorkerID == workerID && run.SourceStorageID == sourceStorageID && run.SourceVolumeID == volumeID &&
		run.SourcePhysicalDeviceID == physicalDeviceID && run.ArchiveStorageID == archiveStorageID &&
		run.FolderDiscoveryRevision == scope.DiscoveryRevision() && equalStrings(run.FolderIDs, scope.FolderIDs()) && run.SelectedBy == selectedBy
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

var _ Starter = (*RemoteStartService)(nil)

// InspectionCompletionCoordinator turns a completed Worker inspection into
// catalog facts, policy decisions, and per-folder stage-one Jobs.
type InspectionCompletionCoordinator struct {
	coordinator *Coordinator
	states      SnapshotLoader
}

func NewInspectionCompletionCoordinator(catalog Catalog, receipts ReceiptStore, jobs JobBus, states SnapshotLoader) (*InspectionCompletionCoordinator, error) {
	coordinator, err := NewInspectedCoordinator(catalog, receipts, jobs)
	if err != nil || states == nil {
		return nil, ErrInvalidRequest
	}
	return &InspectionCompletionCoordinator{coordinator: coordinator, states: states}, nil
}

func (coordinator *InspectionCompletionCoordinator) ObserveCompletion(ctx context.Context, state job.State) error {
	if state.TemplateName != pipeline.ArchiveIntakeInspection().Name {
		return nil
	}
	if len(state.Steps) != 1 {
		return ErrInvalidRequest
	}
	if state.Steps[0].Workflow.State != workflow.Completed {
		return nil
	}
	if state.Steps[0].ExecutionSpec == nil || state.Steps[0].OutputRecord == nil {
		return ErrInvalidRequest
	}
	spec := state.Steps[0].ExecutionSpec
	record := state.Steps[0].OutputRecord
	snapshot, err := coordinator.states.Load(spec.Parameters.ArchiveRunID)
	if err != nil {
		return err
	}
	run, err := archiverun.Decode(snapshot)
	if err != nil || run.WorkerID != record.WorkerID || run.SourceStorageID != spec.Parameters.SourceStorageID ||
		run.ArchiveStorageID != spec.Parameters.ArchiveStorageID || run.FolderDiscoveryRevision != spec.Parameters.InputRevision ||
		!equalStrings(run.FolderIDs, spec.Parameters.FolderIDs) {
		return ErrInvalidRequest
	}
	folderScope, err := run.FolderScope()
	if err != nil {
		return err
	}
	session := coreintake.Session{
		ID: spec.Parameters.ArchiveIntakeSessionID, Revision: 1, CandidateRevision: 1,
		VolumeID: run.SourceVolumeID, WorkspaceDriveID: run.SourceVolumeID,
		PhysicalDeviceID: run.SourcePhysicalDeviceID, WorkerID: run.WorkerID, StorageID: run.SourceStorageID,
		SelectedBy: run.SelectedBy.ID, SelectedAt: spec.Parameters.RequestedAt.UTC(),
	}
	_, err = coordinator.coordinator.ExecuteInspected(ctx, Request{
		ArchiveRunID: run.ID, IdempotencyKey: spec.Parameters.StartIdempotencyKey,
		InputRevision: run.FolderDiscoveryRevision, FolderScope: folderScope,
		Session: &appintake.SessionAccess{Session: session}, ArchiveStorageID: run.ArchiveStorageID,
		ActorID: run.SelectedBy.ID, StartedAt: spec.Parameters.RequestedAt.UTC(), ExecutionPriority: resource.Immediate,
	}, record.InspectedMedia)
	return err
}
