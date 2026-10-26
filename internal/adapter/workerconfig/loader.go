package workerconfig

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"videoarchive/internal/core/config"
)

var ErrInvalidConfiguration = errors.New("worker configuration is invalid")

type Configuration struct {
	SchemaVersion        uint32
	ID                   string
	ControlURL           *url.URL
	CredentialReference  string
	HeartbeatInterval    time.Duration
	AccessibleStorageIDs []string
}

type document struct {
	SchemaVersion uint32 `yaml:"schema_version"`
	Worker        struct {
		ID                   string   `yaml:"id"`
		ControlURL           string   `yaml:"control_url"`
		CredentialReference  string   `yaml:"credential_ref"`
		HeartbeatInterval    string   `yaml:"heartbeat_interval"`
		AccessibleStorageIDs []string `yaml:"accessible_storage_ids"`
	} `yaml:"worker"`
}

func Load(path string) (Configuration, error) {
	file, err := os.Open(strings.TrimSpace(path))
	if err != nil {
		return Configuration{}, fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var source document
	if err := decoder.Decode(&source); err != nil {
		return Configuration{}, fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Configuration{}, fmt.Errorf("%w: configuration must contain one YAML document", ErrInvalidConfiguration)
	}
	return normalize(source)
}

func normalize(source document) (Configuration, error) {
	workerID := strings.TrimSpace(source.Worker.ID)
	credentialReference := strings.TrimSpace(source.Worker.CredentialReference)
	controlURL, err := url.Parse(strings.TrimSpace(source.Worker.ControlURL))
	if source.SchemaVersion != 1 || workerID == "" || !config.ValidSecretReference(credentialReference) ||
		!strings.HasPrefix(credentialReference, "wincred://") || err != nil ||
		controlURL.User != nil || controlURL.RawQuery != "" || controlURL.Fragment != "" ||
		(controlURL.Scheme != "http" && controlURL.Scheme != "https") || controlURL.Host == "" ||
		(controlURL.Path != "" && controlURL.Path != "/") {
		return Configuration{}, ErrInvalidConfiguration
	}
	host := controlURL.Hostname()
	address := net.ParseIP(host)
	if controlURL.Scheme != "https" && (address == nil || !address.IsLoopback()) && host != "localhost" {
		return Configuration{}, ErrInvalidConfiguration
	}
	controlURL.Path = ""
	interval, err := time.ParseDuration(strings.TrimSpace(source.Worker.HeartbeatInterval))
	if err != nil || interval < time.Second {
		return Configuration{}, ErrInvalidConfiguration
	}
	seen := make(map[string]bool, len(source.Worker.AccessibleStorageIDs))
	storageIDs := make([]string, 0, len(source.Worker.AccessibleStorageIDs))
	for _, raw := range source.Worker.AccessibleStorageIDs {
		storageID := strings.TrimSpace(raw)
		if storageID == "" || seen[storageID] {
			return Configuration{}, ErrInvalidConfiguration
		}
		seen[storageID] = true
		storageIDs = append(storageIDs, storageID)
	}
	if len(storageIDs) == 0 {
		return Configuration{}, ErrInvalidConfiguration
	}
	return Configuration{
		SchemaVersion: source.SchemaVersion, ID: workerID, ControlURL: controlURL,
		CredentialReference: credentialReference, HeartbeatInterval: interval,
		AccessibleStorageIDs: storageIDs,
	}, nil
}
