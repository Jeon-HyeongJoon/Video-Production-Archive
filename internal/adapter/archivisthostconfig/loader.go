package archivisthostconfig

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/config"
)

var ErrInvalidConfiguration = errors.New("archivist host configuration is invalid")

type Configuration struct {
	SchemaVersion       uint32
	ID                  string
	ControlURL          *url.URL
	MCPControlURL       *url.URL
	CredentialReference string
	HeartbeatInterval   time.Duration
	ClaimInterval       time.Duration
	MCPListenAddress    string
	LaunchPolicy        archivistruntime.LaunchPolicy
}

type document struct {
	SchemaVersion uint32 `yaml:"schema_version"`
	ArchivistHost struct {
		ID                  string `yaml:"id"`
		ControlURL          string `yaml:"control_url"`
		MCPControlURL       string `yaml:"mcp_control_url"`
		CredentialReference string `yaml:"credential_ref"`
		HeartbeatInterval   string `yaml:"heartbeat_interval"`
		ClaimInterval       string `yaml:"claim_interval"`
		MCPListenAddress    string `yaml:"mcp_listen_address"`
	} `yaml:"archivist_host"`
	CLIs []struct {
		ID             string   `yaml:"id"`
		Executable     string   `yaml:"executable"`
		FixedArguments []string `yaml:"fixed_arguments"`
	} `yaml:"clis"`
	WorkspaceRoots []struct {
		ID   string `yaml:"id"`
		Path string `yaml:"path"`
	} `yaml:"workspace_roots"`
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
	hostID := strings.TrimSpace(source.ArchivistHost.ID)
	credentialReference := strings.TrimSpace(source.ArchivistHost.CredentialReference)
	controlURL, controlErr := parseControlURL(source.ArchivistHost.ControlURL)
	mcpControlURL, mcpErr := parseControlURL(source.ArchivistHost.MCPControlURL)
	listen, listenErr := netip.ParseAddrPort(strings.TrimSpace(source.ArchivistHost.MCPListenAddress))
	heartbeatInterval, heartbeatErr := time.ParseDuration(strings.TrimSpace(source.ArchivistHost.HeartbeatInterval))
	claimInterval, claimErr := time.ParseDuration(strings.TrimSpace(source.ArchivistHost.ClaimInterval))
	if source.SchemaVersion != 1 || hostID == "" || !config.ValidSecretReference(credentialReference) ||
		controlErr != nil || mcpErr != nil || listenErr != nil ||
		!listen.Addr().IsLoopback() || heartbeatErr != nil || heartbeatInterval < time.Second ||
		claimErr != nil || claimInterval < 250*time.Millisecond {
		return Configuration{}, ErrInvalidConfiguration
	}
	policy := archivistruntime.LaunchPolicy{
		CLIs:           make(map[string]archivistruntime.CLIExecutable, len(source.CLIs)),
		WorkspaceRoots: make(map[string]string, len(source.WorkspaceRoots)),
	}
	for _, definition := range source.CLIs {
		id := strings.TrimSpace(definition.ID)
		executable := strings.TrimSpace(definition.Executable)
		if id == "" || !absolutePath(executable) || policy.CLIs[id].Executable != "" {
			return Configuration{}, ErrInvalidConfiguration
		}
		arguments := make([]string, len(definition.FixedArguments))
		for index, argument := range definition.FixedArguments {
			if strings.ContainsRune(argument, '\x00') {
				return Configuration{}, ErrInvalidConfiguration
			}
			arguments[index] = argument
		}
		policy.CLIs[id] = archivistruntime.CLIExecutable{Executable: executable, FixedArguments: arguments}
	}
	for _, definition := range source.WorkspaceRoots {
		id := strings.TrimSpace(definition.ID)
		path := strings.TrimSpace(definition.Path)
		if id == "" || !absolutePath(path) || policy.WorkspaceRoots[id] != "" {
			return Configuration{}, ErrInvalidConfiguration
		}
		policy.WorkspaceRoots[id] = path
	}
	if len(policy.CLIs) == 0 || len(policy.WorkspaceRoots) == 0 {
		return Configuration{}, ErrInvalidConfiguration
	}
	return Configuration{
		SchemaVersion: source.SchemaVersion, ID: hostID, ControlURL: controlURL, MCPControlURL: mcpControlURL,
		CredentialReference: credentialReference, HeartbeatInterval: heartbeatInterval, ClaimInterval: claimInterval,
		MCPListenAddress: listen.String(), LaunchPolicy: policy,
	}, nil
}

func parseControlURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, ErrInvalidConfiguration
	}
	host := parsed.Hostname()
	address := net.ParseIP(host)
	if parsed.Scheme != "https" && (address == nil || !address.IsLoopback()) && !strings.EqualFold(host, "localhost") {
		return nil, ErrInvalidConfiguration
	}
	parsed.Path = ""
	return parsed, nil
}

func absolutePath(path string) bool {
	if strings.HasPrefix(path, "/") {
		return true
	}
	return len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) &&
		path[1] == ':' && (path[2] == '\\' || path[2] == '/')
}
