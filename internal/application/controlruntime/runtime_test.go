package controlruntime_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/adapter/bootconfig"
	"videoarchive/internal/adapter/sqlitestorage"
	"videoarchive/internal/application/controlruntime"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: CTL-002
func TestControlRuntimeBootstrapsPersistentStorageAndHTTPAcrossRestart(t *testing.T) {
	directory := t.TempDir()
	configurationPath := filepath.Join(directory, "config.yaml")
	valid := `schema_version: 1
database: {path: data/control.db}
server:
  listen_address: 127.0.0.1:0
  allowed_cidrs: [127.0.0.0/8]
  allowed_origins: [http://127.0.0.1:8080]
  trusted_proxies: []
security: {auth_mode: disabled, mcp_listen_address: 127.0.0.1:8081}
features: {}
provider_accounts: []
workers:
  heartbeat_ttl: 30s
  definitions:
    - id: worker-main
      enabled: true
      credential_ref: wincred://worker/main
      processing_kinds: [INSPECT, RENDER]
      allowed_storage_ids: [scratch-main]
storage:
  - id: scratch-main
    role: SCRATCH
    physical_device_id: drive-1
    partition_id: partition-1
    volume_id: volume-1
    root: storage/scratch
`
	if err := os.WriteFile(configurationPath, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := bootconfig.Load(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		runtime, err := controlruntime.Open(context.Background(), configuration, "v1-test")
		if err != nil {
			t.Fatalf("runtime boot %d failed: %v", attempt, err)
		}
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		request.RemoteAddr = "127.0.0.1:45000"
		runtime.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"version":"v1-test"`) {
			t.Fatalf("runtime health response=%d %s", recorder.Code, recorder.Body.String())
		}
		if err := runtime.Close(); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := sqlitestorage.Open(configuration.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	access, err := repository.ResolveAccess(context.Background(), "scratch-main")
	repository.Close()
	if err != nil || access.WorkspaceDriveID != "drive-1" || access.Role != vocabulary.StorageScratch || access.Root != "storage/scratch" {
		t.Fatalf("boot storage topology was not persisted: access=%+v err=%v", access, err)
	}

	conflictPath := filepath.Join(directory, "conflict.yaml")
	conflict := strings.Replace(valid, "root: storage/scratch", "root: storage/other", 1)
	if err := os.WriteFile(conflictPath, []byte(conflict), 0o600); err != nil {
		t.Fatal(err)
	}
	conflictingConfiguration, err := bootconfig.Load(conflictPath)
	if err != nil {
		t.Fatal(err)
	}
	conflictingConfiguration.DatabasePath = configuration.DatabasePath
	if _, err := controlruntime.Open(context.Background(), conflictingConfiguration, "v1-test"); !errors.Is(err, sqlitestorage.ErrTopologyConflict) {
		t.Fatalf("conflicting boot topology was accepted: %v", err)
	}
}

// Requirement: CTL-003
func TestBrowserCommandCreatesPersistentJobWithCSRFAndIdempotency(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	runtime, err := controlruntime.Open(context.Background(), configuration, "v1-test")
	if err != nil {
		t.Fatal(err)
	}

	create := `{"job_id":"JOB-HTTP-1","command_id":"COMMAND-CREATE-1","idempotency_key":"JOB-HTTP-1:create","reason":"operator created occasion job","template_name":"occasion-recording","target_title":"가을 행사","control_mode":"MANUAL","expected_revision":0}`
	denied := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs", create, nil, "", "")
	if denied.Code != http.StatusForbidden {
		t.Fatalf("mutation without browser proof returned %d: %s", denied.Code, denied.Body.String())
	}
	cookie, token := runtimeSession(t, runtime.Handler())
	created := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs", create, cookie, token, "http://127.0.0.1:8080")
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"revision":1`) {
		t.Fatalf("create returned %d: %s", created.Code, created.Body.String())
	}
	replayPayload := strings.Replace(create, "COMMAND-CREATE-1", "COMMAND-CREATE-2", 1)
	replayed := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs", replayPayload, cookie, token, "http://127.0.0.1:8080")
	if replayed.Code != http.StatusCreated || !strings.Contains(replayed.Body.String(), `"command_id":"COMMAND-CREATE-1"`) {
		t.Fatalf("idempotent replay returned %d: %s", replayed.Code, replayed.Body.String())
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	ledger, err := sql.Open("sqlite", configuration.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"aggregate_states": 1, "command_records": 2, "domain_events": 1, "idempotency_receipts": 1} {
		var count int
		if err := ledger.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE "+ledgerTargetColumn(table)+" = ?", ledgerTargetValue(table)).Scan(&count); err != nil {
			ledger.Close()
			t.Fatal(err)
		}
		if count != want {
			ledger.Close()
			t.Fatalf("%s count=%d want=%d", table, count, want)
		}
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := controlruntime.Open(context.Background(), configuration, "v1-test")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	jobView := runtimeRequest(restarted.Handler(), http.MethodGet, "/api/v1/jobs/JOB-HTTP-1", "", nil, "", "")
	if jobView.Code != http.StatusOK || !strings.Contains(jobView.Body.String(), `"template_name":"occasion-recording"`) ||
		!strings.Contains(jobView.Body.String(), `"target_title":"가을 행사"`) || !strings.Contains(jobView.Body.String(), `"step_state":"QUEUED"`) {
		t.Fatalf("persistent job view returned %d: %s", jobView.Code, jobView.Body.String())
	}
}

// Requirement: CTL-004
func TestBrowserCancelCommandUsesExpectedRevisionAndWorkflowTransition(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	runtime, err := controlruntime.Open(context.Background(), configuration, "v1-test")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, token := runtimeSession(t, runtime.Handler())
	origin := "http://127.0.0.1:8080"
	create := `{"job_id":"JOB-CANCEL-1","command_id":"COMMAND-CREATE","idempotency_key":"JOB-CANCEL-1:create","reason":"create cancellable job","template_name":"final-edit","target_title":"최종 편집본","control_mode":"ASSISTED","expected_revision":0}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs", create, cookie, token, origin); response.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %s", response.Code, response.Body.String())
	}
	cancel := `{"command_id":"COMMAND-CANCEL","idempotency_key":"JOB-CANCEL-1:cancel","reason":"operator cancelled queued job","expected_revision":1}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs/JOB-CANCEL-1/cancel", cancel, cookie, token, origin); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"revision":2`) {
		t.Fatalf("cancel returned %d: %s", response.Code, response.Body.String())
	}
	stale := strings.Replace(cancel, "JOB-CANCEL-1:cancel", "JOB-CANCEL-1:stale-cancel", 1)
	stale = strings.Replace(stale, "COMMAND-CANCEL", "COMMAND-STALE-CANCEL", 1)
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs/JOB-CANCEL-1/cancel", stale, cookie, token, origin); response.Code != http.StatusConflict {
		t.Fatalf("stale cancel returned %d: %s", response.Code, response.Body.String())
	}
	jobView := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs/JOB-CANCEL-1", "", nil, "", "")
	if jobView.Code != http.StatusOK || !strings.Contains(jobView.Body.String(), `"step_state":"CANCELLED"`) ||
		!strings.Contains(jobView.Body.String(), `"revision":2`) {
		t.Fatalf("cancelled projection returned %d: %s", jobView.Code, jobView.Body.String())
	}
}

type runtimeSecret string

func (secret runtimeSecret) Resolve(string) (string, error) { return string(secret), nil }

// Requirement: WRK-005
func TestWorkerHeartbeatAPIAuthenticatesAndReportsLiveRegistryStatus(t *testing.T) {
	configuration := runtimeTestConfiguration(t)
	now := time.Date(2026, 10, 26, 11, 30, 0, 0, time.UTC)
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "v1-test", controlruntime.Dependencies{
		Secrets: runtimeSecret("service-secret"), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	payload := `{"worker_id":"worker-main","accepting_work":true,"heavy_running":0,"accessible_storage_ids":["scratch-main"]}`
	if response := workerHeartbeatRequest(runtime.Handler(), payload, "wrong-secret"); response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid Worker credential returned %d: %s", response.Code, response.Body.String())
	}
	if response := workerHeartbeatRequest(runtime.Handler(), payload, "service-secret"); response.Code != http.StatusOK {
		t.Fatalf("valid Worker heartbeat returned %d: %s", response.Code, response.Body.String())
	}
	statuses := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/workers", "", nil, "", "")
	body := statuses.Body.String()
	if statuses.Code != http.StatusOK || !strings.Contains(body, `"worker_id":"worker-main"`) ||
		!strings.Contains(body, `"state":"ONLINE"`) || !strings.Contains(body, `"accessible_storage_ids":["scratch-main"]`) ||
		strings.Contains(body, "service-secret") || strings.Contains(body, "wincred://") {
		t.Fatalf("Worker status response=%d %s", statuses.Code, body)
	}
}

func runtimeTestConfiguration(t *testing.T) bootconfig.Configuration {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	payload := `schema_version: 1
database: {path: data/control.db}
server:
  listen_address: 127.0.0.1:0
  allowed_cidrs: [127.0.0.0/8]
  allowed_origins: [http://127.0.0.1:8080]
  trusted_proxies: []
security: {auth_mode: disabled, mcp_listen_address: 127.0.0.1:8081}
features: {}
provider_accounts: []
workers:
  heartbeat_ttl: 30s
  definitions:
    - id: worker-main
      enabled: true
      credential_ref: wincred://worker/main
      processing_kinds: [INSPECT, RENDER]
      allowed_storage_ids: [scratch-main]
storage:
  - id: scratch-main
    role: SCRATCH
    physical_device_id: drive-1
    partition_id: partition-1
    volume_id: volume-1
    root: storage/scratch
`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := bootconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return configuration
}

func runtimeSession(t *testing.T, handler http.Handler) (*http.Cookie, string) {
	t.Helper()
	response := runtimeRequest(handler, http.MethodGet, "/api/v1/session", "", nil, "", "")
	if response.Code != http.StatusOK || len(response.Result().Cookies()) != 1 {
		t.Fatalf("session returned %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Data struct {
			Token string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload.Data.Token == "" {
		t.Fatalf("invalid session response: %s err=%v", response.Body.String(), err)
	}
	return response.Result().Cookies()[0], payload.Data.Token
}

func runtimeRequest(handler http.Handler, method, path, body string, cookie *http.Cookie, token, origin string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.RemoteAddr = "127.0.0.1:45000"
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	if token != "" {
		request.Header.Set("X-CSRF-Token", token)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	handler.ServeHTTP(recorder, request)
	return recorder
}

func workerHeartbeatRequest(handler http.Handler, body, credential string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/heartbeat", bytes.NewBufferString(body))
	request.RemoteAddr = "127.0.0.1:45000"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential)
	handler.ServeHTTP(recorder, request)
	return recorder
}

func ledgerTargetColumn(table string) string {
	if table == "idempotency_receipts" {
		return "idempotency_key"
	}
	return "target_id"
}

func ledgerTargetValue(table string) string {
	if table == "idempotency_receipts" {
		return "JOB-HTTP-1:create"
	}
	return "JOB-HTTP-1"
}
