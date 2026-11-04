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

	"videoarchive/internal/adapter/archivisthostconfig"
	"videoarchive/internal/adapter/mcpproxy"
	"videoarchive/internal/adapter/ossecret"
	"videoarchive/internal/adapter/processruntime"
	"videoarchive/internal/application/archivisthostclient"
)

var version = "dev"

func main() {
	configurationPath := flag.String("config", "archivist-host.yaml", "path to the Archivist Host boot configuration")
	check := flag.Bool("check-config", false, "validate configuration and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	configuration, err := archivisthostconfig.Load(*configurationPath)
	if err != nil {
		slog.Error("load Archivist Host configuration", "error", err)
		os.Exit(1)
	}
	if *check {
		fmt.Printf("configuration ok: %s\n", *configurationPath)
		return
	}
	resolver := ossecret.New()
	client, err := archivisthostclient.New(
		configuration, resolver, &http.Client{Timeout: 30 * time.Second}, processruntime.New(),
	)
	if err != nil {
		slog.Error("initialize Archivist Host", "error", err)
		os.Exit(1)
	}
	proxy, err := mcpproxy.New(mcpproxy.Configuration{
		ListenAddress: configuration.MCPListenAddress, ControlURL: configuration.MCPControlURL.String(),
		ArchivistHostID: configuration.ID, Credential: client.Credential(),
	})
	if err != nil {
		slog.Error("initialize MCP proxy", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	result := make(chan error, 2)
	go func() { result <- proxy.Run(ctx) }()
	go func() {
		result <- client.Run(ctx, func(err error) { slog.Warn("Archivist Host operation failed", "error", err) })
	}()
	slog.Info("Archivist Host started", "archivist_host_id", configuration.ID, "version", version)
	if err := <-result; err != nil {
		slog.Error("Archivist Host stopped", "error", err)
		stop()
		select {
		case <-result:
		case <-time.After(10 * time.Second):
		}
		os.Exit(1)
	}
	stop()
	<-result
}
