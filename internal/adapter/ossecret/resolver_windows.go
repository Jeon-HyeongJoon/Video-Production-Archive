//go:build windows

package ossecret

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

const credentialTypeGeneric = 1

const (
	cryptProtectUIForbidden  = 0x1
	cryptProtectLocalMachine = 0x4
	maximumDPAPIBlobSize     = 1 << 20
	maximumSecretSize        = 64 << 10
)

var (
	advapi32  = windows.NewLazySystemDLL("advapi32.dll")
	credReadW = advapi32.NewProc("CredReadW")
	credFree  = advapi32.NewProc("CredFree")
)

type credential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

type Resolver struct{}

func New() *Resolver { return &Resolver{} }

func ProvisionKeychain(string, string) error { return ErrUnsupportedPlatform }

func DeleteKeychain(string) error { return ErrUnsupportedPlatform }

func (*Resolver) Resolve(reference string) (string, error) {
	switch {
	case strings.HasPrefix(reference, "wincred://"):
		return resolveWindowsCredential(reference)
	case strings.HasPrefix(reference, "dpapi://"):
		return resolveDPAPI(reference)
	default:
		return "", ErrSecretUnavailable
	}
}

func resolveWindowsCredential(reference string) (string, error) {
	target := strings.TrimPrefix(reference, "wincred://")
	if target == "" {
		return "", ErrSecretUnavailable
	}
	targetPointer, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return "", ErrSecretUnavailable
	}
	var value *credential
	result, _, _ := credReadW.Call(
		uintptr(unsafe.Pointer(targetPointer)), credentialTypeGeneric, 0, uintptr(unsafe.Pointer(&value)),
	)
	if result == 0 || value == nil || value.CredentialBlob == nil || value.CredentialBlobSize == 0 {
		return "", ErrSecretUnavailable
	}
	defer credFree.Call(uintptr(unsafe.Pointer(value)))
	blob := append([]byte(nil), unsafe.Slice(value.CredentialBlob, int(value.CredentialBlobSize))...)
	secret := decodeCredentialBlob(blob)
	if secret == "" {
		return "", ErrSecretUnavailable
	}
	return secret, nil
}

func ProvisionDPAPI(reference, secret string) error {
	path, err := dpapiPath(reference)
	if err != nil || secret == "" || len(secret) > maximumSecretSize || strings.ContainsRune(secret, '\x00') {
		return ErrSecretUnavailable
	}
	plaintext := []byte(secret)
	defer clear(plaintext)
	input := windows.DataBlob{Size: uint32(len(plaintext)), Data: &plaintext[0]}
	var output windows.DataBlob
	description, err := windows.UTF16PtrFromString("Archive service credential")
	if err != nil {
		return ErrSecretUnavailable
	}
	if err := windows.CryptProtectData(
		&input, description, nil, 0, nil, cryptProtectUIForbidden|cryptProtectLocalMachine, &output,
	); err != nil || output.Data == nil || output.Size == 0 || output.Size > maximumDPAPIBlobSize {
		return fmt.Errorf("%w: protect DPAPI credential", ErrSecretUnavailable)
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(output.Data))))
	ciphertext := append([]byte(nil), unsafe.Slice(output.Data, int(output.Size))...)

	temporary, err := os.CreateTemp(filepath.Dir(path), ".archive-secret-*.tmp")
	if err != nil {
		return fmt.Errorf("%w: create DPAPI temporary file", ErrSecretUnavailable)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("%w: restrict DPAPI temporary file", ErrSecretUnavailable)
	}
	if _, err := temporary.Write(ciphertext); err != nil {
		return fmt.Errorf("%w: write DPAPI temporary file", ErrSecretUnavailable)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("%w: sync DPAPI temporary file", ErrSecretUnavailable)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("%w: close DPAPI temporary file", ErrSecretUnavailable)
	}
	if err := windows.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("%w: publish DPAPI credential", ErrSecretUnavailable)
	}
	removeTemporary = false
	return nil
}

func resolveDPAPI(reference string) (string, error) {
	path, err := dpapiPath(reference)
	if err != nil {
		return "", err
	}
	ciphertext, err := os.ReadFile(path)
	if err != nil || len(ciphertext) == 0 || len(ciphertext) > maximumDPAPIBlobSize {
		return "", fmt.Errorf("%w: read DPAPI credential", ErrSecretUnavailable)
	}
	input := windows.DataBlob{Size: uint32(len(ciphertext)), Data: &ciphertext[0]}
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, nil, nil, 0, nil, cryptProtectUIForbidden, &output); err != nil ||
		output.Data == nil || output.Size == 0 || output.Size > maximumSecretSize {
		return "", fmt.Errorf("%w: unprotect DPAPI credential", ErrSecretUnavailable)
	}
	plaintext := unsafe.Slice(output.Data, int(output.Size))
	secret := string(plaintext)
	clear(plaintext)
	_, _ = windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(output.Data))))
	if secret == "" || strings.ContainsRune(secret, '\x00') {
		return "", ErrSecretUnavailable
	}
	return secret, nil
}

func dpapiPath(reference string) (string, error) {
	raw := strings.TrimSpace(strings.TrimPrefix(reference, "dpapi://"))
	if raw == "" || strings.ContainsRune(raw, '\x00') || !filepath.IsAbs(raw) {
		return "", ErrSecretUnavailable
	}
	path := filepath.Clean(raw)
	volume := filepath.VolumeName(path)
	if len(volume) != 2 || volume[1] != ':' || strings.Contains(path[len(volume):], ":") {
		return "", ErrSecretUnavailable
	}
	return path, nil
}

func decodeCredentialBlob(blob []byte) string {
	if len(blob)%2 == 0 && (bytes.IndexByte(blob, 0) >= 0 || !utf8.Valid(blob)) {
		values := make([]uint16, 0, len(blob)/2)
		for index := 0; index < len(blob); index += 2 {
			value := uint16(blob[index]) | uint16(blob[index+1])<<8
			if value == 0 {
				break
			}
			values = append(values, value)
		}
		return string(utf16.Decode(values))
	}
	return strings.TrimRight(string(blob), "\x00")
}
