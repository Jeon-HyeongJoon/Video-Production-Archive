package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"time"

	"videoarchive/internal/adapter/hostresource"
	"videoarchive/internal/adapter/ossecret"
	"videoarchive/internal/adapter/storageinventory"
	"videoarchive/internal/adapter/windowsvolume"
	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/workerclient"
	"videoarchive/internal/application/workerexecutor"
)

var version = "dev"

func main() {
	configurationPath := flag.String("config", "worker.yaml", "path to the Worker boot configuration")
	check := flag.Bool("check-config", false, "validate configuration and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	probeAttachedVolumes := flag.Bool("probe-attached-volumes", false, "print safe attached-volume observations and exit")
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
	if *probeAttachedVolumes {
		if runtime.GOOS != "windows" {
			slog.Error("attached volume probe requires Windows")
			os.Exit(1)
		}
		observer, observeErr := windowsvolume.NewSystemObserver()
		if observeErr != nil {
			slog.Error("initialize Windows attached volume observation", "error", observeErr)
			os.Exit(1)
		}
		if _, observeErr = observer.Poll(context.Background(), configuration.ID, time.Now().UTC()); observeErr != nil {
			slog.Error("observe Windows attached volumes", "error", observeErr)
			os.Exit(1)
		}
		safe := make([]map[string]any, 0)
		for _, volume := range observer.Connected() {
			safe = append(safe, map[string]any{
				"physical_device_id": volume.PhysicalDeviceID, "volume_id": volume.VolumeID,
				"storage_id": volume.StorageID, "archive_storage_id": volume.ArchiveStorageID,
				"model": volume.Model, "label": volume.Label,
				"file_system": volume.FileSystem, "capacity_bytes": volume.CapacityBytes, "free_bytes": volume.FreeBytes,
			})
		}
		if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"attached_volumes": safe}); err != nil {
			slog.Error("write attached volume observation", "error", err)
			os.Exit(1)
		}
		return
	}
	storageRoots, err := workerconfig.OpenStorageRoots(configuration)
	if err != nil {
		slog.Error("open Worker Storage roots", "error", err)
		os.Exit(1)
	}
	defer storageRoots.Close()
	if *check {
		fmt.Printf("configuration ok: %s\n", *configurationPath)
		return
	}
	executor, err := workerexecutor.NewProduction(configuration, storageRoots)
	if err != nil {
		slog.Error("initialize production Worker operations", "error", err)
		os.Exit(1)
	}
	storageProbe, err := storageinventory.New(configuration, storageRoots)
	if err != nil {
		slog.Error("initialize Worker storage inventory", "error", err)
		os.Exit(1)
	}
	var volumeProbe workerclient.VolumeProbe
	if runtime.GOOS == "windows" {
		volumeProbe, err = windowsvolume.NewSystemObserver()
		if err != nil {
			slog.Error("initialize Windows attached volume observation", "error", err)
			os.Exit(1)
		}
	}
	client, err := workerclient.NewWithExecutorAndInventoryProbes(
		configuration, ossecret.New(), &http.Client{Timeout: 15 * time.Second},
		hostresource.New(os.TempDir(), time.Now), time.Now, executor, storageProbe, volumeProbe, storageinventory.NewAttachedProbe(), storageRoots,
	)
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
