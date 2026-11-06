//go:build darwin

package ossecret

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"strings"
)

const maximumKeychainSecretSize = 1 << 20

type Resolver struct{}

func New() *Resolver { return &Resolver{} }

func (*Resolver) Resolve(reference string) (string, error) {
	service, account, err := keychainTarget(reference)
	if err != nil {
		return "", err
	}
	command := exec.Command("/usr/bin/security", "find-generic-password", "-a", account, "-s", service, "-w")
	var output bytes.Buffer
	command.Stdout = &limitedBuffer{buffer: &output, remaining: maximumKeychainSecretSize}
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return "", ErrSecretUnavailable
	}
	secret := strings.TrimSuffix(output.String(), "\n")
	secret = strings.TrimSuffix(secret, "\r")
	if secret == "" || strings.ContainsRune(secret, '\x00') {
		return "", ErrSecretUnavailable
	}
	return secret, nil
}

func ProvisionKeychain(reference, secret string) error {
	service, account, err := keychainTarget(reference)
	if err != nil || secret == "" || len(secret) > maximumKeychainSecretSize || strings.ContainsAny(secret, "\x00\r\n") {
		return ErrSecretUnavailable
	}
	command := exec.Command("/usr/bin/security", "add-generic-password", "-U", "-a", account, "-s", service, "-w")
	command.Stdin = strings.NewReader(secret + "\n" + secret + "\n")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return ErrSecretUnavailable
	}
	return nil
}

func DeleteKeychain(reference string) error {
	service, account, err := keychainTarget(reference)
	if err != nil {
		return err
	}
	command := exec.Command("/usr/bin/security", "delete-generic-password", "-a", account, "-s", service)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return ErrSecretUnavailable
	}
	return nil
}

func ProvisionDPAPI(string, string) error { return ErrUnsupportedPlatform }

func keychainTarget(reference string) (string, string, error) {
	raw := strings.TrimSpace(reference)
	if !strings.HasPrefix(raw, "keychain://") {
		return "", "", ErrSecretUnavailable
	}
	raw = strings.TrimPrefix(raw, "keychain://")
	service, account, found := strings.Cut(raw, "/")
	if !found || service == "" || account == "" ||
		strings.ContainsAny(service, "\x00\r\n/?#") || strings.ContainsAny(account, "\x00\r\n/?#") {
		return "", "", ErrSecretUnavailable
	}
	return service, account, nil
}

type limitedBuffer struct {
	buffer    *bytes.Buffer
	remaining int
}

func (writer *limitedBuffer) Write(payload []byte) (int, error) {
	if len(payload) > writer.remaining {
		return 0, errors.New("secret exceeds size limit")
	}
	written, err := writer.buffer.Write(payload)
	writer.remaining -= written
	return written, err
}
