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
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/application/dashboard"
	"videoarchive/internal/core/catalog"
	"videoarchive/internal/core/contentreview"
	"videoarchive/internal/core/security"
	"videoarchive/internal/core/summary"
	"videoarchive/internal/core/transcription"
)

var ErrInvalidConfiguration = errors.New("HTTP control configuration is invalid")

type CatalogReader interface {
	Search(context.Context, catalog.SearchQuery) ([]catalog.SearchResult, error)
	SearchTranscripts(context.Context, string) ([]transcription.SearchHit, error)
	SummariesByOriginal(context.Context, string) ([]summary.Summary, error)
	ContentReviews(context.Context, string, uint64) ([]contentreview.Record, error)
}

type DashboardReader interface {
	Inbox(context.Context) ([]dashboard.JobSummary, error)
	Detail(context.Context, string) (dashboard.JobView, error)
}

type Configuration struct {
	Access         security.AccessConfig
	TrustedProxies []netip.Prefix
	Catalog        CatalogReader
	Dashboard      DashboardReader
	Notifications  http.Handler
	Version        string
}

type Handler struct {
	access         security.AccessConfig
	trustedProxies []netip.Prefix
	catalog        CatalogReader
	dashboard      DashboardReader
	notifications  http.Handler
	version        string
	router         http.Handler
}

//go:embed static/*
var staticFiles embed.FS

func New(configuration Configuration) (*Handler, error) {
	if configuration.Catalog == nil || configuration.Dashboard == nil || configuration.Notifications == nil ||
		strings.TrimSpace(configuration.Version) == "" || len(configuration.Access.AllowedNetworks) == 0 {
		return nil, ErrInvalidConfiguration
	}
	handler := &Handler{
		access: configuration.Access, trustedProxies: append([]netip.Prefix(nil), configuration.TrustedProxies...),
		catalog: configuration.Catalog, dashboard: configuration.Dashboard,
		notifications: configuration.Notifications, version: strings.TrimSpace(configuration.Version),
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
	_, err = security.ResolvePrincipal(handler.access, security.AccessRequest{SourceIP: source})
	if errors.Is(err, security.ErrAuthenticationRequired) {
		writeError(writer, http.StatusUnauthorized, "AUTHENTICATION_REQUIRED")
		return
	}
	if err != nil {
		writeError(writer, http.StatusForbidden, "NETWORK_DENIED")
		return
	}
	handler.router.ServeHTTP(writer, request)
}

func (handler *Handler) index(writer http.ResponseWriter, request *http.Request) {
	payload, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "DASHBOARD_UNAVAILABLE")
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
	jobs, err := handler.dashboard.Inbox(request.Context())
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
	job, err := handler.dashboard.Detail(request.Context(), request.PathValue("jobID"))
	if errors.Is(err, dashboard.ErrJobNotFound) {
		writeError(writer, http.StatusNotFound, "JOB_NOT_FOUND")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "QUERY_FAILED")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": projectJobDetail(job)})
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
	AgentAction            string      `json:"agent_action,omitempty"`
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

type agentActionView struct {
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
	Summary      jobSummaryView    `json:"summary"`
	Steps        []stepView        `json:"steps"`
	AgentActions []agentActionView `json:"agent_actions"`
	Workers      []workerView      `json:"workers"`
	Timeline     []timelineView    `json:"timeline"`
}

func projectJobSummary(value dashboard.JobSummary) jobSummaryView {
	result := jobSummaryView{
		JobID: value.JobID, Revision: value.Revision, TemplateName: value.TemplateName, TargetTitle: value.TargetTitle,
		ControlMode: string(value.ControlMode), CurrentStepID: value.CurrentStepID, StepState: string(value.StepState),
		Progress: value.Progress, ExecutorKind: string(value.ExecutorKind), ActorID: value.ActorID, WorkerID: value.WorkerID,
		AgentAction: value.AgentAction, ToolCallStatus: string(value.ToolCallStatus), WaitReason: string(value.WaitReason),
		ErrorClass: value.ErrorClass, LastEventAt: value.LastEventAt, LogReference: value.LogReference,
		NeedsHumanIntervention: value.NeedsHumanIntervention, InterventionReason: value.InterventionReason,
	}
	if value.Worker != nil {
		worker := projectWorker(*value.Worker)
		result.Worker = &worker
	}
	return result
}

func projectJobDetail(value dashboard.JobView) jobDetailView {
	result := jobDetailView{Summary: projectJobSummary(value.Summary)}
	for _, step := range value.Steps {
		result.Steps = append(result.Steps, stepView{
			ID: step.ID, State: string(step.State), Progress: step.Progress, ExecutorKind: string(step.ExecutorKind),
			ActorID: step.ActorID, WorkerID: step.WorkerID, WaitReason: string(step.WaitReason), ErrorClass: step.ErrorClass,
			LogReference: step.LogReference, LastEventAt: step.LastEventAt,
			DependencyIDs: append([]string(nil), step.DependencyIDs...),
		})
	}
	for _, action := range value.AgentActions {
		result.AgentActions = append(result.AgentActions, agentActionView{
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

func projectWorker(value dashboard.WorkerResource) workerView {
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
