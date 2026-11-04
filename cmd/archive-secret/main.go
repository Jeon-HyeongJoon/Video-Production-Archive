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
	flag.Parse()
	if strings.TrimSpace(*reference) == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "a DPAPI reference and stdin credential are required")
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
	if err := ossecret.ProvisionDPAPI(*reference, string(payload)); err != nil {
		clear(payload)
		fmt.Fprintln(os.Stderr, "DPAPI credential provisioning failed")
		os.Exit(1)
	}
	clear(payload)
	fmt.Println("DPAPI credential stored")
}
