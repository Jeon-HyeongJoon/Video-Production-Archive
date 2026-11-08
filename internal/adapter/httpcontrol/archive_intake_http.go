package httpcontrol

import (
	"errors"
	"net/http"
	"time"

	appdriveintake "videoarchive/internal/application/archiveintake"
	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/application/archiveorganization"
	"videoarchive/internal/application/archivetranscription"
	coreintake "videoarchive/internal/core/archiveintake"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/security"
	"videoarchive/internal/core/vocabulary"
)

func (handler *Handler) archiveIntakeProjection(writer http.ResponseWriter, request *http.Request) {
	views, err := handler.drives.Projection(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "QUERY_FAILED")
		return
	}
	result := make([]driveView, 0, len(views))
	for _, value := range views {
		projected := driveView{
			WorkspaceDriveID: value.WorkspaceDriveID, ArchiveRunID: value.ArchiveRunID, ArchiveRunRevision: value.ArchiveRunRevision,
			CandidateRevision: value.CandidateRevision, FolderDiscoveryRevision: value.FolderDiscoveryRevision,
			FolderDiscoveryState: string(value.FolderDiscoveryState),
			SessionID:            value.SessionID, WorkerID: value.WorkerID, SourceVolumeID: value.SourceVolumeID,
			SourceStorageID:  value.SourceStorageID,
			ArchiveStorageID: value.ArchiveStorageID, State: value.State, ArchiveRunState: value.ArchiveRunState,
			InputRevision: value.InputRevision, FileCount: value.FileCount, FolderCount: value.FolderCount,
			StageOneRemaining: value.StageOneRemaining, StageTwoRemaining: value.StageTwoRemaining,
			HoldCount: value.HoldCount, EligibleTranscriptionCount: value.EligibleTranscriptionCount,
			CatalogRowCount: value.CatalogRowCount, CatalogUpdatedAt: value.CatalogUpdatedAt,
			DiscoveredFileCount: value.DiscoveredFileCount, DiscoveredFolderCount: value.DiscoveredFolderCount,
		}
		for _, completed := range value.CompletedWork {
			projected.CompletedWork = append(projected.CompletedWork, driveCompletedWorkView{
				ArchiveRunID: completed.ArchiveRunID, Folder: completed.Folder, JobID: completed.JobID,
				Disposition: string(completed.Disposition), ProfileReference: completed.ProfileReference,
				SourceVideoCount: completed.SourceVideoCount, ArchiveStorageID: completed.ArchiveStorageID,
				ArchivePackageID: completed.ArchivePackageID, ArchiveDate: completed.ArchiveDate,
				ArchiveDisplayName: completed.ArchiveDisplayName, ArchiveVideoFileName: completed.ArchiveVideoFileName,
				ArchiveRelativePath: completed.ArchiveRelativePath,
				ArchiveState:        string(completed.ArchiveState), CompletedAt: completed.CompletedAt,
			})
		}
		for _, candidate := range value.StorageCandidates {
			projected.StorageCandidates = append(projected.StorageCandidates, driveStorageView{
				StorageID: candidate.StorageID, Role: string(candidate.Role), WorkspaceDriveID: candidate.WorkspaceDriveID,
				AccessKind: string(candidate.AccessKind), Accessible: candidate.Accessible,
				Model: candidate.Model, Label: candidate.Label, FileSystem: candidate.FileSystem,
				CapacityBytes: candidate.CapacityBytes, FreeBytes: candidate.FreeBytes,
			})
		}
		for _, folder := range value.Folders {
			projected.Folders = append(projected.Folders, projectDriveFolder(folder))
		}
		for _, folder := range value.DiscoveredFolders {
			projected.DiscoveredFolders = append(projected.DiscoveredFolders, projectDriveFolder(folder))
		}
		result = append(result, projected)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": result})
}

func projectDriveFolder(folder archiveintakeworkspace.FolderView) driveFolderView {
	holdReasons := make([]string, 0, len(folder.HoldReasons))
	for _, reason := range folder.HoldReasons {
		holdReasons = append(holdReasons, string(reason))
	}
	stages := make([]driveStageView, 0, len(folder.Stages))
	for _, stage := range folder.Stages {
		stages = append(stages, driveStageView{
			ID: stage.ID, State: string(stage.State), WaitReason: string(stage.WaitReason),
			WaitTargetStorageID: stage.WaitTargetStorageID,
			CompletedCount:      stage.CompletedCount, TotalCount: stage.TotalCount,
		})
	}
	qualities := make([]transcriptQualityView, 0, len(folder.TranscriptQualities))
	for _, quality := range folder.TranscriptQualities {
		reasons := make([]string, 0, len(quality.Reasons))
		for _, reason := range quality.Reasons {
			reasons = append(reasons, string(reason))
		}
		qualities = append(qualities, transcriptQualityView{
			TranscriptID: quality.TranscriptID, OriginalAssetID: quality.OriginalAssetID,
			State: string(quality.State), Reasons: reasons, DurationMillis: quality.DurationMillis,
			SegmentCount: quality.SegmentCount, TextCharacterCount: quality.TextCharacterCount,
		})
	}
	workOptions := make([]driveWorkOptionView, 0, len(folder.WorkOptions))
	for _, option := range folder.WorkOptions {
		workOptions = append(workOptions, driveWorkOptionView{
			Disposition: string(option.Disposition), TargetVideoBitRateKbps: option.TargetVideoBitRateKbps,
		})
	}
	return driveFolderView{
		Folder: folder.Folder, DecisionID: folder.DecisionID, InputRevision: folder.InputRevision,
		DecisionSequence: folder.DecisionSequence, Disposition: string(folder.Disposition),
		ProfileReference: folder.ProfileReference, HoldReasons: holdReasons, WorkOptions: workOptions, VideoCount: folder.VideoCount,
		EstimatedOutputBytes: folder.EstimatedOutputBytes, JobID: folder.JobID,
		TranscriptionJobID: folder.TranscriptionJobID, CurrentStepID: folder.CurrentStepID,
		StepState: string(folder.StepState), Stages: stages, ArchivePackageID: folder.ArchivePackageID,
		ArchiveDate: folder.ArchiveDate, ArchiveDisplayName: folder.ArchiveDisplayName,
		ArchiveVideoFileName: folder.ArchiveVideoFileName, ArchiveState: string(folder.ArchiveState),
		CanAddTranscription: folder.CanAddTranscription,
		TranscriptQualities: qualities,
	}
}

type driveOverrideRequest struct {
	CommandID              string `json:"command_id"`
	DecisionID             string `json:"decision_id"`
	IdempotencyKey         string `json:"idempotency_key"`
	ArchiveRunID           string `json:"archive_run_id"`
	Folder                 string `json:"folder"`
	ExpectedRevision       uint64 `json:"expected_revision"`
	Disposition            string `json:"disposition"`
	TargetVideoBitRateKbps int    `json:"target_video_bitrate_kbps,omitempty"`
}

func (handler *Handler) overrideArchiveIntakeDecision(writer http.ResponseWriter, request *http.Request) {
	if !handler.allowMutation(writer, request) {
		return
	}
	var payload driveOverrideRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	principal, _ := request.Context().Value(principalContextKey{}).(security.Principal)
	decision, err := handler.drives.Override(request.Context(), archiveintakeworkspace.OverrideCommand{
		CommandID: payload.CommandID, DecisionID: payload.DecisionID, IdempotencyKey: payload.IdempotencyKey,
		ArchiveRunID: payload.ArchiveRunID, WorkspaceDriveID: request.PathValue("workspaceDriveID"), Folder: payload.Folder,
		ExpectedInputRevision: payload.ExpectedRevision, Disposition: vocabulary.OrganizationDisposition(payload.Disposition),
		TargetVideoBitRateKbps: payload.TargetVideoBitRateKbps,
		Actor:                  command.Actor{Type: principal.ActorType, ID: principal.ID}, OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		switch {
		case errors.Is(err, organization.ErrStaleInputRevision), errors.Is(err, command.ErrIdempotencyConflict):
			writeError(writer, http.StatusConflict, "ORGANIZATION_OVERRIDE_CONFLICT")
		case errors.Is(err, organization.ErrInvalidDecision):
			writeError(writer, http.StatusBadRequest, "INVALID_ORGANIZATION_OVERRIDE")
		default:
			writeError(writer, http.StatusInternalServerError, "ORGANIZATION_OVERRIDE_FAILED")
		}
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"decision_id": decision.ID, "sequence": decision.Sequence, "input_revision": decision.InputRevision,
		"disposition": decision.Disposition, "profile_reference": decision.ProfileReference, "hold_reasons": decision.HoldReasons,
	}})
}

type archiveRunStartRequest struct {
	ArchiveRunID              string   `json:"archive_run_id"`
	IdempotencyKey            string   `json:"idempotency_key"`
	SessionID                 string   `json:"session_id"`
	SourceVolumeID            string   `json:"source_volume_id"`
	ExpectedCandidateRevision uint64   `json:"expected_candidate_revision"`
	ArchiveStorageID          string   `json:"archive_storage_id"`
	InputRevision             uint64   `json:"input_revision"`
	FolderIDs                 []string `json:"folder_ids"`
}

type archiveIntakeSelectionRequest struct {
	SessionID                 string `json:"session_id"`
	VolumeID                  string `json:"volume_id"`
	ExpectedCandidateRevision uint64 `json:"expected_candidate_revision"`
}

func (handler *Handler) selectArchiveIntake(writer http.ResponseWriter, request *http.Request) {
	if !handler.allowMutation(writer, request) {
		return
	}
	var payload archiveIntakeSelectionRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	principal, _ := request.Context().Value(principalContextKey{}).(security.Principal)
	access, err := handler.archiveIntake.Select(request.Context(), appdriveintake.SelectRequest{
		SessionID: payload.SessionID, VolumeID: payload.VolumeID,
		ExpectedCandidateRevision: payload.ExpectedCandidateRevision,
		Actor:                     command.Actor{Type: principal.ActorType, ID: principal.ID}, SelectedAt: time.Now().UTC(),
	})
	if err != nil {
		switch {
		case errors.Is(err, coreintake.ErrStaleCandidate):
			writeError(writer, http.StatusConflict, "DRIVE_SELECTION_STALE")
		case errors.Is(err, coreintake.ErrHumanSelectionRequired), errors.Is(err, coreintake.ErrInvalidCandidate), errors.Is(err, coreintake.ErrInvalidSession):
			writeError(writer, http.StatusBadRequest, "INVALID_DRIVE_SELECTION")
		default:
			writeError(writer, http.StatusInternalServerError, "DRIVE_SELECTION_FAILED")
		}
		return
	}
	if access.Root != nil {
		if closeErr := access.Root.Close(); closeErr != nil {
			writeError(writer, http.StatusInternalServerError, "DRIVE_SELECTION_FAILED")
			return
		}
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"data": map[string]any{
		"session_id": access.Session.ID, "candidate_revision": access.Session.CandidateRevision,
		"workspace_drive_id": access.Session.WorkspaceDriveID, "worker_id": access.Session.WorkerID,
		"source_storage_id": access.Session.StorageID,
	}})
}

func (handler *Handler) startArchiveRun(writer http.ResponseWriter, request *http.Request) {
	if !handler.allowMutation(writer, request) {
		return
	}
	var payload archiveRunStartRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	principal, _ := request.Context().Value(principalContextKey{}).(security.Principal)
	result, err := handler.archiveRuns.Start(request.Context(), archiveorganization.StartRequest{
		ArchiveRunID: payload.ArchiveRunID, IdempotencyKey: payload.IdempotencyKey, SessionID: payload.SessionID,
		SourceVolumeID: payload.SourceVolumeID, ExpectedCandidateRevision: payload.ExpectedCandidateRevision,
		ArchiveStorageID: payload.ArchiveStorageID, InputRevision: payload.InputRevision, FolderIDs: payload.FolderIDs,
		Actor:      command.Actor{Type: principal.ActorType, ID: principal.ID},
		SelectedBy: command.Actor{Type: principal.ActorType, ID: principal.ID}, StartedAt: time.Now().UTC(),
	})
	if err != nil {
		switch {
		case errors.Is(err, archiveorganization.ErrStartRejected), errors.Is(err, organization.ErrInvalidDecision):
			writeError(writer, http.StatusBadRequest, "INVALID_ARCHIVE_RUN_START")
		case errors.Is(err, command.ErrIdempotencyConflict):
			writeError(writer, http.StatusConflict, "ARCHIVE_RUN_START_CONFLICT")
		default:
			writeError(writer, http.StatusInternalServerError, "ARCHIVE_RUN_START_FAILED")
		}
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	writeJSON(writer, status, map[string]any{"data": map[string]any{
		"archive_run_id": result.Receipt.ID, "session_id": result.Receipt.SessionID,
		"workspace_drive_id": result.Receipt.WorkspaceDriveID, "input_revision": result.Receipt.InputRevision,
		"file_count": len(result.Receipt.Files), "folder_count": len(result.Receipt.Folders), "replayed": result.Replayed,
	}})
}

type archiveTranscriptionStartRequest struct {
	IdempotencyKey             string `json:"idempotency_key"`
	ExpectedArchiveRunRevision uint64 `json:"expected_archive_run_revision"`
}

func (handler *Handler) startArchiveTranscription(writer http.ResponseWriter, request *http.Request) {
	if !handler.allowMutation(writer, request) {
		return
	}
	var payload archiveTranscriptionStartRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	principal, _ := request.Context().Value(principalContextKey{}).(security.Principal)
	result, err := handler.archiveTranscriptions.Start(request.Context(), archivetranscription.StartRequest{
		ArchiveRunID: request.PathValue("archiveRunID"), ExpectedArchiveRunRevision: payload.ExpectedArchiveRunRevision,
		IdempotencyKey: payload.IdempotencyKey, Actor: command.Actor{Type: principal.ActorType, ID: principal.ID},
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		switch {
		case errors.Is(err, archivetranscription.ErrStageOneIncomplete), errors.Is(err, command.ErrIdempotencyConflict):
			writeError(writer, http.StatusConflict, "ARCHIVE_TRANSCRIPTION_NOT_READY")
		case errors.Is(err, archivetranscription.ErrInvalidRequest), errors.Is(err, archivetranscription.ErrInvalidStoredResult):
			writeError(writer, http.StatusBadRequest, "INVALID_ARCHIVE_TRANSCRIPTION_START")
		default:
			writeError(writer, http.StatusInternalServerError, "ARCHIVE_TRANSCRIPTION_START_FAILED")
		}
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"data": map[string]any{
		"archive_run_id": result.ArchiveRunID, "job_ids": result.JobIDs,
		"eligible_count": result.EligibleCount, "hold_count": result.HoldCount,
	}})
}
