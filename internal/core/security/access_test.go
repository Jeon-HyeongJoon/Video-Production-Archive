package security

import (
	"errors"
	"net/netip"
	"testing"

	"videoarchive/internal/core/vocabulary"
)

func officeAccess(mode AuthMode) AccessConfig {
	return AccessConfig{Mode: mode, AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("192.168.40.0/24")}}
}

// Requirement: NET-001
func TestAccessRejectsSourceOutsideExplicitInternalCIDRs(t *testing.T) {
	config := officeAccess(AuthDisabled)
	_, err := ResolvePrincipal(config, AccessRequest{SourceIP: netip.MustParseAddr("203.0.113.10")})
	if !errors.Is(err, ErrNetworkDenied) {
		t.Fatalf("external source was accepted: %v", err)
	}
	if _, err := ResolvePrincipal(config, AccessRequest{SourceIP: netip.MustParseAddr("192.168.40.12")}); err != nil {
		t.Fatalf("allowed office source rejected: %v", err)
	}
	if _, err := ResolvePrincipal(config, AccessRequest{SourceIP: netip.MustParseAddr("::ffff:192.168.40.12")}); err != nil {
		t.Fatalf("IPv4-mapped office source rejected: %v", err)
	}
}

// Requirement: NET-002
func TestDisabledAuthUsesSingleInternalOperatorWithoutUserOwnership(t *testing.T) {
	source := netip.MustParseAddr("192.168.40.12")
	principal, err := ResolvePrincipal(officeAccess(AuthDisabled), AccessRequest{SourceIP: source})
	if err != nil {
		t.Fatal(err)
	}
	if principal.Authenticated || principal.ActorType != vocabulary.ActorHuman || principal.Role != Operator || principal.ID != "internal-operator" || principal.SourceIP != source {
		t.Fatalf("unexpected internal operator principal: %+v", principal)
	}
}

// Requirement: NET-003
func TestLocalAuthModeRejectsMissingSessionAndPreservesAuthenticatedIdentity(t *testing.T) {
	config := officeAccess(AuthLocal)
	request := AccessRequest{SourceIP: netip.MustParseAddr("192.168.40.12")}
	if _, err := ResolvePrincipal(config, request); !errors.Is(err, ErrAuthenticationRequired) {
		t.Fatalf("missing login accepted: %v", err)
	}
	request.Authenticated = true
	request.UserID = "operator-1"
	request.Role = Operator
	principal, err := ResolvePrincipal(config, request)
	if err != nil || principal.ID != "operator-1" || !principal.Authenticated {
		t.Fatalf("authenticated identity not preserved: principal=%+v err=%v", principal, err)
	}
}

// Requirement: NET-004
func TestAuthModeCanChangeOnlyFromLocalConfigWithServiceRestart(t *testing.T) {
	for _, attempt := range []struct {
		source  ModeChangeSource
		restart bool
	}{{ModeChangeDashboard, true}, {ModeChangeAPI, true}, {ModeChangeLocalConfig, false}} {
		if err := ValidateAuthModeChange(attempt.source, attempt.restart); !errors.Is(err, ErrModeChangeDenied) {
			t.Fatalf("remote or live mode change accepted: %+v err=%v", attempt, err)
		}
	}
	if err := ValidateAuthModeChange(ModeChangeLocalConfig, true); err != nil {
		t.Fatalf("local restart change rejected: %v", err)
	}
}

// Requirement: NET-005
func TestUnauthenticatedMCPMayListenOnlyOnLoopback(t *testing.T) {
	if !MCPListenAllowed(AuthDisabled, netip.MustParseAddr("127.0.0.1")) {
		t.Fatal("loopback MCP was rejected")
	}
	for _, address := range []string{"0.0.0.0", "192.168.40.8", "::"} {
		if MCPListenAllowed(AuthDisabled, netip.MustParseAddr(address)) {
			t.Fatalf("unauthenticated MCP exposed on %s", address)
		}
	}
}

// Requirement: NET-006
func TestBrowserMutationRequiresAllowedOriginJSONAndMatchingCSRFToken(t *testing.T) {
	valid := BrowserMutation{
		Method: "POST", Origin: "http://192.168.40.8:8080",
		AllowedOrigins: []string{"http://192.168.40.8:8080"}, ContentType: "application/json; charset=utf-8",
		CSRFHeader: "random-session-token", CSRFCookie: "random-session-token",
	}
	if err := ValidateBrowserMutation(valid); err != nil {
		t.Fatalf("valid mutation rejected: %v", err)
	}
	cases := []func(*BrowserMutation){
		func(r *BrowserMutation) { r.Origin = "https://malicious.example" },
		func(r *BrowserMutation) { r.ContentType = "text/plain" },
		func(r *BrowserMutation) { r.CSRFHeader = "" },
		func(r *BrowserMutation) { r.CSRFHeader = "different" },
	}
	for index, mutate := range cases {
		request := valid
		mutate(&request)
		if err := ValidateBrowserMutation(request); !errors.Is(err, ErrBrowserMutationDenied) {
			t.Fatalf("case %d accepted: %+v err=%v", index, request, err)
		}
	}
}

// Requirement: NET-007
func TestForwardedSourceIsIgnoredUnlessDirectPeerIsTrustedProxy(t *testing.T) {
	peer := netip.MustParseAddr("203.0.113.10")
	claimedInternal := []netip.Addr{netip.MustParseAddr("192.168.40.12")}
	trusted := []netip.Prefix{netip.MustParsePrefix("192.168.40.2/32")}
	if source := EffectiveSourceIP(peer, claimedInternal, trusted); source != peer {
		t.Fatalf("untrusted peer spoofed source: %s", source)
	}
	if source := EffectiveSourceIP(netip.MustParseAddr("192.168.40.2"), []netip.Addr{peer, claimedInternal[0]}, trusted); source != netip.MustParseAddr("192.168.40.2") {
		t.Fatalf("ambiguous forwarded chain was trusted: %s", source)
	}
	proxy := netip.MustParseAddr("192.168.40.2")
	if source := EffectiveSourceIP(proxy, claimedInternal, trusted); source != claimedInternal[0] {
		t.Fatalf("trusted proxy source was not used: %s", source)
	}
}
