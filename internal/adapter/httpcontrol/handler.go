package httpcontrol

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	appdriveintake "videoarchive/internal/application/archiveintake"
	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/application/archiveorganization"
	"videoarchive/internal/application/archivetranscription"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/application/archivistcontrol"
	coreintake "videoarchive/internal/core/archiveintake"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/catalog"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/contentreview"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/security"
	"videoarchive/internal/core/summary"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

var ErrInvalidConfiguration = errors.New("HTTP control configuration is invalid")

type CatalogReader interface {
	Search(context.Context, catalog.SearchQuery) ([]catalog.SearchResult, error)
	SearchTranscripts(context.Context, string) ([]transcription.SearchHit, error)
	SummariesByOriginal(context.Context, string) ([]summary.Summary, error)
	ContentReviews(context.Context, string, uint64) ([]contentreview.Record, error)
}

type ArchiveWorkspaceReader interface {
	Inbox(context.Context) ([]archiveworkspace.JobSummary, error)
	Detail(context.Context, string) (archiveworkspace.JobView, error)
}

type JobCommander interface {
	Execute(context.Context, command.Envelope) (command.Result, error)
}

type WorkerReader interface {
	Statuses(context.Context) ([]worker.Status, error)
}

type ArchiveTranscriptionStarter interface {
	Start(context.Context, archivetranscription.StartRequest) (archivetranscription.Result, error)
}

type ArchiveIntakeSelector interface {
	Select(context.Context, appdriveintake.SelectRequest) (*appdriveintake.SessionAccess, error)
}

type ArchivistWorkspace interface {
	CreateSession(context.Context, archivistruntime.SessionSpec, string) (archivistcontrol.SessionRecord, error)
	CreateArchivist(context.Context, string, string, string, string, workflow.ControlMode) (archivistcontrol.SessionRecord, error)
	SubmitTurnWithDriveSelection(context.Context, string, string, string, string, *archivistruntime.DriveSelection) (archivistcontrol.TurnRecord, error)
	Projection(context.Context) ([]archivistcontrol.SessionProjection, error)
	HostStatuses() []archivistruntime.HostStatus
	HostProfiles() []archivistruntime.HostProfile
	PrepareActionCard(context.Context, string, string) (command.Envelope, error)
	CompleteActionCard(context.Context, string) error
	RejectActionCard(context.Context, string, string, string) error
}

type Configuration struct {
	Access                security.AccessConfig
	TrustedProxies        []netip.Prefix
	Catalog               CatalogReader
	ArchiveWorkspace      ArchiveWorkspaceReader
	Notifications         http.Handler
	Commands              JobCommander
	WorkerAPI             http.Handler
	Workers               WorkerReader
	ArchivistAPI          http.Handler
	Archivists            ArchivistWorkspace
	Drives                archiveintakeworkspace.Workspace
	ArchiveIntake         ArchiveIntakeSelector
	ArchiveRuns           archiveorganization.Starter
	ArchiveTranscriptions ArchiveTranscriptionStarter
	AllowedOrigins        []string
	Version               string
}

type Handler struct {
	access                security.AccessConfig
	trustedProxies        []netip.Prefix
	catalog               CatalogReader
	archiveWorkspace      ArchiveWorkspaceReader
	notifications         http.Handler
	commands              JobCommander
	workerAPI             http.Handler
	workers               WorkerReader
	archivistAPI          http.Handler
	archivists            ArchivistWorkspace
	drives                archiveintakeworkspace.Workspace
	archiveIntake         ArchiveIntakeSelector
	archiveRuns           archiveorganization.Starter
	archiveTranscriptions ArchiveTranscriptionStarter
	allowedOrigins        []string
	version               string
	router                http.Handler
}

//go:embed static/*
var staticFiles embed.FS

func New(configuration Configuration) (*Handler, error) {
	if configuration.Catalog == nil || configuration.ArchiveWorkspace == nil || configuration.Notifications == nil ||
		strings.TrimSpace(configuration.Version) == "" || len(configuration.Access.AllowedNetworks) == 0 ||
		((configuration.Commands != nil || configuration.ArchiveIntake != nil || configuration.ArchiveRuns != nil || configuration.ArchiveTranscriptions != nil) && len(configuration.AllowedOrigins) == 0) ||
		(configuration.WorkerAPI == nil) != (configuration.Workers == nil) ||
		(configuration.ArchivistAPI == nil) != (configuration.Archivists == nil) {
		return nil, ErrInvalidConfiguration
	}
	handler := &Handler{
		access: configuration.Access, trustedProxies: append([]netip.Prefix(nil), configuration.TrustedProxies...),
		catalog: configuration.Catalog, archiveWorkspace: configuration.ArchiveWorkspace,
		notifications: configuration.Notifications, commands: configuration.Commands,
		workerAPI: configuration.WorkerAPI, workers: configuration.Workers,
		archivistAPI: configuration.ArchivistAPI, archivists: configuration.Archivists,
		drives: configuration.Drives, archiveIntake: configuration.ArchiveIntake,
		archiveRuns: configuration.ArchiveRuns, archiveTranscriptions: configuration.ArchiveTranscriptions,
		allowedOrigins: append([]string(nil), configuration.AllowedOrigins...),
		version:        strings.TrimSpace(configuration.Version),
	}
	assets, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", cacheAssets(http.FileServer(http.FS(assets)))))
	mux.HandleFunc("GET /{$}", handler.index)
	mux.HandleFunc("GET /healthz", handler.health)
	mux.HandleFunc("GET /api/v1/assets", handler.searchAssets)
	mux.HandleFunc("GET /api/v1/transcripts/search", handler.searchTranscripts)
	mux.HandleFunc("GET /api/v1/assets/{assetID}/summaries", handler.summaries)
	mux.HandleFunc("GET /api/v1/summaries/{summaryID}/reviews", handler.reviews)
	mux.HandleFunc("GET /api/v1/jobs", handler.jobs)
	mux.HandleFunc("GET /api/v1/jobs/{jobID}", handler.job)
	if handler.workers != nil {
		mux.HandleFunc("GET /api/v1/workers", handler.workerStatuses)
	}
	if handler.commands != nil || handler.archivists != nil || handler.drives != nil || handler.archiveIntake != nil || handler.archiveRuns != nil || handler.archiveTranscriptions != nil {
		mux.HandleFunc("GET /api/v1/session", handler.session)
	}
	if handler.commands != nil {
		mux.HandleFunc("POST /api/v1/jobs", handler.createJob)
		mux.HandleFunc("POST /api/v1/jobs/{jobID}/cancel", handler.cancelJob)
	}
	if handler.archivists != nil {
		mux.HandleFunc("GET /api/v1/archivist-hosts", handler.archivistHostStatuses)
		mux.HandleFunc("GET /api/v1/archivist-sessions", handler.archivistSessions)
		mux.HandleFunc("POST /api/v1/archivist-sessions", handler.createArchivistSession)
		mux.HandleFunc("POST /api/v1/archivist-sessions/{sessionID}/turns", handler.createArchivistTurn)
		mux.HandleFunc("POST /api/v1/action-cards/{cardID}/execute", handler.executeActionCard)
		mux.HandleFunc("POST /api/v1/action-cards/{cardID}/reject", handler.rejectActionCard)
	}
	if handler.drives != nil {
		mux.HandleFunc("GET /api/v1/archive-intake", handler.archiveIntakeProjection)
		mux.HandleFunc("POST /api/v1/archive-intake/{workspaceDriveID}/override", handler.overrideArchiveIntakeDecision)
	}
	if handler.archiveIntake != nil {
		mux.HandleFunc("POST /api/v1/archive-intake-sessions", handler.selectArchiveIntake)
	}
	if handler.archiveRuns != nil {
		mux.HandleFunc("POST /api/v1/archive-intake/runs", handler.startArchiveRun)
	}
	if handler.archiveTranscriptions != nil {
		mux.HandleFunc("POST /api/v1/archive-runs/{archiveRunID}/transcription", handler.startArchiveTranscription)
	}
	mux.Handle("GET /api/v1/notifications", handler.notifications)
	handler.router = securityHeaders(mux)
	return handler, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	peer, err := peerAddress(request.RemoteAddr)
	if err != nil {
		writeError(writer, http.StatusForbidden, "NETWORK_DENIED")
		return
	}
	forwarded := forwardedAddresses(request.Header.Values("X-Forwarded-For"))
	source := security.EffectiveSourceIP(peer, forwarded, handler.trustedProxies)
	if strings.HasPrefix(request.URL.Path, "/worker/v1/") && handler.workerAPI != nil {
		_, err := security.ResolvePrincipal(security.AccessConfig{
			Mode: security.AuthDisabled, AllowedNetworks: handler.access.AllowedNetworks,
		}, security.AccessRequest{SourceIP: source})
		if err != nil {
			writeError(writer, http.StatusForbidden, "NETWORK_DENIED")
			return
		}
		securityHeaders(handler.workerAPI).ServeHTTP(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/archivist/v1/") && handler.archivistAPI != nil {
		_, err := security.ResolvePrincipal(security.AccessConfig{
			Mode: security.AuthDisabled, AllowedNetworks: handler.access.AllowedNetworks,
		}, security.AccessRequest{SourceIP: source})
		if err != nil {
			writeError(writer, http.StatusForbidden, "NETWORK_DENIED")
			return
		}
		securityHeaders(handler.archivistAPI).ServeHTTP(writer, request)
		return
	}
	principal, err := security.ResolvePrincipal(handler.access, security.AccessRequest{SourceIP: source})
	if errors.Is(err, security.ErrAuthenticationRequired) {
		writeError(writer, http.StatusUnauthorized, "AUTHENTICATION_REQUIRED")
		return
	}
	if err != nil {
		writeError(writer, http.StatusForbidden, "NETWORK_DENIED")
		return
	}
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
	handler.router.ServeHTTP(writer, request)
}

type principalContextKey struct{}

func (handler *Handler) index(writer http.ResponseWriter, request *http.Request) {
	payload, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "ARCHIVE_WORKSPACE_UNAVAILABLE")
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Write(payload)
}

func (handler *Handler) health(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "version": handler.version})
}

func (handler *Handler) searchAssets(writer http.ResponseWriter, request *http.Request) {
	query := catalog.SearchQuery{
		Text: request.URL.Query().Get("text"), VideoCodec: request.URL.Query().Get("video_codec"),
	}
	var err error
	if raw := request.URL.Query().Get("min_duration_ms"); raw != "" {
		query.MinDurationMillis, err = strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "INVALID_QUERY")
			return
		}
	}
	if raw := request.URL.Query().Get("max_duration_ms"); raw != "" {
		query.MaxDurationMillis, err = strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "INVALID_QUERY")
			return
		}
	}
	results, err := handler.catalog.Search(request.Context(), query)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "INVALID_QUERY")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": results})
}

func (handler *Handler) searchTranscripts(writer http.ResponseWriter, request *http.Request) {
	hits, err := handler.catalog.SearchTranscripts(request.Context(), request.URL.Query().Get("text"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "INVALID_QUERY")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": hits})
}

func (handler *Handler) summaries(writer http.ResponseWriter, request *http.Request) {
	values, err := handler.catalog.SummariesByOriginal(request.Context(), request.PathValue("assetID"))
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "QUERY_FAILED")
		return
	}
	result := make([]summaryView, 0, len(values))
	for _, value := range values {
		result = append(result, summaryView{
			ID: value.ID, Revision: value.Revision, OriginalAssetID: value.Artifact.Origin.OriginalAssetID,
			TranscriptID: value.TranscriptID, ProfileReference: value.Artifact.Origin.ProfileVersion,
			Language: value.Language, Content: value.Content, AuthorType: string(value.AuthorType),
			AuthorID: value.AuthorID, CreatedAt: value.Artifact.Origin.CreatedAt,
		})
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": result})
}

func (handler *Handler) reviews(writer http.ResponseWriter, request *http.Request) {
	revision, err := strconv.ParseUint(request.URL.Query().Get("revision"), 10, 64)
	if err != nil || revision == 0 {
		writeError(writer, http.StatusBadRequest, "INVALID_QUERY")
		return
	}
	reviews, err := handler.catalog.ContentReviews(request.Context(), request.PathValue("summaryID"), revision)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "QUERY_FAILED")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": reviews})
}

func (handler *Handler) jobs(writer http.ResponseWriter, request *http.Request) {
	jobs, err := handler.archiveWorkspace.Inbox(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "QUERY_FAILED")
		return
	}
	result := make([]jobSummaryView, 0, len(jobs))
	for _, job := range jobs {
		result = append(result, projectJobSummary(job))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": result})
}

func (handler *Handler) job(writer http.ResponseWriter, request *http.Request) {
	job, err := handler.archiveWorkspace.Detail(request.Context(), request.PathValue("jobID"))
	if errors.Is(err, archiveworkspace.ErrJobNotFound) {
		writeError(writer, http.StatusNotFound, "JOB_NOT_FOUND")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "QUERY_FAILED")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": projectJobDetail(job)})
}

func (handler *Handler) workerStatuses(writer http.ResponseWriter, request *http.Request) {
	statuses, err := handler.workers.Statuses(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "QUERY_FAILED")
		return
	}
	result := make([]workerStatusView, 0, len(statuses))
	for _, status := range statuses {
		storageIDs := make([]string, 0, len(status.Metrics.AccessibleStorageIDs))
		for storageID, available := range status.Metrics.AccessibleStorageIDs {
			if available {
				storageIDs = append(storageIDs, storageID)
			}
		}
		sort.Strings(storageIDs)
		result = append(result, workerStatusView{
			WorkerID: status.WorkerID, State: status.State, LastHeartbeat: status.LastHeartbeat,
			SnapshotID: status.Metrics.SnapshotID, ObservedAt: status.Metrics.ObservedAt,
			CPUAverage5m: status.Metrics.AverageCPU5m, GPUAverage5m: status.Metrics.AverageGPU5m,
			IdleStableMillis:     status.Metrics.IdleStableFor.Milliseconds(),
			AvailableMemoryBytes: status.Metrics.AvailableMemoryBytes,
			AvailableVRAMBytes:   status.Metrics.AvailableVRAMBytes, ScratchFreeBytes: status.Metrics.ScratchFreeBytes,
			AcceptingWork: status.Metrics.AcceptingWork, HeavyRunning: status.Metrics.HeavyRunning,
			LightRunning: status.Metrics.LightRunning, AccessibleStorageIDs: storageIDs,
			ReservedMemoryBytes: status.Capacity.ReservedMemoryBytes, ReservedScratchBytes: status.Capacity.ReservedScratchBytes,
			CapacityAvailableMemoryBytes: status.Capacity.AvailableMemoryBytes, CapacityScratchFreeBytes: status.Capacity.ScratchFreeBytes,
			HeavySlotsUsed: status.Capacity.HeavySlotsUsed, HeavySlotsMaximum: status.Capacity.HeavySlotsMaximum,
			LightSlotsUsed: status.Capacity.LightSlotsUsed, LightSlotsMaximum: status.Capacity.LightSlotsMaximum,
			AdditionalWorkPossible: status.Capacity.AdditionalWorkPossible,
			HeavyAdmission:         projectAdmission(status.Capacity.HeavyAdmission), LightAdmission: projectAdmission(status.Capacity.LightAdmission),
		})
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": result})
}

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
			WorkspaceRootID: projection.Session.WorkspaceRootID, ControlMode: string(projection.Session.ControlMode),
			JobID: projection.JobID, CreatedAt: projection.CreatedAt,
		}
		for _, turn := range projection.Turns {
			turnView := archivistTurnView{
				TurnID: turn.ID, RequestID: turn.RequestID, State: turn.State,
				Prompt: safeArchivistPrompt(turn.Prompt), SubmittedBy: string(turn.SubmittedBy),
				CreatedAt: turn.CreatedAt, UpdatedAt: turn.UpdatedAt, FinalResponse: turn.FinalResponse,
				Entries: turn.Entries, DriveSelection: turn.DriveSelection, WorkContext: turn.WorkContext,
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
	ControlMode     string `json:"control_mode"`
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
	if strings.TrimSpace(payload.Name) != "" || strings.TrimSpace(payload.ControlMode) != "" {
		if strings.TrimSpace(payload.Backend) != "" || strings.TrimSpace(payload.WorkspaceRootID) != "" || strings.TrimSpace(payload.JobID) != "" {
			writeArchivistError(writer, archivistcontrol.ErrInvalidRequest)
			return
		}
		record, err = handler.archivists.CreateArchivist(request.Context(), payload.SessionID, payload.Name, payload.ArchivistHostID, payload.CLIID, workflow.ControlMode(payload.ControlMode))
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
		"name": record.Session.Name, "control_mode": record.Session.ControlMode,
		"backend": record.Session.Backend, "cli_id": record.Session.CLIID,
		"workspace_root_id": record.Session.WorkspaceRootID, "job_id": record.JobID,
	}})
}

type createArchivistTurnRequest struct {
	TurnID         string                           `json:"turn_id"`
	RequestID      string                           `json:"request_id"`
	Prompt         string                           `json:"prompt"`
	DriveSelection *archivistruntime.DriveSelection `json:"drive_selection,omitempty"`
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
	record, err := handler.archivists.SubmitTurnWithDriveSelection(
		request.Context(), request.PathValue("sessionID"), payload.TurnID, payload.RequestID, payload.Prompt, payload.DriveSelection,
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
	envelope, err := handler.archivists.PrepareActionCard(request.Context(), request.PathValue("cardID"), principal.ID)
	if err != nil {
		writeArchivistError(writer, err)
		return
	}
	var response map[string]any
	switch envelope.CommandType {
	case archiveintakeworkspace.ClassifyHoldCommand:
		if handler.drives == nil {
			writeError(writer, http.StatusConflict, "ARCHIVE_INTAKE_UNAVAILABLE")
			return
		}
		decision, classifyErr := handler.drives.Override(request.Context(), archiveintakeworkspace.OverrideCommand{
			CommandID: envelope.CommandID, DecisionID: parameterString(envelope.Parameters, "decision_id"),
			IdempotencyKey: envelope.IdempotencyKey, WorkspaceDriveID: parameterString(envelope.Parameters, "workspace_drive_id"),
			Folder: envelope.TargetID, ExpectedInputRevision: envelope.ExpectedRevision,
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
	if err := handler.archivists.CompleteActionCard(request.Context(), request.PathValue("cardID")); err != nil {
		writeError(writer, http.StatusInternalServerError, "ACTION_CARD_UPDATE_FAILED")
		return
	}
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
				ArchiveState: string(completed.ArchiveState), CompletedAt: completed.CompletedAt,
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
		WorkspaceDriveID: request.PathValue("workspaceDriveID"), Folder: payload.Folder,
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

func (handler *Handler) session(writer http.ResponseWriter, request *http.Request) {
	csrfToken, err := randomToken()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "SESSION_UNAVAILABLE")
		return
	}
	http.SetCookie(writer, &http.Cookie{
		Name: "archive_csrf", Value: csrfToken, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: request.TLS != nil,
	})
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]string{"csrf_token": csrfToken}})
}

type createJobRequest struct {
	JobID            string `json:"job_id"`
	CommandID        string `json:"command_id"`
	IdempotencyKey   string `json:"idempotency_key"`
	Reason           string `json:"reason"`
	TemplateName     string `json:"template_name"`
	TargetTitle      string `json:"target_title"`
	ControlMode      string `json:"control_mode"`
	ExpectedRevision uint64 `json:"expected_revision"`
}

type commandRequest struct {
	CommandID        string `json:"command_id"`
	IdempotencyKey   string `json:"idempotency_key"`
	Reason           string `json:"reason"`
	ExpectedRevision uint64 `json:"expected_revision"`
}

func (handler *Handler) createJob(writer http.ResponseWriter, request *http.Request) {
	if !handler.allowMutation(writer, request) {
		return
	}
	var payload createJobRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	result, err := handler.commands.Execute(request.Context(), handler.envelope(request, command.Envelope{
		CommandID: payload.CommandID, CommandType: job.CreateCommand, TargetID: payload.JobID,
		ExpectedRevision: payload.ExpectedRevision, IdempotencyKey: payload.IdempotencyKey, Reason: payload.Reason,
		Parameters: map[string]any{
			"template_name": payload.TemplateName, "target_title": payload.TargetTitle, "control_mode": payload.ControlMode,
		},
	}))
	if err != nil {
		writeCommandError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, commandResultView(result))
}

func (handler *Handler) cancelJob(writer http.ResponseWriter, request *http.Request) {
	if !handler.allowMutation(writer, request) {
		return
	}
	var payload commandRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	result, err := handler.commands.Execute(request.Context(), handler.envelope(request, command.Envelope{
		CommandID: payload.CommandID, CommandType: job.CancelCommand, TargetID: request.PathValue("jobID"),
		ExpectedRevision: payload.ExpectedRevision, IdempotencyKey: payload.IdempotencyKey, Reason: payload.Reason,
	}))
	if err != nil {
		writeCommandError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, commandResultView(result))
}

func (handler *Handler) envelope(request *http.Request, envelope command.Envelope) command.Envelope {
	principal, _ := request.Context().Value(principalContextKey{}).(security.Principal)
	envelope.Actor = command.Actor{Type: principal.ActorType, ID: principal.ID}
	envelope.RequestedVia = "archive_workspace"
	return envelope
}

func (handler *Handler) allowMutation(writer http.ResponseWriter, request *http.Request) bool {
	cookie, err := request.Cookie("archive_csrf")
	if err != nil || security.ValidateBrowserMutation(security.BrowserMutation{
		Method: request.Method, Origin: request.Header.Get("Origin"), AllowedOrigins: handler.allowedOrigins,
		ContentType: request.Header.Get("Content-Type"), CSRFHeader: request.Header.Get("X-CSRF-Token"),
		CSRFCookie: cookieValue(cookie),
	}) != nil {
		writeError(writer, http.StatusForbidden, "BROWSER_MUTATION_DENIED")
		return false
	}
	return true
}

func cookieValue(cookie *http.Cookie) string {
	if cookie == nil {
		return ""
	}
	return cookie.Value
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<10))
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

func commandResultView(result command.Result) map[string]any {
	return map[string]any{"data": map[string]any{
		"command_id": result.CommandID, "target_id": result.TargetID, "revision": result.Revision,
	}}
}

func writeCommandError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, command.ErrRevisionConflict), errors.Is(err, command.ErrIdempotencyConflict),
		errors.Is(err, workflow.ErrIllegalTransition):
		writeError(writer, http.StatusConflict, "COMMAND_CONFLICT")
	case errors.Is(err, command.ErrInvalidEnvelope), errors.Is(err, job.ErrInvalidJob):
		writeError(writer, http.StatusBadRequest, "INVALID_COMMAND")
	default:
		writeError(writer, http.StatusInternalServerError, "COMMAND_FAILED")
	}
}

func randomToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

type summaryView struct {
	ID               string          `json:"id"`
	Revision         uint64          `json:"revision"`
	OriginalAssetID  string          `json:"original_asset_id"`
	TranscriptID     string          `json:"transcript_id"`
	ProfileReference string          `json:"profile_reference"`
	Language         string          `json:"language"`
	Content          summary.Content `json:"content"`
	AuthorType       string          `json:"author_type"`
	AuthorID         string          `json:"author_id"`
	CreatedAt        time.Time       `json:"created_at"`
}

type jobSummaryView struct {
	JobID                  string      `json:"job_id"`
	Revision               uint64      `json:"revision"`
	TemplateName           string      `json:"template_name"`
	TargetTitle            string      `json:"target_title"`
	ControlMode            string      `json:"control_mode"`
	CurrentStepID          string      `json:"current_step_id"`
	StepState              string      `json:"step_state"`
	Progress               float64     `json:"progress"`
	ExecutorKind           string      `json:"executor_kind"`
	ActorID                string      `json:"actor_id,omitempty"`
	WorkerID               string      `json:"worker_id,omitempty"`
	ArchivistAction        string      `json:"archivist_action,omitempty"`
	ToolCallStatus         string      `json:"tool_call_status,omitempty"`
	WaitReason             string      `json:"wait_reason,omitempty"`
	ErrorClass             string      `json:"error_class,omitempty"`
	LastEventAt            time.Time   `json:"last_event_at"`
	LogReference           string      `json:"log_reference,omitempty"`
	NeedsHumanIntervention bool        `json:"needs_human_intervention"`
	InterventionReason     string      `json:"intervention_reason,omitempty"`
	Worker                 *workerView `json:"worker,omitempty"`
}

type workerView struct {
	WorkerID             string    `json:"worker_id"`
	CPUPercent           float64   `json:"cpu_percent"`
	GPUPercent           float64   `json:"gpu_percent"`
	AvailableMemoryBytes uint64    `json:"available_memory_bytes"`
	AvailableVRAMBytes   uint64    `json:"available_vram_bytes"`
	ScratchFreeBytes     uint64    `json:"scratch_free_bytes"`
	HeavyRunning         int       `json:"heavy_running"`
	AcceptingWork        bool      `json:"accepting_work"`
	AccessibleStorageIDs []string  `json:"accessible_storage_ids"`
	HeartbeatAt          time.Time `json:"heartbeat_at"`
}

type workerStatusView struct {
	WorkerID                     string        `json:"worker_id"`
	State                        string        `json:"state"`
	LastHeartbeat                time.Time     `json:"last_heartbeat,omitempty"`
	SnapshotID                   uint64        `json:"snapshot_id"`
	ObservedAt                   time.Time     `json:"observed_at,omitempty"`
	CPUAverage5m                 float64       `json:"cpu_average_5m"`
	GPUAverage5m                 float64       `json:"gpu_average_5m"`
	IdleStableMillis             int64         `json:"idle_stable_millis"`
	AvailableMemoryBytes         uint64        `json:"available_memory_bytes"`
	AvailableVRAMBytes           uint64        `json:"available_vram_bytes"`
	ScratchFreeBytes             uint64        `json:"scratch_free_bytes"`
	AcceptingWork                bool          `json:"accepting_work"`
	HeavyRunning                 int           `json:"heavy_running"`
	LightRunning                 int           `json:"light_running"`
	AccessibleStorageIDs         []string      `json:"accessible_storage_ids"`
	ReservedMemoryBytes          uint64        `json:"reserved_memory_bytes"`
	ReservedScratchBytes         uint64        `json:"reserved_scratch_bytes"`
	CapacityAvailableMemoryBytes uint64        `json:"capacity_available_memory_bytes"`
	CapacityScratchFreeBytes     uint64        `json:"capacity_scratch_free_bytes"`
	HeavySlotsUsed               int           `json:"heavy_slots_used"`
	HeavySlotsMaximum            int           `json:"heavy_slots_maximum"`
	LightSlotsUsed               int           `json:"light_slots_used"`
	LightSlotsMaximum            int           `json:"light_slots_maximum"`
	AdditionalWorkPossible       bool          `json:"additional_work_possible"`
	HeavyAdmission               admissionView `json:"heavy_admission"`
	LightAdmission               admissionView `json:"light_admission"`
}

type admissionView struct {
	Available  bool   `json:"available"`
	WaitReason string `json:"wait_reason,omitempty"`
	Code       string `json:"code,omitempty"`
}

func projectAdmission(value worker.Admission) admissionView {
	return admissionView{Available: value.Available, WaitReason: string(value.WaitReason), Code: value.Code}
}

type archivistHostStatusView struct {
	ArchivistHostID string    `json:"archivist_host_id"`
	State           string    `json:"state"`
	LastHeartbeat   time.Time `json:"last_heartbeat,omitempty"`
	DetectedCLIIDs  []string  `json:"detected_cli_ids"`
}

type driveView struct {
	WorkspaceDriveID           string                   `json:"workspace_drive_id"`
	ArchiveRunID               string                   `json:"archive_run_id"`
	ArchiveRunRevision         uint64                   `json:"archive_run_revision"`
	CandidateRevision          uint64                   `json:"candidate_revision"`
	FolderDiscoveryRevision    uint64                   `json:"folder_discovery_revision"`
	FolderDiscoveryState       string                   `json:"folder_discovery_state"`
	SessionID                  string                   `json:"session_id"`
	WorkerID                   string                   `json:"worker_id"`
	SourceVolumeID             string                   `json:"source_volume_id"`
	SourceStorageID            string                   `json:"source_storage_id"`
	ArchiveStorageID           string                   `json:"archive_storage_id"`
	State                      string                   `json:"state"`
	ArchiveRunState            string                   `json:"archive_run_state"`
	InputRevision              uint64                   `json:"input_revision"`
	FileCount                  int                      `json:"file_count"`
	FolderCount                int                      `json:"folder_count"`
	StageOneRemaining          int                      `json:"stage_one_remaining"`
	StageTwoRemaining          int                      `json:"stage_two_remaining"`
	HoldCount                  int                      `json:"hold_count"`
	EligibleTranscriptionCount int                      `json:"eligible_transcription_count"`
	CatalogRowCount            int                      `json:"catalog_row_count"`
	CatalogUpdatedAt           time.Time                `json:"catalog_updated_at,omitempty"`
	Folders                    []driveFolderView        `json:"folders"`
	DiscoveredFileCount        int                      `json:"discovered_file_count"`
	DiscoveredFolderCount      int                      `json:"discovered_folder_count"`
	DiscoveredFolders          []driveFolderView        `json:"discovered_folders"`
	StorageCandidates          []driveStorageView       `json:"storage_candidates"`
	CompletedWork              []driveCompletedWorkView `json:"completed_work"`
}

type driveCompletedWorkView struct {
	ArchiveRunID         string    `json:"archive_run_id"`
	Folder               string    `json:"folder"`
	JobID                string    `json:"job_id"`
	Disposition          string    `json:"disposition"`
	ProfileReference     string    `json:"profile_reference,omitempty"`
	SourceVideoCount     int       `json:"source_video_count"`
	ArchiveStorageID     string    `json:"archive_storage_id"`
	ArchivePackageID     string    `json:"archive_package_id"`
	ArchiveDate          string    `json:"archive_date"`
	ArchiveDisplayName   string    `json:"archive_display_name"`
	ArchiveVideoFileName string    `json:"archive_video_file_name"`
	ArchiveState         string    `json:"archive_state"`
	CompletedAt          time.Time `json:"completed_at"`
}

type driveStorageView struct {
	StorageID        string `json:"storage_id"`
	Role             string `json:"role"`
	WorkspaceDriveID string `json:"workspace_drive_id"`
	AccessKind       string `json:"access_kind"`
	Accessible       bool   `json:"accessible"`
	Model            string `json:"model,omitempty"`
	Label            string `json:"label,omitempty"`
	FileSystem       string `json:"file_system,omitempty"`
	CapacityBytes    uint64 `json:"capacity_bytes"`
	FreeBytes        uint64 `json:"free_bytes"`
}

type driveFolderView struct {
	Folder               string                  `json:"folder"`
	DecisionID           string                  `json:"decision_id"`
	InputRevision        uint64                  `json:"input_revision"`
	DecisionSequence     uint64                  `json:"decision_sequence"`
	Disposition          string                  `json:"disposition"`
	ProfileReference     string                  `json:"profile_reference,omitempty"`
	HoldReasons          []string                `json:"hold_reasons"`
	WorkOptions          []driveWorkOptionView   `json:"work_options"`
	VideoCount           int                     `json:"video_count"`
	EstimatedOutputBytes uint64                  `json:"estimated_output_bytes"`
	JobID                string                  `json:"job_id,omitempty"`
	TranscriptionJobID   string                  `json:"transcription_job_id,omitempty"`
	CurrentStepID        string                  `json:"current_step_id"`
	StepState            string                  `json:"step_state"`
	Stages               []driveStageView        `json:"stages"`
	ArchivePackageID     string                  `json:"archive_package_id,omitempty"`
	ArchiveDate          string                  `json:"archive_date,omitempty"`
	ArchiveDisplayName   string                  `json:"archive_display_name,omitempty"`
	ArchiveVideoFileName string                  `json:"archive_video_file_name,omitempty"`
	ArchiveState         string                  `json:"archive_state,omitempty"`
	CanAddTranscription  bool                    `json:"can_add_transcription"`
	TranscriptQualities  []transcriptQualityView `json:"transcript_qualities"`
}

type driveWorkOptionView struct {
	Disposition            string `json:"disposition"`
	TargetVideoBitRateKbps int    `json:"target_video_bitrate_kbps,omitempty"`
}

type transcriptQualityView struct {
	TranscriptID       string   `json:"transcript_id"`
	OriginalAssetID    string   `json:"original_asset_id,omitempty"`
	State              string   `json:"state"`
	Reasons            []string `json:"reasons"`
	DurationMillis     int64    `json:"duration_millis"`
	SegmentCount       int      `json:"segment_count"`
	TextCharacterCount int      `json:"text_character_count"`
}

type driveStageView struct {
	ID                  string `json:"stage_id"`
	State               string `json:"state"`
	WaitReason          string `json:"wait_reason,omitempty"`
	WaitTargetStorageID string `json:"wait_target_storage_id,omitempty"`
	CompletedCount      int    `json:"completed_count"`
	TotalCount          int    `json:"total_count"`
}

type archivistSessionView struct {
	SessionID       string              `json:"session_id"`
	Name            string              `json:"name"`
	ArchivistHostID string              `json:"archivist_host_id"`
	Backend         string              `json:"backend"`
	CLIID           string              `json:"cli_id"`
	WorkspaceRootID string              `json:"workspace_root_id"`
	ControlMode     string              `json:"control_mode"`
	JobID           string              `json:"job_id,omitempty"`
	CreatedAt       time.Time           `json:"created_at"`
	Turns           []archivistTurnView `json:"turns"`
	ActionCards     []actionCardView    `json:"action_cards"`
}

type archivistTurnView struct {
	TurnID         string                                  `json:"turn_id"`
	RequestID      string                                  `json:"request_id"`
	State          string                                  `json:"state"`
	Prompt         string                                  `json:"prompt"`
	SubmittedBy    string                                  `json:"submitted_by"`
	CreatedAt      time.Time                               `json:"created_at"`
	UpdatedAt      time.Time                               `json:"updated_at"`
	FinalResponse  *archivistruntime.FinalResponse         `json:"final_response,omitempty"`
	Entries        []archivistruntime.TurnEntry            `json:"entries"`
	ToolCalls      []archivistToolCallView                 `json:"tool_calls"`
	DriveSelection *archivistruntime.DriveSelection        `json:"drive_selection,omitempty"`
	WorkContext    *archivistruntime.ArchiveRunWorkContext `json:"work_context,omitempty"`
}

type archivistToolCallView struct {
	ToolCallID string    `json:"tool_call_id"`
	ToolName   string    `json:"tool_name"`
	TargetID   string    `json:"target_id,omitempty"`
	State      string    `json:"state"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type actionCardView struct {
	ActionCardID     string           `json:"action_card_id"`
	ArchivistTurnID  string           `json:"archivist_turn_id"`
	CommandType      string           `json:"command_type"`
	TargetID         string           `json:"target_id"`
	ExpectedRevision uint64           `json:"expected_revision"`
	State            string           `json:"state"`
	ApprovalRequired bool             `json:"approval_required"`
	Scope            *actionCardScope `json:"scope,omitempty"`
	RejectedBy       string           `json:"rejected_by,omitempty"`
	RejectedAt       time.Time        `json:"rejected_at,omitempty"`
	RejectionReason  string           `json:"rejection_reason,omitempty"`
}

type actionCardScope struct {
	WorkspaceDriveID string `json:"workspace_drive_id,omitempty"`
	ArchiveStorageID string `json:"archive_storage_id,omitempty"`
	FolderCount      int    `json:"folder_count,omitempty"`
}

type stepView struct {
	ID            string    `json:"id"`
	State         string    `json:"state"`
	Progress      float64   `json:"progress"`
	ExecutorKind  string    `json:"executor_kind"`
	ActorID       string    `json:"actor_id,omitempty"`
	WorkerID      string    `json:"worker_id,omitempty"`
	WaitReason    string    `json:"wait_reason,omitempty"`
	ErrorClass    string    `json:"error_class,omitempty"`
	LogReference  string    `json:"log_reference,omitempty"`
	LastEventAt   time.Time `json:"last_event_at"`
	DependencyIDs []string  `json:"dependency_ids"`
}

type archivistActionView struct {
	Action         string    `json:"action"`
	ToolCallID     string    `json:"tool_call_id"`
	ToolCallStatus string    `json:"tool_call_status"`
	OccurredAt     time.Time `json:"occurred_at"`
}

type timelineView struct {
	Kind       string    `json:"kind"`
	Reference  string    `json:"reference"`
	Summary    string    `json:"summary"`
	OccurredAt time.Time `json:"occurred_at"`
}

type jobDetailView struct {
	Summary          jobSummaryView        `json:"summary"`
	Steps            []stepView            `json:"steps"`
	ArchivistActions []archivistActionView `json:"archivist_actions"`
	Workers          []workerView          `json:"workers"`
	Timeline         []timelineView        `json:"timeline"`
}

func projectJobSummary(value archiveworkspace.JobSummary) jobSummaryView {
	result := jobSummaryView{
		JobID: value.JobID, Revision: value.Revision, TemplateName: value.TemplateName, TargetTitle: value.TargetTitle,
		ControlMode: string(value.ControlMode), CurrentStepID: value.CurrentStepID, StepState: string(value.StepState),
		Progress: value.Progress, ExecutorKind: string(value.ExecutorKind), ActorID: value.ActorID, WorkerID: value.WorkerID,
		ArchivistAction: value.ArchivistAction, ToolCallStatus: string(value.ToolCallStatus), WaitReason: string(value.WaitReason),
		ErrorClass: value.ErrorClass, LastEventAt: value.LastEventAt, LogReference: value.LogReference,
		NeedsHumanIntervention: value.NeedsHumanIntervention, InterventionReason: value.InterventionReason,
	}
	if value.Worker != nil {
		worker := projectWorker(*value.Worker)
		result.Worker = &worker
	}
	return result
}

func projectJobDetail(value archiveworkspace.JobView) jobDetailView {
	result := jobDetailView{Summary: projectJobSummary(value.Summary)}
	for _, step := range value.Steps {
		result.Steps = append(result.Steps, stepView{
			ID: step.ID, State: string(step.State), Progress: step.Progress, ExecutorKind: string(step.ExecutorKind),
			ActorID: step.ActorID, WorkerID: step.WorkerID, WaitReason: string(step.WaitReason), ErrorClass: step.ErrorClass,
			LogReference: step.LogReference, LastEventAt: step.LastEventAt,
			DependencyIDs: append([]string(nil), step.DependencyIDs...),
		})
	}
	for _, action := range value.ArchivistActions {
		result.ArchivistActions = append(result.ArchivistActions, archivistActionView{
			Action: action.Action, ToolCallID: action.ToolCallID,
			ToolCallStatus: string(action.ToolCallStatus), OccurredAt: action.OccurredAt,
		})
	}
	for _, worker := range value.Workers {
		result.Workers = append(result.Workers, projectWorker(worker))
	}
	for _, item := range value.Timeline {
		result.Timeline = append(result.Timeline, timelineView{
			Kind: item.Kind, Reference: item.Reference, Summary: item.Summary, OccurredAt: item.OccurredAt,
		})
	}
	return result
}

func projectWorker(value archiveworkspace.WorkerResource) workerView {
	return workerView{
		WorkerID: value.WorkerID, CPUPercent: value.CPUPercent, GPUPercent: value.GPUPercent,
		AvailableMemoryBytes: value.AvailableMemoryBytes, AvailableVRAMBytes: value.AvailableVRAMBytes,
		ScratchFreeBytes: value.ScratchFreeBytes, HeavyRunning: value.HeavyRunning, AcceptingWork: value.AcceptingWork,
		AccessibleStorageIDs: append([]string(nil), value.AccessibleStorageIDs...), HeartbeatAt: value.HeartbeatAt,
	}
}

func peerAddress(remote string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return netip.Addr{}, err
	}
	return netip.ParseAddr(strings.Trim(host, "[]"))
}

func forwardedAddresses(values []string) []netip.Addr {
	if len(values) != 1 || strings.Contains(values[0], ",") {
		return nil
	}
	address, err := netip.ParseAddr(strings.TrimSpace(values[0]))
	if err != nil {
		return nil
	}
	return []netip.Addr{address}
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, code string) {
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code}})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self'; style-src 'self'; script-src 'self'")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(writer, request)
	})
}

func cacheAssets(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "public, max-age=300")
		next.ServeHTTP(writer, request)
	})
}
