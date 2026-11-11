//go:build !windows

package main

import "errors"

func runPlatformTray(_ trayConfiguration, _ func() error) error {
	return errors.New("Archive Tray is supported on Windows")
}

func startChromeProcess(_ string, _ ...string) error {
	return errors.New("Chrome process launch is supported on Windows")
}
