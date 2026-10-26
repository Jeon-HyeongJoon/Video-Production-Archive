package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"videoarchive/internal/adapter/bootconfig"
	"videoarchive/internal/application/controlruntime"
)

var version = "dev"

func main() {
	configurationPath := flag.String("config", "config.yaml", "path to the boot configuration")
	check := flag.Bool("check-config", false, "validate configuration and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	configuration, err := bootconfig.Load(*configurationPath)
	if err != nil {
		slog.Error("load boot configuration", "error", err)
		os.Exit(1)
	}
	if *check {
		fmt.Printf("configuration ok: %s\n", *configurationPath)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	runtime, err := controlruntime.Open(ctx, configuration, version)
	if err != nil {
		slog.Error("open control runtime", "error", err)
		os.Exit(1)
	}
	defer runtime.Close()
	slog.Info("control service started", "listen_address", configuration.Settings.ListenAddress, "version", version)
	if err := runtime.Run(ctx); err != nil {
		slog.Error("control service stopped", "error", err)
		os.Exit(1)
	}
}
