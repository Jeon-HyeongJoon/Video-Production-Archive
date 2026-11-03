//go:build windows

package ossecret

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Requirement: LRH-010
func TestWindowsDPAPISecretSurvivesProcessBoundaryWithoutPlaintextAtRest(t *testing.T) {
	if os.Getenv("ARCHIVE_WINDOWS_DPAPI_ACCEPTANCE") != "1" {
		t.Skip("set ARCHIVE_WINDOWS_DPAPI_ACCEPTANCE=1 on the isolated Windows acceptance host")
	}
	if os.Getenv("ARCHIVE_WINDOWS_DPAPI_CHILD") == "1" {
		resolved, err := New().Resolve(os.Getenv("ARCHIVE_WINDOWS_DPAPI_REFERENCE"))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(resolved))
		if hex.EncodeToString(digest[:]) != os.Getenv("ARCHIVE_WINDOWS_DPAPI_DIGEST") {
			t.Fatal("resolved credential digest does not match")
		}
		return
	}

	directory := t.TempDir()
	path := filepath.Join(directory, "runtime-service.dpapi")
	reference := "dpapi://" + path
	secret := "runtime-dpapi-acceptance-secret"
	if err := ProvisionDPAPI(reference, secret); err != nil {
		t.Fatal(err)
	}
	ciphertext, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ciphertext) == 0 || bytes.Contains(ciphertext, []byte(secret)) {
		t.Fatal("DPAPI blob is empty or contains the plaintext credential")
	}

	digest := sha256.Sum256([]byte(secret))
	command := exec.Command(os.Args[0], "-test.run=^TestWindowsDPAPISecretSurvivesProcessBoundaryWithoutPlaintextAtRest$", "-test.v=true")
	command.Env = append(os.Environ(),
		"ARCHIVE_WINDOWS_DPAPI_CHILD=1",
		"ARCHIVE_WINDOWS_DPAPI_REFERENCE="+reference,
		"ARCHIVE_WINDOWS_DPAPI_DIGEST="+hex.EncodeToString(digest[:]),
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("fresh process did not resolve DPAPI secret: %v: %s", err, output)
	}
	if bytes.Contains(output, []byte(secret)) {
		t.Fatal("fresh process logged the plaintext credential")
	}
	if err := ProvisionDPAPI("dpapi://relative.dpapi", secret); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("relative DPAPI reference error=%v want=%v", err, ErrSecretUnavailable)
	}
	corruptReference := "dpapi://" + filepath.Join(directory, "corrupt.dpapi")
	if err := os.WriteFile(filepath.Join(directory, "corrupt.dpapi"), []byte("not a DPAPI blob"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New().Resolve(corruptReference); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("corrupt DPAPI blob error=%v want=%v", err, ErrSecretUnavailable)
	}
}
