package httpmcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	appintake "videoarchive/internal/application/archiveintake"
	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/application/archiveorganization"
	"videoarchive/internal/application/archivetranscription"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/application/archivistcontrol"
	"videoarchive/internal/application/systemstatus"
	"videoarchive/internal/application/workdiagnostics"
	"videoarchive/internal/application/workrecovery"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/catalog"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
)

const (
	ToolJobsGet                   = "archive.jobs.get.v1"
	ToolWorkReferenceGet          = "archive.work.reference.get.v1"
	ToolWorkDiagnosticsGet        = "archive.work.diagnostics.get.v1"
	ToolWorkRecoveryApply         = "archive.work.recovery.apply.v1"
	ToolSystemStatusGet           = "archive.system.status.get.v1"
	ToolAssetsSearch              = "archive.assets.search.v1"
	ToolJobsCreate                = "archive.jobs.create.v1"
	ToolIntakeCandidatesSearch    = "archive.intake.candidates.search.v1"
	ToolIntakeSourceSelect        = "archive.intake.source.select.v1"
	ToolIntakeSelectionPin        = "archive.intake.selection.pin.v1"
	ToolDriveSelectionGet         = "archive.intake.selection.get.v1"
	ToolArchiveRunStatus          = "archive.run.status.v1"
	ToolArchiveRunStart           = "archive.run.start.v1"
	ToolArchiveTranscriptionStart = "archive.run.transcription.start.v1"
	ToolOrganizationHoldsList     = "archive.organization.holds.list.v1"
	ToolOrganizationHoldGet       = "archive.organization.hold.get.v1"
	ToolOrganizationHoldClassify  = "archive.organization.hold.classify.v1"
)

type ArchivistRuntime interface {
	Authenticate(string, string) error
	ValidateToolContext(context.Context, string, string, string, uint64) error
	RecordToolCall(context.Context, string, string, string, string, string, string) error
	RecordActionCard(context.Context, string, archivistcontrol.ActionCardProjection) error
	TurnDriveSelection(context.Context, string, string) (archivistruntime.DriveSelection, error)
	TurnWorkReference(context.Context, string, string) (archivistruntime.WorkReference, error)
	BindDriveSelection(context.Context, string, string, archivistruntime.DriveSelection) error
	BindArchiveRun(context.Context, string, string, string, uint64) error
}

type CatalogReader interface {
	Search(context.Context, catalog.SearchQuery) ([]catalog.SearchResult, error)
}

type WorkDiagnosticsReader interface {
	Diagnose(context.Context, archivistruntime.WorkReference) (workdiagnostics.Snapshot, error)
}

type WorkRecoveryExecutor interface {
	Apply(context.Context, archivistruntime.WorkReference, command.Envelope) (workrecovery.Result, error)
}

type SystemStatusReader interface {
	Snapshot(context.Context) (systemstatus.Snapshot, error)
}

type turnPolicyReader interface {
	TurnToolPolicy(context.Context, string, string) (archivistruntime.ArchivistTurnToolPolicy, error)
}

type ArchiveWorkspaceReader interface {
	Detail(context.Context, string) (archiveworkspace.JobView, error)
}

type JobCommander interface {
	Execute(context.Context, command.Envelope) (command.Result, error)
}

type ArchiveIntakeWorkspace interface {
	Projection(context.Context) ([]archiveintakeworkspace.DriveView, error)
	Override(context.Context, archiveintakeworkspace.OverrideCommand) (organization.OrganizationDecision, error)
}

type ArchiveIntakeSelector interface {
	Select(context.Context, appintake.SelectRequest) (*appintake.SessionAccess, error)
}

type ArchiveRunStarter interface {
	Start(context.Context, archiveorganization.StartRequest) (archiveorganization.Result, error)
}

type ArchiveTranscriptionStarter interface {
	Start(context.Context, archivetranscription.StartRequest) (archivetranscription.Result, error)
}

type Configuration struct {
	Archivists            ArchivistRuntime
	Catalog               CatalogReader
	ArchiveWorkspace      ArchiveWorkspaceReader
	Commands              JobCommander
	Drives                ArchiveIntakeWorkspace
	IntakeSelector        ArchiveIntakeSelector
	ArchiveRuns           ArchiveRunStarter
	ArchiveTranscriptions ArchiveTranscriptionStarter
	WorkDiagnostics       WorkDiagnosticsReader
	WorkRecovery          WorkRecoveryExecutor
	SystemStatus          SystemStatusReader
}

type Handler struct {
	archivists            ArchivistRuntime
	catalog               CatalogReader
	archiveWorkspace      ArchiveWorkspaceReader
	commands              JobCommander
	drives                ArchiveIntakeWorkspace
	intakeSelector        ArchiveIntakeSelector
	archiveRuns           ArchiveRunStarter
	archiveTranscriptions ArchiveTranscriptionStarter
	workDiagnostics       WorkDiagnosticsReader
	workRecovery          WorkRecoveryExecutor
	systemStatus          SystemStatusReader
	router                http.Handler
}

func New(configuration Configuration) (*Handler, error) {
	driveCount := 0
	for _, configured := range []bool{configuration.Drives != nil, configuration.IntakeSelector != nil, configuration.ArchiveRuns != nil, configuration.ArchiveTranscriptions != nil} {
		if configured {
			driveCount++
		}
	}
	if configuration.Archivists == nil || configuration.Catalog == nil || configuration.ArchiveWorkspace == nil || configuration.Commands == nil ||
		(configuration.WorkRecovery != nil && configuration.WorkDiagnostics == nil) ||
		(driveCount != 0 && driveCount != 4) {
		return nil, errors.New("MCP adapter configuration is incomplete")
	}
	handler := &Handler{
		archivists: configuration.Archivists, catalog: configuration.Catalog,
		archiveWorkspace: configuration.ArchiveWorkspace, commands: configuration.Commands,
		drives: configuration.Drives, intakeSelector: configuration.IntakeSelector,
		archiveRuns: configuration.ArchiveRuns, archiveTranscriptions: configuration.ArchiveTranscriptions,
		workDiagnostics: configuration.WorkDiagnostics,
		workRecovery:    configuration.WorkRecovery,
		systemStatus:    configuration.SystemStatus,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /mcp/v1/tools", handler.tools)
	mux.HandleFunc("POST /mcp/v1/call", handler.call)
	handler.router = mux
	return handler, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	hostID, credential, ok := identity(request)
	if !ok || handler.archivists.Authenticate(hostID, credential) != nil {
		writeError(writer, http.StatusUnauthorized, "MCP_AUTHENTICATION_FAILED")
		return
	}
	request = request.WithContext(context.WithValue(request.Context(), hostContextKey{}, hostID))
	handler.router.ServeHTTP(writer, request)
}

type hostContextKey struct{}

func (handler *Handler) tools(writer http.ResponseWriter, _ *http.Request) {
	result := []map[string]string{
		{"name": ToolJobsGet, "action_class": string(vocabulary.ActionRead)},
		{"name": ToolWorkReferenceGet, "action_class": string(vocabulary.ActionRead)},
		{"name": ToolAssetsSearch, "action_class": string(vocabulary.ActionRead)},
		{"name": ToolJobsCreate, "action_class": string(vocabulary.ActionProcess)},
	}
	if handler.workDiagnostics != nil {
		result = append(result, map[string]string{"name": ToolWorkDiagnosticsGet, "action_class": string(vocabulary.ActionRead)})
	}
	if handler.systemStatus != nil {
		result = append(result, map[string]string{"name": ToolSystemStatusGet, "action_class": string(vocabulary.ActionRead)})
	}
	if handler.workDiagnostics != nil && handler.workRecovery != nil {
		result = append(result, map[string]string{"name": ToolWorkRecoveryApply, "action_class": string(vocabulary.ActionProcess)})
	}
	if handler.drives != nil && handler.archiveRuns != nil && handler.archiveTranscriptions != nil {
		result = append(result,
			map[string]string{"name": ToolIntakeCandidatesSearch, "action_class": string(vocabulary.ActionRead)},
			map[string]string{"name": ToolIntakeSourceSelect, "action_class": string(vocabulary.ActionProcess)},
			map[string]string{"name": ToolIntakeSelectionPin, "action_class": string(vocabulary.ActionProcess)},
			map[string]string{"name": ToolDriveSelectionGet, "action_class": string(vocabulary.ActionRead)},
			map[string]string{"name": ToolArchiveRunStatus, "action_class": string(vocabulary.ActionRead)},
			map[string]string{"name": ToolOrganizationHoldsList, "action_class": string(vocabulary.ActionRead)},
			map[string]string{"name": ToolOrganizationHoldGet, "action_class": string(vocabulary.ActionRead)},
			map[string]string{"name": ToolOrganizationHoldClassify, "action_class": string(vocabulary.ActionProcess)},
			map[string]string{"name": ToolArchiveRunStart, "action_class": string(vocabulary.ActionProcess)},
			map[string]string{"name": ToolArchiveTranscriptionStart, "action_class": string(vocabulary.ActionProcess)},
		)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": result})
}

type callRequest struct {
	SessionID      string          `json:"session_id"`
	TurnID         string          `json:"turn_id"`
	FencingToken   uint64          `json:"fencing_token"`
	ToolCallID     string          `json:"tool_call_id"`
	CommandID      string          `json:"command_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	InputDigest    string          `json:"input_digest"`
	Tool           string          `json:"tool"`
	Input          json.RawMessage `json:"input"`
}

func (handler *Handler) call(writer http.ResponseWriter, request *http.Request) {
	var payload callRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	hostID, _ := request.Context().Value(hostContextKey{}).(string)
	if err := handler.archivists.ValidateToolContext(request.Context(), hostID, payload.SessionID, payload.TurnID, payload.FencingToken); err != nil {
		writeError(writer, http.StatusConflict, "MCP_TURN_CONTEXT_INVALID")
		return
	}
	policy := archivistruntime.ToolPolicyStandard
	if reader, ok := handler.archivists.(turnPolicyReader); ok {
		resolved, err := reader.TurnToolPolicy(request.Context(), payload.SessionID, payload.TurnID)
		if err != nil {
			writeError(writer, http.StatusConflict, "MCP_TURN_CONTEXT_INVALID")
			return
		}
		policy = resolved
	}
	if payload.Tool != ToolJobsGet && payload.Tool != ToolWorkReferenceGet && payload.Tool != ToolAssetsSearch && payload.Tool != ToolJobsCreate &&
		payload.Tool != ToolWorkDiagnosticsGet &&
		payload.Tool != ToolSystemStatusGet &&
		payload.Tool != ToolWorkRecoveryApply &&
		payload.Tool != ToolIntakeCandidatesSearch && payload.Tool != ToolIntakeSourceSelect && payload.Tool != ToolIntakeSelectionPin &&
		payload.Tool != ToolDriveSelectionGet && payload.Tool != ToolArchiveRunStatus &&
		payload.Tool != ToolOrganizationHoldsList && payload.Tool != ToolOrganizationHoldGet &&
		payload.Tool != ToolOrganizationHoldClassify && payload.Tool != ToolArchiveRunStart && payload.Tool != ToolArchiveTranscriptionStart {
		writeError(writer, http.StatusNotFound, "MCP_CAPABILITY_NOT_FOUND")
		return
	}
	if policy == archivistruntime.ToolPolicyAdvisory && !advisoryToolAllowed(payload.Tool) {
		_ = handler.archivists.RecordToolCall(request.Context(), payload.SessionID, payload.TurnID, payload.ToolCallID, payload.Tool, "", "FAILED")
		writeError(writer, http.StatusForbidden, "MCP_ADVISORY_POLICY_DENIED")
		return
	}
	if policy == archivistruntime.ToolPolicyRemediation && !remediationToolAllowed(payload.Tool) {
		_ = handler.archivists.RecordToolCall(request.Context(), payload.SessionID, payload.TurnID, payload.ToolCallID, payload.Tool, "", "FAILED")
		writeError(writer, http.StatusForbidden, "MCP_REMEDIATION_POLICY_DENIED")
		return
	}
	if err := handler.archivists.RecordToolCall(request.Context(), payload.SessionID, payload.TurnID, payload.ToolCallID, payload.Tool, "", "RUNNING"); err != nil {
		writeError(writer, http.StatusBadRequest, "MCP_INPUT_INVALID")
		return
	}
	switch payload.Tool {
	case ToolJobsGet:
		handler.getJob(writer, request, payload)
	case ToolWorkReferenceGet:
		handler.getWorkReference(writer, request, payload)
	case ToolWorkDiagnosticsGet:
		handler.getWorkDiagnostics(writer, request, payload)
	case ToolSystemStatusGet:
		handler.getSystemStatus(writer, request, payload)
	case ToolWorkRecoveryApply:
		handler.applyWorkRecovery(writer, request, hostID, payload)
	case ToolAssetsSearch:
		handler.searchAssets(writer, request, payload)
	case ToolJobsCreate:
		handler.createJob(writer, request, hostID, payload)
	case ToolIntakeCandidatesSearch:
		handler.searchArchiveIntakeCandidates(writer, request, payload)
	case ToolIntakeSourceSelect:
		handler.selectArchiveIntakeSource(writer, request, payload)
	case ToolIntakeSelectionPin:
		handler.pinArchiveIntakeSelection(writer, request, payload)
	case ToolDriveSelectionGet:
		handler.getArchiveIntakeSelection(writer, request, payload)
	case ToolArchiveRunStatus:
		handler.getArchiveRunStatus(writer, request, payload)
	case ToolOrganizationHoldsList:
		handler.listOrganizationHolds(writer, request, payload)
	case ToolOrganizationHoldGet:
		handler.getOrganizationHold(writer, request, payload)
	case ToolOrganizationHoldClassify:
		handler.classifyOrganizationHold(writer, request, payload)
	case ToolArchiveRunStart:
		handler.startArchiveRun(writer, request, payload, policy)
	case ToolArchiveTranscriptionStart:
		handler.startArchiveTranscription(writer, request, payload)
	}
}

func remediationToolAllowed(tool string) bool {
	switch tool {
	case ToolJobsGet, ToolWorkReferenceGet, ToolWorkDiagnosticsGet, ToolSystemStatusGet, ToolArchiveRunStatus, ToolWorkRecoveryApply:
		return true
	default:
		return false
	}
}

func advisoryToolAllowed(tool string) bool {
	switch tool {
	case ToolJobsGet, ToolWorkReferenceGet, ToolWorkDiagnosticsGet, ToolSystemStatusGet, ToolAssetsSearch,
		ToolIntakeCandidatesSearch, ToolIntakeSourceSelect, ToolIntakeSelectionPin,
		ToolDriveSelectionGet, ToolArchiveRunStatus, ToolOrganizationHoldsList,
		ToolOrganizationHoldGet, ToolArchiveRunStart:
		return true
	default:
		return false
	}
}

func (handler *Handler) getWorkReference(writer http.ResponseWriter, request *http.Request, call callRequest) {
	if !validEmptyInput(call.Input) {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, "", "FAILED")
		writeError(writer, http.StatusBadRequest, "MCP_INPUT_INVALID")
		return
	}
	reference, err := handler.archivists.TurnWorkReference(request.Context(), call.SessionID, call.TurnID)
	if errors.Is(err, archivistcontrol.ErrWorkReferenceNotBound) {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, "", "COMPLETED")
		writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{"bound": false}})
		return
	}
	if err != nil {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, "", "FAILED")
		if errors.Is(err, archivistcontrol.ErrInvalidRequest) || errors.Is(err, archivistcontrol.ErrTurnNotFound) {
			writeError(writer, http.StatusConflict, "MCP_TURN_CONTEXT_INVALID")
		} else {
			writeError(writer, http.StatusInternalServerError, "MCP_WORK_REFERENCE_LOOKUP_FAILED")
		}
		return
	}
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, reference.WorkID, "COMPLETED")
	writeJSON(writer, http.StatusOK, map[string]any{"data": struct {
		Bound bool `json:"bound"`
		archivistruntime.WorkReference
	}{Bound: true, WorkReference: reference}})
}

func (handler *Handler) getWorkDiagnostics(writer http.ResponseWriter, request *http.Request, call callRequest) {
	if handler.workDiagnostics == nil {
		writeError(writer, http.StatusNotFound, "MCP_CAPABILITY_NOT_FOUND")
		return
	}
	if !validEmptyInput(call.Input) {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, "", "FAILED")
		writeError(writer, http.StatusBadRequest, "MCP_INPUT_INVALID")
		return
	}
	reference, err := handler.archivists.TurnWorkReference(request.Context(), call.SessionID, call.TurnID)
	if err != nil {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, "", "FAILED")
		writeError(writer, http.StatusConflict, "MCP_WORK_REFERENCE_REQUIRED")
		return
	}
	diagnostic, err := handler.workDiagnostics.Diagnose(request.Context(), reference)
	if err != nil {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, reference.WorkID, "FAILED")
		writeError(writer, http.StatusConflict, "MCP_WORK_DIAGNOSTICS_UNAVAILABLE")
		return
	}
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, reference.WorkID, "COMPLETED")
	writeJSON(writer, http.StatusOK, map[string]any{"data": workdiagnostics.Sanitize(diagnostic)})
}

func (handler *Handler) getSystemStatus(writer http.ResponseWriter, request *http.Request, call callRequest) {
	if handler.systemStatus == nil {
		writeError(writer, http.StatusNotFound, "MCP_CAPABILITY_NOT_FOUND")
		return
	}
	if !validEmptyInput(call.Input) {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, "", "FAILED")
		writeError(writer, http.StatusBadRequest, "MCP_INPUT_INVALID")
		return
	}
	snapshot, err := handler.systemStatus.Snapshot(request.Context())
	if err != nil {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, "", "FAILED")
		writeError(writer, http.StatusInternalServerError, "MCP_SYSTEM_STATUS_UNAVAILABLE")
		return
	}
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, "", "COMPLETED")
	writeJSON(writer, http.StatusOK, map[string]any{"data": snapshot})
}

func (handler *Handler) applyWorkRecovery(writer http.ResponseWriter, request *http.Request, hostID string, call callRequest) {
	if handler.workRecovery == nil || handler.workDiagnostics == nil {
		writeError(writer, http.StatusNotFound, "MCP_CAPABILITY_NOT_FOUND")
		return
	}
	var input struct {
		ActionCode            string   `json:"action_code"`
		JobID                 string   `json:"job_id"`
		ExpectedJobRevision   uint64   `json:"expected_job_revision"`
		StepID                string   `json:"step_id"`
		ReservationID         string   `json:"reservation_id"`
		FencingToken          uint64   `json:"fencing_token"`
		DiagnosticFingerprint string   `json:"diagnostic_fingerprint"`
		Basis                 []string `json:"basis"`
		Reason                string   `json:"reason"`
	}
	if !decodeToolInput(call.Input, &input) {
		handler.failRecoveryTool(writer, request, call, http.StatusBadRequest, "MCP_INPUT_INVALID", "")
		return
	}
	input.ActionCode = strings.TrimSpace(input.ActionCode)
	input.JobID = strings.TrimSpace(input.JobID)
	input.StepID = strings.TrimSpace(input.StepID)
	input.ReservationID = strings.TrimSpace(input.ReservationID)
	input.DiagnosticFingerprint = strings.TrimSpace(input.DiagnosticFingerprint)
	input.Reason = strings.TrimSpace(input.Reason)
	expiredExecution := input.ActionCode == "RECONCILE_EXPIRED_EXECUTION" && input.ReservationID != ""
	terminalPublication := input.ActionCode == job.RecoveryActionRetryTerminalArchivePublication && input.ReservationID == ""
	if (!expiredExecution && !terminalPublication) || input.JobID == "" || input.ExpectedJobRevision == 0 ||
		input.StepID == "" || input.FencingToken == 0 || input.DiagnosticFingerprint == "" ||
		len(input.Basis) == 0 || input.Reason == "" || len(input.Reason) > 256 {
		handler.failRecoveryTool(writer, request, call, http.StatusBadRequest, "MCP_INPUT_INVALID", input.JobID)
		return
	}
	reference, err := handler.archivists.TurnWorkReference(request.Context(), call.SessionID, call.TurnID)
	if err != nil {
		handler.failRecoveryTool(writer, request, call, http.StatusConflict, "MCP_WORK_REFERENCE_REQUIRED", input.JobID)
		return
	}
	commandType := job.RecoverExpiredExecutionCommand
	if terminalPublication {
		commandType = workrecovery.CreateTerminalRecoveryCommand
	}
	decision, err := archivistruntime.PrepareToolCall(archivistruntime.ToolCallRequest{
		ID: call.ToolCallID, CommandID: call.CommandID, ArchivistHostID: hostID,
		ArchivistSessionID: call.SessionID, ArchivistTurnID: call.TurnID, ArchivistID: "archivist:" + call.SessionID,
		FencingToken: call.FencingToken, ToolName: commandType,
		InputDigest: call.InputDigest, IdempotencyKey: call.IdempotencyKey,
		TargetID: input.JobID, ExpectedRevision: input.ExpectedJobRevision, Reason: input.Reason,
		Parameters: map[string]any{
			"action_code": input.ActionCode, "step_id": input.StepID, "reservation_id": input.ReservationID,
			"fencing_token": input.FencingToken, "diagnostic_fingerprint": input.DiagnosticFingerprint,
			"basis": append([]string(nil), input.Basis...),
		},
	}, nil)
	if err != nil || decision.Command == nil {
		handler.failRecoveryTool(writer, request, call, http.StatusBadRequest, "MCP_INPUT_INVALID", input.JobID)
		return
	}
	result, err := handler.workRecovery.Apply(request.Context(), reference, *decision.Command)
	if err != nil {
		status := http.StatusConflict
		code := "MCP_WORK_RECOVERY_REJECTED"
		if !errors.Is(err, workrecovery.ErrRecoveryNotSafe) && !errors.Is(err, command.ErrRevisionConflict) &&
			!errors.Is(err, command.ErrIdempotencyConflict) {
			status, code = http.StatusInternalServerError, "MCP_WORK_RECOVERY_FAILED"
		}
		handler.failRecoveryTool(writer, request, call, status, code, input.JobID)
		return
	}
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, input.JobID, "COMPLETED")
	writeJSON(writer, http.StatusOK, map[string]any{"data": result})
}

func (handler *Handler) failRecoveryTool(writer http.ResponseWriter, request *http.Request, call callRequest, status int, code, targetID string) {
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, strings.TrimSpace(targetID), "FAILED")
	writeError(writer, status, code)
}

func (handler *Handler) searchArchiveIntakeCandidates(writer http.ResponseWriter, request *http.Request, call callRequest) {
	var input struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if !decodeToolInput(call.Input, &input) {
		handler.failIntakeTool(writer, request, call, http.StatusBadRequest, "MCP_INPUT_INVALID", "")
		return
	}
	input.Query = strings.TrimSpace(input.Query)
	if len(input.Query) > 256 || input.Limit < 0 || input.Limit > 50 {
		handler.failIntakeTool(writer, request, call, http.StatusBadRequest, "MCP_INPUT_INVALID", "")
		return
	}
	if input.Limit == 0 {
		input.Limit = 20
	}
	drives, err := handler.drives.Projection(request.Context())
	if err != nil {
		handler.failIntakeTool(writer, request, call, http.StatusInternalServerError, "MCP_INTAKE_CANDIDATES_FAILED", "")
		return
	}
	tokens := strings.Fields(strings.ToLower(input.Query))
	result := make([]map[string]any, 0, len(drives))
	for _, drive := range drives {
		if drive.ConnectionState != vocabulary.VolumeConnected {
			continue
		}
		folders := make([]map[string]any, 0)
		for _, folder := range drive.DiscoveredFolders {
			if !matchesAllTokens(folder.Folder, tokens) {
				continue
			}
			folders = append(folders, map[string]any{
				"folder_id": folder.Folder, "video_count": folder.VideoCount,
				"estimated_output_bytes": folder.EstimatedOutputBytes,
			})
			if len(folders) == input.Limit {
				break
			}
		}
		archives := make([]map[string]any, 0)
		for _, storage := range drive.StorageCandidates {
			if storage.Role != vocabulary.StorageArchivePrimary || !storage.Accessible || storage.StorageID == drive.SourceStorageID {
				continue
			}
			archives = append(archives, map[string]any{
				"storage_id": storage.StorageID, "access_kind": storage.AccessKind,
				"label": storage.Label, "file_system": storage.FileSystem,
				"capacity_bytes": storage.CapacityBytes, "free_bytes": storage.FreeBytes,
			})
		}
		workerID := drive.ConnectionWorkerID
		if workerID == "" {
			workerID = drive.WorkerID
		}
		result = append(result, map[string]any{
			"workspace_drive_id": drive.WorkspaceDriveID, "worker_id": workerID,
			"source_storage_id": drive.SourceStorageID, "candidate_revision": drive.CandidateRevision,
			"selection_session_id":      drive.SelectionSessionID,
			"folder_discovery_revision": drive.FolderDiscoveryRevision,
			"folder_discovery_state":    drive.FolderDiscoveryState,
			"matched_folders":           folders, "archive_storage_candidates": archives,
		})
	}
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, "", "COMPLETED")
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{"drives": result}})
}

func (handler *Handler) selectArchiveIntakeSource(writer http.ResponseWriter, request *http.Request, call callRequest) {
	var input struct {
		WorkspaceDriveID          string `json:"workspace_drive_id"`
		ExpectedCandidateRevision uint64 `json:"expected_candidate_revision"`
	}
	if !decodeToolInput(call.Input, &input) || strings.TrimSpace(input.WorkspaceDriveID) == "" || input.ExpectedCandidateRevision == 0 {
		handler.failIntakeTool(writer, request, call, http.StatusBadRequest, "MCP_INPUT_INVALID", "")
		return
	}
	input.WorkspaceDriveID = strings.TrimSpace(input.WorkspaceDriveID)
	drive, found, err := handler.currentCandidateDrive(request.Context(), input.WorkspaceDriveID)
	if err != nil {
		handler.failIntakeTool(writer, request, call, http.StatusInternalServerError, "MCP_INTAKE_CANDIDATES_FAILED", input.WorkspaceDriveID)
		return
	}
	if !found || drive.CandidateRevision != input.ExpectedCandidateRevision {
		handler.failIntakeTool(writer, request, call, http.StatusConflict, "MCP_INTAKE_CANDIDATE_STALE", input.WorkspaceDriveID)
		return
	}
	sessionID := stableToolTarget("INTAKE-MCP", call.SessionID, call.TurnID, input.WorkspaceDriveID)
	if drive.SelectionSessionID != sessionID {
		access, selectErr := handler.intakeSelector.Select(request.Context(), appintake.SelectRequest{
			SessionID: sessionID, VolumeID: input.WorkspaceDriveID, ExpectedCandidateRevision: input.ExpectedCandidateRevision,
			Actor: command.Actor{Type: vocabulary.ActorArchivist, ID: "archivist:" + call.SessionID}, SelectedAt: time.Now().UTC(),
		})
		if selectErr != nil {
			handler.failIntakeTool(writer, request, call, http.StatusConflict, "MCP_INTAKE_SOURCE_SELECTION_REJECTED", input.WorkspaceDriveID)
			return
		}
		if access.Root != nil {
			_ = access.Root.Close()
		}
	}
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, sessionID, "COMPLETED")
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"intake_session_id": sessionID, "workspace_drive_id": input.WorkspaceDriveID,
		"candidate_revision": input.ExpectedCandidateRevision,
	}})
}

func (handler *Handler) pinArchiveIntakeSelection(writer http.ResponseWriter, request *http.Request, call callRequest) {
	var input struct {
		WorkspaceDriveID        string   `json:"workspace_drive_id"`
		WorkerID                string   `json:"worker_id"`
		IntakeSessionID         string   `json:"intake_session_id"`
		SourceStorageID         string   `json:"source_storage_id"`
		ArchiveStorageID        string   `json:"archive_storage_id"`
		CandidateRevision       uint64   `json:"candidate_revision"`
		FolderDiscoveryRevision uint64   `json:"folder_discovery_revision"`
		FolderIDs               []string `json:"folder_ids"`
	}
	if !decodeToolInput(call.Input, &input) {
		handler.failIntakeTool(writer, request, call, http.StatusBadRequest, "MCP_INPUT_INVALID", "")
		return
	}
	drive, found, err := handler.currentCandidateDrive(request.Context(), strings.TrimSpace(input.WorkspaceDriveID))
	if err != nil {
		handler.failIntakeTool(writer, request, call, http.StatusInternalServerError, "MCP_INTAKE_CANDIDATES_FAILED", "")
		return
	}
	workerID := drive.ConnectionWorkerID
	if workerID == "" {
		workerID = drive.WorkerID
	}
	if !found || drive.CandidateRevision != input.CandidateRevision || drive.FolderDiscoveryRevision != input.FolderDiscoveryRevision ||
		drive.FolderDiscoveryState != vocabulary.FolderDiscoveryReady || workerID != strings.TrimSpace(input.WorkerID) ||
		drive.SourceStorageID != strings.TrimSpace(input.SourceStorageID) || drive.SelectionSessionID != strings.TrimSpace(input.IntakeSessionID) ||
		len(input.FolderIDs) == 0 {
		handler.failIntakeTool(writer, request, call, http.StatusConflict, "MCP_INTAKE_SELECTION_STALE", input.WorkspaceDriveID)
		return
	}
	availableFolders := make(map[string]archiveintakeworkspace.FolderView, len(drive.DiscoveredFolders))
	for _, folder := range drive.DiscoveredFolders {
		availableFolders[folder.Folder] = folder
	}
	var selectedBytes uint64
	seenFolders := make(map[string]bool, len(input.FolderIDs))
	for _, folderID := range input.FolderIDs {
		folderID = strings.TrimSpace(strings.ReplaceAll(folderID, "\\", "/"))
		folder, ok := availableFolders[folderID]
		if !ok || seenFolders[folderID] {
			handler.failIntakeTool(writer, request, call, http.StatusConflict, "MCP_INTAKE_SELECTION_OUTSIDE_CANDIDATES", folderID)
			return
		}
		seenFolders[folderID] = true
		selectedBytes += folder.EstimatedOutputBytes
	}
	archiveFound := false
	for _, storage := range drive.StorageCandidates {
		if storage.StorageID != strings.TrimSpace(input.ArchiveStorageID) {
			continue
		}
		archiveFound = storage.Role == vocabulary.StorageArchivePrimary && storage.Accessible && storage.StorageID != drive.SourceStorageID &&
			archiveorganization.ValidateArchiveCapacity(storage.StorageID, selectedBytes, storage.FreeBytes) == nil
		break
	}
	if !archiveFound {
		handler.failIntakeTool(writer, request, call, http.StatusConflict, "MCP_ARCHIVE_STORAGE_INELIGIBLE", input.ArchiveStorageID)
		return
	}
	selection, err := archivistruntime.NewDriveSelection(archivistruntime.DriveSelection{
		WorkerID: workerID, WorkspaceDriveID: drive.WorkspaceDriveID, IntakeSessionID: input.IntakeSessionID,
		SourceVolumeID: drive.WorkspaceDriveID, SourceStorageID: drive.SourceStorageID, ArchiveStorageID: input.ArchiveStorageID,
		CandidateRevision: drive.CandidateRevision, FolderDiscoveryRevision: drive.FolderDiscoveryRevision,
		FolderIDs: input.FolderIDs, SelectedByType: vocabulary.ActorArchivist, SelectedBy: "archivist:" + call.SessionID,
	})
	if err != nil || handler.archivists.BindDriveSelection(request.Context(), call.SessionID, call.TurnID, selection) != nil {
		handler.failIntakeTool(writer, request, call, http.StatusConflict, "MCP_INTAKE_SELECTION_CONFLICT", input.WorkspaceDriveID)
		return
	}
	runID := stableToolTarget("ARCHIVE-RUN", call.SessionID, call.TurnID, strings.Join(selection.FolderIDs, "\x00"))
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, drive.WorkspaceDriveID, "COMPLETED")
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"selection": selection, "archive_run_id": runID, "estimated_output_bytes": selectedBytes,
	}})
}

func (handler *Handler) currentCandidateDrive(ctx context.Context, workspaceDriveID string) (archiveintakeworkspace.DriveView, bool, error) {
	drives, err := handler.drives.Projection(ctx)
	if err != nil {
		return archiveintakeworkspace.DriveView{}, false, err
	}
	for _, drive := range drives {
		if drive.WorkspaceDriveID == workspaceDriveID && drive.ConnectionState == vocabulary.VolumeConnected {
			return drive, true, nil
		}
	}
	return archiveintakeworkspace.DriveView{}, false, nil
}

func matchesAllTokens(value string, tokens []string) bool {
	value = strings.ToLower(value)
	for _, token := range tokens {
		if !strings.Contains(value, token) {
			return false
		}
	}
	return true
}

func stableToolTarget(prefix string, parts ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + "-" + hex.EncodeToString(digest[:12])
}

func decodeToolInput(raw json.RawMessage, target any) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return false
	}
	return decoder.Decode(&struct{}{}) == io.EOF
}

func (handler *Handler) failIntakeTool(writer http.ResponseWriter, request *http.Request, call callRequest, status int, code, targetID string) {
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, strings.TrimSpace(targetID), "FAILED")
	writeError(writer, status, code)
}

func (handler *Handler) archiveIntakeSelection(ctx context.Context, call callRequest) (archivistruntime.DriveSelection, error) {
	if handler.drives == nil || handler.archiveRuns == nil || handler.archiveTranscriptions == nil {
		return archivistruntime.DriveSelection{}, errors.New("drive capabilities are unavailable")
	}
	return handler.archivists.TurnDriveSelection(ctx, call.SessionID, call.TurnID)
}

func (handler *Handler) getArchiveIntakeSelection(writer http.ResponseWriter, request *http.Request, call callRequest) {
	selection, err := handler.archiveIntakeSelection(request.Context(), call)
	if errors.Is(err, archivistcontrol.ErrDriveSelectionNotBound) {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, "", "COMPLETED")
		writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{"bound": false}})
		return
	}
	if err != nil {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, "", "FAILED")
		if errors.Is(err, archivistcontrol.ErrInvalidRequest) || errors.Is(err, archivistcontrol.ErrTurnNotFound) {
			writeError(writer, http.StatusConflict, "MCP_TURN_CONTEXT_INVALID")
		} else {
			writeError(writer, http.StatusInternalServerError, "MCP_DRIVE_SELECTION_LOOKUP_FAILED")
		}
		return
	}
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, selection.ArchiveRunID, "COMPLETED")
	writeJSON(writer, http.StatusOK, map[string]any{"data": struct {
		Bound bool `json:"bound"`
		archivistruntime.DriveSelection
	}{Bound: true, DriveSelection: selection}})
}

func (handler *Handler) getArchiveRunStatus(writer http.ResponseWriter, request *http.Request, call callRequest) {
	selection, err := handler.archiveIntakeSelection(request.Context(), call)
	if err != nil {
		writeError(writer, http.StatusConflict, "MCP_DRIVE_SELECTION_REQUIRED")
		return
	}
	drives, err := handler.drives.Projection(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "MCP_DRIVE_STATUS_FAILED")
		return
	}
	for _, drive := range drives {
		if drive.WorkspaceDriveID == selection.WorkspaceDriveID {
			_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, selection.ArchiveRunID, "COMPLETED")
			writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
				"workspace_drive_id": drive.WorkspaceDriveID, "worker_id": drive.WorkerID, "state": drive.State,
				"input_revision": drive.InputRevision, "file_count": drive.FileCount,
				"folder_count": drive.FolderCount, "folders": drive.Folders,
			}})
			return
		}
	}
	writeError(writer, http.StatusNotFound, "MCP_DRIVE_STATUS_NOT_FOUND")
}

func (handler *Handler) selectedDriveProjection(ctx context.Context, call callRequest) (archivistruntime.DriveSelection, archiveintakeworkspace.DriveView, error) {
	selection, err := handler.archiveIntakeSelection(ctx, call)
	if err != nil {
		return archivistruntime.DriveSelection{}, archiveintakeworkspace.DriveView{}, err
	}
	drives, err := handler.drives.Projection(ctx)
	if err != nil {
		return archivistruntime.DriveSelection{}, archiveintakeworkspace.DriveView{}, err
	}
	for _, drive := range drives {
		if drive.WorkspaceDriveID == selection.WorkspaceDriveID &&
			(selection.ArchiveRunID == "" || drive.ArchiveRunID == selection.ArchiveRunID) {
			return selection, drive, nil
		}
	}
	return archivistruntime.DriveSelection{}, archiveintakeworkspace.DriveView{}, errors.New("selected archive run is unavailable")
}

func selectedHold(selection archivistruntime.DriveSelection, drive archiveintakeworkspace.DriveView, folder string) (archiveintakeworkspace.FolderView, bool) {
	folder = strings.TrimSpace(folder)
	selected := false
	for _, selectedFolder := range selection.FolderIDs {
		if selectedFolder == folder {
			selected = true
			break
		}
	}
	if !selected {
		return archiveintakeworkspace.FolderView{}, false
	}
	for _, candidate := range drive.Folders {
		if candidate.Folder == folder && candidate.Disposition == vocabulary.OrganizationHold {
			return candidate, true
		}
	}
	return archiveintakeworkspace.FolderView{}, false
}

func (handler *Handler) listOrganizationHolds(writer http.ResponseWriter, request *http.Request, call callRequest) {
	if !validEmptyInput(call.Input) {
		handler.failHoldTool(writer, request, call, http.StatusBadRequest, "MCP_INPUT_INVALID", "")
		return
	}
	selection, drive, err := handler.selectedDriveProjection(request.Context(), call)
	if err != nil {
		handler.failHoldTool(writer, request, call, http.StatusConflict, "MCP_HOLD_SELECTION_REQUIRED", "")
		return
	}
	holds := make([]map[string]any, 0)
	for _, folderID := range selection.FolderIDs {
		folder, ok := selectedHold(selection, drive, folderID)
		if !ok {
			continue
		}
		holds = append(holds, map[string]any{
			"folder": folder.Folder, "decision_id": folder.DecisionID,
			"decision_sequence": folder.DecisionSequence, "input_revision": folder.InputRevision,
			"hold_reasons": folder.HoldReasons, "video_count": folder.VideoCount,
		})
	}
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, selection.ArchiveRunID, "COMPLETED")
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"archive_run_id": selection.ArchiveRunID, "workspace_drive_id": selection.WorkspaceDriveID, "holds": holds,
	}})
}

func (handler *Handler) getOrganizationHold(writer http.ResponseWriter, request *http.Request, call callRequest) {
	var input struct {
		Folder string `json:"folder"`
	}
	if json.Unmarshal(call.Input, &input) != nil || strings.TrimSpace(input.Folder) == "" {
		handler.failHoldTool(writer, request, call, http.StatusBadRequest, "MCP_INPUT_INVALID", "")
		return
	}
	selection, drive, err := handler.selectedDriveProjection(request.Context(), call)
	if err != nil {
		handler.failHoldTool(writer, request, call, http.StatusConflict, "MCP_HOLD_SELECTION_REQUIRED", input.Folder)
		return
	}
	folder, ok := selectedHold(selection, drive, input.Folder)
	if !ok {
		handler.failHoldTool(writer, request, call, http.StatusNotFound, "MCP_HOLD_NOT_FOUND", input.Folder)
		return
	}
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, folder.DecisionID, "COMPLETED")
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"archive_run_id": selection.ArchiveRunID, "workspace_drive_id": selection.WorkspaceDriveID,
		"folder": folder.Folder, "decision_id": folder.DecisionID, "decision_sequence": folder.DecisionSequence,
		"input_revision": folder.InputRevision, "hold_reasons": folder.HoldReasons,
		"assets": folder.ClassificationAssets,
	}})
}

func (handler *Handler) classifyOrganizationHold(writer http.ResponseWriter, request *http.Request, call callRequest) {
	var input struct {
		Folder                 string   `json:"folder"`
		ExpectedInputRevision  uint64   `json:"expected_input_revision"`
		Disposition            string   `json:"disposition"`
		TargetVideoBitRateKbps int      `json:"target_video_bitrate_kbps"`
		Reason                 string   `json:"reason"`
		Basis                  []string `json:"basis"`
	}
	if json.Unmarshal(call.Input, &input) != nil {
		handler.failHoldTool(writer, request, call, http.StatusBadRequest, "MCP_INPUT_INVALID", "")
		return
	}
	input.Folder = strings.TrimSpace(input.Folder)
	input.Reason = strings.TrimSpace(input.Reason)
	input.Basis = normalizeClassificationBasis(input.Basis)
	disposition := vocabulary.OrganizationDisposition(strings.TrimSpace(input.Disposition))
	if input.Folder == "" || input.ExpectedInputRevision == 0 || input.Reason == "" || len(input.Basis) == 0 ||
		!validArchivistDisposition(disposition, input.TargetVideoBitRateKbps) {
		handler.failHoldTool(writer, request, call, http.StatusBadRequest, "MCP_CLASSIFICATION_INVALID", input.Folder)
		return
	}
	selection, drive, err := handler.selectedDriveProjection(request.Context(), call)
	if err != nil {
		handler.failHoldTool(writer, request, call, http.StatusConflict, "MCP_HOLD_SELECTION_REQUIRED", input.Folder)
		return
	}
	folder, ok := selectedHold(selection, drive, input.Folder)
	if !ok || folder.InputRevision != input.ExpectedInputRevision {
		handler.failHoldTool(writer, request, call, http.StatusConflict, "MCP_HOLD_REFERENCE_STALE", input.Folder)
		return
	}
	route, err := archivistruntime.RouteArchivistCommand(vocabulary.ActionProcess)
	if err != nil {
		handler.failHoldTool(writer, request, call, http.StatusForbidden, "MCP_ARCHIVIST_POLICY_DENIED", input.Folder)
		return
	}
	decisionID := holdDecisionID(call, folder)
	if route == archivistruntime.RouteActionCard {
		card := archivistcontrol.ActionCardProjection{
			ID: "ACTION-" + call.ToolCallID, ArchivistTurnID: call.TurnID,
			CommandType: archiveintakeworkspace.ClassifyHoldCommand, TargetID: folder.Folder,
			ExpectedRevision: folder.InputRevision, State: "PENDING", CommandID: call.CommandID,
			IdempotencyKey: call.IdempotencyKey, Reason: input.Reason,
			Basis: append([]string(nil), input.Basis...), Parameters: map[string]any{
				"archive_run_id": selection.ArchiveRunID, "workspace_drive_id": selection.WorkspaceDriveID, "decision_id": decisionID,
				"disposition": disposition, "target_video_bitrate_kbps": input.TargetVideoBitRateKbps,
			},
		}
		handler.writeArchiveActionCard(writer, request, call, card)
		return
	}
	decision, err := handler.drives.Override(request.Context(), archiveintakeworkspace.OverrideCommand{
		CommandID: call.CommandID, DecisionID: decisionID, IdempotencyKey: call.IdempotencyKey,
		ArchiveRunID: selection.ArchiveRunID, WorkspaceDriveID: selection.WorkspaceDriveID, Folder: folder.Folder,
		ExpectedInputRevision: input.ExpectedInputRevision, Disposition: disposition,
		TargetVideoBitRateKbps: input.TargetVideoBitRateKbps,
		Actor:                  command.Actor{Type: vocabulary.ActorArchivist, ID: "archivist:" + call.SessionID},
		Reason:                 input.Reason, Basis: input.Basis, OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		status := http.StatusBadRequest
		code := "MCP_CLASSIFICATION_INVALID"
		if errors.Is(err, organization.ErrStaleInputRevision) || errors.Is(err, command.ErrIdempotencyConflict) {
			status, code = http.StatusConflict, "MCP_HOLD_REFERENCE_STALE"
		}
		handler.failHoldTool(writer, request, call, status, code, folder.Folder)
		return
	}
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, decision.ID, "COMPLETED")
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"decision_id": decision.ID, "decision_sequence": decision.Sequence,
		"input_revision": decision.InputRevision, "disposition": decision.Disposition,
		"profile_reference": decision.ProfileReference, "decided_by_type": decision.DecidedByType,
	}})
}

func (handler *Handler) failHoldTool(writer http.ResponseWriter, request *http.Request, call callRequest, status int, code, targetID string) {
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, strings.TrimSpace(targetID), "FAILED")
	writeError(writer, status, code)
}

func normalizeClassificationBasis(source []string) []string {
	seen := make(map[string]bool, len(source))
	result := make([]string, 0, len(source))
	for _, value := range source {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 512 || seen[value] || len(result) >= 32 {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func validArchivistDisposition(disposition vocabulary.OrganizationDisposition, targetKbps int) bool {
	switch disposition {
	case vocabulary.OrganizationAsIs, vocabulary.OrganizationCompileSourceSequence:
		return targetKbps == 0
	case vocabulary.OrganizationRenderRecordings:
		return targetKbps == 6_000 || targetKbps == 20_000
	default:
		return false
	}
}

func holdDecisionID(call callRequest, folder archiveintakeworkspace.FolderView) string {
	digest := sha256.Sum256([]byte(call.SessionID + "\x00" + call.ToolCallID + "\x00" + folder.DecisionID + "\x00" + folder.Folder))
	return "DECISION-" + hex.EncodeToString(digest[:12])
}

func (handler *Handler) startArchiveRun(writer http.ResponseWriter, request *http.Request, call callRequest, policy archivistruntime.ArchivistTurnToolPolicy) {
	selection, err := handler.archiveIntakeSelection(request.Context(), call)
	var input struct {
		ArchiveRunID string `json:"archive_run_id"`
	}
	if err != nil {
		handler.archiveIntakeSelectionActionCard(writer, request, call)
		return
	}
	if json.Unmarshal(call.Input, &input) != nil || strings.TrimSpace(input.ArchiveRunID) == "" ||
		(selection.ArchiveRunID != "" && selection.ArchiveRunID != strings.TrimSpace(input.ArchiveRunID)) {
		writeError(writer, http.StatusConflict, "MCP_DRIVE_SELECTION_REQUIRED")
		return
	}
	route, err := archivistruntime.RouteArchivistCommand(vocabulary.ActionProcess)
	if err != nil {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, "", "FAILED")
		writeError(writer, http.StatusForbidden, "MCP_ARCHIVIST_POLICY_DENIED")
		return
	}
	if policy == archivistruntime.ToolPolicyAdvisory || route == archivistruntime.RouteActionCard {
		handler.archiveRunActionCard(writer, request, call, strings.TrimSpace(input.ArchiveRunID), selection)
		return
	}
	result, err := handler.archiveRuns.Start(request.Context(), archiveorganization.StartRequest{
		ArchiveRunID: strings.TrimSpace(input.ArchiveRunID), IdempotencyKey: call.IdempotencyKey,
		SessionID: selection.IntakeSessionID, SourceVolumeID: selection.SourceVolumeID,
		ExpectedCandidateRevision: selection.CandidateRevision, ArchiveStorageID: selection.ArchiveStorageID,
		InputRevision: selection.FolderDiscoveryRevision, FolderIDs: selection.FolderIDs,
		ExpectedWorkerID: selection.WorkerID, ExpectedSourceStorageID: selection.SourceStorageID,
		ExpectedWorkspaceDriveID: selection.WorkspaceDriveID,
		Actor:                    command.Actor{Type: vocabulary.ActorArchivist, ID: "archivist:" + call.SessionID},
		SelectedBy:               command.Actor{Type: selection.SelectedByType, ID: selection.SelectedBy}, StartedAt: time.Now().UTC(),
	})
	if err != nil {
		slog.Warn("MCP archive run start rejected", "session_id", call.SessionID, "turn_id", call.TurnID, "error", err)
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, input.ArchiveRunID, "FAILED")
		writeError(writer, http.StatusConflict, "MCP_ARCHIVE_RUN_START_REJECTED")
		return
	}
	if err := handler.archivists.BindArchiveRun(request.Context(), call.SessionID, call.TurnID, result.Receipt.ID, 1); err != nil {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, result.Receipt.ID, "FAILED")
		writeError(writer, http.StatusInternalServerError, "MCP_ARCHIVE_RUN_BIND_FAILED")
		return
	}
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, result.Receipt.ID, "COMPLETED")
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"archive_run_id": result.Receipt.ID, "folder_count": len(result.Receipt.Folders), "replayed": result.Replayed,
	}})
}

func (handler *Handler) archiveRunActionCard(writer http.ResponseWriter, request *http.Request, call callRequest, runID string, selection archivistruntime.DriveSelection) {
	card := archivistcontrol.ActionCardProjection{
		ID: "ACTION-" + call.ToolCallID, ArchivistTurnID: call.TurnID,
		CommandType: archiveorganization.StartActionCardCommand, TargetID: runID, State: "PENDING",
		CommandID: call.CommandID, IdempotencyKey: call.IdempotencyKey,
		Reason: "start selected archive run", Basis: []string{"ArchivistTurn " + call.TurnID, "ToolCall " + call.ToolCallID},
		Parameters: map[string]any{
			"session_id": selection.IntakeSessionID, "source_volume_id": selection.SourceVolumeID,
			"expected_candidate_revision": selection.CandidateRevision, "archive_storage_id": selection.ArchiveStorageID,
			"input_revision": selection.FolderDiscoveryRevision, "folder_ids": selection.FolderIDs,
			"expected_worker_id": selection.WorkerID, "expected_source_storage_id": selection.SourceStorageID,
			"expected_workspace_drive_id": selection.WorkspaceDriveID, "selected_by": selection.SelectedBy,
		},
	}
	handler.writeArchiveActionCard(writer, request, call, card)
}

func (handler *Handler) archiveIntakeSelectionActionCard(writer http.ResponseWriter, request *http.Request, call callRequest) {
	card := archivistcontrol.ActionCardProjection{
		ID: "ACTION-SELECT-" + call.ToolCallID, ArchivistTurnID: call.TurnID,
		CommandType: "archive_intake.selection.required", TargetID: call.SessionID, State: "PENDING",
		CommandID: call.CommandID, IdempotencyKey: call.IdempotencyKey,
		Reason: "current Worker, source, archive, and folder selection required",
		Basis:  []string{"ArchivistTurn " + call.TurnID, "ToolCall " + call.ToolCallID},
	}
	if err := handler.archivists.RecordActionCard(request.Context(), call.SessionID, card); err != nil {
		writeError(writer, http.StatusInternalServerError, "MCP_ACTION_CARD_FAILED")
		return
	}
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, "", "ACTION_CARD")
	writeJSON(writer, http.StatusAccepted, map[string]any{"data": map[string]any{
		"action_card_id": card.ID, "state": card.State,
	}})
}

func (handler *Handler) startArchiveTranscription(writer http.ResponseWriter, request *http.Request, call callRequest) {
	selection, err := handler.archiveIntakeSelection(request.Context(), call)
	if err != nil || !validEmptyInput(call.Input) ||
		selection.ArchiveRunID == "" || selection.ArchiveRunRevision == 0 {
		writeError(writer, http.StatusConflict, "MCP_ARCHIVE_RUN_REFERENCE_REQUIRED")
		return
	}
	route, err := archivistruntime.RouteArchivistCommand(vocabulary.ActionProcess)
	if err != nil {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, selection.ArchiveRunID, "FAILED")
		writeError(writer, http.StatusForbidden, "MCP_ARCHIVIST_POLICY_DENIED")
		return
	}
	if route == archivistruntime.RouteActionCard {
		card := archivistcontrol.ActionCardProjection{
			ID: "ACTION-" + call.ToolCallID, ArchivistTurnID: call.TurnID,
			CommandType: archivetranscription.StartActionCardCommand, TargetID: selection.ArchiveRunID,
			ExpectedRevision: selection.ArchiveRunRevision, State: "PENDING",
			CommandID: call.CommandID, IdempotencyKey: call.IdempotencyKey,
			Reason: "start selected archive run transcription", Basis: []string{"ArchivistTurn " + call.TurnID, "ToolCall " + call.ToolCallID},
			Parameters: map[string]any{"expected_archive_run_revision": selection.ArchiveRunRevision},
		}
		handler.writeArchiveActionCard(writer, request, call, card)
		return
	}
	result, err := handler.archiveTranscriptions.Start(request.Context(), archivetranscription.StartRequest{
		ArchiveRunID: selection.ArchiveRunID, ExpectedArchiveRunRevision: selection.ArchiveRunRevision,
		IdempotencyKey: call.IdempotencyKey, Actor: command.Actor{Type: vocabulary.ActorArchivist, ID: "archivist:" + call.SessionID},
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, selection.ArchiveRunID, "FAILED")
		writeError(writer, http.StatusConflict, "MCP_ARCHIVE_TRANSCRIPTION_START_FAILED")
		return
	}
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, selection.ArchiveRunID, "COMPLETED")
	writeJSON(writer, http.StatusOK, map[string]any{"data": result})
}

func (handler *Handler) writeArchiveActionCard(writer http.ResponseWriter, request *http.Request, call callRequest, card archivistcontrol.ActionCardProjection) {
	if err := handler.archivists.RecordActionCard(request.Context(), call.SessionID, card); err != nil {
		writeError(writer, http.StatusInternalServerError, "MCP_ACTION_CARD_FAILED")
		return
	}
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, card.TargetID, "ACTION_CARD")
	writeJSON(writer, http.StatusAccepted, map[string]any{"data": map[string]any{"action_card_id": card.ID, "state": card.State}})
}

func validEmptyInput(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var input map[string]any
	return json.Unmarshal(raw, &input) == nil
}

func (handler *Handler) getJob(writer http.ResponseWriter, request *http.Request, call callRequest) {
	var input struct {
		JobID string `json:"job_id"`
	}
	if json.Unmarshal(call.Input, &input) != nil || strings.TrimSpace(input.JobID) == "" {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, input.JobID, "FAILED")
		writeError(writer, http.StatusBadRequest, "MCP_INPUT_INVALID")
		return
	}
	view, err := handler.archiveWorkspace.Detail(request.Context(), input.JobID)
	if err != nil {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, input.JobID, "FAILED")
		writeError(writer, http.StatusNotFound, "JOB_NOT_FOUND")
		return
	}
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, input.JobID, "COMPLETED")
	writeJSON(writer, http.StatusOK, map[string]any{"data": view})
}

func (handler *Handler) searchAssets(writer http.ResponseWriter, request *http.Request, call callRequest) {
	var input struct {
		Text              string `json:"text"`
		VideoCodec        string `json:"video_codec"`
		MinDurationMillis int64  `json:"min_duration_ms"`
		MaxDurationMillis int64  `json:"max_duration_ms"`
	}
	if json.Unmarshal(call.Input, &input) != nil {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, "", "FAILED")
		writeError(writer, http.StatusBadRequest, "MCP_INPUT_INVALID")
		return
	}
	results, err := handler.catalog.Search(request.Context(), catalog.SearchQuery{
		Text: input.Text, VideoCodec: input.VideoCodec,
		MinDurationMillis: input.MinDurationMillis, MaxDurationMillis: input.MaxDurationMillis,
	})
	if err != nil {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, "", "FAILED")
		writeError(writer, http.StatusBadRequest, "MCP_INPUT_INVALID")
		return
	}
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, "", "COMPLETED")
	writeJSON(writer, http.StatusOK, map[string]any{"data": results})
}

func (handler *Handler) createJob(writer http.ResponseWriter, request *http.Request, hostID string, call callRequest) {
	var input struct {
		JobID        string `json:"job_id"`
		TemplateName string `json:"template_name"`
		TargetTitle  string `json:"target_title"`
		Reason       string `json:"reason"`
	}
	if json.Unmarshal(call.Input, &input) != nil {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, "", "FAILED")
		writeError(writer, http.StatusBadRequest, "MCP_INPUT_INVALID")
		return
	}
	route, err := archivistruntime.RouteArchivistCommand(vocabulary.ActionProcess)
	if err != nil || route != archivistruntime.RouteDirectCommand {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, input.JobID, "FAILED")
		writeError(writer, http.StatusConflict, "MCP_ACTION_CARD_REQUIRED")
		return
	}
	decision, err := archivistruntime.PrepareToolCall(archivistruntime.ToolCallRequest{
		ID: call.ToolCallID, CommandID: call.CommandID, ArchivistHostID: hostID,
		ArchivistSessionID: call.SessionID, ArchivistTurnID: call.TurnID, ArchivistID: "archivist:" + call.SessionID,
		FencingToken: call.FencingToken, ToolName: job.CreateCommand, InputDigest: call.InputDigest,
		IdempotencyKey: call.IdempotencyKey, TargetID: input.JobID, Reason: input.Reason,
		Parameters: map[string]any{"template_name": input.TemplateName, "target_title": input.TargetTitle},
	}, nil)
	if err != nil || decision.Command == nil {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, input.JobID, "FAILED")
		writeError(writer, http.StatusBadRequest, "MCP_INPUT_INVALID")
		return
	}
	result, err := handler.commands.Execute(request.Context(), *decision.Command)
	if err != nil {
		_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, input.JobID, "FAILED")
		writeError(writer, http.StatusConflict, "MCP_COMMAND_FAILED")
		return
	}
	_ = handler.archivists.RecordToolCall(request.Context(), call.SessionID, call.TurnID, call.ToolCallID, call.Tool, input.JobID, "COMPLETED")
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"command_id": result.CommandID, "target_id": result.TargetID, "revision": result.Revision,
	}})
}

func identity(request *http.Request) (string, string, bool) {
	hostID := strings.TrimSpace(request.Header.Get("X-Archivist-Host-ID"))
	authorization := request.Header.Get("Authorization")
	if hostID == "" || !strings.HasPrefix(authorization, "Bearer ") {
		return "", "", false
	}
	credential := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	return hostID, credential, credential != ""
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) bool {
	if request.Header.Get("Content-Type") != "application/json" {
		writeError(writer, http.StatusUnsupportedMediaType, "JSON_REQUIRED")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 128<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(writer, http.StatusBadRequest, "INVALID_JSON")
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "INVALID_JSON")
		return false
	}
	return true
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, code string) {
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code}})
}
