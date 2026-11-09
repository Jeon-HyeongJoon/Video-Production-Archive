package httpcontrol

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/application/archiveorganization"
	"videoarchive/internal/application/archivetranscription"
	"videoarchive/internal/application/archivistcontrol"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/security"
	"videoarchive/internal/core/vocabulary"
)

func (handler *Handler) archivistHostStatuses(writer http.ResponseWriter, _ *http.Request) {
	profiles := handler.archivists.HostProfiles()
	result := make([]archivistHostStatusView, 0, len(profiles))
	for _, profile := range profiles {
		status := profile.HostStatus
		result = append(result, archivistHostStatusView{
			ArchivistHostID: status.ArchivistHostID, State: string(status.State), LastHeartbeat: status.LastHeartbeat,
			DetectedCLIIDs: append([]string(nil), profile.DetectedCLIIDs...),
		})
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": result})
}

func (handler *Handler) archivistSessions(writer http.ResponseWriter, request *http.Request) {
	projections, err := handler.archivists.Projection(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "QUERY_FAILED")
		return
	}
	result := make([]archivistSessionView, 0, len(projections))
	for _, projection := range projections {
		view := archivistSessionView{
			SessionID: projection.Session.ID, Name: projection.Session.Name, ArchivistHostID: projection.Session.ArchivistHostID,
			Backend: string(projection.Session.Backend), CLIID: projection.Session.CLIID,
			WorkspaceRootID: projection.Session.WorkspaceRootID,
			JobID:           projection.JobID, CreatedAt: projection.CreatedAt,
		}
		for _, turn := range projection.Turns {
			turnView := archivistTurnView{
				TurnID: turn.ID, RequestID: turn.RequestID, State: turn.State,
				Prompt: safeArchivistPrompt(turn.Prompt), SubmittedBy: string(turn.SubmittedBy),
				CreatedAt: turn.CreatedAt, UpdatedAt: turn.UpdatedAt, FinalResponse: turn.FinalResponse,
				Entries: turn.Entries, DriveSelection: turn.DriveSelection, WorkReference: turn.WorkReference, WorkContext: turn.WorkContext,
			}
			for _, call := range turn.ToolCalls {
				turnView.ToolCalls = append(turnView.ToolCalls, archivistToolCallView{
					ToolCallID: call.ID, ToolName: call.ToolName, TargetID: call.TargetID,
					State: call.State, UpdatedAt: call.UpdatedAt,
				})
			}
			view.Turns = append(view.Turns, turnView)
		}
		for _, card := range projection.ActionCards {
			view.ActionCards = append(view.ActionCards, actionCardView{
				ActionCardID: card.ID, ArchivistTurnID: card.ArchivistTurnID, CommandType: card.CommandType,
				TargetID: card.TargetID, ExpectedRevision: card.ExpectedRevision,
				State: card.State, ApprovalRequired: card.ApprovalRequired, Scope: actionCardScopeOf(card.Parameters),
				RejectedBy: card.RejectedBy, RejectedAt: card.RejectedAt, RejectionReason: card.RejectionReason,
			})
		}
		result = append(result, view)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": result})
}

func actionCardScopeOf(parameters map[string]any) *actionCardScope {
	scope := actionCardScope{}
	scope.WorkspaceDriveID, _ = parameters["expected_workspace_drive_id"].(string)
	scope.ArchiveStorageID, _ = parameters["archive_storage_id"].(string)
	switch folders := parameters["folder_ids"].(type) {
	case []string:
		scope.FolderCount = len(folders)
	case []any:
		scope.FolderCount = len(folders)
	}
	if scope == (actionCardScope{}) {
		return nil
	}
	return &scope
}

func safeArchivistPrompt(prompt string) string {
	trimmed := strings.TrimSpace(prompt)
	lower := strings.ToLower(trimmed)
	for _, unsafe := range []string{"raw terminal", "resume-token", "credential", "secret"} {
		if strings.Contains(lower, unsafe) {
			return "REDACTED"
		}
	}
	return trimmed
}

type createArchivistSessionRequest struct {
	SessionID       string `json:"session_id"`
	Name            string `json:"name"`
	ArchivistHostID string `json:"archivist_host_id"`
	Backend         string `json:"backend"`
	CLIID           string `json:"cli_id"`
	WorkspaceRootID string `json:"workspace_root_id"`
	JobID           string `json:"job_id"`
}

func (handler *Handler) createArchivistSession(writer http.ResponseWriter, request *http.Request) {
	if !handler.allowMutation(writer, request) {
		return
	}
	var payload createArchivistSessionRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	var record archivistcontrol.SessionRecord
	var err error
	if strings.TrimSpace(payload.Name) != "" {
		if strings.TrimSpace(payload.Backend) != "" || strings.TrimSpace(payload.WorkspaceRootID) != "" || strings.TrimSpace(payload.JobID) != "" {
			writeArchivistError(writer, archivistcontrol.ErrInvalidRequest)
			return
		}
		record, err = handler.archivists.CreateArchivist(request.Context(), payload.SessionID, payload.Name, payload.ArchivistHostID, payload.CLIID)
	} else {
		record, err = handler.archivists.CreateSession(request.Context(), archivistruntime.SessionSpec{
			ID: payload.SessionID, ArchivistHostID: payload.ArchivistHostID,
			Backend: vocabulary.RuntimeBackend(payload.Backend), CLIID: payload.CLIID, WorkspaceRootID: payload.WorkspaceRootID,
		}, payload.JobID)
	}
	if err != nil {
		writeArchivistError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"data": map[string]any{
		"session_id": record.Session.ID, "archivist_host_id": record.Session.ArchivistHostID,
		"name":    record.Session.Name,
		"backend": record.Session.Backend, "cli_id": record.Session.CLIID,
		"workspace_root_id": record.Session.WorkspaceRootID, "job_id": record.JobID,
	}})
}

type createArchivistTurnRequest struct {
	TurnID         string                           `json:"turn_id"`
	RequestID      string                           `json:"request_id"`
	Prompt         string                           `json:"prompt"`
	DriveSelection *archivistruntime.DriveSelection `json:"drive_selection,omitempty"`
	WorkReference  *archivistruntime.WorkReference  `json:"work_reference,omitempty"`
}

func (handler *Handler) createArchivistTurn(writer http.ResponseWriter, request *http.Request) {
	if !handler.allowMutation(writer, request) {
		return
	}
	var payload createArchivistTurnRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	if payload.DriveSelection != nil {
		principal, _ := request.Context().Value(principalContextKey{}).(security.Principal)
		copy := *payload.DriveSelection
		copy.SelectedBy = principal.ID
		payload.DriveSelection = &copy
	}
	record, err := handler.archivists.SubmitTurnWithReferences(
		request.Context(), request.PathValue("sessionID"), payload.TurnID, payload.RequestID, payload.Prompt, payload.DriveSelection, payload.WorkReference,
	)
	if err != nil {
		writeArchivistError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"data": map[string]any{
		"session_id": record.Turn.SessionID, "turn_id": record.Turn.ID,
		"request_id": record.Turn.RequestID, "state": record.Turn.State,
	}})
}

func (handler *Handler) executeActionCard(writer http.ResponseWriter, request *http.Request) {
	if !handler.allowMutation(writer, request) {
		return
	}
	var payload struct{}
	if !decodeJSON(writer, request, &payload) {
		return
	}
	principal, _ := request.Context().Value(principalContextKey{}).(security.Principal)
	cardID := request.PathValue("cardID")
	envelope, err := handler.archivists.PrepareActionCard(request.Context(), cardID, principal.ID)
	if err != nil {
		writeArchivistError(writer, err)
		return
	}
	completed := false
	defer func() {
		if !completed {
			_ = handler.archivists.ReleaseActionCard(context.WithoutCancel(request.Context()), cardID)
		}
	}()
	var response map[string]any
	switch envelope.CommandType {
	case archiveintakeworkspace.ClassifyHoldCommand:
		if handler.drives == nil {
			writeError(writer, http.StatusConflict, "ARCHIVE_INTAKE_UNAVAILABLE")
			return
		}
		decision, classifyErr := handler.drives.Override(request.Context(), archiveintakeworkspace.OverrideCommand{
			CommandID: envelope.CommandID, DecisionID: parameterString(envelope.Parameters, "decision_id"),
			IdempotencyKey: envelope.IdempotencyKey, ArchiveRunID: parameterString(envelope.Parameters, "archive_run_id"),
			WorkspaceDriveID: parameterString(envelope.Parameters, "workspace_drive_id"),
			Folder:           envelope.TargetID, ExpectedInputRevision: envelope.ExpectedRevision,
			Disposition:            vocabulary.OrganizationDisposition(parameterString(envelope.Parameters, "disposition")),
			TargetVideoBitRateKbps: parameterInt(envelope.Parameters, "target_video_bitrate_kbps"),
			Actor:                  envelope.Actor, Reason: envelope.Reason, Basis: parameterStrings(envelope.Parameters, "basis"),
			OccurredAt: time.Now().UTC(),
		})
		if classifyErr != nil {
			writeError(writer, http.StatusConflict, "ARCHIVE_HOLD_CLASSIFICATION_FAILED")
			return
		}
		response = map[string]any{"data": map[string]any{
			"decision_id": decision.ID, "decision_sequence": decision.Sequence,
			"input_revision": decision.InputRevision, "disposition": decision.Disposition,
		}}
	case archiveorganization.StartActionCardCommand:
		if handler.archiveRuns == nil {
			writeError(writer, http.StatusConflict, "ARCHIVE_RUN_UNAVAILABLE")
			return
		}
		result, startErr := handler.archiveRuns.Start(request.Context(), archiveorganization.StartRequest{
			ArchiveRunID: envelope.TargetID, IdempotencyKey: envelope.IdempotencyKey,
			SessionID:                 parameterString(envelope.Parameters, "session_id"),
			SourceVolumeID:            parameterString(envelope.Parameters, "source_volume_id"),
			ExpectedCandidateRevision: parameterUint64(envelope.Parameters, "expected_candidate_revision"),
			ArchiveStorageID:          parameterString(envelope.Parameters, "archive_storage_id"),
			InputRevision:             parameterUint64(envelope.Parameters, "input_revision"),
			FolderIDs:                 parameterStrings(envelope.Parameters, "folder_ids"),
			ExpectedWorkerID:          parameterString(envelope.Parameters, "expected_worker_id"),
			ExpectedSourceStorageID:   parameterString(envelope.Parameters, "expected_source_storage_id"),
			ExpectedWorkspaceDriveID:  parameterString(envelope.Parameters, "expected_workspace_drive_id"),
			Actor:                     envelope.Actor,
			SelectedBy:                command.Actor{Type: vocabulary.ActorHuman, ID: parameterString(envelope.Parameters, "selected_by")},
			StartedAt:                 time.Now().UTC(),
		})
		if startErr != nil {
			writeError(writer, http.StatusConflict, "ARCHIVE_RUN_START_FAILED")
			return
		}
		if bindErr := handler.archivists.BindActionCardArchiveRun(request.Context(), cardID, result.Receipt.ID, 1); bindErr != nil {
			writeError(writer, http.StatusInternalServerError, "ACTION_CARD_ARCHIVE_RUN_BIND_FAILED")
			return
		}
		response = map[string]any{"data": map[string]any{
			"archive_run_id": result.Receipt.ID, "folder_count": len(result.Receipt.Folders), "replayed": result.Replayed,
		}}
	case archivetranscription.StartActionCardCommand:
		if handler.archiveTranscriptions == nil {
			writeError(writer, http.StatusConflict, "ARCHIVE_TRANSCRIPTION_UNAVAILABLE")
			return
		}
		result, startErr := handler.archiveTranscriptions.Start(request.Context(), archivetranscription.StartRequest{
			ArchiveRunID: envelope.TargetID, ExpectedArchiveRunRevision: envelope.ExpectedRevision,
			IdempotencyKey: envelope.IdempotencyKey, Actor: envelope.Actor, StartedAt: time.Now().UTC(),
		})
		if startErr != nil {
			writeError(writer, http.StatusConflict, "ARCHIVE_TRANSCRIPTION_START_FAILED")
			return
		}
		response = map[string]any{"data": map[string]any{
			"archive_run_id": result.ArchiveRunID, "job_ids": result.JobIDs,
			"eligible_count": result.EligibleCount, "hold_count": result.HoldCount,
		}}
	default:
		if handler.commands == nil {
			writeError(writer, http.StatusConflict, "COMMAND_UNAVAILABLE")
			return
		}
		result, executeErr := handler.commands.Execute(request.Context(), envelope)
		if executeErr != nil {
			writeCommandError(writer, executeErr)
			return
		}
		response = commandResultView(result)
	}
	if err := handler.archivists.CompleteActionCard(request.Context(), cardID); err != nil {
		writeError(writer, http.StatusInternalServerError, "ACTION_CARD_UPDATE_FAILED")
		return
	}
	completed = true
	writeJSON(writer, http.StatusOK, response)
}

func (handler *Handler) rejectActionCard(writer http.ResponseWriter, request *http.Request) {
	if !handler.allowMutation(writer, request) {
		return
	}
	var payload struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(writer, request, &payload) {
		return
	}
	principal, _ := request.Context().Value(principalContextKey{}).(security.Principal)
	if err := handler.archivists.RejectActionCard(request.Context(), request.PathValue("cardID"), principal.ID, payload.Reason); err != nil {
		writeArchivistError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"action_card_id": request.PathValue("cardID"), "state": "REJECTED",
	}})
}

func parameterString(parameters map[string]any, key string) string {
	value, _ := parameters[key].(string)
	return strings.TrimSpace(value)
}

func parameterUint64(parameters map[string]any, key string) uint64 {
	switch value := parameters[key].(type) {
	case uint64:
		return value
	case int:
		if value > 0 {
			return uint64(value)
		}
	case float64:
		if value > 0 && value == float64(uint64(value)) {
			return uint64(value)
		}
	}
	return 0
}

func parameterInt(parameters map[string]any, key string) int {
	switch value := parameters[key].(type) {
	case int:
		return value
	case float64:
		if value == float64(int(value)) {
			return int(value)
		}
	}
	return 0
}

func parameterStrings(parameters map[string]any, key string) []string {
	switch values := parameters[key].(type) {
	case []string:
		return append([]string(nil), values...)
	case []any:
		result := make([]string, 0, len(values))
		for _, raw := range values {
			value, ok := raw.(string)
			if !ok || strings.TrimSpace(value) == "" {
				return nil
			}
			result = append(result, strings.TrimSpace(value))
		}
		return result
	default:
		return nil
	}
}

func writeArchivistError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, archivistruntime.ErrArchivistHostNotConfigured), errors.Is(err, archivistruntime.ErrInvalidSession),
		errors.Is(err, archivistruntime.ErrInvalidTurn), errors.Is(err, archivistcontrol.ErrInvalidRequest):
		writeError(writer, http.StatusBadRequest, "INVALID_ARCHIVIST_REQUEST")
	case errors.Is(err, archivistcontrol.ErrSessionNotFound):
		writeError(writer, http.StatusNotFound, "ARCHIVIST_SESSION_NOT_FOUND")
	default:
		writeError(writer, http.StatusConflict, "ARCHIVIST_REQUEST_CONFLICT")
	}
}
