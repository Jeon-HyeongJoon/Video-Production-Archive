package httpworker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"videoarchive/internal/application/workercontrol"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

var ErrInvalidConfiguration = errors.New("worker HTTP configuration is invalid")

type Handler struct {
	registry          *worker.Registry
	now               func() time.Time
	dispatcher        Dispatcher
	execution         *workercontrol.Service
	volumes           VolumeInventory
	inventoryObserved func()
}

type Dispatcher interface {
	DispatchAvailable(context.Context) error
}

type VolumeInventory interface {
	Reconcile(context.Context, string, time.Time, []worker.AttachedVolumeObservation) error
	SelectedStorageIDs(context.Context, string) ([]string, error)
}

type heartbeatRequest struct {
	WorkerID              string                             `json:"worker_id"`
	SnapshotID            uint64                             `json:"snapshot_id"`
	ObservedAt            time.Time                          `json:"observed_at"`
	CPUAverage5m          float64                            `json:"cpu_average_5m"`
	GPUAverage5m          float64                            `json:"gpu_average_5m"`
	IdleStableMillis      int64                              `json:"idle_stable_millis"`
	AvailableMemoryBytes  uint64                             `json:"available_memory_bytes"`
	AvailableVRAMBytes    uint64                             `json:"available_vram_bytes"`
	ScratchFreeBytes      uint64                             `json:"scratch_free_bytes"`
	HeavyRunning          int                                `json:"heavy_running"`
	LightRunning          int                                `json:"light_running"`
	BusyPhysicalDeviceIDs []string                           `json:"busy_physical_device_ids"`
	AccessibleStorageIDs  []string                           `json:"accessible_storage_ids"`
	AcceptingWork         bool                               `json:"accepting_work"`
	StorageObservations   []worker.StorageObservation        `json:"storage_observations"`
	AttachedVolumes       []worker.AttachedVolumeObservation `json:"attached_volumes"`
}

type storageInventoryRequest struct {
	WorkerID            string                      `json:"worker_id"`
	SnapshotID          uint64                      `json:"snapshot_id"`
	StorageObservations []worker.StorageObservation `json:"storage_observations"`
}

func New(registry *worker.Registry, now func() time.Time) (*Handler, error) {
	return NewWithDispatcher(registry, now, nil)
}

func NewWithDispatcher(registry *worker.Registry, now func() time.Time, dispatcher Dispatcher) (*Handler, error) {
	return NewWithExecution(registry, now, dispatcher, nil)
}

func NewWithExecution(registry *worker.Registry, now func() time.Time, dispatcher Dispatcher, execution *workercontrol.Service) (*Handler, error) {
	return NewWithExecutionAndVolumes(registry, now, dispatcher, execution, nil)
}

func NewWithExecutionAndVolumes(registry *worker.Registry, now func() time.Time, dispatcher Dispatcher, execution *workercontrol.Service, volumes VolumeInventory) (*Handler, error) {
	if registry == nil || now == nil {
		return nil, ErrInvalidConfiguration
	}
	return &Handler{registry: registry, now: now, dispatcher: dispatcher, execution: execution, volumes: volumes}, nil
}

// SetInventoryObserved installs the Control-owned wake-up signal used after a
// complete storage inventory is committed. The callback must be non-blocking.
func (handler *Handler) SetInventoryObserved(notify func()) {
	handler.inventoryObserved = notify
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/worker/v1/heartbeat":
		handler.heartbeat(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/worker/v1/storage-inventory":
		handler.storageInventory(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/worker/v1/steps/claim" && handler.execution != nil:
		handler.claim(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/worker/v1/steps/lease-renewal" && handler.execution != nil:
		handler.renewLease(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/worker/v1/steps/progress" && handler.execution != nil:
		handler.progress(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/worker/v1/steps/result" && handler.execution != nil:
		handler.result(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/worker/v1/steps/cancellation" && handler.execution != nil:
		handler.cancellation(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/worker/v1/steps/cancellation-result" && handler.execution != nil:
		handler.cancellationResult(writer, request)
	default:
		writeError(writer, http.StatusNotFound, "NOT_FOUND")
	}
}

type progressRequest struct {
	JobID          string                   `json:"job_id"`
	StepID         string                   `json:"step_id"`
	FencingToken   uint64                   `json:"fencing_token"`
	ReportSequence uint64                   `json:"report_sequence"`
	Update         execution.ProgressUpdate `json:"update"`
}

func (handler *Handler) progress(writer http.ResponseWriter, request *http.Request) {
	workerID, credential, ok := workerIdentity(request)
	if !ok || handler.registry.Authenticate(workerID, credential) != nil {
		writeError(writer, http.StatusUnauthorized, "WORKER_AUTHENTICATION_FAILED")
		return
	}
	var payload progressRequest
	if !decodeWorkerJSON(writer, request, &payload) || strings.TrimSpace(payload.JobID) == "" ||
		strings.TrimSpace(payload.StepID) == "" || payload.FencingToken == 0 || payload.ReportSequence == 0 || payload.Update.Validate() != nil {
		writeError(writer, http.StatusBadRequest, "INVALID_STEP_PROGRESS")
		return
	}
	err := handler.execution.ReportProgress(request.Context(), workercontrol.ProgressReport{
		WorkerID: workerID, JobID: strings.TrimSpace(payload.JobID), StepID: strings.TrimSpace(payload.StepID),
		FencingToken: payload.FencingToken, ReportSequence: payload.ReportSequence, Update: payload.Update,
	})
	if errors.Is(err, workflow.ErrStaleFencingToken) {
		writeError(writer, http.StatusConflict, "STALE_FENCING_TOKEN")
		return
	}
	if err != nil {
		writeError(writer, http.StatusConflict, "STEP_PROGRESS_REJECTED")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]string{"state": "RECORDED"}})
}

type leaseRenewalRequest struct {
	JobID        string `json:"job_id"`
	StepID       string `json:"step_id"`
	FencingToken uint64 `json:"fencing_token"`
}

func (handler *Handler) renewLease(writer http.ResponseWriter, request *http.Request) {
	workerID, credential, ok := workerIdentity(request)
	if !ok || handler.registry.Authenticate(workerID, credential) != nil {
		writeError(writer, http.StatusUnauthorized, "WORKER_AUTHENTICATION_FAILED")
		return
	}
	var payload leaseRenewalRequest
	if !decodeWorkerJSON(writer, request, &payload) || strings.TrimSpace(payload.JobID) == "" ||
		strings.TrimSpace(payload.StepID) == "" || payload.FencingToken == 0 {
		writeError(writer, http.StatusBadRequest, "INVALID_LEASE_RENEWAL")
		return
	}
	renewal, err := handler.execution.RenewLease(
		request.Context(), workerID, strings.TrimSpace(payload.JobID), strings.TrimSpace(payload.StepID), payload.FencingToken,
	)
	if errors.Is(err, workflow.ErrStaleFencingToken) {
		writeError(writer, http.StatusConflict, "STALE_FENCING_TOKEN")
		return
	}
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "LEASE_RENEWAL_FAILED")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"lease_expires_at": renewal.ExpiresAt, "renew_after_millis": renewal.RenewAfter.Milliseconds(),
	}})
}

func (handler *Handler) storageInventory(writer http.ResponseWriter, request *http.Request) {
	workerID, credential, ok := workerIdentity(request)
	if !ok || handler.registry.Authenticate(workerID, credential) != nil {
		writeError(writer, http.StatusUnauthorized, "WORKER_AUTHENTICATION_FAILED")
		return
	}
	if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(request.Header.Get("Content-Type"), ";")[0])); mediaType != "application/json" {
		writeError(writer, http.StatusUnsupportedMediaType, "JSON_REQUIRED")
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 32<<20))
	decoder.DisallowUnknownFields()
	var payload storageInventoryRequest
	if err := decoder.Decode(&payload); err != nil || strings.TrimSpace(payload.WorkerID) != workerID || payload.SnapshotID == 0 {
		writeError(writer, http.StatusBadRequest, "INVALID_STORAGE_INVENTORY")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "INVALID_STORAGE_INVENTORY")
		return
	}
	if err := handler.registry.UpdateStorageInventory(workerID, payload.SnapshotID, payload.StorageObservations); err != nil {
		if errors.Is(err, worker.ErrStaleSnapshot) {
			writeError(writer, http.StatusConflict, "STALE_RESOURCE_SNAPSHOT")
			return
		}
		writeError(writer, http.StatusBadRequest, "INVALID_STORAGE_INVENTORY")
		return
	}
	if handler.inventoryObserved != nil {
		handler.inventoryObserved()
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]string{"state": "STORED"}})
}

func (handler *Handler) heartbeat(writer http.ResponseWriter, request *http.Request) {
	if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(request.Header.Get("Content-Type"), ";")[0])); mediaType != "application/json" {
		writeError(writer, http.StatusUnsupportedMediaType, "JSON_REQUIRED")
		return
	}
	credential := bearerCredential(request.Header.Get("Authorization"))
	if credential == "" {
		writeError(writer, http.StatusUnauthorized, "WORKER_AUTHENTICATION_FAILED")
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	var payload heartbeatRequest
	if err := decoder.Decode(&payload); err != nil {
		writeError(writer, http.StatusBadRequest, "INVALID_HEARTBEAT")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) || strings.TrimSpace(payload.WorkerID) == "" ||
		payload.SnapshotID == 0 || payload.ObservedAt.IsZero() || payload.CPUAverage5m < 0 || payload.CPUAverage5m > 100 ||
		payload.GPUAverage5m < 0 || payload.GPUAverage5m > 100 || payload.IdleStableMillis < 0 ||
		payload.HeavyRunning < 0 || payload.LightRunning < 0 {
		writeError(writer, http.StatusBadRequest, "INVALID_HEARTBEAT")
		return
	}
	accessible := make(map[string]bool, len(payload.AccessibleStorageIDs))
	for _, raw := range payload.AccessibleStorageIDs {
		storageID := strings.TrimSpace(raw)
		if storageID == "" || accessible[storageID] {
			writeError(writer, http.StatusBadRequest, "INVALID_HEARTBEAT")
			return
		}
		accessible[storageID] = true
	}
	busyDevices := make(map[string]bool, len(payload.BusyPhysicalDeviceIDs))
	for _, raw := range payload.BusyPhysicalDeviceIDs {
		deviceID := strings.TrimSpace(raw)
		if deviceID == "" || busyDevices[deviceID] {
			writeError(writer, http.StatusBadRequest, "INVALID_HEARTBEAT")
			return
		}
		busyDevices[deviceID] = true
	}
	if err := handler.registry.Connect(strings.TrimSpace(payload.WorkerID), credential, handler.now().UTC(), worker.Metrics{
		SnapshotID: payload.SnapshotID, ObservedAt: payload.ObservedAt.UTC(),
		AverageCPU5m: payload.CPUAverage5m, AverageGPU5m: payload.GPUAverage5m,
		IdleStableFor:        time.Duration(payload.IdleStableMillis) * time.Millisecond,
		AvailableMemoryBytes: payload.AvailableMemoryBytes, AvailableVRAMBytes: payload.AvailableVRAMBytes,
		ScratchFreeBytes: payload.ScratchFreeBytes, HeavyRunning: payload.HeavyRunning, LightRunning: payload.LightRunning,
		BusyPhysicalDeviceIDs: busyDevices, AccessibleStorageIDs: accessible, AcceptingWork: payload.AcceptingWork,
		StorageObservations: payload.StorageObservations, AttachedVolumes: payload.AttachedVolumes,
	}); err != nil {
		if errors.Is(err, worker.ErrInvalidSnapshot) {
			writeError(writer, http.StatusBadRequest, "INVALID_HEARTBEAT")
			return
		}
		if errors.Is(err, worker.ErrStaleSnapshot) {
			writeError(writer, http.StatusConflict, "STALE_RESOURCE_SNAPSHOT")
			return
		}
		writeError(writer, http.StatusUnauthorized, "WORKER_AUTHENTICATION_FAILED")
		return
	}
	if handler.volumes != nil {
		if err := handler.volumes.Reconcile(
			request.Context(), strings.TrimSpace(payload.WorkerID), payload.ObservedAt.UTC(), payload.AttachedVolumes,
		); err != nil {
			writeError(writer, http.StatusServiceUnavailable, "VOLUME_INVENTORY_UNAVAILABLE")
			return
		}
	}
	selectedStorageIDs := []string{}
	if handler.volumes != nil {
		selected, selectionErr := handler.volumes.SelectedStorageIDs(request.Context(), strings.TrimSpace(payload.WorkerID))
		if selectionErr != nil {
			writeError(writer, http.StatusServiceUnavailable, "VOLUME_SELECTION_UNAVAILABLE")
			return
		}
		selectedStorageIDs = selected
	}
	if handler.dispatcher != nil {
		if err := handler.dispatcher.DispatchAvailable(request.Context()); err != nil {
			writeError(writer, http.StatusServiceUnavailable, "DISPATCH_UNAVAILABLE")
			return
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"state": "ONLINE", "selected_source_storage_ids": selectedStorageIDs,
	}})
}

func (handler *Handler) claim(writer http.ResponseWriter, request *http.Request) {
	workerID, credential, ok := workerIdentity(request)
	if !ok || handler.registry.Authenticate(workerID, credential) != nil {
		writeError(writer, http.StatusUnauthorized, "WORKER_AUTHENTICATION_FAILED")
		return
	}
	assignment, err := handler.execution.Claim(request.Context(), workerID)
	if errors.Is(err, workercontrol.ErrNoWork) {
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeError(writer, http.StatusConflict, "WORKER_CLAIM_FAILED")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"job_id": assignment.JobID, "step_id": assignment.StepID,
		"fencing_token": assignment.FencingToken, "lease_expires_at": assignment.LeaseExpiresAt,
		"renew_after_millis": assignment.LeaseRenewAfter.Milliseconds(),
		"resource_class":     assignment.ResourceClass, "processing_kinds": assignment.RequiredProcessingKinds,
		"execution_spec": assignment.ExecutionSpec,
	}})
}

type resultRequest struct {
	JobID               string                      `json:"job_id"`
	StepID              string                      `json:"step_id"`
	FencingToken        uint64                      `json:"fencing_token"`
	Success             bool                        `json:"success"`
	FailureClass        workflow.FailureClass       `json:"failure_class"`
	WaitReason          vocabulary.WaitReason       `json:"wait_reason"`
	WaitTargetStorageID string                      `json:"wait_target_storage_id"`
	ErrorClass          string                      `json:"error_class"`
	CauseCode           string                      `json:"cause_code"`
	ErrorSummary        string                      `json:"error_summary"`
	ProcessExitCode     *int                        `json:"process_exit_code"`
	LogReference        string                      `json:"log_reference"`
	OutputRecord        *execution.StepOutputRecord `json:"output_record"`
}

func (handler *Handler) result(writer http.ResponseWriter, request *http.Request) {
	workerID, credential, ok := workerIdentity(request)
	if !ok || handler.registry.Authenticate(workerID, credential) != nil {
		writeError(writer, http.StatusUnauthorized, "WORKER_AUTHENTICATION_FAILED")
		return
	}
	if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(request.Header.Get("Content-Type"), ";")[0])); mediaType != "application/json" {
		writeError(writer, http.StatusUnsupportedMediaType, "JSON_REQUIRED")
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 32<<20))
	decoder.DisallowUnknownFields()
	var payload resultRequest
	if err := decoder.Decode(&payload); err != nil {
		slog.Warn("reject invalid Worker result payload", "error", err)
		writeError(writer, http.StatusBadRequest, "INVALID_WORKER_RESULT")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) || strings.TrimSpace(payload.JobID) == "" ||
		strings.TrimSpace(payload.StepID) == "" || payload.FencingToken == 0 ||
		(!payload.Success && payload.FailureClass == "") || (payload.Success && payload.OutputRecord == nil) ||
		(!payload.Success && payload.OutputRecord != nil) {
		slog.Warn("reject structurally invalid Worker result", "trailing_error", err, "job_id_present", strings.TrimSpace(payload.JobID) != "",
			"step_id_present", strings.TrimSpace(payload.StepID) != "", "fencing_token_present", payload.FencingToken != 0,
			"success", payload.Success, "output_record_present", payload.OutputRecord != nil)
		writeError(writer, http.StatusBadRequest, "INVALID_WORKER_RESULT")
		return
	}
	err := handler.execution.Report(request.Context(), workercontrol.Result{
		WorkerID: workerID, JobID: payload.JobID, StepID: payload.StepID, FencingToken: payload.FencingToken,
		Success: payload.Success, FailureClass: payload.FailureClass, WaitReason: payload.WaitReason,
		WaitTargetStorageID: strings.TrimSpace(payload.WaitTargetStorageID),
		ErrorClass:          strings.TrimSpace(payload.ErrorClass), CauseCode: strings.TrimSpace(payload.CauseCode),
		ErrorSummary: strings.TrimSpace(payload.ErrorSummary), ProcessExitCode: payload.ProcessExitCode,
		LogReference: strings.TrimSpace(payload.LogReference), OutputRecord: payload.OutputRecord,
	})
	if errors.Is(err, workflow.ErrStaleFencingToken) {
		writeError(writer, http.StatusConflict, "STALE_FENCING_TOKEN")
		return
	}
	if err != nil {
		writeError(writer, http.StatusConflict, "WORKER_RESULT_REJECTED")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]string{"state": "SETTLED"}})
}

type cancellationLookupRequest struct {
	JobID        string `json:"job_id"`
	StepID       string `json:"step_id"`
	FencingToken uint64 `json:"fencing_token"`
}

func (handler *Handler) cancellation(writer http.ResponseWriter, request *http.Request) {
	workerID, credential, ok := workerIdentity(request)
	if !ok || handler.registry.Authenticate(workerID, credential) != nil {
		writeError(writer, http.StatusUnauthorized, "WORKER_AUTHENTICATION_FAILED")
		return
	}
	var payload cancellationLookupRequest
	if !decodeWorkerJSON(writer, request, &payload) || strings.TrimSpace(payload.JobID) == "" ||
		strings.TrimSpace(payload.StepID) == "" || payload.FencingToken == 0 {
		writeError(writer, http.StatusBadRequest, "INVALID_CANCELLATION_LOOKUP")
		return
	}
	cancellation, err := handler.execution.PendingCancellation(
		request.Context(), workerID, strings.TrimSpace(payload.JobID), strings.TrimSpace(payload.StepID), payload.FencingToken,
	)
	if errors.Is(err, workercontrol.ErrNoCancellation) {
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeError(writer, http.StatusConflict, "CANCELLATION_LOOKUP_FAILED")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"cancellation_id": cancellation.ID, "job_id": cancellation.JobID, "step_id": cancellation.StepID,
		"fencing_token": cancellation.FencingToken, "requested_at": cancellation.RequestedAt,
	}})
}

type cancellationResultRequest struct {
	CancellationID           string   `json:"cancellation_id"`
	JobID                    string   `json:"job_id"`
	StepID                   string   `json:"step_id"`
	FencingToken             uint64   `json:"fencing_token"`
	ProcessStopped           bool     `json:"process_stopped"`
	CleanupConfirmed         bool     `json:"cleanup_confirmed"`
	ExternalEffectReferences []string `json:"external_effect_references"`
	LogReference             string   `json:"log_reference"`
}

func (handler *Handler) cancellationResult(writer http.ResponseWriter, request *http.Request) {
	workerID, credential, ok := workerIdentity(request)
	if !ok || handler.registry.Authenticate(workerID, credential) != nil {
		writeError(writer, http.StatusUnauthorized, "WORKER_AUTHENTICATION_FAILED")
		return
	}
	var payload cancellationResultRequest
	if !decodeWorkerJSON(writer, request, &payload) || strings.TrimSpace(payload.CancellationID) == "" ||
		strings.TrimSpace(payload.JobID) == "" || strings.TrimSpace(payload.StepID) == "" || payload.FencingToken == 0 {
		writeError(writer, http.StatusBadRequest, "INVALID_CANCELLATION_RESULT")
		return
	}
	for _, reference := range payload.ExternalEffectReferences {
		if strings.TrimSpace(reference) == "" {
			writeError(writer, http.StatusBadRequest, "INVALID_CANCELLATION_RESULT")
			return
		}
	}
	err := handler.execution.ReportCancellation(request.Context(), workercontrol.CancellationResult{
		CancellationID: strings.TrimSpace(payload.CancellationID), WorkerID: workerID,
		JobID: strings.TrimSpace(payload.JobID), StepID: strings.TrimSpace(payload.StepID), FencingToken: payload.FencingToken,
		ProcessStopped: payload.ProcessStopped, CleanupConfirmed: payload.CleanupConfirmed,
		ExternalEffectReferences: append([]string(nil), payload.ExternalEffectReferences...),
		LogReference:             strings.TrimSpace(payload.LogReference),
	})
	if errors.Is(err, workflow.ErrStaleFencingToken) || errors.Is(err, workercontrol.ErrNoCancellation) {
		writeError(writer, http.StatusConflict, "STALE_FENCING_TOKEN")
		return
	}
	if err != nil {
		writeError(writer, http.StatusConflict, "CANCELLATION_RESULT_REJECTED")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]string{"state": "SETTLED"}})
}

func decodeWorkerJSON(writer http.ResponseWriter, request *http.Request, target any) bool {
	if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(request.Header.Get("Content-Type"), ";")[0])); mediaType != "application/json" {
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return false
	}
	var trailing any
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}

func workerIdentity(request *http.Request) (string, string, bool) {
	workerID := strings.TrimSpace(request.Header.Get("X-Worker-ID"))
	credential := bearerCredential(request.Header.Get("Authorization"))
	return workerID, credential, workerID != "" && credential != ""
}

func (handler *Handler) Statuses(context.Context) ([]worker.Status, error) {
	return handler.registry.Statuses(handler.now().UTC()), nil
}

func bearerCredential(header string) string {
	prefix, credential, found := strings.Cut(strings.TrimSpace(header), " ")
	if !found || !strings.EqualFold(prefix, "Bearer") {
		return ""
	}
	return strings.TrimSpace(credential)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, code string) {
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code}})
}
