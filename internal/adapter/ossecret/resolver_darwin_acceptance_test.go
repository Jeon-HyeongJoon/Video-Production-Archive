//go:build darwin

package ossecret

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
)

// Requirement: CFG-015
func TestMacOSKeychainSecretRoundTripDoesNotExposeCredentialInReference(t *testing.T) {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(random)
	reference := "keychain://archive-acceptance/" + suffix
	secret := "host-credential-" + suffix
	if err := ProvisionKeychain(reference, secret); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DeleteKeychain(reference) })
	resolved, err := New().Resolve(reference)
	if err != nil || resolved != secret {
		t.Fatalf("resolved=%q err=%v", resolved, err)
	}
	if reference == secret {
		t.Fatal("secret leaked into reference")
	}
	if err := DeleteKeychain(reference); err != nil {
		t.Fatal(err)
	}
	if _, err := New().Resolve(reference); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("deleted secret remained available: %v", err)
	}
}
