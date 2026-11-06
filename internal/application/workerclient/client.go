package workerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/core/device"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

var (
	ErrCredentialUnavailable = errors.New("worker credential is unavailable")
	ErrHeartbeatRejected     = errors.New("worker heartbeat was rejected")
	ErrInvalidAssignment     = errors.New("worker assignment is invalid")
	ErrLeaseRenewalRejected  = errors.New("worker execution lease renewal was rejected")
)

type SecretResolver interface {
	Resolve(reference string) (string, error)
}

type ResourceProbe interface {
	Snapshot(context.Context) (worker.Metrics, error)
}

type StorageProbe interface {
	Observe(context.Context) ([]worker.StorageObservation, error)
}

type VolumeProbe interface {
	Poll(context.Context, string, time.Time) ([]device.VolumeAttachmentEvent, error)
	Connected() []device.ConnectedVolume
}

type SelectedStorageProbe interface {
	Observe(context.Context, []string, []device.ConnectedVolume) ([]worker.StorageObservation, error)
}

type Assignment struct {
	JobID                   string                      `json:"job_id"`
	StepID                  string                      `json:"step_id"`
	FencingToken            uint64                      `json:"fencing_token"`
	LeaseExpiresAt          time.Time                   `json:"lease_expires_at"`
	LeaseRenewAfterMillis   int64                       `json:"renew_after_millis"`
	ResourceClass           vocabulary.ResourceClass    `json:"resource_class"`
	RequiredProcessingKinds []vocabulary.ProcessingKind `json:"processing_kinds"`
	ExecutionSpec           execution.StepExecutionSpec `json:"execution_spec"`
}

type ExecutionResult struct {
	Success                bool
	FailureClass           workflow.FailureClass
	WaitReason             vocabulary.WaitReason
	WaitTargetStorageID    string
	ErrorClass             string
	LogReference           string
	Outputs                []execution.StepOutput
	VerificationReferences []string
	InspectedMedia         []execution.InspectedMedia
	Cancellation           *CancellationExecutionResult
}

type CancellationExecutionResult struct {
	ProcessStopped           bool
	CleanupConfirmed         bool
	ExternalEffectReferences []string
	LogReference             string
}

type cancellation struct {
	ID           string
	JobID        string
	StepID       string
	FencingToken uint64
	RequestedAt  time.Time
}

type StepExecutor interface {
	Execute(context.Context, Assignment) ExecutionResult
}

type AttachedStorageRoots interface {
	SyncAttachedVolumes([]device.ConnectedVolume) error
}

type Client struct {
	configuration            workerconfig.Configuration
	secrets                  SecretResolver
	http                     *http.Client
	probe                    ResourceProbe
	now                      func() time.Time
	mu                       sync.Mutex
	lastSnapshotID           uint64
	executor                 StepExecutor
	storageProbe             StorageProbe
	volumeProbe              VolumeProbe
	selectedProbe            SelectedStorageProbe
	attachedStorageRoots     AttachedStorageRoots
	selectedSourceStorageIDs []string
	executing                bool
}

func New(configuration workerconfig.Configuration, secrets SecretResolver, httpClient *http.Client, probe ResourceProbe, now func() time.Time) (*Client, error) {
	return NewWithExecutor(configuration, secrets, httpClient, probe, now, nil)
}

func NewWithExecutor(configuration workerconfig.Configuration, secrets SecretResolver, httpClient *http.Client, probe ResourceProbe, now func() time.Time, executor StepExecutor) (*Client, error) {
	return NewWithExecutorAndStorageProbe(configuration, secrets, httpClient, probe, now, executor, nil)
}

func NewWithExecutorAndStorageProbe(configuration workerconfig.Configuration, secrets SecretResolver, httpClient *http.Client, probe ResourceProbe, now func() time.Time, executor StepExecutor, storageProbe StorageProbe) (*Client, error) {
	return NewWithExecutorAndInventoryProbes(configuration, secrets, httpClient, probe, now, executor, storageProbe, nil, nil, nil)
}

func NewWithExecutorAndInventoryProbes(configuration workerconfig.Configuration, secrets SecretResolver, httpClient *http.Client, probe ResourceProbe, now func() time.Time, executor StepExecutor, storageProbe StorageProbe, volumeProbe VolumeProbe, selectedProbe SelectedStorageProbe, attachedStorageRoots AttachedStorageRoots) (*Client, error) {
	if configuration.ID == "" || configuration.ControlURL == nil || configuration.HeartbeatInterval <= 0 ||
		len(configuration.AccessibleStorageIDs) == 0 || secrets == nil || httpClient == nil || probe == nil || now == nil {
		return nil, workerconfig.ErrInvalidConfiguration
	}
	if configuration.ClaimInterval <= 0 {
		configuration.ClaimInterval = time.Second
	}
	return &Client{
		configuration: configuration, secrets: secrets, http: httpClient, probe: probe, now: now,
		executor: executor, storageProbe: storageProbe, volumeProbe: volumeProbe, selectedProbe: selectedProbe,
		attachedStorageRoots: attachedStorageRoots,
	}, nil
}

func (client *Client) Heartbeat(ctx context.Context) error {
	credential, err := client.secrets.Resolve(client.configuration.CredentialReference)
	if err != nil {
		return fmt.Errorf("resolve worker credential: %w", err)
	}
	if credential == "" {
		return ErrCredentialUnavailable
	}
	snapshot, err := client.probe.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("observe Worker resources: %w", err)
	}
	if client.storageProbe != nil {
		snapshot.StorageObservations, err = client.storageProbe.Observe(ctx)
		if err != nil {
			return fmt.Errorf("observe Worker storage: %w", err)
		}
	}
	observedAt := client.now().UTC()
	if !snapshot.ObservedAt.IsZero() {
		observedAt = snapshot.ObservedAt.UTC()
	}
	accessibleStorageIDs := append([]string(nil), client.configuration.AccessibleStorageIDs...)
	var connectedVolumes []device.ConnectedVolume
	if client.volumeProbe != nil {
		if _, err := client.volumeProbe.Poll(ctx, client.configuration.ID, observedAt); err != nil {
			return fmt.Errorf("observe attached volumes: %w", err)
		}
		connectedVolumes = client.volumeProbe.Connected()
		for _, volume := range connectedVolumes {
			snapshot.AttachedVolumes = append(snapshot.AttachedVolumes, worker.AttachedVolumeObservation{
				PhysicalDeviceID: volume.PhysicalDeviceID, VolumeID: volume.VolumeID, StorageID: volume.StorageID,
				ArchiveStorageID: volume.ArchiveStorageID,
				Model:            volume.Model, Label: volume.Label, FileSystem: volume.FileSystem,
				CapacityBytes: volume.CapacityBytes, FreeBytes: volume.FreeBytes,
			})
			accessibleStorageIDs = append(accessibleStorageIDs, volume.StorageID, volume.ArchiveStorageID)
		}
		sort.Slice(snapshot.AttachedVolumes, func(i, j int) bool {
			return snapshot.AttachedVolumes[i].VolumeID < snapshot.AttachedVolumes[j].VolumeID
		})
		if err := client.syncAttachedStorageRoots(connectedVolumes); err != nil {
			return fmt.Errorf("open attached Storage roots: %w", err)
		}
	}
	if client.selectedProbe != nil && len(connectedVolumes) > 0 {
		selectedObservations, observeErr := client.selectedProbe.Observe(ctx, client.selectedSourceIDs(), connectedVolumes)
		if observeErr != nil {
			return fmt.Errorf("observe selected source storage: %w", observeErr)
		}
		snapshot.StorageObservations = append(snapshot.StorageObservations, selectedObservations...)
		sort.Slice(snapshot.StorageObservations, func(i, j int) bool {
			return snapshot.StorageObservations[i].StorageID < snapshot.StorageObservations[j].StorageID
		})
	}
	accessibleStorageIDs = uniqueSorted(accessibleStorageIDs)
	snapshotID := client.nextSnapshotID(observedAt)
	busyDevices := trueKeys(snapshot.BusyPhysicalDeviceIDs)
	heartbeatStorage := make([]worker.StorageObservation, 0, len(snapshot.StorageObservations))
	for _, observation := range snapshot.StorageObservations {
		observation.Folders = nil
		heartbeatStorage = append(heartbeatStorage, observation)
	}
	payload, err := json.Marshal(map[string]any{
		"worker_id": client.configuration.ID, "snapshot_id": snapshotID, "observed_at": observedAt,
		"cpu_average_5m": snapshot.AverageCPU5m, "gpu_average_5m": snapshot.AverageGPU5m,
		"idle_stable_millis":     snapshot.IdleStableFor.Milliseconds(),
		"available_memory_bytes": snapshot.AvailableMemoryBytes, "available_vram_bytes": snapshot.AvailableVRAMBytes,
		"scratch_free_bytes": snapshot.ScratchFreeBytes, "heavy_running": snapshot.HeavyRunning,
		"light_running": snapshot.LightRunning, "busy_physical_device_ids": busyDevices,
		"accepting_work": snapshot.AcceptingWork, "accessible_storage_ids": accessibleStorageIDs,
		"storage_observations": heartbeatStorage,
		"attached_volumes":     snapshot.AttachedVolumes,
	})
	if err != nil {
		return err
	}
	endpoint := *client.configuration.ControlURL
	endpoint.Path = "/worker/v1/heartbeat"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential)
	response, err := client.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return fmt.Errorf("%w: HTTP %d", ErrHeartbeatRejected, response.StatusCode)
	}
	var heartbeatResponse struct {
		Data struct {
			State                    string   `json:"state"`
			SelectedSourceStorageIDs []string `json:"selected_source_storage_ids"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&heartbeatResponse); err != nil || heartbeatResponse.Data.State != "ONLINE" {
		return ErrHeartbeatRejected
	}
	client.setSelectedSourceIDs(heartbeatResponse.Data.SelectedSourceStorageIDs)
	if len(snapshot.StorageObservations) == 0 {
		return nil
	}
	return client.reportStorageInventory(ctx, credential, snapshotID, snapshot.StorageObservations)
}

func (client *Client) reportStorageInventory(ctx context.Context, credential string, snapshotID uint64, observations []worker.StorageObservation) error {
	payload, err := json.Marshal(map[string]any{
		"worker_id": client.configuration.ID, "snapshot_id": snapshotID, "storage_observations": observations,
	})
	if err != nil {
		return err
	}
	endpoint := *client.configuration.ControlURL
	endpoint.Path = "/worker/v1/storage-inventory"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("X-Worker-ID", client.configuration.ID)
	response, err := client.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: storage inventory HTTP %d", ErrHeartbeatRejected, response.StatusCode)
	}
	return nil
}

func (client *Client) syncAttachedStorageRoots(volumes []device.ConnectedVolume) error {
	if client.attachedStorageRoots == nil {
		return nil
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	// StorageRoot removal can close an OS handle, so reconciliation is deferred
	// while the single Worker execution slot owns any current capability.
	if client.executing {
		return nil
	}
	return client.attachedStorageRoots.SyncAttachedVolumes(volumes)
}

func (client *Client) selectedSourceIDs() []string {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]string(nil), client.selectedSourceStorageIDs...)
}

func (client *Client) setSelectedSourceIDs(values []string) {
	client.mu.Lock()
	client.selectedSourceStorageIDs = uniqueSorted(values)
	client.mu.Unlock()
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func (client *Client) nextSnapshotID(observedAt time.Time) uint64 {
	client.mu.Lock()
	defer client.mu.Unlock()
	candidate := uint64(observedAt.UnixNano())
	if candidate <= client.lastSnapshotID {
		candidate = client.lastSnapshotID + 1
	}
	client.lastSnapshotID = candidate
	return candidate
}

func trueKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key, enabled := range values {
		if enabled {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func (client *Client) Run(ctx context.Context, report func(error)) error {
	if report == nil {
		report = func(error) {}
	}
	heartbeats := time.NewTicker(client.configuration.HeartbeatInterval)
	defer heartbeats.Stop()
	claims := time.NewTicker(client.configuration.ClaimInterval)
	defer claims.Stop()
	if err := client.Heartbeat(ctx); err != nil && !errors.Is(err, context.Canceled) {
		report(err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-heartbeats.C:
			if err := client.Heartbeat(ctx); err != nil && !errors.Is(err, context.Canceled) {
				report(err)
			}
		case <-claims.C:
			if client.executor != nil && client.beginExecution() {
				assignment, err := client.claim(ctx)
				if err != nil {
					client.endExecution()
					if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
						report(err)
					}
				} else if assignment == nil {
					client.endExecution()
				} else {
					go client.execute(ctx, *assignment, report)
				}
			}
		}
	}
}

func (client *Client) beginExecution() bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.executing {
		return false
	}
	client.executing = true
	return true
}

func (client *Client) endExecution() {
	client.mu.Lock()
	client.executing = false
	client.mu.Unlock()
}

func (client *Client) claim(ctx context.Context) (*Assignment, error) {
	credential, err := client.secrets.Resolve(client.configuration.CredentialReference)
	if err != nil || credential == "" {
		return nil, ErrCredentialUnavailable
	}
	endpoint := *client.configuration.ControlURL
	endpoint.Path = "/worker/v1/steps/claim"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("X-Worker-ID", client.configuration.ID)
	response, err := client.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return nil, fmt.Errorf("Worker claim HTTP %d: %s", response.StatusCode, payload)
	}
	var payload struct {
		Data Assignment `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&payload); err != nil {
		return nil, err
	}
	if payload.Data.ExecutionSpec.Validate() != nil || payload.Data.ExecutionSpec.JobID != payload.Data.JobID ||
		payload.Data.ExecutionSpec.StepID != payload.Data.StepID {
		return nil, ErrInvalidAssignment
	}
	return &payload.Data, nil
}

func (client *Client) execute(ctx context.Context, assignment Assignment, reportError func(error)) {
	defer client.endExecution()
	executionContext, cancelExecution := context.WithCancel(ctx)
	defer cancelExecution()
	resultChannel := make(chan ExecutionResult, 1)
	go func() { resultChannel <- client.executor.Execute(executionContext, assignment) }()
	ticker := time.NewTicker(client.configuration.ClaimInterval)
	defer ticker.Stop()
	renewAfter := time.Duration(assignment.LeaseRenewAfterMillis) * time.Millisecond
	if renewAfter <= 0 {
		renewAfter = 30 * time.Second
	}
	nextLeaseRenewal := time.Now().Add(renewAfter)
	leaseDeadline := time.Now().Add(renewAfter * 2)
	for {
		select {
		case <-ctx.Done():
			cancelExecution()
			return
		case result := <-resultChannel:
			if err := client.reportResult(ctx, assignment, result); err != nil && !errors.Is(err, context.Canceled) {
				reportError(err)
			}
			return
		case <-ticker.C:
			if !time.Now().Before(nextLeaseRenewal) {
				renewedAfter, err := client.renewLease(ctx, assignment)
				if err != nil {
					reportError(err)
					if !time.Now().Before(leaseDeadline) {
						cancelExecution()
						<-resultChannel
						return
					}
				} else {
					renewAfter = renewedAfter
					nextLeaseRenewal = time.Now().Add(renewAfter)
					leaseDeadline = time.Now().Add(renewAfter * 2)
				}
			}
			pending, err := client.pendingCancellation(ctx, assignment)
			if err != nil {
				reportError(err)
				continue
			}
			if pending == nil {
				continue
			}
			cancelExecution()
			settlement := &CancellationExecutionResult{ProcessStopped: false, CleanupConfirmed: false}
			confirmationTimer := time.NewTimer(client.cancellationConfirmationTimeout())
			select {
			case result := <-resultChannel:
				if result.Cancellation != nil {
					settlement = result.Cancellation
				} else {
					settlement = &CancellationExecutionResult{ProcessStopped: true, CleanupConfirmed: false, LogReference: result.LogReference}
				}
				if !confirmationTimer.Stop() {
					select {
					case <-confirmationTimer.C:
					default:
					}
				}
			case <-confirmationTimer.C:
				// A subprocess that ignores cancellation must never let the Worker
				// silently release itself for new work. Reporting uncertainty keeps
				// the reservation and quarantines this Worker until reconciliation.
			case <-ctx.Done():
				confirmationTimer.Stop()
				return
			}
			if err := client.reportCancellation(ctx, assignment, *pending, *settlement); err != nil && !errors.Is(err, context.Canceled) {
				reportError(err)
			}
			return
		}
	}
}

func (client *Client) cancellationConfirmationTimeout() time.Duration {
	timeout := client.configuration.ClaimInterval * 2
	if timeout < 100*time.Millisecond {
		return 100 * time.Millisecond
	}
	if timeout > 10*time.Second {
		return 10 * time.Second
	}
	return timeout
}

func (client *Client) renewLease(ctx context.Context, assignment Assignment) (time.Duration, error) {
	payload, err := json.Marshal(map[string]any{
		"job_id": assignment.JobID, "step_id": assignment.StepID, "fencing_token": assignment.FencingToken,
	})
	if err != nil {
		return 0, err
	}
	response, err := client.workerRequest(ctx, "/worker/v1/steps/lease-renewal", payload)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return 0, fmt.Errorf("%w: HTTP %d", ErrLeaseRenewalRejected, response.StatusCode)
	}
	var result struct {
		Data struct {
			LeaseExpiresAt   time.Time `json:"lease_expires_at"`
			RenewAfterMillis int64     `json:"renew_after_millis"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil || result.Data.LeaseExpiresAt.IsZero() || result.Data.RenewAfterMillis <= 0 {
		return 0, ErrLeaseRenewalRejected
	}
	return time.Duration(result.Data.RenewAfterMillis) * time.Millisecond, nil
}

func (client *Client) pendingCancellation(ctx context.Context, assignment Assignment) (*cancellation, error) {
	payload, err := json.Marshal(map[string]any{
		"job_id": assignment.JobID, "step_id": assignment.StepID, "fencing_token": assignment.FencingToken,
	})
	if err != nil {
		return nil, err
	}
	response, err := client.workerRequest(ctx, "/worker/v1/steps/cancellation", payload)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return nil, fmt.Errorf("Worker cancellation lookup HTTP %d: %s", response.StatusCode, body)
	}
	var decoded struct {
		Data struct {
			ID           string    `json:"cancellation_id"`
			JobID        string    `json:"job_id"`
			StepID       string    `json:"step_id"`
			FencingToken uint64    `json:"fencing_token"`
			RequestedAt  time.Time `json:"requested_at"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&decoded); err != nil {
		return nil, err
	}
	if decoded.Data.ID == "" || decoded.Data.JobID != assignment.JobID || decoded.Data.StepID != assignment.StepID ||
		decoded.Data.FencingToken != assignment.FencingToken {
		return nil, errors.New("Worker cancellation response does not match assignment")
	}
	return &cancellation{
		ID: decoded.Data.ID, JobID: decoded.Data.JobID, StepID: decoded.Data.StepID,
		FencingToken: decoded.Data.FencingToken, RequestedAt: decoded.Data.RequestedAt,
	}, nil
}

func (client *Client) reportCancellation(
	ctx context.Context,
	assignment Assignment,
	pending cancellation,
	result CancellationExecutionResult,
) error {
	credential, err := client.secrets.Resolve(client.configuration.CredentialReference)
	if err != nil || credential == "" {
		return ErrCredentialUnavailable
	}
	payload, err := json.Marshal(map[string]any{
		"cancellation_id": pending.ID, "job_id": assignment.JobID, "step_id": assignment.StepID,
		"fencing_token": assignment.FencingToken, "process_stopped": result.ProcessStopped,
		"cleanup_confirmed":          result.CleanupConfirmed,
		"external_effect_references": result.ExternalEffectReferences, "log_reference": result.LogReference,
	})
	if err != nil {
		return err
	}
	for {
		retry, reportErr := client.postCancellationResult(ctx, credential, payload)
		if reportErr == nil || !retry {
			return reportErr
		}
		timer := time.NewTimer(client.configuration.ClaimInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (client *Client) postCancellationResult(ctx context.Context, credential string, payload []byte) (bool, error) {
	endpoint := *client.configuration.ControlURL
	endpoint.Path = "/worker/v1/steps/cancellation-result"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return false, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("X-Worker-ID", client.configuration.ID)
	response, err := client.http.Do(request)
	if err != nil {
		return !errors.Is(err, context.Canceled), err
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return true, errors.Join(readErr, closeErr)
	}
	if response.StatusCode == http.StatusOK {
		return false, nil
	}
	retry := response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
	return retry, fmt.Errorf("Worker cancellation result HTTP %d: %s", response.StatusCode, body)
}

func (client *Client) workerRequest(ctx context.Context, path string, payload []byte) (*http.Response, error) {
	credential, err := client.secrets.Resolve(client.configuration.CredentialReference)
	if err != nil || credential == "" {
		return nil, ErrCredentialUnavailable
	}
	endpoint := *client.configuration.ControlURL
	endpoint.Path = path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("X-Worker-ID", client.configuration.ID)
	return client.http.Do(request)
}

func (client *Client) reportResult(ctx context.Context, assignment Assignment, result ExecutionResult) error {
	credential, err := client.secrets.Resolve(client.configuration.CredentialReference)
	if err != nil || credential == "" {
		return ErrCredentialUnavailable
	}
	var outputRecord *execution.StepOutputRecord
	if result.Success {
		record, recordErr := execution.NewStepOutputRecordWithInspections(
			assignment.ExecutionSpec, assignment.FencingToken, client.configuration.ID, client.now().UTC(),
			result.Outputs, result.VerificationReferences, result.InspectedMedia,
		)
		if recordErr != nil {
			return recordErr
		}
		outputRecord = &record
	}
	payload, err := json.Marshal(map[string]any{
		"job_id": assignment.JobID, "step_id": assignment.StepID, "fencing_token": assignment.FencingToken,
		"success": result.Success, "failure_class": result.FailureClass, "wait_reason": result.WaitReason,
		"wait_target_storage_id": result.WaitTargetStorageID,
		"error_class":            result.ErrorClass, "log_reference": result.LogReference, "output_record": outputRecord,
	})
	if err != nil {
		return err
	}
	renewAfter := time.Duration(assignment.LeaseRenewAfterMillis) * time.Millisecond
	if renewAfter <= 0 {
		renewAfter = 30 * time.Second
	}
	nextLeaseRenewal := time.Now().Add(renewAfter)
	for {
		retry, reportErr := client.postResult(ctx, credential, payload)
		if reportErr == nil || !retry {
			return reportErr
		}
		if !time.Now().Before(nextLeaseRenewal) {
			renewedAfter, renewalErr := client.renewLease(ctx, assignment)
			if renewalErr == nil {
				renewAfter = renewedAfter
				nextLeaseRenewal = time.Now().Add(renewAfter)
			}
		}
		wait := client.configuration.ClaimInterval
		if untilRenewal := time.Until(nextLeaseRenewal); untilRenewal > 0 && untilRenewal < wait {
			wait = untilRenewal
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (client *Client) postResult(ctx context.Context, credential string, payload []byte) (bool, error) {
	endpoint := *client.configuration.ControlURL
	endpoint.Path = "/worker/v1/steps/result"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return false, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("X-Worker-ID", client.configuration.ID)
	response, err := client.http.Do(request)
	if err != nil {
		return !errors.Is(err, context.Canceled), err
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return true, errors.Join(readErr, closeErr)
	}
	if response.StatusCode == http.StatusOK {
		return false, nil
	}
	retry := response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
	return retry, fmt.Errorf("Worker result HTTP %d: %s", response.StatusCode, body)
}
