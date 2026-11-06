package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"videoarchive/internal/adapter/ossecret"
)

const maximumInputSize = 64 << 10

func main() {
	reference := flag.String("dpapi-reference", "", "absolute dpapi:// blob reference")
	keychainReference := flag.String("keychain-reference", "", "keychain:// service/account reference")
	flag.Parse()
	dpapi := strings.TrimSpace(*reference)
	keychain := strings.TrimSpace(*keychainReference)
	if (dpapi == "") == (keychain == "") || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "exactly one OS secret reference and stdin credential are required")
		os.Exit(2)
	}
	payload, err := io.ReadAll(io.LimitReader(os.Stdin, maximumInputSize+1))
	if err != nil || len(payload) > maximumInputSize {
		fmt.Fprintln(os.Stderr, "credential input is unavailable or too large")
		os.Exit(2)
	}
	payload = bytes.TrimSuffix(payload, []byte("\r\n"))
	payload = bytes.TrimSuffix(payload, []byte("\n"))
	if len(payload) == 0 {
		fmt.Fprintln(os.Stderr, "credential input is empty")
		os.Exit(2)
	}
	var provisionErr error
	label := "DPAPI"
	if dpapi != "" {
		provisionErr = ossecret.ProvisionDPAPI(dpapi, string(payload))
	} else {
		label = "Keychain"
		provisionErr = ossecret.ProvisionKeychain(keychain, string(payload))
	}
	if provisionErr != nil {
		clear(payload)
		fmt.Fprintf(os.Stderr, "%s credential provisioning failed\n", label)
		os.Exit(1)
	}
	clear(payload)
	fmt.Printf("%s credential stored\n", label)
}
