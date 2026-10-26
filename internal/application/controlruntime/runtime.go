package controlruntime

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"videoarchive/internal/adapter/bootconfig"
	"videoarchive/internal/adapter/httpcontrol"
	"videoarchive/internal/adapter/httpworker"
	"videoarchive/internal/adapter/notificationsse"
	"videoarchive/internal/adapter/ossecret"
	"videoarchive/internal/adapter/sqlitecatalog"
	"videoarchive/internal/adapter/sqlitecommand"
	"videoarchive/internal/adapter/sqlitedashboard"
	"videoarchive/internal/adapter/sqlitestorage"
	"videoarchive/internal/application/dashboard"
	"videoarchive/internal/application/jobcontrol"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/notification"
	"videoarchive/internal/core/security"
	"videoarchive/internal/core/worker"
)

type SecretResolver interface {
	Resolve(reference string) (string, error)
}

type Dependencies struct {
	Secrets SecretResolver
	Now     func() time.Time
}

type credentialVerifier struct {
	resolver SecretResolver
}

func (verifier credentialVerifier) Verify(_ string, credentialReference, presentedCredential string) bool {
	want, err := verifier.resolver.Resolve(credentialReference)
	return err == nil && want != "" && len(want) == len(presentedCredential) &&
		subtle.ConstantTimeCompare([]byte(want), []byte(presentedCredential)) == 1
}

type Runtime struct {
	configuration bootconfig.Configuration
	catalog       *sqlitecatalog.Store
	dashboard     *sqlitedashboard.Store
	commands      *sqlitecommand.Store
	storage       *sqlitestorage.Repository
	handler       *httpcontrol.Handler
}

func Open(ctx context.Context, configuration bootconfig.Configuration, version string) (*Runtime, error) {
	return OpenWithDependencies(ctx, configuration, version, Dependencies{Secrets: ossecret.New(), Now: time.Now})
}

func OpenWithDependencies(ctx context.Context, configuration bootconfig.Configuration, version string, dependencies Dependencies) (*Runtime, error) {
	if dependencies.Secrets == nil || dependencies.Now == nil {
		return nil, errors.New("control runtime dependencies are incomplete")
	}
	if err := os.MkdirAll(filepath.Dir(configuration.DatabasePath), 0o750); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	catalogStore, err := sqlitecatalog.Open(configuration.DatabasePath)
	if err != nil {
		return nil, err
	}
	runtime := &Runtime{configuration: configuration, catalog: catalogStore}
	fail := func(cause error) (*Runtime, error) {
		runtime.Close()
		return nil, cause
	}
	dashboardStore, err := sqlitedashboard.Open(configuration.DatabasePath)
	if err != nil {
		return fail(err)
	}
	runtime.dashboard = dashboardStore
	commandStore, err := sqlitecommand.Open(configuration.DatabasePath)
	if err != nil {
		return fail(err)
	}
	runtime.commands = commandStore
	storageRepository, err := sqlitestorage.Open(configuration.DatabasePath)
	if err != nil {
		return fail(err)
	}
	runtime.storage = storageRepository
	for _, topology := range configuration.Storages {
		if err := storageRepository.Register(ctx, topology); err != nil {
			return fail(err)
		}
	}
	queries, err := dashboard.NewQueryService(dashboardStore)
	if err != nil {
		return fail(err)
	}
	jobHandler, err := job.NewHandler(dependencies.Now)
	if err != nil {
		return fail(err)
	}
	executor := command.NewAuthorizedCreatingExecutor(commandStore, func(envelope command.Envelope) error {
		if !security.Authorize(security.Request{
			ActorType: envelope.Actor.Type, Role: security.Operator, Action: security.Process,
			AuthMode: configuration.Access.Mode,
		}) {
			return errors.New("job command is not authorized")
		}
		return nil
	}, jobHandler.Handle)
	jobCommands, err := jobcontrol.New(executor, commandStore, dashboardStore)
	if err != nil {
		return fail(err)
	}
	workerRegistry := worker.NewRegistry(configuration.Workers, credentialVerifier{resolver: dependencies.Secrets}, configuration.HeartbeatTTL)
	workerAPI, err := httpworker.New(workerRegistry, dependencies.Now)
	if err != nil {
		return fail(err)
	}
	handler, err := httpcontrol.New(httpcontrol.Configuration{
		Access: configuration.Access, TrustedProxies: configuration.TrustedProxies,
		Catalog: catalogStore, Dashboard: queries, Notifications: notificationsse.New(&notification.Feed{}),
		Commands: jobCommands, WorkerAPI: workerAPI, Workers: workerAPI,
		AllowedOrigins: configuration.AllowedOrigins, Version: version,
	})
	if err != nil {
		return fail(err)
	}
	runtime.handler = handler
	return runtime, nil
}

func (runtime *Runtime) Handler() http.Handler { return runtime.handler }

func (runtime *Runtime) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", runtime.configuration.Settings.ListenAddress)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler: runtime.handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
	}
	result := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		result <- err
	}()
	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return err
		}
		return <-result
	case err := <-result:
		return err
	}
}

func (runtime *Runtime) Close() error {
	var combined error
	if runtime.storage != nil {
		combined = errors.Join(combined, runtime.storage.Close())
	}
	if runtime.dashboard != nil {
		combined = errors.Join(combined, runtime.dashboard.Close())
	}
	if runtime.commands != nil {
		combined = errors.Join(combined, runtime.commands.Close())
	}
	if runtime.catalog != nil {
		combined = errors.Join(combined, runtime.catalog.Close())
	}
	return combined
}
