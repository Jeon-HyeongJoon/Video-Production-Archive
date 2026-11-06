package chatgptauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryStore struct {
	mu      sync.Mutex
	profile Profile
}

func (store *memoryStore) Load(string) (Profile, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if !store.profile.Valid() {
		return Profile{}, ErrInvalidProfile
	}
	return store.profile, nil
}

func (store *memoryStore) Save(_ string, profile Profile) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.profile = profile
	return nil
}

func (store *memoryStore) Delete(string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.profile = Profile{}
	return nil
}

type authFixture struct {
	server           *httptest.Server
	key              *rsa.PrivateKey
	now              time.Time
	nonce            string
	token            atomic.Int32
	invalidGrantOnce atomic.Bool
}

func newAuthFixture(t *testing.T, now time.Time) *authFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &authFixture{key: key, now: now}
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	fixture.server = server
	t.Cleanup(server.Close)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]string{
			"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize",
			"token_endpoint": server.URL + "/token", "jwks_uri": server.URL + "/jwks",
			"revocation_endpoint": server.URL + "/revoke",
		})
	})
	mux.HandleFunc("GET /jwks", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "test-key", "kty": "RSA", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", func(writer http.ResponseWriter, request *http.Request) {
		call := fixture.token.Add(1)
		if fixture.invalidGrantOnce.CompareAndSwap(true, false) {
			writer.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(writer).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		_ = request.ParseForm()
		clientID := request.Form.Get("client_id")
		nonce := fixture.nonce
		claims := map[string]any{
			"iss": server.URL, "sub": "account-subject", "aud": clientID, "email": "archive@example.com",
			"exp": now.Add(time.Hour).Unix(),
		}
		if nonce != "" {
			claims["nonce"] = nonce
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"access_token": fmt.Sprintf("access-%d", call), "refresh_token": fmt.Sprintf("refresh-%d", call),
			"id_token": signJWT(t, key, claims), "token_type": "Bearer", "expires_in": 3600,
			"scope": requiredScopes,
		})
	})
	return fixture
}

// Requirement: AGT-016
func TestChatGPTLoginValidatesPKCECallbackIdentityAndStoresProtectedProfile(t *testing.T) {
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	fixture := newAuthFixture(t, now)
	fixture.invalidGrantOnce.Store(true)
	store := &memoryStore{}
	var authorizationCalls atomic.Int32
	manager, err := New(Configuration{
		Store: store, HTTPClient: fixture.server.Client(), IssuerURL: fixture.server.URL, ResourceURL: fixture.server.URL,
		Now: func() time.Time { return now },
		OpenBrowser: func(target string) error {
			call := authorizationCalls.Add(1)
			authorize, err := url.Parse(target)
			if err != nil {
				return err
			}
			query := authorize.Query()
			expectedClientID := "dynamic_agent_client"
			if call == 2 {
				expectedClientID = "oaiapp_archive_test"
			}
			if call > 2 || query.Get("client_id") != expectedClientID || query.Get("code_challenge_method") != "S256" ||
				query.Get("code_challenge") == "" || query.Get("state") == "" || query.Get("nonce") == "" ||
				query.Get("ext_agent_host_id") == "" || !strings.Contains(query.Get("scope"), "chatgpt.tokens.use.direct") {
				return fmt.Errorf("authorization request is incomplete: %s", target)
			}
			fixture.nonce = query.Get("nonce")
			callback, _ := url.Parse(query.Get("redirect_uri"))
			callbackQuery := callback.Query()
			callbackQuery.Set("code", "authorization-code")
			callbackQuery.Set("state", query.Get("state"))
			if call == 1 {
				callbackQuery.Set("client_id", "oaiapp_archive_test")
			}
			callback.RawQuery = callbackQuery.Encode()
			go func() { _, _ = fixture.server.Client().Get(callback.String()) }()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := manager.Login(context.Background(), "keychain://archive/test-profile")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Load("keychain://archive/test-profile")
	if err != nil || profile.ClientID != "oaiapp_archive_test" || profile.Subject != "account-subject" ||
		stored.AccessToken != profile.AccessToken || authorizationCalls.Load() != 2 || fixture.token.Load() != 2 ||
		!strings.HasPrefix(profile.HostID, "urn:uuid:") {
		t.Fatalf("profile=%+v stored=%+v err=%v", profile, stored, err)
	}
}

// Requirement: AGT-017
func TestChatGPTRefreshSerializesRotatingTokenAndAtomicallyReplacesProfile(t *testing.T) {
	now := time.Date(2026, 11, 7, 11, 0, 0, 0, time.UTC)
	fixture := newAuthFixture(t, now)
	store := &memoryStore{profile: Profile{
		Version: profileVersion, Email: "archive@example.com", Issuer: fixture.server.URL, Subject: "account-subject",
		ClientID: "oaiapp_archive_test", HostID: "urn:uuid:11111111-1111-4111-8111-111111111111",
		IDToken: "old-id", AccessToken: "old-access", RefreshToken: "old-refresh", TokenType: "Bearer",
		ExpiresIn: 3600, Scopes: splitScopes(requiredScopes), SavedAt: now.Add(-2 * time.Hour),
	}}
	manager, err := New(Configuration{
		Store: store, HTTPClient: fixture.server.Client(), IssuerURL: fixture.server.URL, ResourceURL: fixture.server.URL,
		Now: func() time.Time { return now }, OpenBrowser: func(string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan string, 2)
	errorsFound := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			token, _, err := manager.AccessToken(context.Background(), "keychain://archive/test-profile")
			results <- token
			errorsFound <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	for token := range results {
		if token != "access-1" {
			t.Fatalf("concurrent caller saw stale or competing token %q", token)
		}
	}
	if fixture.token.Load() != 1 {
		t.Fatalf("rotating refresh token used %d times", fixture.token.Load())
	}
	updated, _ := store.Load("keychain://archive/test-profile")
	if updated.AccessToken != "access-1" || updated.RefreshToken != "refresh-1" || !updated.SavedAt.Equal(now) {
		t.Fatalf("profile was not atomically replaced: %+v", updated)
	}
}

func signJWT(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test-key", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}
