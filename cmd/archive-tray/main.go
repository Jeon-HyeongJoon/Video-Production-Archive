package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

var version = "dev"

type trayConfiguration struct {
	DashboardURL     string
	ChromeExecutable string
}

type trayRunner func(trayConfiguration, func() error) error
type processStarter func(string, ...string) error

func main() {
	os.Exit(run(os.Args[1:], runPlatformTray, startChromeProcess))
}

func run(arguments []string, runner trayRunner, starter processStarter) int {
	set := flag.NewFlagSet("archive-tray", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	configuration := trayConfiguration{}
	showVersion := false
	set.StringVar(&configuration.DashboardURL, "dashboard-url", "", "configured Archive Workspace http or https URL")
	set.StringVar(&configuration.ChromeExecutable, "chrome", "", "absolute Chrome executable path")
	set.BoolVar(&showVersion, "version", false, "print version")
	if err := set.Parse(arguments); err != nil || set.NArg() != 0 {
		return 2
	}
	if showVersion {
		fmt.Println(version)
		return 0
	}
	if err := configuration.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "invalid Archive Tray configuration")
		return 2
	}
	openDashboard := func() error {
		return configuration.openDashboard(starter)
	}
	if err := runner(configuration, openDashboard); err != nil {
		fmt.Fprintln(os.Stderr, "Archive Tray stopped")
		return 1
	}
	return 0
}

func (configuration trayConfiguration) validate() error {
	target, err := url.Parse(strings.TrimSpace(configuration.DashboardURL))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" ||
		target.User != nil || target.Fragment != "" {
		return errors.New("dashboard URL must be an absolute http or https URL without user information or fragment")
	}
	configuration.ChromeExecutable = filepath.Clean(strings.TrimSpace(configuration.ChromeExecutable))
	if !filepath.IsAbs(configuration.ChromeExecutable) {
		return errors.New("Chrome executable path must be absolute")
	}
	info, err := os.Stat(configuration.ChromeExecutable)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("Chrome executable is unavailable")
	}
	return nil
}

func (configuration trayConfiguration) openDashboard(start processStarter) error {
	if start == nil {
		return errors.New("process starter is unavailable")
	}
	return start(configuration.ChromeExecutable, "--new-tab", configuration.DashboardURL)
}
