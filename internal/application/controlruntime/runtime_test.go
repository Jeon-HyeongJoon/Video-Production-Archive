package controlruntime_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
