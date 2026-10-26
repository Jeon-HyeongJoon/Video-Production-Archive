package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"videoarchive/internal/adapter/ossecret"
	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/workerclient"
)

var version = "dev"

func main() {
	configurationPath := flag.String("config", "worker.yaml", "path to the Worker boot configuration")
	check := flag.Bool("check-config", false, "validate configuration and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	configuration, err := workerconfig.Load(*configurationPath)
	if err != nil {
		slog.Error("load Worker configuration", "error", err)
		os.Exit(1)
	}
	if *check {
		fmt.Printf("configuration ok: %s\n", *configurationPath)
		return
	}
	client, err := workerclient.New(configuration, ossecret.New(), &http.Client{Timeout: 15 * time.Second})
	if err != nil {
		slog.Error("initialize Worker", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	slog.Info("Worker started", "worker_id", configuration.ID, "control_url", configuration.ControlURL.Redacted(), "version", version)
	if err := client.Run(ctx, func(err error) { slog.Warn("Worker heartbeat failed", "error", err) }); err != nil {
		slog.Error("Worker stopped", "error", err)
		os.Exit(1)
	}
}
