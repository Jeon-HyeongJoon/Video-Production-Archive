package archiveorganization

import (
	"context"
	"errors"
	"strings"
	"time"

	appintake "videoarchive/internal/application/archiveintake"
	"videoarchive/internal/core/archive"
	coreintake "videoarchive/internal/core/archiveintake"
	"videoarchive/internal/core/archiverun"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/resource"
	"videoarchive/internal/core/storage"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
)

var ErrStartRejected = errors.New("archive intake start request is invalid or not authorized by topology")

const StartActionCardCommand = "archive_run.start"

type IntakeRepository interface {
	appintake.Repository
	Session(context.Context, string) (coreintake.Session, error)
}

type StorageResolver interface {
	ResolveAccess(context.Context, string) (storage.Access, error)
}

type AnalyzerFactory interface {
	New(*archive.ReadOnlyStorageRoot) (MediaAnalyzer, error)
}

type WorkerSelector interface {
	Select(worker.Requirement, time.Time) (worker.Definition, error)
}

type StartRequest struct {
	ArchiveRunID              string
	IdempotencyKey            string
	SessionID                 string
	SourceVolumeID            string
	ExpectedCandidateRevision uint64
	ArchiveStorageID          string
	InputRevision             uint64
	FolderIDs                 []string
	ExpectedWorkerID          string
	ExpectedSourceStorageID   string
	ExpectedWorkspaceDriveID  string
	Actor                     command.Actor
	SelectedBy                command.Actor
	StartedAt                 time.Time
}

type Starter interface {
	Start(context.Context, StartRequest) (Result, error)
}

type StartService struct {
	intake           *appintake.Service
	intakeRepository IntakeRepository
	storage          StorageResolver
	workers          WorkerSelector
	catalog          Catalog
	receipts         ReceiptStore
	jobs             JobBus
	archiveRuns      JobBus
	analyzers        AnalyzerFactory
	now              func() time.Time
}

func NewStartService(
	intakeRepository IntakeRepository,
	storageResolver StorageResolver,
	workers WorkerSelector,
	catalog Catalog,
	receipts ReceiptStore,
	jobs JobBus,
	archiveRuns JobBus,
	analyzers AnalyzerFactory,
	now func() time.Time,
) (*StartService, error) {
	intake, err := appintake.NewService(intakeRepository)
	if err != nil || storageResolver == nil || workers == nil || catalog == nil || receipts == nil || jobs == nil || archiveRuns == nil || analyzers == nil || now == nil {
		return nil, ErrStartRejected
	}
	return &StartService{
		intake: intake, intakeRepository: intakeRepository, storage: storageResolver, workers: workers,
		catalog: catalog, receipts: receipts, jobs: jobs, archiveRuns: archiveRuns, analyzers: analyzers,
		now: now,
	}, nil
}

func (service *StartService) Start(ctx context.Context, request StartRequest) (Result, error) {
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
	if request.ArchiveRunID == "" || request.IdempotencyKey == "" || request.SessionID == "" || request.SourceVolumeID == "" ||
		request.ExpectedCandidateRevision == 0 || request.ArchiveStorageID == "" || request.InputRevision == 0 || len(request.FolderIDs) == 0 ||
		(request.Actor.Type != vocabulary.ActorHuman && request.Actor.Type != vocabulary.ActorArchivist) || request.Actor.ID == "" ||
		!validSelectionActor(request.SelectedBy) || request.StartedAt.IsZero() {
		return Result{}, ErrStartRejected
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
		StorageIDs:      []string{candidate.StorageID, archiveStorage.StorageID},
		PinnedWorkerID:  candidate.WorkerID,
	}, service.now().UTC())
	if err != nil || !definition.Enabled || definition.ID != candidate.WorkerID {
		return Result{}, ErrStartRejected
	}

	var sessionAccess *appintake.SessionAccess
	stored, sessionErr := service.intakeRepository.Session(ctx, request.SessionID)
	if sessionErr == nil {
		if stored.VolumeID != candidate.VolumeID || stored.CandidateRevision != candidate.Revision || stored.StorageID != candidate.StorageID ||
			stored.WorkerID != candidate.WorkerID {
			return Result{}, ErrStartRejected
		}
		root, err := archive.OpenReadOnlyStorageRoot(candidate.Root)
		if err != nil {
			return Result{}, err
		}
		sessionAccess = &appintake.SessionAccess{Session: stored, Root: root}
	} else {
		sessionAccess, err = service.intake.Select(ctx, appintake.SelectRequest{
			SessionID: request.SessionID, VolumeID: request.SourceVolumeID,
			ExpectedCandidateRevision: request.ExpectedCandidateRevision, Actor: request.SelectedBy, SelectedAt: request.StartedAt,
		})
		if err != nil {
			return Result{}, errors.Join(ErrStartRejected, err)
		}
	}
	defer sessionAccess.Close()
	analyzer, err := service.analyzers.New(sessionAccess.Root)
	if err != nil {
		return Result{}, err
	}
	coordinator, err := NewCoordinator(service.catalog, service.receipts, service.jobs, analyzer)
	if err != nil {
		return Result{}, err
	}
	folderScope, err := organization.FixFolderScope(organization.FixFolderScopeRequest{
		DiscoveryRevision: request.InputRevision, DiscoveredFolderIDs: request.FolderIDs, SelectAll: true,
	})
	if err != nil {
		return Result{}, errors.Join(ErrStartRejected, err)
	}
	if _, err := service.archiveRuns.Execute(command.Envelope{
		CommandID: request.ArchiveRunID + ":create", CommandType: archiverun.CreateCommand,
		Actor: request.SelectedBy, RequestedVia: selectionRequestedVia(request.SelectedBy), TargetID: request.ArchiveRunID, ExpectedRevision: 0,
		IdempotencyKey: request.IdempotencyKey + ":archive-run",
		Parameters: map[string]any{
			"start_idempotency_key": request.IdempotencyKey,
			"worker_id":             candidate.WorkerID, "source_storage_id": candidate.StorageID,
			"source_volume_id": candidate.VolumeID, "source_physical_device_id": candidate.PhysicalDeviceID,
			"archive_storage_id": archiveStorage.StorageID, "folder_discovery_revision": request.InputRevision,
			"folder_ids": folderScope.FolderIDs(), "selected_by": request.SelectedBy,
		},
		Reason: "explicitly selected archive run",
	}); err != nil {
		return Result{}, errors.Join(ErrStartRejected, err)
	}
	return coordinator.Execute(ctx, Request{
		ArchiveRunID: request.ArchiveRunID, IdempotencyKey: request.IdempotencyKey, InputRevision: request.InputRevision,
		FolderScope: folderScope, Session: sessionAccess, ArchiveStorageID: archiveStorage.StorageID,
		ActorID: request.Actor.ID, StartedAt: request.StartedAt,
		ExecutionPriority: resource.Immediate,
	})
}

func BuildStaticCandidate(topology storage.Topology, workerID string, observedAt time.Time) (coreintake.Candidate, error) {
	candidate := coreintake.Candidate{
		VolumeID: topology.VolumeID, Revision: 1, PhysicalDeviceID: topology.PhysicalDeviceID,
		WorkerID: strings.TrimSpace(workerID), StorageID: topology.StorageID, Root: topology.Root,
		Connected: true, ObservedAt: observedAt.UTC(),
	}
	if topology.Role != vocabulary.StorageSourceMedia || candidate.Validate() != nil {
		return coreintake.Candidate{}, ErrStartRejected
	}
	return candidate, nil
}

var _ Starter = (*StartService)(nil)
