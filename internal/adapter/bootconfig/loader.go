package bootconfig

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"videoarchive/internal/core/config"
	"videoarchive/internal/core/security"
	"videoarchive/internal/core/storage"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
)

var ErrInvalidConfiguration = errors.New("boot configuration is invalid")

type Configuration struct {
	SchemaVersion  uint32
	DatabasePath   string
	Settings       config.Settings
	Access         security.AccessConfig
	AllowedOrigins []string
	TrustedProxies []netip.Prefix
	HeartbeatTTL   time.Duration
	Workers        []worker.Definition
	Storages       []storage.Topology
}

type document struct {
	SchemaVersion uint32 `yaml:"schema_version"`
	Database      struct {
		Path string `yaml:"path"`
	} `yaml:"database"`
	Server struct {
		ListenAddress  string   `yaml:"listen_address"`
		AllowedCIDRs   []string `yaml:"allowed_cidrs"`
		AllowedOrigins []string `yaml:"allowed_origins"`
		TrustedProxies []string `yaml:"trusted_proxies"`
	} `yaml:"server"`
	Security struct {
		AuthMode         security.AuthMode `yaml:"auth_mode"`
		MCPListenAddress string            `yaml:"mcp_listen_address"`
	} `yaml:"security"`
	Features struct {
		AllowDeviceFormat   bool `yaml:"allow_device_format"`
		AllowOriginalDelete bool `yaml:"allow_original_delete"`
		AllowArchiveReplace bool `yaml:"allow_archive_replace"`
	} `yaml:"features"`
	ProviderAccounts []struct {
		ID        string `yaml:"id"`
		Provider  string `yaml:"provider"`
		Enabled   bool   `yaml:"enabled"`
		SecretRef string `yaml:"secret_ref"`
	} `yaml:"provider_accounts"`
	Workers struct {
		HeartbeatTTL string `yaml:"heartbeat_ttl"`
		Definitions  []struct {
			ID                string   `yaml:"id"`
			Enabled           bool     `yaml:"enabled"`
			CredentialRef     string   `yaml:"credential_ref"`
			ProcessingKinds   []string `yaml:"processing_kinds"`
			AllowedStorageIDs []string `yaml:"allowed_storage_ids"`
		} `yaml:"definitions"`
	} `yaml:"workers"`
	Storage []struct {
		ID               string `yaml:"id"`
		Role             string `yaml:"role"`
		PhysicalDeviceID string `yaml:"physical_device_id"`
		PartitionID      string `yaml:"partition_id"`
		VolumeID         string `yaml:"volume_id"`
		Root             string `yaml:"root"`
	} `yaml:"storage"`
}

func Load(path string) (Configuration, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Configuration{}, ErrInvalidConfiguration
	}
	file, err := os.Open(path)
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
	return normalize(path, source)
}

func normalize(configurationPath string, source document) (Configuration, error) {
	if source.SchemaVersion != 1 || strings.TrimSpace(source.Database.Path) == "" {
		return Configuration{}, ErrInvalidConfiguration
	}
	databasePath := strings.TrimSpace(source.Database.Path)
	if !filepath.IsAbs(databasePath) {
		databasePath = filepath.Join(filepath.Dir(configurationPath), databasePath)
	}
	databasePath, err := filepath.Abs(databasePath)
	if err != nil {
		return Configuration{}, ErrInvalidConfiguration
	}

	settings := config.Settings{
		AuthMode: source.Security.AuthMode, ListenAddress: strings.TrimSpace(source.Server.ListenAddress),
		MCPListenAddress:  strings.TrimSpace(source.Security.MCPListenAddress),
		AllowDeviceFormat: source.Features.AllowDeviceFormat, AllowOriginalDelete: source.Features.AllowOriginalDelete,
		AllowArchiveReplace: source.Features.AllowArchiveReplace,
	}
	for _, provider := range source.ProviderAccounts {
		settings.Providers = append(settings.Providers, config.ProviderDefinition{ID: strings.TrimSpace(provider.ID), SecretRef: strings.TrimSpace(provider.SecretRef)})
	}

	allowedNetworks, err := parsePrefixes(source.Server.AllowedCIDRs, true)
	if err != nil {
		return Configuration{}, err
	}
	trustedProxies, err := parsePrefixes(source.Server.TrustedProxies, false)
	if err != nil {
		return Configuration{}, err
	}
	allowedOrigins := make([]string, 0, len(source.Server.AllowedOrigins))
	seenOrigins := make(map[string]bool, len(source.Server.AllowedOrigins))
	for _, raw := range source.Server.AllowedOrigins {
		origin := strings.TrimSpace(raw)
		parsed, parseErr := url.Parse(origin)
		if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
			parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" || seenOrigins[origin] {
			return Configuration{}, ErrInvalidConfiguration
		}
		seenOrigins[origin] = true
		allowedOrigins = append(allowedOrigins, origin)
	}
	if len(allowedOrigins) == 0 {
		return Configuration{}, ErrInvalidConfiguration
	}

	heartbeatTTL, err := time.ParseDuration(strings.TrimSpace(source.Workers.HeartbeatTTL))
	if err != nil || heartbeatTTL <= 0 {
		return Configuration{}, ErrInvalidConfiguration
	}
	storageIDs := make(map[string]bool, len(source.Storage))
	storages := make([]storage.Topology, 0, len(source.Storage))
	for _, definition := range source.Storage {
		topology := storage.Topology{
			PhysicalDeviceID: strings.TrimSpace(definition.PhysicalDeviceID), PartitionID: strings.TrimSpace(definition.PartitionID),
			VolumeID: strings.TrimSpace(definition.VolumeID), StorageID: strings.TrimSpace(definition.ID),
			Role: vocabulary.StorageRole(strings.TrimSpace(definition.Role)), Root: strings.TrimSpace(definition.Root),
		}
		if topology.Validate() != nil || storageIDs[topology.StorageID] {
			return Configuration{}, ErrInvalidConfiguration
		}
		storageIDs[topology.StorageID] = true
		storages = append(storages, topology)
	}
	if len(storages) == 0 {
		return Configuration{}, ErrInvalidConfiguration
	}

	workers := make([]worker.Definition, 0, len(source.Workers.Definitions))
	for _, definition := range source.Workers.Definitions {
		id := strings.TrimSpace(definition.ID)
		credentialRef := strings.TrimSpace(definition.CredentialRef)
		if !strings.HasPrefix(credentialRef, "wincred://") {
			return Configuration{}, ErrInvalidConfiguration
		}
		settings.Workers = append(settings.Workers, config.WorkerDefinition{ID: id, CredentialRef: credentialRef})
		processingKinds := make(map[vocabulary.ProcessingKind]bool, len(definition.ProcessingKinds))
		for _, raw := range definition.ProcessingKinds {
			kind := vocabulary.ProcessingKind(strings.TrimSpace(raw))
			if !kind.Valid() || processingKinds[kind] {
				return Configuration{}, ErrInvalidConfiguration
			}
			processingKinds[kind] = true
		}
		allowedStorageIDs := make(map[string]bool, len(definition.AllowedStorageIDs))
		for _, raw := range definition.AllowedStorageIDs {
			storageID := strings.TrimSpace(raw)
			if !storageIDs[storageID] || allowedStorageIDs[storageID] {
				return Configuration{}, ErrInvalidConfiguration
			}
			allowedStorageIDs[storageID] = true
		}
		if id == "" || len(processingKinds) == 0 || len(allowedStorageIDs) == 0 {
			return Configuration{}, ErrInvalidConfiguration
		}
		workers = append(workers, worker.Definition{
			ID: id, Enabled: definition.Enabled, CredentialRef: credentialRef,
			ProcessingKinds: processingKinds, AllowedStorageIDs: allowedStorageIDs,
		})
	}
	if len(workers) == 0 || settings.Validate() != nil {
		return Configuration{}, ErrInvalidConfiguration
	}
	return Configuration{
		SchemaVersion: source.SchemaVersion, DatabasePath: databasePath, Settings: settings,
		Access:         security.AccessConfig{Mode: settings.AuthMode, AllowedNetworks: allowedNetworks},
		AllowedOrigins: allowedOrigins, TrustedProxies: trustedProxies,
		HeartbeatTTL: heartbeatTTL, Workers: workers, Storages: storages,
	}, nil
}

func parsePrefixes(values []string, required bool) ([]netip.Prefix, error) {
	result := make([]netip.Prefix, 0, len(values))
	seen := make(map[netip.Prefix]bool, len(values))
	for _, raw := range values {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			return nil, ErrInvalidConfiguration
		}
		prefix = prefix.Masked()
		if seen[prefix] {
			return nil, ErrInvalidConfiguration
		}
		seen[prefix] = true
		result = append(result, prefix)
	}
	if required && len(result) == 0 {
		return nil, ErrInvalidConfiguration
	}
	return result, nil
}
