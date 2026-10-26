//go:build windows

package ossecret

import (
	"bytes"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

const credentialTypeGeneric = 1

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

func (*Resolver) Resolve(reference string) (string, error) {
	if !strings.HasPrefix(reference, "wincred://") {
		return "", ErrSecretUnavailable
	}
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
