package config

import (
	"errors"
	"testing"

	"videoarchive/internal/core/security"
)

// Requirement: CFG-001
func TestV1OmitsRemoteManagementSurfaceForEveryAuthMode(t *testing.T) {
	for _, mode := range []security.AuthMode{security.AuthDisabled, security.AuthLocal} {
		if routes := (Settings{AuthMode: mode}).ManagementRoutes(); len(routes) != 0 {
			t.Fatalf("remote management was enabled for %s: %v", mode, routes)
		}
	}
}

// Requirement: CFG-002
func TestManagementConfigurationAppliesOnlyFromLocalFileAtProcessStart(t *testing.T) {
	for _, attempt := range []struct {
		source   ChangeSource
		starting bool
	}{{ChangeArchiveWorkspace, true}, {ChangeAPI, true}, {ChangeLocalFile, false}} {
		if err := ValidateBootApplication(attempt.source, attempt.starting); !errors.Is(err, ErrRestartRequired) {
			t.Fatalf("runtime or remote change accepted: %+v err=%v", attempt, err)
		}
	}
	if err := ValidateBootApplication(ChangeLocalFile, true); err != nil {
		t.Fatalf("startup local config rejected: %v", err)
	}
}

// Requirement: CFG-003
func TestBootConfigAcceptsOnlyUniqueOSSecretReferences(t *testing.T) {
	valid := Settings{
		AuthMode: security.AuthDisabled, ListenAddress: "192.168.40.8:8080", MCPListenAddress: "127.0.0.1:8081",
		Providers: []ProviderDefinition{{ID: "provider-primary", SecretRef: "wincred://archive/provider-primary"}},
		Workers:   []WorkerDefinition{{ID: "worker-main", CredentialRef: "dpapi://C:/Archive/secrets/worker-main.dpapi"}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	rawSecret := valid
	rawSecret.Providers = []ProviderDefinition{{ID: "provider-primary", SecretRef: "sk-plaintext-secret"}}
	if err := rawSecret.Validate(); !errors.Is(err, ErrInvalidSecretReference) {
		t.Fatalf("raw credential accepted: %v", err)
	}
	relativeDPAPI := valid
	relativeDPAPI.Workers = []WorkerDefinition{{ID: "worker-main", CredentialRef: "dpapi://archive/worker-main.dpapi"}}
	if err := relativeDPAPI.Validate(); !errors.Is(err, ErrInvalidSecretReference) {
		t.Fatalf("relative DPAPI reference accepted: %v", err)
	}
	duplicate := valid
	duplicate.Workers = append(duplicate.Workers, duplicate.Workers[0])
	if err := duplicate.Validate(); !errors.Is(err, ErrDuplicateDefinition) {
		t.Fatalf("duplicate worker accepted: %v", err)
	}
}

// Requirement: CFG-004
func TestBootConfigRejectsWildcardListenAndRemoteMCPWithoutAuthentication(t *testing.T) {
	valid := Settings{AuthMode: security.AuthDisabled, ListenAddress: "192.168.40.8:8080", MCPListenAddress: "127.0.0.1:8081"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("safe listen config rejected: %v", err)
	}
	for _, edit := range []func(*Settings){
		func(s *Settings) { s.ListenAddress = "0.0.0.0:8080" },
		func(s *Settings) { s.ListenAddress = "[::]:8080" },
		func(s *Settings) { s.MCPListenAddress = "192.168.40.8:8081" },
	} {
		candidate := valid
		edit(&candidate)
		if err := candidate.Validate(); !errors.Is(err, ErrUnsafeListenAddress) {
			t.Fatalf("unsafe listen config accepted: %+v err=%v", candidate, err)
		}
	}
}

// Requirement: CFG-005
func TestDestructiveFeatureFlagsAreIndependent(t *testing.T) {
	settings := Settings{AllowDeviceFormat: true}
	if !settings.AllowsDestructive(DeviceFormat) || settings.AllowsDestructive(OriginalDelete) || settings.AllowsDestructive(ArchiveReplace) {
		t.Fatalf("destructive feature flags were coupled: %+v", settings)
	}
}
