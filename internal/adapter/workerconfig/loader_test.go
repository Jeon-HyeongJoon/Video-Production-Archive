package workerconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Requirement: CFG-007
func TestWorkerConfigurationRequiresStrictSecureOutboundSettings(t *testing.T) {
	directory := t.TempDir()
	valid := `schema_version: 1
worker:
  id: worker-main
  control_url: https://control.internal:8443
  credential_ref: wincred://video-archive/workers/worker-main
  heartbeat_interval: 10s
  accessible_storage_ids: [source-main, scratch-main]
`
	path := filepath.Join(directory, "worker.yaml")
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.ID != "worker-main" || configuration.ControlURL.String() != "https://control.internal:8443" ||
		configuration.CredentialReference != "wincred://video-archive/workers/worker-main" ||
		configuration.HeartbeatInterval != 10*time.Second || len(configuration.AccessibleStorageIDs) != 2 {
		t.Fatalf("worker configuration was not normalized: %+v", configuration)
	}
	for index, payload := range []string{
		valid + "unknown: true\n",
		stringsReplace(valid, "https://control.internal:8443", "http://192.168.40.8:8080"),
		stringsReplace(valid, "wincred://video-archive/workers/worker-main", "plaintext-secret"),
		stringsReplace(valid, "wincred://video-archive/workers/worker-main", "dpapi://video-archive/workers/worker-main"),
	} {
		candidate := filepath.Join(directory, fmtName(index)+".yaml")
		if err := os.WriteFile(candidate, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(candidate); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("invalid worker configuration %d was accepted: %v", index, err)
		}
	}
}

func stringsReplace(value, old, replacement string) string {
	return strings.Replace(value, old, replacement, 1)
}

func fmtName(index int) string {
	return fmt.Sprintf("invalid-%d", index)
}
