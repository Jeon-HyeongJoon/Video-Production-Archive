package security

import (
	"crypto/subtle"
	"errors"
	"mime"
	"net/netip"
	"strings"

	"videoarchive/internal/core/vocabulary"
)

type AuthMode string

const (
	AuthDisabled AuthMode = "disabled"
	AuthLocal    AuthMode = "local"
)

var (
	ErrNetworkDenied          = errors.New("source network is not allowed")
	ErrAuthenticationRequired = errors.New("authentication is required")
	ErrInvalidAccessConfig    = errors.New("invalid access configuration")
	ErrModeChangeDenied       = errors.New("authentication mode can only change from local configuration during restart")
	ErrBrowserMutationDenied  = errors.New("browser mutation request was denied")
)

type AccessConfig struct {
	Mode            AuthMode
	AllowedNetworks []netip.Prefix
}

type AccessRequest struct {
	SourceIP      netip.Addr
	Authenticated bool
	UserID        string
	Role          UserRole
}

type Principal struct {
	ID            string
	ActorType     vocabulary.ActorType
	Role          UserRole
	Authenticated bool
	SourceIP      netip.Addr
}

func ResolvePrincipal(config AccessConfig, request AccessRequest) (Principal, error) {
	if config.Mode != AuthDisabled && config.Mode != AuthLocal {
		return Principal{}, ErrInvalidAccessConfig
	}
	if !request.SourceIP.IsValid() || !addressAllowed(request.SourceIP, config.AllowedNetworks) {
		return Principal{}, ErrNetworkDenied
	}

	if config.Mode == AuthDisabled {
		return Principal{
			ID: "internal-operator", ActorType: vocabulary.ActorHuman,
			Role: Operator, Authenticated: false, SourceIP: request.SourceIP,
		}, nil
	}

	if !request.Authenticated || strings.TrimSpace(request.UserID) == "" {
		return Principal{}, ErrAuthenticationRequired
	}
	return Principal{ID: request.UserID, ActorType: vocabulary.ActorHuman, Role: request.Role, Authenticated: true, SourceIP: request.SourceIP}, nil
}

func addressAllowed(address netip.Addr, networks []netip.Prefix) bool {
	address = address.Unmap()
	for _, network := range networks {
		if !network.IsValid() {
			continue
		}
		networkAddress := network.Addr()
		if networkAddress.Is4In6() {
			network = netip.PrefixFrom(networkAddress.Unmap(), network.Bits()-96)
		}
		if network.Contains(address) {
			return true
		}
	}
	return false
}

func EffectiveSourceIP(peer netip.Addr, forwardedFor []netip.Addr, trustedProxies []netip.Prefix) netip.Addr {
	if !addressAllowed(peer, trustedProxies) || len(forwardedFor) != 1 || !forwardedFor[0].IsValid() {
		return peer
	}
	return forwardedFor[0]
}

type ModeChangeSource string

const (
	ModeChangeArchiveWorkspace ModeChangeSource = "archive_workspace"
	ModeChangeAPI              ModeChangeSource = "api"
	ModeChangeLocalConfig      ModeChangeSource = "local_config"
)

func ValidateAuthModeChange(source ModeChangeSource, serviceRestart bool) error {
	if source != ModeChangeLocalConfig || !serviceRestart {
		return ErrModeChangeDenied
	}
	return nil
}

func MCPListenAllowed(mode AuthMode, listenAddress netip.Addr) bool {
	if !listenAddress.IsValid() {
		return false
	}
	if mode == AuthDisabled {
		return listenAddress.IsLoopback()
	}
	return mode == AuthLocal
}

type BrowserMutation struct {
	Method         string
	Origin         string
	AllowedOrigins []string
	ContentType    string
	CSRFHeader     string
	CSRFCookie     string
}

func ValidateBrowserMutation(request BrowserMutation) error {
	method := strings.ToUpper(request.Method)
	if method == "GET" || method == "HEAD" || method == "OPTIONS" {
		return nil
	}
	allowedOrigin := false
	for _, origin := range request.AllowedOrigins {
		if request.Origin == origin {
			allowedOrigin = true
			break
		}
	}
	mediaType, _, mediaErr := mime.ParseMediaType(request.ContentType)
	validCSRF := request.CSRFHeader != "" && request.CSRFCookie != "" &&
		len(request.CSRFHeader) == len(request.CSRFCookie) &&
		subtle.ConstantTimeCompare([]byte(request.CSRFHeader), []byte(request.CSRFCookie)) == 1
	if !allowedOrigin || mediaErr != nil || mediaType != "application/json" || !validCSRF {
		return ErrBrowserMutationDenied
	}
	return nil
}
