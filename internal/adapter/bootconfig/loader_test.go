package bootconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
    secret_ref: wincred://archive/providers/provider-primary
workers:
  heartbeat_ttl: 30s
  definitions:
    - id: worker-main
      enabled: true
      credential_ref: wincred://archive/workers/worker-main
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
	dpapiReference := `dpapi://C:/Archive/secrets/worker-main.dpapi`
	dpapiDocument := strings.Replace(valid, "wincred://archive/workers/worker-main", dpapiReference, 1)
	dpapiPath := filepath.Join(directory, "control-dpapi.yaml")
	if err := os.WriteFile(dpapiPath, []byte(dpapiDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	dpapiConfiguration, err := Load(dpapiPath)
	if err != nil || len(dpapiConfiguration.Workers) != 1 || dpapiConfiguration.Workers[0].CredentialRef != dpapiReference {
		t.Fatalf("valid Control DPAPI reference was rejected: workers=%+v err=%v", dpapiConfiguration.Workers, err)
	}

	invalidCases := []string{
		valid + "unknown_root_field: true\n",
		strings.Replace(valid, "wincred://archive/workers/worker-main", "dpapi://relative/worker.dpapi", 1),
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

func TestBootConfigurationValidatesOptionalArchiveOrganizationProbe(t *testing.T) {
	directory := t.TempDir()
	probePath := filepath.Join(directory, "ffprobe")
	if err := os.WriteFile(probePath, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	base := `schema_version: 1
database: {path: data/control.db}
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
      processing_kinds: [INSPECT, RENDER, QC]
      allowed_storage_ids: [source-main, working-main]
storage:
  - {id: source-main, role: SOURCE_MEDIA, physical_device_id: drive-1, partition_id: p1, volume_id: source-volume, root: source}
  - {id: working-main, role: WORKING, physical_device_id: drive-2, partition_id: p2, volume_id: working-volume, root: working}
`
	valid := base + `archive_organization:
  ffprobe:
    executable: ` + probePath + `
    version: ffprobe-fixture-v1
`
	path := filepath.Join(directory, "drive-control.yaml")
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := Load(path)
	if err != nil || configuration.ArchiveOrganization == nil || configuration.ArchiveOrganization.FFprobe.Executable != probePath ||
		configuration.ArchiveOrganization.FFprobe.Version != "ffprobe-fixture-v1" {
		t.Fatalf("valid Archive Organization probe was rejected: %+v err=%v", configuration.ArchiveOrganization, err)
	}
	absentPath := filepath.Join(directory, "drive-control-absent.yaml")
	if err := os.WriteFile(absentPath, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	absent, err := Load(absentPath)
	if err != nil || absent.ArchiveOrganization != nil {
		t.Fatalf("optional Archive Organization probe was not optional: %+v err=%v", absent.ArchiveOrganization, err)
	}
	for index, payload := range []string{
		strings.Replace(valid, probePath, "relative/ffprobe", 1),
		strings.Replace(valid, probePath, filepath.Join(directory, "missing"), 1),
		strings.Replace(valid, "version: ffprobe-fixture-v1", "version: ''", 1),
		strings.Replace(valid, "version: ffprobe-fixture-v1", "version: ffprobe-fixture-v1\n    unknown: true", 1),
	} {
		candidate := filepath.Join(directory, fmt.Sprintf("drive-control-invalid-%d.yaml", index))
		if err := os.WriteFile(candidate, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(candidate); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("invalid Archive Organization probe %d was accepted: %v", index, err)
		}
	}
}
