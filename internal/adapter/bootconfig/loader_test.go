package bootconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"videoarchive/internal/core/security"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: CFG-006
func TestBootConfigurationStrictlyLoadsRuntimeDatabaseNetworkWorkersAndStorage(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	valid := `schema_version: 1
database:
  path: data/control.db
server:
  listen_address: 127.0.0.1:8080
  allowed_cidrs: [127.0.0.0/8]
  allowed_origins: [http://127.0.0.1:8080]
  trusted_proxies: []
security:
  auth_mode: disabled
  mcp_listen_address: 127.0.0.1:8081
features:
  allow_device_format: false
  allow_original_delete: false
  allow_archive_replace: false
provider_accounts:
  - id: provider-primary
    provider: openai
    enabled: false
    secret_ref: wincred://video-archive/providers/provider-primary
workers:
  heartbeat_ttl: 30s
  definitions:
    - id: worker-main
      enabled: true
      credential_ref: wincred://video-archive/workers/worker-main
      processing_kinds: [INSPECT, RENDER, AUDIO_EXTRACT, STT, QC, CHECKSUM]
      allowed_storage_ids: [scratch-main]
storage:
  - id: scratch-main
    role: SCRATCH
    physical_device_id: internal-nvme-01
    partition_id: internal-nvme-01-p1
    volume_id: scratch-volume
    root: storage/scratch
`
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	wantDatabase := filepath.Join(directory, "data", "control.db")
	if configuration.SchemaVersion != 1 || configuration.DatabasePath != wantDatabase ||
		configuration.Settings.AuthMode != security.AuthDisabled || configuration.Settings.ListenAddress != "127.0.0.1:8080" ||
		len(configuration.Access.AllowedNetworks) != 1 || !configuration.Access.AllowedNetworks[0].Contains(configuration.Access.AllowedNetworks[0].Addr()) ||
		len(configuration.AllowedOrigins) != 1 || configuration.AllowedOrigins[0] != "http://127.0.0.1:8080" ||
		configuration.HeartbeatTTL != 30*time.Second || len(configuration.Workers) != 1 ||
		!configuration.Workers[0].ProcessingKinds[vocabulary.ProcessingSTT] ||
		len(configuration.Storages) != 1 || configuration.Storages[0].PhysicalDeviceID != "internal-nvme-01" ||
		configuration.Storages[0].PartitionID != "internal-nvme-01-p1" || configuration.Storages[0].VolumeID != "scratch-volume" ||
		configuration.Storages[0].Role != vocabulary.StorageScratch {
		t.Fatalf("runtime configuration was not fully normalized: %+v", configuration)
	}

	invalidCases := []string{
		valid + "unknown_root_field: true\n",
		`schema_version: 1
database: {path: control.db}
server:
  listen_address: 127.0.0.1:8080
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
      processing_kinds: [STT]
      allowed_storage_ids: [missing]
storage:
  - {id: scratch-main, role: SCRATCH, physical_device_id: drive-1, partition_id: p1, volume_id: v1, root: scratch}
`,
	}
	for index, payload := range invalidCases {
		invalidPath := filepath.Join(directory, "invalid-"+string(rune('a'+index))+".yaml")
		if err := os.WriteFile(invalidPath, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(invalidPath); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("invalid boot configuration %d was accepted: %v", index, err)
		}
	}
}
