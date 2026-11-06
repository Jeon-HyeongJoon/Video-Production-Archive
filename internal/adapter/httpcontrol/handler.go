package httpcontrol

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	appdriveintake "videoarchive/internal/application/archiveintake"
	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/application/archiveorganization"
	"videoarchive/internal/application/archivetranscription"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/application/archivistcontrol"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/catalog"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/contentreview"
	"videoarchive/internal/core/security"
	"videoarchive/internal/core/summary"
	"videoarchive/internal/core/transcription"
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
	ReleaseActionCard(context.Context, string) error
	BindActionCardArchiveRun(context.Context, string, string, uint64) error
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
