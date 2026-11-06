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
	"sort"
	"sync"
	"time"

	"videoarchive/internal/adapter/bootconfig"
	"videoarchive/internal/adapter/ffprobe"
	"videoarchive/internal/adapter/httparchivist"
	"videoarchive/internal/adapter/httpcontrol"
	"videoarchive/internal/adapter/httpmcp"
	"videoarchive/internal/adapter/httpworker"
	"videoarchive/internal/adapter/notificationsse"
	"videoarchive/internal/adapter/ossecret"
	"videoarchive/internal/adapter/sqlitearchiveintake"
	"videoarchive/internal/adapter/sqlitearchiveorganization"
	"videoarchive/internal/adapter/sqlitearchiveworkspace"
	"videoarchive/internal/adapter/sqlitearchivist"
	"videoarchive/internal/adapter/sqlitecatalog"
	"videoarchive/internal/adapter/sqlitecommand"
	"videoarchive/internal/adapter/sqlitestorage"
	"videoarchive/internal/application/archiveintake"
	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/application/archiveorganization"
	"videoarchive/internal/application/archivetranscription"
	"videoarchive/internal/application/archivework"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/application/archivistcontrol"
	"videoarchive/internal/application/dispatchcoordinator"
	"videoarchive/internal/application/jobcontrol"
	"videoarchive/internal/application/workercapacity"
	"videoarchive/internal/application/workercontrol"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/archiverun"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/asset"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/notification"
	"videoarchive/internal/core/security"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
)

type SecretResolver interface {
	Resolve(reference string) (string, error)
}

type Dependencies struct {
	Secrets                SecretResolver
	Now                    func() time.Time
	ArchiveIntakeAnalyzers archiveorganization.AnalyzerFactory
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
	configuration          bootconfig.Configuration
	catalog                *sqlitecatalog.Store
	archiveWorkspace       *sqlitearchiveworkspace.Store
	commands               *sqlitecommand.Store
	organization           *sqlitearchiveorganization.Store
	storage                *sqlitestorage.Repository
	archivistRuntime       *sqlitearchivist.Store
	archiveIntake          *sqlitearchiveintake.Repository
	archivists             *archivistcontrol.Service
	archiveIntakeWorkspace *archiveintakeworkspace.Service
	workerExecution        *workercontrol.Service
	handler                *httpcontrol.Handler
	mcpHandler             *httpmcp.Handler
	backgroundCancel       context.CancelFunc
	backgroundWait         sync.WaitGroup
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
	archiveWorkspaceStore, err := sqlitearchiveworkspace.Open(configuration.DatabasePath)
	if err != nil {
		return fail(err)
	}
	runtime.archiveWorkspace = archiveWorkspaceStore
	commandStore, err := sqlitecommand.Open(configuration.DatabasePath)
	if err != nil {
		return fail(err)
	}
	runtime.commands = commandStore
	organizationStore, err := sqlitearchiveorganization.Open(configuration.DatabasePath)
	if err != nil {
		return fail(err)
	}
	runtime.organization = organizationStore
	archiveIntakeRepository, err := sqlitearchiveintake.Open(configuration.DatabasePath)
	if err != nil {
		return fail(err)
	}
	runtime.archiveIntake = archiveIntakeRepository
	storageRepository, err := sqlitestorage.Open(configuration.DatabasePath)
	if err != nil {
		return fail(err)
	}
	runtime.storage = storageRepository
	archivistRuntimeStore, err := sqlitearchivist.Open(configuration.DatabasePath)
	if err != nil {
		return fail(err)
	}
	runtime.archivistRuntime = archivistRuntimeStore
	for _, topology := range configuration.Storages {
		if err := storageRepository.Register(ctx, topology); err != nil {
			return fail(err)
		}
	}
	queries, err := archiveworkspace.NewQueryService(archiveWorkspaceStore)
	if err != nil {
		return fail(err)
	}
	jobHandler, err := job.NewHandler(dependencies.Now)
	if err != nil {
		return fail(err)
	}
	executor := command.NewAuthorizedCreatingExecutor(commandStore, func(envelope command.Envelope) error {
		if envelope.Actor.Type == vocabulary.ActorSystem && envelope.Actor.ID == "archive-intake-coordinator" && envelope.RequestedVia == "APPLICATION" {
			return nil
		}
		if !security.Authorize(security.Request{
			ActorType: envelope.Actor.Type, Role: security.Operator, Action: security.Process,
			AuthMode:           configuration.Access.Mode,
			ArchivistMCPDirect: envelope.Actor.Type == command.ArchivistActor && envelope.RequestedVia == "mcp",
		}) {
			return errors.New("job command is not authorized")
		}
		return nil
	}, jobHandler.Handle)
	jobCommands, err := jobcontrol.New(executor, commandStore, archiveWorkspaceStore)
	if err != nil {
		return fail(err)
	}
	if err := jobcontrol.RebuildProjections(ctx, commandStore, archiveWorkspaceStore); err != nil {
		return fail(err)
	}
	workerRegistry := worker.NewRegistry(configuration.Workers, credentialVerifier{resolver: dependencies.Secrets}, configuration.HeartbeatTTL)
	archiveIntakeSelection, err := archiveintake.NewService(archiveIntakeRepository)
	if err != nil {
		return fail(err)
	}
	resolvedFolders, err := archiveorganization.NewResolvedFolderService(coordinatorJobBus{service: jobCommands})
	if err != nil {
		return fail(err)
	}
	archiveIntakeWorkspace, err := archiveintakeworkspace.New(
		organizationStore, commandStore,
		archiveintakeworkspace.WithIntakeSessions(archiveIntakeRepository),
		archiveintakeworkspace.WithInventory(archiveIntakeRepository, storageRepository, workerRegistry, dependencies.Now),
		archiveintakeworkspace.WithResolvedFolderStarter(resolvedFolders),
	)
	if err != nil {
		return fail(err)
	}
	runtime.archiveIntakeWorkspace = archiveIntakeWorkspace
	workQueries, err := archivework.New(commandStore, queries, archiveIntakeWorkspace)
	if err != nil {
		return fail(err)
	}
	batchHandler, batchErr := archiverun.NewHandler(dependencies.Now)
	if batchErr != nil {
		return fail(batchErr)
	}
	batchCommands := command.NewAuthorizedCreatingExecutor(commandStore, func(envelope command.Envelope) error {
		if envelope.Actor.Type != vocabulary.ActorHuman || envelope.RequestedVia != "APPLICATION" {
			return errors.New("archive run command is not authorized")
		}
		return nil
	}, batchHandler.Handle)
	for _, topology := range configuration.Storages {
		if topology.Role != vocabulary.StorageSourceMedia {
			continue
		}
		workerID := sourceWorkerID(configuration.Workers, topology.StorageID)
		candidate, buildErr := archiveorganization.BuildStaticCandidate(topology, workerID, dependencies.Now())
		if buildErr != nil {
			return fail(buildErr)
		}
		if _, candidateErr := archiveIntakeRepository.Candidate(ctx, topology.VolumeID); errors.Is(candidateErr, sqlitearchiveintake.ErrRecordNotFound) {
			if saveErr := archiveIntakeRepository.SaveCandidate(ctx, candidate); saveErr != nil {
				return fail(saveErr)
			}
		} else if candidateErr != nil {
			return fail(candidateErr)
		}
	}
	var archiveRuns archiveorganization.Starter
	if configuration.ArchiveOrganization != nil {
		analyzerFactory := archiveorganization.AnalyzerFactory(localArchiveIntakeAnalyzerFactory{
			executable: configuration.ArchiveOrganization.FFprobe.Executable,
			version:    configuration.ArchiveOrganization.FFprobe.Version,
		})
		if dependencies.ArchiveIntakeAnalyzers != nil {
			analyzerFactory = dependencies.ArchiveIntakeAnalyzers
		}
		starter, startErr := archiveorganization.NewStartService(
			archiveIntakeRepository, storageRepository, workerRegistry, catalogStore, organizationStore,
			coordinatorJobBus{service: jobCommands}, executorCommandBus{executor: batchCommands}, analyzerFactory, dependencies.Now,
		)
		if startErr != nil {
			return fail(startErr)
		}
		archiveRuns = starter
	} else {
		starter, startErr := archiveorganization.NewRemoteStartService(
			archiveIntakeRepository, storageRepository, workerRegistry, organizationStore,
			coordinatorJobBus{service: jobCommands}, executorCommandBus{executor: batchCommands}, commandStore, dependencies.Now,
		)
		if startErr != nil {
			return fail(startErr)
		}
		archiveRuns = starter
	}
	stageOneProjection, projectionErr := archivetranscription.NewJobStageOneProjection(organizationStore, commandStore)
	if projectionErr != nil {
		return fail(projectionErr)
	}
	archiveTranscriptions, projectionErr := archivetranscription.New(commandStore, stageOneProjection, coordinatorJobBus{service: jobCommands})
	if projectionErr != nil {
		return fail(projectionErr)
	}
	dispatcher, err := dispatchcoordinator.New(commandStore, workerRegistry, archiveWorkspaceStore, dependencies.Now, configuration.HeartbeatTTL, time.Minute)
	if err != nil {
		return fail(err)
	}
	workerCapacity, err := workercapacity.New(workerRegistry, commandStore, dependencies.Now)
	if err != nil {
		return fail(err)
	}
	if err := jobCommands.SetDispatcher(dispatcher); err != nil {
		return fail(err)
	}
	inspectionCompletion, err := archiveorganization.NewInspectionCompletionCoordinator(
		catalogStore, organizationStore, coordinatorJobBus{service: jobCommands}, commandStore,
	)
	if err != nil {
		return fail(err)
	}
	workerExecution, err := workercontrol.New(commandStore, archiveWorkspaceStore, dispatcher, dependencies.Now, inspectionCompletion)
	if err != nil {
		return fail(err)
	}
	runtime.workerExecution = workerExecution
	volumeInventory, err := archiveintake.NewInventoryService(archiveIntakeRepository)
	if err != nil {
		return fail(err)
	}
	workerAPI, err := httpworker.NewWithExecutionAndVolumes(workerRegistry, dependencies.Now, dispatcher, workerExecution, volumeInventory)
	if err != nil {
		return fail(err)
	}
	archivistHostRegistry := archivistruntime.NewHostRegistry(
		configuration.ArchivistHosts, credentialVerifier{resolver: dependencies.Secrets}, configuration.RuntimeHeartbeatTTL,
	)
	archivists, err := archivistcontrol.New(archivistRuntimeStore, archivistHostRegistry, dependencies.Now, configuration.ArchivistTurnLeaseTTL)
	if err != nil {
		return fail(err)
	}
	runtime.archivists = archivists
	archivistAPI, err := httparchivist.New(archivists)
	if err != nil {
		return fail(err)
	}
	var mcpArchiveIntake httpmcp.ArchiveIntakeWorkspace
	var mcpArchiveRuns httpmcp.ArchiveRunStarter
	var mcpArchiveTranscriptions httpmcp.ArchiveTranscriptionStarter
	if archiveRuns != nil && archiveTranscriptions != nil {
		mcpArchiveIntake = archiveIntakeWorkspace
		mcpArchiveRuns = archiveRuns
		mcpArchiveTranscriptions = archiveTranscriptions
	}
	mcpHandler, err := httpmcp.New(httpmcp.Configuration{
		Archivists: archivists, Catalog: catalogStore, ArchiveWorkspace: queries, Commands: jobCommands,
		Drives: mcpArchiveIntake, ArchiveRuns: mcpArchiveRuns, ArchiveTranscriptions: mcpArchiveTranscriptions,
	})
	if err != nil {
		return fail(err)
	}
	runtime.mcpHandler = mcpHandler
	handler, err := httpcontrol.New(httpcontrol.Configuration{
		Access: configuration.Access, TrustedProxies: configuration.TrustedProxies,
		Catalog: catalogStore, ArchiveWorkspace: queries, ArchiveWork: workQueries, Notifications: notificationsse.New(&notification.Feed{}),
		Commands: jobCommands, WorkerAPI: workerAPI, Workers: workerCapacity,
		ArchivistAPI: archivistAPI, Archivists: archivists,
		Drives: archiveIntakeWorkspace, ArchiveIntake: archiveIntakeSelection,
		ArchiveRuns: archiveRuns, ArchiveTranscriptions: archiveTranscriptions,
		AllowedOrigins: configuration.AllowedOrigins, Version: version,
	})
	if err != nil {
		return fail(err)
	}
	runtime.handler = handler
	backgroundContext, backgroundCancel := context.WithCancel(ctx)
	runtime.backgroundCancel = backgroundCancel
	runtime.backgroundWait.Add(1)
	go func() {
		defer runtime.backgroundWait.Done()
		runtime.monitorWorkerCompletions(backgroundContext)
	}()
	return runtime, nil
}

func (runtime *Runtime) Handler() http.Handler { return runtime.handler }

func (runtime *Runtime) MCPHandler() http.Handler { return runtime.mcpHandler }

func (runtime *Runtime) Run(ctx context.Context) error {
	controlListener, err := net.Listen("tcp", runtime.configuration.Settings.ListenAddress)
	if err != nil {
		return err
	}
	mcpListener, err := net.Listen("tcp", runtime.configuration.Settings.MCPListenAddress)
	if err != nil {
		controlListener.Close()
		return err
	}
	controlServer := &http.Server{
		Handler: runtime.handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
	}
	mcpServer := &http.Server{
		Handler: runtime.mcpHandler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
	}
	result := make(chan error, 2)
	serve := func(server *http.Server, listener net.Listener) {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		result <- err
	}
	go serve(controlServer, controlListener)
	go serve(mcpServer, mcpListener)
	go runtime.monitorArchiveRunRecalls(ctx)
	shutdown := func() error {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return errors.Join(controlServer.Shutdown(shutdownContext), mcpServer.Shutdown(shutdownContext))
	}
	select {
	case <-ctx.Done():
		if err := shutdown(); err != nil {
			return err
		}
		return errors.Join(<-result, <-result)
	case err := <-result:
		shutdownErr := shutdown()
		return errors.Join(err, shutdownErr, <-result)
	}
}

func (runtime *Runtime) monitorArchiveRunRecalls(ctx context.Context) {
	if runtime == nil || runtime.archivists == nil || runtime.archiveIntakeWorkspace == nil {
		return
	}
	project := func() {
		views, err := runtime.archiveIntakeWorkspace.Projection(ctx)
		if err != nil {
			return
		}
		for _, view := range views {
			if view.ArchiveRunID == "" || view.ArchiveRunRevision == 0 {
				continue
			}
			err := runtime.archivists.RecallArchiveRunProjection(
				ctx, view.ArchiveRunID, view.ArchiveRunRevision, view.ArchiveRunState, view.HoldCount,
			)
			if err != nil && !errors.Is(err, archivistcontrol.ErrArchiveRunNotBound) {
				continue
			}
		}
	}
	project()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			project()
		}
	}
}

func (runtime *Runtime) monitorWorkerCompletions(ctx context.Context) {
	if runtime == nil || runtime.workerExecution == nil {
		return
	}
	reconcile := func() { _ = runtime.workerExecution.ReconcileCompletions(ctx) }
	reconcile()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}

func (runtime *Runtime) Close() error {
	if runtime.backgroundCancel != nil {
		runtime.backgroundCancel()
		runtime.backgroundCancel = nil
		runtime.backgroundWait.Wait()
	}
	var combined error
	if runtime.archiveIntake != nil {
		combined = errors.Join(combined, runtime.archiveIntake.Close())
	}
	if runtime.organization != nil {
		combined = errors.Join(combined, runtime.organization.Close())
	}
	if runtime.archivistRuntime != nil {
		combined = errors.Join(combined, runtime.archivistRuntime.Close())
	}
	if runtime.storage != nil {
		combined = errors.Join(combined, runtime.storage.Close())
	}
	if runtime.archiveWorkspace != nil {
		combined = errors.Join(combined, runtime.archiveWorkspace.Close())
	}
	if runtime.commands != nil {
		combined = errors.Join(combined, runtime.commands.Close())
	}
	if runtime.catalog != nil {
		combined = errors.Join(combined, runtime.catalog.Close())
	}
	return combined
}

type coordinatorJobBus struct {
	service *jobcontrol.Service
}

type executorCommandBus struct {
	executor *command.Executor
}

func (bus executorCommandBus) Execute(envelope command.Envelope) (command.Result, error) {
	return bus.executor.Execute(envelope)
}

func (bus coordinatorJobBus) Execute(envelope command.Envelope) (command.Result, error) {
	return bus.service.Execute(context.Background(), envelope)
}

type localArchiveIntakeAnalyzerFactory struct {
	executable string
	version    string
}

func (factory localArchiveIntakeAnalyzerFactory) New(root *archive.ReadOnlyStorageRoot) (archiveorganization.MediaAnalyzer, error) {
	if root == nil || factory.executable == "" || factory.version == "" {
		return nil, archiveorganization.ErrStartRejected
	}
	return localArchiveIntakeAnalyzer{root: root, executable: factory.executable, version: factory.version}, nil
}

type localArchiveIntakeAnalyzer struct {
	root       *archive.ReadOnlyStorageRoot
	executable string
	version    string
}

func (analyzer localArchiveIntakeAnalyzer) Inspect(ctx context.Context, inspectionID string, original asset.OriginalAsset, relativePath, workerID string) (mediaanalysis.MediaInspection, error) {
	resolved, err := analyzer.root.ExternalProcessPath(relativePath)
	if err != nil {
		return mediaanalysis.MediaInspection{}, err
	}
	inspector, err := ffprobe.NewInspector(analyzer.executable, analyzer.version, workerID, ffprobe.OSRunner{})
	if err != nil {
		return mediaanalysis.MediaInspection{}, err
	}
	return inspector.Inspect(ctx, ffprobe.Request{
		InspectionID: inspectionID, OriginalAssetID: original.ID,
		OriginalChecksum: original.Checksum, ResolvedInputPath: resolved,
	})
}

func sourceWorkerID(definitions []worker.Definition, storageID string) string {
	var ids []string
	for _, definition := range definitions {
		if definition.Enabled && definition.AllowedStorageIDs[storageID] && definition.ProcessingKinds[vocabulary.ProcessingInspect] {
			ids = append(ids, definition.ID)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}
