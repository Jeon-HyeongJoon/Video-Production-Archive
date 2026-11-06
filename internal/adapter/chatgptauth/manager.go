package chatgptauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"videoarchive/internal/adapter/ossecret"
)

const (
	defaultIssuer   = "https://auth.openai.com"
	defaultResource = "https://api.openai.com/v1"
	requiredScopes  = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	profileVersion  = 1
)

var (
	ErrAuthenticationFailed = errors.New("ChatGPT authentication failed")
	ErrReconnectRequired    = errors.New("ChatGPT reconnection is required")
	ErrInvalidProfile       = errors.New("ChatGPT profile is invalid")
	ErrInvalidGrant         = errors.New("ChatGPT authorization grant is invalid")
)

type Profile struct {
	Version      int       `json:"version"`
	Email        string    `json:"email,omitempty"`
	Issuer       string    `json:"issuer"`
	Subject      string    `json:"subject"`
	ClientID     string    `json:"client_id"`
	HostID       string    `json:"ext_agent_host_id"`
	IDToken      string    `json:"id_token"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	ExpiresIn    int64     `json:"expires_in"`
	Scopes       []string  `json:"scopes"`
	SavedAt      time.Time `json:"saved_at"`
}

func (profile Profile) Valid() bool {
	return profile.Version == profileVersion && profile.Issuer != "" && profile.Subject != "" &&
		profile.ClientID != "" && profile.HostID != "" && profile.IDToken != "" &&
		profile.AccessToken != "" && profile.RefreshToken != "" && strings.EqualFold(profile.TokenType, "Bearer") && profile.ExpiresIn > 0 &&
		hasPlanScopes(profile.Scopes)
}

type Store interface {
	Load(reference string) (Profile, error)
	Save(reference string, profile Profile) error
	Delete(reference string) error
}

type OSStore struct{ resolver *ossecret.Resolver }

func NewOSStore() *OSStore { return &OSStore{resolver: ossecret.New()} }

func (store *OSStore) Load(reference string) (Profile, error) {
	encoded, err := store.resolver.Resolve(reference)
	if err != nil {
		return Profile{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var profile Profile
	if err := decoder.Decode(&profile); err != nil || !profile.Valid() {
		return Profile{}, ErrInvalidProfile
	}
	return profile, nil
}

func (*OSStore) Save(reference string, profile Profile) error {
	if !profile.Valid() {
		return ErrInvalidProfile
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		return ErrInvalidProfile
	}
	defer clear(encoded)
	return ossecret.ProvisionKeychain(reference, string(encoded))
}

func (*OSStore) Delete(reference string) error { return ossecret.DeleteKeychain(reference) }

type Configuration struct {
	Store       Store
	HTTPClient  *http.Client
	IssuerURL   string
	ResourceURL string
	AgentName   string
	OpenBrowser func(string) error
	Now         func() time.Time
}

type Manager struct {
	store       Store
	httpClient  *http.Client
	issuerURL   string
	resourceURL string
	agentName   string
	openBrowser func(string) error
	now         func() time.Time
}

func New(configuration Configuration) (*Manager, error) {
	issuer := strings.TrimRight(strings.TrimSpace(configuration.IssuerURL), "/")
	if issuer == "" {
		issuer = defaultIssuer
	}
	resource := strings.TrimRight(strings.TrimSpace(configuration.ResourceURL), "/")
	if resource == "" {
		resource = defaultResource
	}
	agentName := strings.TrimSpace(configuration.AgentName)
	if agentName == "" {
		agentName = "Archive Archivist"
	}
	client := configuration.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	now := configuration.Now
	if now == nil {
		now = time.Now
	}
	if configuration.Store == nil || configuration.OpenBrowser == nil || !safeEndpoint(issuer) || !safeEndpoint(resource) {
		return nil, ErrInvalidProfile
	}
	return &Manager{
		store: configuration.Store, httpClient: client, issuerURL: issuer, resourceURL: resource,
		agentName: agentName, openBrowser: configuration.OpenBrowser, now: now,
	}, nil
}

type discoveryDocument struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	RevocationEndpoint    string `json:"revocation_endpoint"`
}

func (manager *Manager) discovery(ctx context.Context, issuer string) (discoveryDocument, error) {
	target := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	response, err := manager.httpClient.Do(request)
	if err != nil {
		return discoveryDocument{}, ErrAuthenticationFailed
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return discoveryDocument{}, ErrAuthenticationFailed
	}
	var result discoveryDocument
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil ||
		result.Issuer != issuer || !safeEndpoint(result.AuthorizationEndpoint) ||
		!safeEndpoint(result.TokenEndpoint) || !safeEndpoint(result.JWKSURI) {
		return discoveryDocument{}, ErrAuthenticationFailed
	}
	return result, nil
}

type callbackResult struct {
	code     string
	state    string
	clientID string
	err      string
}

func (manager *Manager) Login(ctx context.Context, reference string) (Profile, error) {
	discovery, err := manager.discovery(ctx, manager.issuerURL)
	if err != nil {
		return Profile{}, err
	}
	existing, _ := manager.store.Load(reference)
	hostID := existing.HostID
	if hostID == "" {
		hostID, err = newHostID()
		if err != nil {
			return Profile{}, ErrAuthenticationFailed
		}
	}
	issuedClientID := existing.ClientID
	if issuedClientID == "" {
		issuedClientID = "dynamic_agent_client"
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Profile{}, ErrAuthenticationFailed
	}
	defer listener.Close()
	redirectURI := "http://" + listener.Addr().String() + "/auth/callback"
	callback := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/callback", func(writer http.ResponseWriter, request *http.Request) {
		result := callbackResult{
			code: request.URL.Query().Get("code"), state: request.URL.Query().Get("state"),
			clientID: request.URL.Query().Get("client_id"), err: request.URL.Query().Get("error"),
		}
		select {
		case callback <- result:
			writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(writer, "Archive Archivist 인증 응답을 받았습니다. CLI 결과를 확인한 뒤 이 창을 닫아도 됩니다.")
		default:
			http.Error(writer, "callback already received", http.StatusConflict)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	serverDone := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serverDone <- err
	}()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		<-serverDone
	}()

	for authorizationAttempt := 0; authorizationAttempt < 2; authorizationAttempt++ {
		state, err := randomURLToken(32)
		if err != nil {
			return Profile{}, ErrAuthenticationFailed
		}
		nonce, err := randomURLToken(32)
		if err != nil {
			return Profile{}, ErrAuthenticationFailed
		}
		verifier, err := randomURLToken(64)
		if err != nil {
			return Profile{}, ErrAuthenticationFailed
		}
		challengeSum := sha256.Sum256([]byte(verifier))
		challenge := base64.RawURLEncoding.EncodeToString(challengeSum[:])
		dynamicRegistration := issuedClientID == "dynamic_agent_client"

		authorize, _ := url.Parse(discovery.AuthorizationEndpoint)
		query := authorize.Query()
		query.Set("client_id", issuedClientID)
		query.Set("ext_agent_host_id", hostID)
		if dynamicRegistration {
			query.Set("agent_name_hint", manager.agentName)
		} else {
			if existing.IDToken != "" {
				query.Set("id_token_hint", existing.IDToken)
			}
			if existing.Email != "" {
				query.Set("login_hint", existing.Email)
			}
		}
		query.Set("response_type", "code")
		query.Set("redirect_uri", redirectURI)
		query.Set("scope", requiredScopes)
		query.Set("resource", manager.resourceURL)
		query.Set("state", state)
		query.Set("nonce", nonce)
		query.Set("code_challenge_method", "S256")
		query.Set("code_challenge", challenge)
		authorize.RawQuery = query.Encode()
		if err := manager.openBrowser(authorize.String()); err != nil {
			return Profile{}, ErrAuthenticationFailed
		}

		var result callbackResult
		select {
		case <-ctx.Done():
			return Profile{}, ctx.Err()
		case result = <-callback:
		}
		if result.err != "" || result.state != state || result.code == "" {
			return Profile{}, ErrAuthenticationFailed
		}
		if dynamicRegistration {
			issuedClientID = strings.TrimSpace(result.clientID)
			if issuedClientID == "" || issuedClientID == "dynamic_agent_client" {
				return Profile{}, ErrAuthenticationFailed
			}
		} else if result.clientID != "" && result.clientID != issuedClientID {
			return Profile{}, ErrAuthenticationFailed
		}

		tokens, err := manager.exchange(ctx, discovery.TokenEndpoint, url.Values{
			"grant_type":    {"authorization_code"},
			"client_id":     {issuedClientID},
			"code":          {result.code},
			"code_verifier": {verifier},
			"redirect_uri":  {redirectURI},
			"resource":      {manager.resourceURL},
		})
		if err != nil {
			if authorizationAttempt == 0 && errors.Is(err, ErrInvalidGrant) {
				continue
			}
			return Profile{}, fmt.Errorf("token exchange: %w", err)
		}
		identity, err := manager.validateIDToken(ctx, discovery, tokens.IDToken, issuedClientID, nonce)
		if err != nil {
			return Profile{}, fmt.Errorf("identity validation: %w", err)
		}
		if !hasPlanScopes(splitScopes(tokens.Scope)) {
			return Profile{}, fmt.Errorf("scope validation: %w", ErrAuthenticationFailed)
		}
		if existing.Subject != "" && existing.Subject != identity.Subject {
			return Profile{}, ErrAuthenticationFailed
		}
		profile := Profile{
			Version: profileVersion, Email: identity.Email, Issuer: discovery.Issuer, Subject: identity.Subject,
			ClientID: issuedClientID, HostID: hostID, IDToken: tokens.IDToken, AccessToken: tokens.AccessToken,
			RefreshToken: tokens.RefreshToken, TokenType: tokens.TokenType, ExpiresIn: tokens.ExpiresIn,
			Scopes: splitScopes(tokens.Scope), SavedAt: manager.now().UTC(),
		}
		if !profile.Valid() {
			return Profile{}, fmt.Errorf("profile validation: %w", ErrAuthenticationFailed)
		}
		if err := manager.store.Save(reference, profile); err != nil {
			return Profile{}, fmt.Errorf("profile storage: %w", ErrAuthenticationFailed)
		}
		return profile, nil
	}
	return Profile{}, ErrAuthenticationFailed
}

type tokenResponse struct {
	AccessToken       string          `json:"access_token"`
	RefreshToken      string          `json:"refresh_token"`
	IDToken           string          `json:"id_token"`
	TokenType         string          `json:"token_type"`
	ExpiresIn         int64           `json:"expires_in"`
	Scope             string          `json:"scope"`
	EarliestRefreshAt json.RawMessage `json:"earliest_refresh_at"`
}

func (manager *Manager) exchange(ctx context.Context, endpoint string, values url.Values) (tokenResponse, error) {
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := manager.httpClient.Do(request)
	if err != nil {
		return tokenResponse{}, ErrAuthenticationFailed
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var oauthFailure struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(&oauthFailure)
		failureCode := strings.TrimSpace(oauthFailure.Error)
		if failureCode == "" || strings.IndexFunc(failureCode, func(character rune) bool {
			return !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') &&
				!(character >= '0' && character <= '9') && character != '_' && character != '-'
		}) >= 0 {
			failureCode = "oauth_error"
		}
		if response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusUnauthorized {
			if failureCode == "invalid_grant" {
				return tokenResponse{}, fmt.Errorf("%w: %w", ErrInvalidGrant, ErrReconnectRequired)
			}
			return tokenResponse{}, fmt.Errorf("%s: %w", failureCode, ErrReconnectRequired)
		}
		return tokenResponse{}, ErrAuthenticationFailed
	}
	var tokens tokenResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&tokens); err != nil ||
		tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.IDToken == "" || tokens.ExpiresIn <= 0 {
		return tokenResponse{}, ErrAuthenticationFailed
	}
	return tokens, nil
}

var refreshLocks sync.Map

func (manager *Manager) AccessToken(ctx context.Context, reference string) (string, Profile, error) {
	lockValue, _ := refreshLocks.LoadOrStore(reference, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	profile, err := manager.store.Load(reference)
	if err != nil || !profile.Valid() {
		return "", Profile{}, ErrReconnectRequired
	}
	if manager.now().Before(profile.SavedAt.Add(time.Duration(profile.ExpiresIn)*time.Second - 2*time.Minute)) {
		return profile.AccessToken, profile, nil
	}
	discovery, err := manager.discovery(ctx, profile.Issuer)
	if err != nil {
		return "", Profile{}, err
	}
	tokens, err := manager.exchange(ctx, discovery.TokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {profile.ClientID},
		"refresh_token": {profile.RefreshToken},
		"resource":      {manager.resourceURL},
	})
	if err != nil {
		return "", Profile{}, err
	}
	identity, err := manager.validateIDToken(ctx, discovery, tokens.IDToken, profile.ClientID, "")
	if err != nil || identity.Subject != profile.Subject {
		return "", Profile{}, ErrReconnectRequired
	}
	scopes := splitScopes(tokens.Scope)
	if len(scopes) == 0 {
		scopes = append([]string(nil), profile.Scopes...)
	}
	if !hasPlanScopes(scopes) {
		return "", Profile{}, ErrReconnectRequired
	}
	profile.Email = identity.Email
	profile.IDToken = tokens.IDToken
	profile.AccessToken = tokens.AccessToken
	profile.RefreshToken = tokens.RefreshToken
	profile.TokenType = tokens.TokenType
	profile.ExpiresIn = tokens.ExpiresIn
	profile.Scopes = scopes
	profile.SavedAt = manager.now().UTC()
	if err := manager.store.Save(reference, profile); err != nil {
		return "", Profile{}, ErrAuthenticationFailed
	}
	return profile.AccessToken, profile, nil
}

func (manager *Manager) Profile(reference string) (Profile, error) {
	return manager.store.Load(reference)
}

func (manager *Manager) Logout(ctx context.Context, reference string) error {
	profile, err := manager.store.Load(reference)
	if err != nil {
		return err
	}
	discovery, discoveryErr := manager.discovery(ctx, profile.Issuer)
	if discoveryErr == nil && discovery.RevocationEndpoint != "" && safeEndpoint(discovery.RevocationEndpoint) {
		values := url.Values{"token": {profile.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {profile.ClientID}}
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, discovery.RevocationEndpoint, strings.NewReader(values.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, requestErr := manager.httpClient.Do(request)
		if requestErr == nil {
			response.Body.Close()
		}
	}
	return manager.store.Delete(reference)
}

type identity struct {
	Subject string
	Email   string
}

type jwkSet struct {
	Keys []struct {
		KID string `json:"kid"`
		KTY string `json:"kty"`
		Alg string `json:"alg"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

func (manager *Manager) validateIDToken(ctx context.Context, discovery discoveryDocument, token, audience, nonce string) (identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return identity{}, ErrAuthenticationFailed
	}
	var header struct {
		Alg string `json:"alg"`
		KID string `json:"kid"`
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(headerBytes, &header) != nil || header.Alg != "RS256" || header.KID == "" {
		return identity{}, ErrAuthenticationFailed
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, discovery.JWKSURI, nil)
	response, err := manager.httpClient.Do(request)
	if err != nil {
		return identity{}, ErrAuthenticationFailed
	}
	defer response.Body.Close()
	var keys jwkSet
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 128<<10)).Decode(&keys) != nil {
		return identity{}, ErrAuthenticationFailed
	}
	var publicKey *rsa.PublicKey
	for _, key := range keys.Keys {
		if key.KID != header.KID || key.KTY != "RSA" || (key.Alg != "" && key.Alg != "RS256") {
			continue
		}
		modulusBytes, modulusErr := base64.RawURLEncoding.DecodeString(key.N)
		exponentBytes, exponentErr := base64.RawURLEncoding.DecodeString(key.E)
		if modulusErr != nil || exponentErr != nil || len(exponentBytes) == 0 || len(exponentBytes) > 4 {
			continue
		}
		exponent := 0
		for _, value := range exponentBytes {
			exponent = exponent<<8 | int(value)
		}
		publicKey = &rsa.PublicKey{N: new(big.Int).SetBytes(modulusBytes), E: exponent}
		break
	}
	if publicKey == nil {
		return identity{}, ErrAuthenticationFailed
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return identity{}, ErrAuthenticationFailed
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature) != nil {
		return identity{}, ErrAuthenticationFailed
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return identity{}, ErrAuthenticationFailed
	}
	var claims map[string]json.RawMessage
	if json.Unmarshal(claimsBytes, &claims) != nil {
		return identity{}, ErrAuthenticationFailed
	}
	issuer, subject, email, claimNonce := claimString(claims, "iss"), claimString(claims, "sub"), claimString(claims, "email"), claimString(claims, "nonce")
	expires, err := claimInt64(claims, "exp")
	if err != nil || issuer != discovery.Issuer || subject == "" || manager.now().Unix() >= expires ||
		(nonce != "" && claimNonce != nonce) || !claimAudience(claims["aud"], audience) {
		return identity{}, ErrAuthenticationFailed
	}
	return identity{Subject: subject, Email: email}, nil
}

func claimString(claims map[string]json.RawMessage, name string) string {
	var value string
	_ = json.Unmarshal(claims[name], &value)
	return value
}

func claimInt64(claims map[string]json.RawMessage, name string) (int64, error) {
	var number json.Number
	if err := json.Unmarshal(claims[name], &number); err != nil {
		return 0, err
	}
	return strconv.ParseInt(string(number), 10, 64)
}

func claimAudience(raw json.RawMessage, audience string) bool {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single == audience
	}
	var values []string
	if json.Unmarshal(raw, &values) != nil {
		return false
	}
	for _, value := range values {
		if value == audience {
			return true
		}
	}
	return false
}

func splitScopes(source string) []string {
	result := strings.Fields(source)
	sort.Strings(result)
	return result
}

func hasPlanScopes(scopes []string) bool {
	set := make(map[string]bool, len(scopes))
	for _, scope := range scopes {
		set[scope] = true
	}
	for _, required := range []string{"openid", "offline_access", "resource.invoke", "chatgpt.tokens.use.direct"} {
		if !set[required] {
			return false
		}
	}
	return true
}

func randomURLToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func newHostID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

func safeEndpoint(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	host := parsed.Hostname()
	return parsed.Scheme == "http" && (host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost"))
}
