package httpcontrol_test

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/adapter/httpcontrol"
	"videoarchive/internal/adapter/notificationsse"
	"videoarchive/internal/adapter/sqlitearchiveworkspace"
	"videoarchive/internal/adapter/sqlitecatalog"
	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/application/archiveorganization"
	"videoarchive/internal/application/archivework"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/application/archivistcontrol"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/asset"
	"videoarchive/internal/core/catalog"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/contentreview"
	"videoarchive/internal/core/intake"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/notification"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/security"
	"videoarchive/internal/core/summary"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

const checksumA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// Requirement: CTL-001
func TestControlHTTPServesSQLiteArchiveWorkspaceAndSearchOnlyToAllowedNetwork(t *testing.T) {
	testControlHTTPWorkspace(t, nil)
}

func TestArchiveIntakeWorkspaceProjectsSafeStateAndAcceptsOnlyExplicitCurrentCSRFOverride(t *testing.T) {
	drives := newArchiveIntakeWorkspaceFixture(t)
	handler, err := httpcontrol.New(httpcontrol.Configuration{
		Access: security.AccessConfig{
			Mode: security.AuthDisabled, AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		},
		Catalog: emptyCatalogReader{}, ArchiveWorkspace: emptyArchiveWorkspaceReader{}, Notifications: http.NotFoundHandler(),
		Commands: noopCommander{}, ArchivistAPI: http.NotFoundHandler(), Archivists: chatOnlyArchivistWorkspace{},
		Drives: drives, AllowedOrigins: []string{"http://control.local"}, Version: "v1-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	get := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/archive-intake", nil)
	request.RemoteAddr = "127.0.0.1:45000"
	handler.ServeHTTP(get, request)
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"workspace_drive_id":"VOLUME-1"`) ||
		!strings.Contains(get.Body.String(), `"disposition":"HOLD"`) || strings.Contains(get.Body.String(), `C:\\`) ||
		strings.Contains(strings.ToLower(get.Body.String()), "credential") {
		t.Fatalf("drive projection exposed the wrong boundary: status=%d body=%s", get.Code, get.Body.String())
	}

	postOverride := func(payload string, withCSRF bool) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/archive-intake/VOLUME-1/override", strings.NewReader(payload))
		req.RemoteAddr = "127.0.0.1:45000"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://control.local")
		if withCSRF {
			req.AddCookie(&http.Cookie{Name: "archive_csrf", Value: "csrf-drive"})
			req.Header.Set("X-CSRF-Token", "csrf-drive")
		}
		handler.ServeHTTP(recorder, req)
		return recorder
	}
	denied := postOverride(`{"command_id":"COMMAND-NO-CSRF","decision_id":"DECISION-NO-CSRF","idempotency_key":"NO-CSRF","archive_run_id":"ARCHIVE-RUN-1","folder":"hold","expected_revision":7,"disposition":"COMPILE_SOURCE_SEQUENCE"}`, false)
	if denied.Code != http.StatusForbidden || drives.latest["hold"].Sequence != 1 {
		t.Fatalf("override without CSRF changed a decision: status=%d decision=%+v", denied.Code, drives.latest["hold"])
	}
	stale := postOverride(`{"command_id":"COMMAND-STALE","decision_id":"DECISION-STALE","idempotency_key":"STALE","archive_run_id":"ARCHIVE-RUN-1","folder":"hold","expected_revision":6,"disposition":"COMPILE_SOURCE_SEQUENCE"}`, true)
	if stale.Code != http.StatusConflict || drives.latest["hold"].Sequence != 1 {
		t.Fatalf("stale override changed a decision: status=%d body=%s", stale.Code, stale.Body.String())
	}

	cases := []struct {
		folder      string
		decisionID  string
		disposition string
		target      int
	}{
		{folder: "render", decisionID: "DECISION-6", disposition: "RENDER_RECORDINGS", target: 6_000},
		{folder: "render", decisionID: "DECISION-20", disposition: "RENDER_RECORDINGS", target: 20_000},
		{folder: "hold", decisionID: "DECISION-SOURCE", disposition: "COMPILE_SOURCE_SEQUENCE"},
		{folder: "ready", decisionID: "DECISION-HOLD", disposition: "HOLD"},
	}
	for index, testCase := range cases {
		payload := fmt.Sprintf(`{"command_id":"COMMAND-%d","decision_id":%q,"idempotency_key":"OVERRIDE-%d","archive_run_id":"ARCHIVE-RUN-1","folder":%q,"expected_revision":7,"disposition":%q,"target_video_bitrate_kbps":%d}`,
			index, testCase.decisionID, index, testCase.folder, testCase.disposition, testCase.target)
		response := postOverride(payload, true)
		if response.Code != http.StatusOK || drives.latest[testCase.folder].ID != testCase.decisionID {
			t.Fatalf("explicit override failed: case=%+v status=%d body=%s", testCase, response.Code, response.Body.String())
		}
	}
	sequenceBeforeChat := drives.latest["render"].Sequence
	chat := httptest.NewRecorder()
	chatRequest := httptest.NewRequest(http.MethodPost, "/api/v1/archivist-sessions/SESSION-1/turns", strings.NewReader(`{"turn_id":"TURN-1","request_id":"REQUEST-1","prompt":"render 폴더를 6Mbps로 바꿔"}`))
	chatRequest.RemoteAddr = "127.0.0.1:45000"
	chatRequest.Header.Set("Content-Type", "application/json")
	chatRequest.Header.Set("Origin", "http://control.local")
	chatRequest.Header.Set("X-CSRF-Token", "csrf-drive")
	chatRequest.AddCookie(&http.Cookie{Name: "archive_csrf", Value: "csrf-drive"})
	handler.ServeHTTP(chat, chatRequest)
	if chat.Code != http.StatusCreated || drives.latest["render"].Sequence != sequenceBeforeChat {
		t.Fatalf("chat text changed an organization decision: status=%d decision=%+v", chat.Code, drives.latest["render"])
	}
}

func TestAssistedHoldClassificationActionCardExecutesAsHumanDecision(t *testing.T) {
	drives := newArchiveIntakeWorkspaceFixture(t)
	archivists := &actionCardArchivistWorkspace{envelope: command.Envelope{
		CommandID: "COMMAND-ACTION-HOLD", CommandType: archiveintakeworkspace.ClassifyHoldCommand,
		Actor: command.Actor{Type: vocabulary.ActorHuman, ID: "operator-local"}, RequestedVia: "archive_workspace_action_card",
		TargetID: "hold", ExpectedRevision: 7, IdempotencyKey: "ACTION-HOLD-1", Reason: "confirmed source sequence",
		Parameters: map[string]any{
			"archive_run_id": "ARCHIVE-RUN-1", "workspace_drive_id": "VOLUME-1", "decision_id": "DECISION-ACTION-HOLD",
			"disposition": string(vocabulary.OrganizationCompileSourceSequence), "target_video_bitrate_kbps": 0,
			"basis": []string{"ASSET-A", "ASSET-B"},
		},
	}}
	handler, err := httpcontrol.New(httpcontrol.Configuration{
		Access: security.AccessConfig{
			Mode: security.AuthDisabled, AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		},
		Catalog: emptyCatalogReader{}, ArchiveWorkspace: emptyArchiveWorkspaceReader{}, Notifications: http.NotFoundHandler(),
		Commands: noopCommander{}, ArchivistAPI: http.NotFoundHandler(), Archivists: archivists,
		Drives: drives, AllowedOrigins: []string{"http://control.local"}, Version: "v1-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/action-cards/ACTION-HOLD-1/execute", strings.NewReader(`{}`))
	request.RemoteAddr = "127.0.0.1:45000"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://control.local")
	request.Header.Set("X-CSRF-Token", "csrf-action")
	request.AddCookie(&http.Cookie{Name: "archive_csrf", Value: "csrf-action"})
	handler.ServeHTTP(recorder, request)
	decision := drives.latest["hold"]
	if recorder.Code != http.StatusOK || !archivists.completed || decision.ID != "DECISION-ACTION-HOLD" ||
		decision.Disposition != vocabulary.OrganizationCompileSourceSequence || decision.DecidedByType != vocabulary.ActorHuman {
		t.Fatalf("assisted HOLD ActionCard did not execute as an explicit human decision: %d %s completed=%t decision=%+v", recorder.Code, recorder.Body.String(), archivists.completed, decision)
	}
}

func TestAssistedArchiveRunActionCardBindsStartedRunToOriginTurn(t *testing.T) {
	archivists := &actionCardArchivistWorkspace{envelope: command.Envelope{
		CommandID: "COMMAND-ACTION-ARCHIVE", CommandType: archiveorganization.StartActionCardCommand,
		Actor: command.Actor{Type: vocabulary.ActorHuman, ID: "operator-local"}, RequestedVia: "archive_workspace_action_card",
		TargetID: "ARCHIVE-RUN-ACTION-1", ExpectedRevision: 7, IdempotencyKey: "ACTION-ARCHIVE-1", Reason: "start selected archive run",
		Parameters: map[string]any{
			"session_id": "INTAKE-1", "source_volume_id": "VOLUME-1", "expected_candidate_revision": float64(7),
			"archive_storage_id": "ARCHIVE-1", "input_revision": float64(7), "folder_ids": []string{"recordings"},
			"expected_worker_id": "WORKER-1", "expected_source_storage_id": "SOURCE-1",
			"expected_workspace_drive_id": "VOLUME-1", "selected_by": "operator-local",
		},
	}}
	starter := &fixedArchiveRunStarter{result: archiveorganization.Result{Receipt: organization.ArchiveOrganizationReceipt{
		ID: "ARCHIVE-RUN-ACTION-1", Folders: []organization.CoordinatedFolder{{Folder: "recordings"}},
	}}}
	handler, err := httpcontrol.New(httpcontrol.Configuration{
		Access: security.AccessConfig{
			Mode: security.AuthDisabled, AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		},
		Catalog: emptyCatalogReader{}, ArchiveWorkspace: emptyArchiveWorkspaceReader{}, Notifications: http.NotFoundHandler(),
		Commands: noopCommander{}, ArchivistAPI: http.NotFoundHandler(), Archivists: archivists,
		ArchiveRuns: starter, AllowedOrigins: []string{"http://control.local"}, Version: "v1-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/action-cards/ACTION-ARCHIVE-1/execute", strings.NewReader(`{}`))
	request.RemoteAddr = "127.0.0.1:45000"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://control.local")
	request.Header.Set("X-CSRF-Token", "csrf-action")
	request.AddCookie(&http.Cookie{Name: "archive_csrf", Value: "csrf-action"})
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !archivists.completed || starter.request.ArchiveRunID != "ARCHIVE-RUN-ACTION-1" ||
		archivists.boundCardID != "ACTION-ARCHIVE-1" || archivists.boundArchiveRunID != "ARCHIVE-RUN-ACTION-1" || archivists.boundRevision != 1 {
		t.Fatalf("ActionCard ArchiveRun was not bound to its origin Turn: status=%d body=%s completed=%t request=%+v binding=%s/%s/%d",
			recorder.Code, recorder.Body.String(), archivists.completed, starter.request,
			archivists.boundCardID, archivists.boundArchiveRunID, archivists.boundRevision)
	}
}

// Requirement: DSH-007
func TestArchiveOrganizationWorkspaceMatchesOperatingSystemGolden(t *testing.T) {
	for _, scene := range archiveWorkspaceScenes {
		t.Run(string(scene), func(t *testing.T) {
			testControlHTTPScene(t, scene, func(t *testing.T, handler http.Handler) {
				assertArchiveWorkspaceGolden(t, handler, scene)
			})
		})
	}
}

// Requirement: DSH-004
func TestArchiveWorkspaceJobManagementWithArchivistPaneMatchesOperatingSystemGolden(t *testing.T) {
	testControlHTTPScene(t, sceneComplete, func(t *testing.T, handler http.Handler) {
		assertArchiveWorkspaceGolden(t, handler, sceneJobs)
	})
}

// Requirement: DSH-009
func TestArchiveWorkspaceKeepsLayoutAcrossViewportWidths(t *testing.T) {
	testControlHTTPWorkspace(t, assertArchiveWorkspaceLayout)
}

// Requirement: DSH-010
func TestJobManagementSelectsArchiveDriveAndFolderScopeAndDriveDetailDoesNotCreateWork(t *testing.T) {
	for _, scene := range []archiveWorkspaceScene{scenePreRun, sceneStageOne} {
		t.Run(string(scene), func(t *testing.T) {
			testControlHTTPScene(t, scene, func(t *testing.T, handler http.Handler) {
				assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_selection.py", string(scene))
			})
		})
	}
}

// Requirement: DSH-033
func TestDriveDetailShowsFolderStructureAndVideoRecordsWithoutWorkCreation(t *testing.T) {
	testControlHTTPScene(t, scenePreRun, func(t *testing.T, handler http.Handler) {
		assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_drive_contents.py")
	})
}

// Requirement: DSH-011
func TestArchivistPaneShowsRecallToolsSelectionAndDisablesBusySend(t *testing.T) {
	for _, scene := range []archiveWorkspaceScene{sceneStageOne, sceneTranscription, sceneComplete} {
		t.Run(string(scene), func(t *testing.T) {
			testControlHTTPScene(t, scene, func(t *testing.T, handler http.Handler) {
				assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_archivist.py", string(scene))
			})
		})
	}
}

// Requirement: DSH-013
func TestWorkspaceSeparatesStoredAndCommittedArchiveRecords(t *testing.T) {
	testControlHTTPWorkspace(t, func(t *testing.T, handler http.Handler) {
		assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_archive.py")
	})
}

// Requirement: DSH-012
func TestArchivistAddSettingsShowsOnlyNameHostDetectedCLIAndControlMode(t *testing.T) {
	testControlHTTPWorkspace(t, func(t *testing.T, handler http.Handler) {
		assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_archivist_settings.py")
	})
}

// Requirement: DSH-014
func TestWorkspaceShowsReservationAdjustedWorkerCapacity(t *testing.T) {
	testControlHTTPWorkspace(t, func(t *testing.T, handler http.Handler) {
		assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_capacity.py")
	})
}

// Requirement: DSH-016
func TestWorkspaceCardsOpenPersistentJobDetail(t *testing.T) {
	testControlHTTPScene(t, sceneComplete, func(t *testing.T, handler http.Handler) {
		assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_jobs.py")
	})
}

// Requirement: DSH-017
func TestWorkspaceSearchesCatalogAndShowsRecordWithoutStoragePaths(t *testing.T) {
	testControlHTTPWorkspace(t, func(t *testing.T, handler http.Handler) {
		assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_search.py")
	})
}

// Requirement: DSH-018
func TestWorkspaceShowsTranscriptQualityReviewAttentionWithoutBlockingCompletion(t *testing.T) {
	testControlHTTPScene(t, sceneComplete, func(t *testing.T, handler http.Handler) {
		assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_transcript_quality.py")
	})
}

// Requirement: DSH-019
func TestWorkspaceJobManagementSeparatesActiveAndAttentionJobs(t *testing.T) {
	testControlHTTPWorkspace(t, func(t *testing.T, handler http.Handler) {
		assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_job_management.py")
	})
}

// Requirement: DSH-020
func TestWorkspaceAlertsListProblemsAndAnnounceOnlyNewOnes(t *testing.T) {
	testControlHTTPWorkspace(t, func(t *testing.T, handler http.Handler) {
		assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_fresh.py")
	})
}

// Requirement: DSH-021
func TestWorkspaceLocksDecisionsWhileRefreshIsStale(t *testing.T) {
	testControlHTTPWorkspace(t, func(t *testing.T, handler http.Handler) {
		assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_stale.py")
	})
}

// Requirement: DSH-022
func TestWorkspaceShowsIncrementalFolderDiscoveryAndLocksPartialScope(t *testing.T) {
	testControlHTTPScene(t, scenePreRun, func(t *testing.T, handler http.Handler) {
		assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_discovery.py")
	})
}

// Requirement: DSH-023
func TestWorkspaceShowsArchiveDateAndSourceDerivedNameBeforeOpaqueIDs(t *testing.T) {
	testControlHTTPWorkspace(t, func(t *testing.T, handler http.Handler) {
		assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_archive_name.py")
	})
}

// Requirement: DSH-024
func TestWorkspaceKeepsCompletedPackageResultsVisibleAcrossArchiveRuns(t *testing.T) {
	testControlHTTPScene(t, sceneComplete, func(t *testing.T, handler http.Handler) {
		assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_completed_work.py")
	})
}

// Requirement: DSH-025
func TestWorkspaceRoutesJobListsAndCompletedResultsToPersistentJobDetail(t *testing.T) {
	testControlHTTPScene(t, sceneComplete, func(t *testing.T, handler http.Handler) {
		assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_job_navigation.py")
		assertArchiveWorkspaceGolden(t, handler, sceneJobs)
		assertArchiveWorkspaceGolden(t, handler, sceneJobDetail)
	})
}

// Requirement: DSH-026
func TestWorkspaceCombinesWorkerDriveAndAgentConnections(t *testing.T) {
	testControlHTTPWorkspace(t, func(t *testing.T, handler http.Handler) {
		assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_connections.py")
	})
}

// Requirement: DSH-031
func TestWorkspaceLoadsJobVideoInformationOnlyWhenDisclosureOpens(t *testing.T) {
	testControlHTTPScene(t, sceneComplete, func(t *testing.T, handler http.Handler) {
		assertArchiveWorkspaceScript(t, handler, "check_archive_workspace_job_media.py")
	})
}

func testControlHTTPWorkspace(t *testing.T, check func(*testing.T, http.Handler)) {
	testControlHTTPScene(t, sceneStageOne, check)
}

func testControlHTTPScene(t *testing.T, scene archiveWorkspaceScene, check func(*testing.T, http.Handler)) {
	ctx := context.Background()
	directory := t.TempDir()
	catalogStore, err := sqlitecatalog.Open(filepath.Join(directory, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer catalogStore.Close()
	observedAt := time.Date(2026, time.October, 26, 9, 30, 0, 0, time.UTC)
	observation, err := intake.NewInputFileObservation(intake.InputFileObservation{
		ID: "OBSERVATION-1", StorageID: "source-main", WorkspaceDriveID: "drive-1", RelativePath: "camera/clip.mov",
		OriginalName: "clip.mov", SizeBytes: 42, Checksum: checksumA, ObservedBy: "worker-main", ObservedAt: observedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalogStore.AppendObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	original, err := asset.RegisterOriginalAsset("ASSET-1", observation, "operator-1", observation.ObservedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := catalogStore.AppendOriginal(ctx, original); err != nil {
		t.Fatal(err)
	}
	if err := catalogStore.AppendCatalogRecord(ctx, catalog.CatalogRecord{
		ID: "CATALOG-1", OriginalAssetID: original.ID, Revision: 1, Title: "가을 행사",
		Description: "전체 행사 기록", Keywords: []string{"강연"}, AuthorType: vocabulary.ActorHuman,
		AuthorID: "operator-1", RecordedAt: observation.ObservedAt.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	archiveWorkspaceStore, err := sqlitearchiveworkspace.Open(filepath.Join(directory, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer archiveWorkspaceStore.Close()
	view, err := archiveworkspace.Project(archiveworkspace.ProjectionInput{
		Job: archiveworkspace.Job{
			ID: "JOB-RUNNING", Revision: 3, TemplateName: "occasion-recording", TargetTitle: "가을 행사 편집",
			ControlMode: workflow.AssistedMode, CurrentStepID: "render",
		},
		Steps: []archiveworkspace.StepStatus{{
			ID: "render", State: workflow.Running, Progress: 0.64, ExecutorKind: vocabulary.ExecutorWorker,
			WorkerID: "worker-main", LastEventAt: observation.ObservedAt,
		}},
		Workers: []archiveworkspace.WorkerResource{{
			WorkerID: "worker-main", CPUPercent: 38, GPUPercent: 71,
			AvailableMemoryBytes: 18 << 30, AvailableVRAMBytes: 7 << 30, ScratchFreeBytes: 420 << 30,
			HeavyRunning: 1, AcceptingWork: true, AccessibleStorageIDs: []string{"archive-main", "source-main"},
			HeartbeatAt: observation.ObservedAt,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := archiveWorkspaceStore.Replace(ctx, view); err != nil {
		t.Fatal(err)
	}
	queuedView, err := archiveworkspace.Project(archiveworkspace.ProjectionInput{
		Job: archiveworkspace.Job{
			ID: "JOB-QUEUED", Revision: 1, TemplateName: "archive-copy", TargetTitle: "카메라 카드 보관",
			ControlMode: workflow.AssistedMode, CurrentStepID: "copy",
		},
		Steps: []archiveworkspace.StepStatus{{
			ID: "copy", State: workflow.Queued, Progress: 0, ExecutorKind: vocabulary.ExecutorWorker,
			LastEventAt: observation.ObservedAt.Add(30 * time.Second),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := archiveWorkspaceStore.Replace(ctx, queuedView); err != nil {
		t.Fatal(err)
	}
	waitingView, err := archiveworkspace.Project(archiveworkspace.ProjectionInput{
		Job: archiveworkspace.Job{
			ID: "JOB-REVIEW", Revision: 2, TemplateName: "final-edit", TargetTitle: "인터뷰 요약 검수",
			ControlMode: workflow.ArchivistMode, CurrentStepID: "approve-summary",
		},
		Steps: []archiveworkspace.StepStatus{{
			ID: "approve-summary", State: workflow.Waiting, Progress: 0.82, ExecutorKind: vocabulary.ExecutorHuman,
			WaitReason: vocabulary.WaitApproval, LastEventAt: observation.ObservedAt.Add(time.Minute),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := archiveWorkspaceStore.Replace(ctx, waitingView); err != nil {
		t.Fatal(err)
	}
	completedView, err := archiveworkspace.Project(archiveworkspace.ProjectionInput{
		Job: archiveworkspace.Job{
			ID: "JOB-SOURCE", Revision: 9, TemplateName: "archive-intake-source-sequence", TargetTitle: "source.mp4",
			ControlMode: workflow.ManualMode, CurrentStepID: "organize",
		},
		Steps: []archiveworkspace.StepStatus{{
			ID: "organize", State: workflow.Completed, Progress: 1, ExecutorKind: vocabulary.ExecutorWorker,
			WorkerID: "WORKER-1", LastEventAt: observation.ObservedAt.Add(2 * time.Hour),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := archiveWorkspaceStore.Replace(ctx, completedView); err != nil {
		t.Fatal(err)
	}
	queries, err := archiveworkspace.NewQueryService(archiveWorkspaceStore)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpcontrol.New(httpcontrol.Configuration{
		Access: security.AccessConfig{
			Mode: security.AuthDisabled, AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		},
		Catalog: catalogStore, ArchiveWorkspace: queries, ArchiveWork: newArchiveWorkSceneFixture(scene, observation.ObservedAt),
		Notifications: notificationsse.New(&notification.Feed{}), Version: "v1-test",
		WorkerAPI: http.NotFoundHandler(),
		Workers: fixedWorkerReader{{
			WorkerID: "WORKER-1", State: sceneWorkerState(scene), LastHeartbeat: observation.ObservedAt,
			Metrics: worker.Metrics{
				SnapshotID: 7, ObservedAt: observation.ObservedAt, AverageCPU5m: 38, AverageGPU5m: 71,
				IdleStableFor: 10 * time.Minute, AvailableMemoryBytes: 18 << 30, ScratchFreeBytes: 420 << 30,
				AcceptingWork:        true,
				AccessibleStorageIDs: map[string]bool{"archive-main": true, "source-main": true},
			},
			Capacity: worker.Capacity{
				ReservedMemoryBytes: 4 << 30, ReservedScratchBytes: 25 << 30,
				AvailableMemoryBytes: 14 << 30, ScratchFreeBytes: 395 << 30,
				HeavySlotsUsed: 1, HeavySlotsMaximum: 1, LightSlotsMaximum: 2, AdditionalWorkPossible: true,
				HeavyAdmission: worker.Admission{WaitReason: vocabulary.WaitSlot, Code: "RESOURCE"},
				LightAdmission: worker.Admission{Available: true},
			},
		}, {
			WorkerID: "WORKER-2", State: "OFFLINE", LastHeartbeat: observation.ObservedAt.Add(-50 * time.Minute), Metrics: worker.Metrics{
				AccessibleStorageIDs: map[string]bool{"archive-nas": true, "source-review": true},
			},
			Capacity: worker.Capacity{
				HeavySlotsMaximum: 1, LightSlotsMaximum: 2,
				HeavyAdmission: worker.Admission{Code: "OFFLINE"}, LightAdmission: worker.Admission{Code: "OFFLINE"},
			},
		}},
		ArchivistAPI: http.NotFoundHandler(),
		Archivists:   newArchivistSceneFixture(scene, time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)),
		Drives:       newArchiveIntakeWorkspaceSceneFixture(t, scene),
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, request := range []struct {
		path       string
		wantStatus int
		want       string
	}{
		{path: "/", wantStatus: http.StatusOK, want: "아카이브 작업 공간"},
		{path: "/healthz", wantStatus: http.StatusOK, want: `"version":"v1-test"`},
		{path: "/api/v1/assets?text=가을", wantStatus: http.StatusOK, want: `"title":"가을 행사"`},
		{path: "/api/v1/jobs", wantStatus: http.StatusOK, want: `"job_id":"JOB-RUNNING"`},
	} {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, request.path, nil)
		req.RemoteAddr = "127.0.0.1:45000"
		handler.ServeHTTP(recorder, req)
		if recorder.Code != request.wantStatus || !strings.Contains(recorder.Body.String(), request.want) {
			t.Fatalf("GET %s status=%d body=%s", request.path, recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), "camera/clip.mov") {
			t.Fatalf("GET %s exposed a storage path: %s", request.path, recorder.Body.String())
		}
	}

	denied := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/assets", nil)
	request.RemoteAddr = "203.0.113.7:45000"
	request.Header.Set("X-Forwarded-For", "127.0.0.1")
	handler.ServeHTTP(denied, request)
	payload, _ := io.ReadAll(denied.Result().Body)
	if denied.Code != http.StatusForbidden || !strings.Contains(string(payload), "NETWORK_DENIED") {
		t.Fatalf("untrusted network reached control API: status=%d body=%s", denied.Code, payload)
	}

	if check != nil {
		check(t, handler)
	}
}

func assertArchiveWorkspaceLayout(t *testing.T, handler http.Handler) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()

	python := os.Getenv("ARCHIVE_WORKSPACE_GOLDEN_PYTHON")
	if python == "" {
		python = "python3"
	}
	renderContext, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(renderContext, python, filepath.Join("testdata", "check_archive_workspace_layout.py"), server.URL)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("check Archive Workspace layout with Playwright: %v\n%s\nrun `make visual-deps` before the test", err, output)
	}
}

func assertArchiveWorkspaceScript(t *testing.T, handler http.Handler, script string, arguments ...string) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()
	python := os.Getenv("ARCHIVE_WORKSPACE_GOLDEN_PYTHON")
	if python == "" {
		python = "python3"
	}
	renderContext, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(renderContext, python, append([]string{filepath.Join("testdata", script), server.URL}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("check Archive Workspace surface with %s: %v\n%s\nrun `make visual-deps` before the test", script, err, output)
	}
}

type fixedWorkerReader []worker.Status

func (reader fixedWorkerReader) Statuses(context.Context) ([]worker.Status, error) {
	return append([]worker.Status(nil), reader...), nil
}

type emptyCatalogReader struct{}

func (emptyCatalogReader) Search(context.Context, catalog.SearchQuery) ([]catalog.SearchResult, error) {
	return nil, nil
}

func (emptyCatalogReader) SearchTranscripts(context.Context, string) ([]transcription.SearchHit, error) {
	return nil, nil
}

func (emptyCatalogReader) SummariesByOriginal(context.Context, string) ([]summary.Summary, error) {
	return nil, nil
}

func (emptyCatalogReader) ContentReviews(context.Context, string, uint64) ([]contentreview.Record, error) {
	return nil, nil
}

type emptyArchiveWorkspaceReader struct{}

func (emptyArchiveWorkspaceReader) Inbox(context.Context) ([]archiveworkspace.JobSummary, error) {
	return nil, nil
}

func (emptyArchiveWorkspaceReader) Detail(context.Context, string) (archiveworkspace.JobView, error) {
	return archiveworkspace.JobView{}, archiveworkspace.ErrJobNotFound
}

type noopCommander struct{}

func (noopCommander) Execute(context.Context, command.Envelope) (command.Result, error) {
	return command.Result{}, errors.New("unused command")
}

type fixedArchivistWorkspace struct {
	statuses    []archivistruntime.HostStatus
	detected    map[string][]string
	projections []archivistcontrol.SessionProjection
}

type chatOnlyArchivistWorkspace struct{ fixedArchivistWorkspace }

type actionCardArchivistWorkspace struct {
	chatOnlyArchivistWorkspace
	envelope          command.Envelope
	completed         bool
	boundCardID       string
	boundArchiveRunID string
	boundRevision     uint64
}

func (workspace *actionCardArchivistWorkspace) PrepareActionCard(context.Context, string, string) (command.Envelope, error) {
	return workspace.envelope, nil
}

func (workspace *actionCardArchivistWorkspace) CompleteActionCard(context.Context, string) error {
	workspace.completed = true
	return nil
}

func (*actionCardArchivistWorkspace) ReleaseActionCard(context.Context, string) error { return nil }

func (workspace *actionCardArchivistWorkspace) BindActionCardArchiveRun(_ context.Context, cardID, archiveRunID string, revision uint64) error {
	workspace.boundCardID = cardID
	workspace.boundArchiveRunID = archiveRunID
	workspace.boundRevision = revision
	return nil
}

type fixedArchiveRunStarter struct {
	request archiveorganization.StartRequest
	result  archiveorganization.Result
}

func (starter *fixedArchiveRunStarter) Start(_ context.Context, request archiveorganization.StartRequest) (archiveorganization.Result, error) {
	starter.request = request
	return starter.result, nil
}

func (chatOnlyArchivistWorkspace) SubmitTurnWithDriveSelection(_ context.Context, sessionID, turnID, requestID, prompt string, selection *archivistruntime.DriveSelection) (archivistcontrol.TurnRecord, error) {
	turn, err := archivistruntime.NewTurnWithDriveSelection(turnID, sessionID, requestID, selection)
	if err != nil {
		return archivistcontrol.TurnRecord{}, err
	}
	return archivistcontrol.TurnRecord{Turn: turn, Prompt: prompt, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}, nil
}

func sceneWorkerState(scene archiveWorkspaceScene) string {
	if scene == sceneStageOne {
		return "BUSY"
	}
	return "ONLINE"
}

func newArchivistSceneFixture(scene archiveWorkspaceScene, at time.Time) fixedArchivistWorkspace {
	selection := &archivistruntime.DriveSelection{
		ArchiveRunID: "ARCHIVE-RUN-1", ArchiveRunRevision: 1,
		WorkerID: "WORKER-1", WorkspaceDriveID: "VOLUME-1", IntakeSessionID: "INTAKE-1", SourceVolumeID: "VOLUME-1",
		SourceStorageID: "source-main", ArchiveStorageID: "archive-main", CandidateRevision: 7, FolderDiscoveryRevision: 7,
		FolderIDs: sceneFolderIDs, SelectedBy: "operator-1",
	}
	turn := func(id, state, prompt string, actor vocabulary.ActorType, minute int, summary string, calls ...archivistcontrol.ToolCallProjection) archivistcontrol.TurnProjection {
		projection := archivistcontrol.TurnProjection{
			ID: id, RequestID: "REQUEST-" + id, State: state, Prompt: prompt, SubmittedBy: actor,
			CreatedAt: at.Add(time.Duration(minute) * time.Minute), UpdatedAt: at.Add(time.Duration(minute+1) * time.Minute), ToolCalls: calls,
		}
		if summary != "" {
			projection.FinalResponse = &archivistruntime.FinalResponse{Summary: summary, ResultIDs: []string{"ARCHIVE-RUN-1"}}
			projection.Entries = []archivistruntime.TurnEntry{{
				Cursor: 3, Kind: archivistruntime.TurnEntryProgress, Summary: "구조화된 업무 상태 확인", CreatedAt: at.Add(time.Duration(minute)*time.Minute + 30*time.Second),
			}}
		}
		return projection
	}
	attachWork := func(turn *archivistcontrol.TurnProjection, trigger archivistruntime.ArchiveRunWorkTrigger) {
		turn.WorkContext = &archivistruntime.ArchiveRunWorkContext{
			WorkID: "ARCHIVE-RUN-1", OriginTurnID: "TURN-ARCHIVE-1", Trigger: trigger,
		}
	}
	call := func(id, tool, state string) archivistcontrol.ToolCallProjection {
		return archivistcontrol.ToolCallProjection{ID: id, ToolName: tool, TargetID: "ARCHIVE-RUN-1", State: state, UpdatedAt: at}
	}
	start := turn("TURN-ARCHIVE-1", "COMPLETED", "이 하드를 분류하고 렌더링하고 기록 파일 생성 후 내용 텍스트파일까지 분류를 함께하여 저장해둬",
		vocabulary.ActorHuman, 0, "선택한 8 폴더의 분류·렌더 시작을 요청했습니다.",
		call("CALL-SELECT-1", "archive.intake.selection.get.v1", "COMPLETED"), call("CALL-ARCHIVE-RUN-1", "archive.run.start.v1", "ACTION_CARD"))
	start.DriveSelection = selection
	attachWork(&start, archivistruntime.WorkArchiveRunStarted)
	startCard := archivistcontrol.ActionCardProjection{
		ID: "ACTION-CALL-ARCHIVE-RUN-1", ArchivistTurnID: start.ID, CommandType: "archive_run.start", TargetID: "ARCHIVE-RUN-1",
		State: "COMPLETED", ApprovalRequired: false, Parameters: map[string]any{
			"expected_workspace_drive_id": "VOLUME-1", "archive_storage_id": "archive-main", "folder_ids": sceneFolderIDs,
		},
	}
	sttCard := archivistcontrol.ActionCardProjection{
		ID: "ACTION-CALL-STT-1", ArchivistTurnID: "SYSTEM-RECALL-STAGE-1", CommandType: "archive_run.transcription.start",
		TargetID: "ARCHIVE-RUN-1", ExpectedRevision: 1, State: "PENDING", ApprovalRequired: false,
		Parameters: map[string]any{"expected_archive_run_revision": 1},
	}
	holdRecall := turn("SYSTEM-RECALL-HOLD-1", "COMPLETED", "보류 발생 · hold", vocabulary.ActorSystem, 30,
		"hold 폴더 3개 영상이 보류되었습니다. 판정이 필요합니다.", call("CALL-STATUS-1", "archive.run.status.v1", "COMPLETED"))
	attachWork(&holdRecall, archivistruntime.WorkNewHold)
	stageRecall := turn("SYSTEM-RECALL-STAGE-1", "COMPLETED", "1단계 완료", vocabulary.ActorSystem, 40,
		"1단계가 끝났습니다. STT 대상 4 폴더의 시작을 요청했습니다.",
		call("CALL-STATUS-2", "archive.run.status.v1", "COMPLETED"), call("CALL-STT-1", "archive.run.transcription.start.v1", "ACTION_CARD"))
	attachWork(&stageRecall, archivistruntime.WorkStageOneComplete)
	var turns []archivistcontrol.TurnProjection
	cards := []archivistcontrol.ActionCardProjection{startCard}
	switch scene {
	case scenePreRun:
		turns, cards = nil, nil
	case sceneStageOne:
		holdRecall.State, holdRecall.FinalResponse, holdRecall.ToolCalls, holdRecall.Entries = string(vocabulary.ArchivistTurnQueued), nil, nil, nil
		turns = []archivistcontrol.TurnProjection{start, holdRecall}
	case sceneTranscription:
		turns = []archivistcontrol.TurnProjection{start, holdRecall, stageRecall}
		cards = append(cards, sttCard)
	default:
		sttCard.State = "COMPLETED"
		completeRecall := turn("SYSTEM-RECALL-COMPLETE-1", "COMPLETED", "전체 완료", vocabulary.ActorSystem, 180,
			"완료했습니다. 8 폴더 중 보관됨 5 · 확정됨 1 · 보류 1 · 실패 1입니다. STT 4 폴더와 CSV 795행을 archive-main에 기록했습니다. hold 폴더는 판정이 필요합니다.",
			call("CALL-STATUS-3", "archive.run.status.v1", "COMPLETED"))
		attachWork(&completeRecall, archivistruntime.WorkArchiveRunComplete)
		turns = []archivistcontrol.TurnProjection{start, holdRecall, stageRecall, completeRecall}
		cards = append(cards, sttCard)
	}
	return fixedArchivistWorkspace{
		statuses: []archivistruntime.HostStatus{
			{ArchivistHostID: "archivist-windows", State: vocabulary.ArchivistHostOnline, LastHeartbeat: at},
			{ArchivistHostID: "archivist-mac", State: vocabulary.ArchivistHostOffline, LastHeartbeat: at.Add(-2 * time.Hour)},
		},
		detected: map[string][]string{"archivist-windows": {"claude", "codex"}},
		projections: []archivistcontrol.SessionProjection{{
			Session: archivistruntime.Session{
				ID: "SESSION-ARCHIVE-1", Name: "Archive Archivist", ArchivistHostID: "archivist-windows", Backend: vocabulary.RuntimeEmbeddedCore,
				CLIID: "codex", WorkspaceRootID: "video-project", ControlMode: workflow.AssistedMode,
			},
			CreatedAt: at, Turns: turns, ActionCards: cards,
		}},
	}
}

type archiveWorkspaceScene string

const (
	scenePreRun        archiveWorkspaceScene = "pre-run"
	sceneStageOne      archiveWorkspaceScene = "stage-one"
	sceneTranscription archiveWorkspaceScene = "stt-ready"
	sceneComplete      archiveWorkspaceScene = "complete"
	sceneJobs          archiveWorkspaceScene = "jobs"
	sceneJobDetail     archiveWorkspaceScene = "job-detail"
)

var archiveWorkspaceScenes = []archiveWorkspaceScene{scenePreRun, sceneStageOne, sceneTranscription, sceneComplete}

var sceneFolderIDs = []string{"broken", "event-b", "hold", "intro", "ready", "render", "source", "source-b"}

type archiveWorkSceneFixture struct {
	summaries []archivework.Summary
	details   map[string]archivework.View
}

func newArchiveWorkSceneFixture(scene archiveWorkspaceScene, at time.Time) archiveWorkSceneFixture {
	standalone := func(id, title string, state archivework.State, minute int) archivework.Summary {
		return archivework.Summary{
			WorkID: id, Kind: archivework.StandaloneJob, State: state, CreatedAt: at.Add(time.Duration(minute) * time.Minute),
			LastEventAt: at.Add(time.Duration(minute) * time.Minute), WorkerID: "WORKER-1", ChildJobCount: 1, Title: title,
		}
	}
	values := []archivework.Summary{
		standalone("JOB-RUNNING", "행사 하이라이트", archivework.StateActive, 1),
		standalone("JOB-QUEUED", "외장 드라이브 복사", archivework.StateQueued, 2),
		standalone("JOB-REVIEW", "인터뷰 요약 검수", archivework.StateAttention, 3),
	}
	fixture := archiveWorkSceneFixture{details: make(map[string]archivework.View)}
	if scene == scenePreRun {
		values = append(values, standalone("JOB-SOURCE", "source.mp4", archivework.StateCompleted, 4))
	} else {
		runState := archivework.StateActive
		if scene == sceneTranscription {
			runState = archivework.StateAttention
		} else if scene == sceneComplete || scene == sceneJobs || scene == sceneJobDetail {
			runState = archivework.StateCompleted
		}
		run := archivework.Summary{
			WorkID: "ARCHIVE-RUN-1", Kind: archivework.ArchiveRunWork, State: runState,
			CreatedAt: at, LastEventAt: at.Add(3 * time.Hour), WorkerID: "WORKER-1",
			SourceVolumeID: "VOLUME-1", SourceStorageID: "source-main", ArchiveStorageID: "archive-main",
			FolderCount: len(sceneFolderIDs), VideoCount: 795, HoldCount: 1, ChildJobCount: 1,
		}
		switch runState {
		case archivework.StateCompleted:
			run.Progress = 1
		case archivework.StateAttention:
			run.Progress = 0.9
		default:
			run.Progress = 0.54
		}
		sourceJob := archiveworkspace.JobSummary{
			JobID: "JOB-SOURCE", Revision: 9, TemplateName: "archive-intake-source-sequence", TargetTitle: "source.mp4",
			ControlMode: workflow.ManualMode, CurrentStepID: "organize", StepState: workflow.Completed, Progress: 1,
			ExecutorKind: vocabulary.ExecutorWorker, WorkerID: "WORKER-1", LastEventAt: at.Add(2 * time.Hour),
		}
		view := archivework.View{
			Summary: run, FolderIDs: append([]string(nil), sceneFolderIDs...),
			Children: []archivework.ChildJob{{Role: archivework.ChildFolder, Folder: "source", Job: sourceJob}},
		}
		if runState == archivework.StateCompleted {
			view.CompletedWork = []archiveintakeworkspace.CompletedWorkView{{
				ArchiveRunID: run.WorkID, Folder: "source", JobID: sourceJob.JobID,
				Disposition: vocabulary.OrganizationCompileSourceSequence, ProfileReference: "h264-1080p-20mbps",
				SourceVideoCount: 86, ArchiveStorageID: run.ArchiveStorageID, ArchivePackageID: "VP-20261030-SOURCE",
				ArchiveDate: "2026-10-30", ArchiveDisplayName: "2026-10-30 · source · source.mp4",
				ArchiveVideoFileName: "source.mp4", ArchiveRelativePath: "packages/2026/10/30/VP-20261030-SOURCE/revisions/000001",
				ArchiveState: archive.PackageCommitted, CompletedAt: at.Add(2 * time.Hour),
			}}
			run.ResultCount = len(view.CompletedWork)
			view.Summary = run
		}
		values = append(values, run)
		fixture.details[run.WorkID] = view
	}
	for _, summary := range values {
		if summary.Kind != archivework.StandaloneJob {
			continue
		}
		fixture.details[summary.WorkID] = archivework.View{Summary: summary}
	}
	fixture.summaries = values
	return fixture
}

func (fixture archiveWorkSceneFixture) Inbox(context.Context) ([]archivework.Summary, error) {
	return append([]archivework.Summary(nil), fixture.summaries...), nil
}

func (fixture archiveWorkSceneFixture) Detail(_ context.Context, workID string) (archivework.View, error) {
	view, exists := fixture.details[workID]
	if !exists {
		return archivework.View{}, archivework.ErrWorkNotFound
	}
	return view, nil
}

func (fixture archiveWorkSceneFixture) Media(_ context.Context, jobID string) ([]archivework.VideoInfo, error) {
	if jobID != "JOB-SOURCE" {
		for _, detail := range fixture.details {
			for _, child := range detail.Children {
				if child.Job.JobID == jobID {
					return []archivework.VideoInfo{}, nil
				}
			}
		}
		return nil, archivework.ErrJobNotFound
	}
	at := time.Date(2026, 10, 30, 8, 30, 0, 0, time.UTC)
	return []archivework.VideoInfo{
		{
			OriginalAssetID: "ASSET-C0001", OriginalName: "C0001.MP4", RelativePath: "source/CLIP/C0001.MP4",
			Checksum: strings.Repeat("a", 64), SourceCreatedAt: at, DurationMillis: 61_200, SizeBytes: 154_000_000,
			Container: "mp4", BitRateBPS: 20_100_000, VideoCodec: "h264", VideoProfile: "High",
			Width: 1920, Height: 1080, VideoBitRateBPS: 19_800_000, AudioCodec: "aac", AudioBitRateBPS: 256_000,
			AudioSampleRateHz: 48_000, AudioChannels: 2, ProfileReference: "source-1080p-20mbps@v1",
		},
		{
			OriginalAssetID: "ASSET-C0002", OriginalName: "C0002.MP4", RelativePath: "source/CLIP/C0002.MP4",
			Checksum: strings.Repeat("b", 64), SourceCreatedAt: at.Add(2 * time.Minute), DurationMillis: 58_800, SizeBytes: 148_000_000,
			Container: "mp4", BitRateBPS: 20_100_000, VideoCodec: "h264", VideoProfile: "High",
			Width: 1920, Height: 1080, VideoBitRateBPS: 19_800_000, AudioCodec: "aac", AudioBitRateBPS: 256_000,
			AudioSampleRateHz: 48_000, AudioChannels: 2, ProfileReference: "source-1080p-20mbps@v1",
		},
	}, nil
}

type archiveIntakeWorkspaceFixture struct {
	scene    archiveWorkspaceScene
	latest   map[string]organization.OrganizationDecision
	receipts map[string]organization.OrganizationDecision
}

func newArchiveIntakeWorkspaceFixture(t *testing.T) *archiveIntakeWorkspaceFixture {
	return newArchiveIntakeWorkspaceSceneFixture(t, sceneStageOne)
}

func newArchiveIntakeWorkspaceSceneFixture(t *testing.T, scene archiveWorkspaceScene) *archiveIntakeWorkspaceFixture {
	t.Helper()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	input := func(assetID, relativePath string, duration, bitRate int64) organization.Input {
		return organization.Input{
			OriginalAssetID: assetID, RelativePath: relativePath,
			Inspection: mediaanalysis.MediaInspection{
				ID: "INSPECTION-" + assetID, OriginalAssetID: assetID,
				OriginalChecksum: checksumA, Containers: []string{"mp4"}, DurationMillis: duration, SizeBytes: 1_000,
				VideoStreams: []mediaanalysis.VideoStream{{
					Index: 0, Codec: "h264", Width: 1920, Height: 1080,
					FrameRate: mediaanalysis.Rational{Numerator: 30, Denominator: 1}, BitRate: bitRate,
				}},
				AudioStreams:     []mediaanalysis.AudioStream{{Index: 1, Codec: "aac", SampleRateHz: 48_000, Channels: 2}},
				ProcessorVersion: "fixture", ObservedBy: "WORKER-1", InspectedAt: now,
			},
		}
	}
	ready, err := organization.Classify(organization.AutoRequest{
		DecisionID: "DECISION-READY", WorkspaceDriveID: "VOLUME-1", Folder: "ready", InputRevision: 7,
		Inputs: []organization.Input{input("ASSET-READY", "ready/event.mp4", 60_000, 6_000_000)},
		Policy: organization.DefaultPolicy(), DecidedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	render, err := organization.Classify(organization.AutoRequest{
		DecisionID: "DECISION-RENDER", WorkspaceDriveID: "VOLUME-1", Folder: "render", InputRevision: 7,
		Inputs: []organization.Input{input("ASSET-RENDER", "render/master.mov", 600_000, 40_000_000)},
		Policy: organization.DefaultPolicy(), DecidedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	hold, err := organization.Classify(organization.AutoRequest{
		DecisionID: "DECISION-HOLD", WorkspaceDriveID: "VOLUME-1", Folder: "hold", InputRevision: 7,
		Inputs: []organization.Input{
			input("ASSET-HOLD-1", "hold/a.mov", 10_000, 40_000_000),
			input("ASSET-HOLD-2", "hold/b.mov", 12_000, 40_000_000),
		},
		Policy: organization.DefaultPolicy(), DecidedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := organization.Classify(organization.AutoRequest{
		DecisionID: "DECISION-SOURCE", WorkspaceDriveID: "VOLUME-1", Folder: "source", InputRevision: 7,
		Inputs: []organization.Input{
			input("ASSET-SOURCE-1", "source/C0001.MP4", 10_000, 20_000_000),
			input("ASSET-SOURCE-2", "source/C0002.MP4", 12_000, 20_000_000),
		},
		Policy: organization.DefaultPolicy(), DecidedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &archiveIntakeWorkspaceFixture{
		scene:    scene,
		latest:   map[string]organization.OrganizationDecision{"ready": ready, "render": render, "hold": hold, "source": source},
		receipts: make(map[string]organization.OrganizationDecision),
	}
}

func (workspace *archiveIntakeWorkspaceFixture) Projection(context.Context) ([]archiveintakeworkspace.DriveView, error) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	done := func(ids ...string) []archiveintakeworkspace.StageView {
		stages := make([]archiveintakeworkspace.StageView, 0, len(ids))
		for _, id := range ids {
			stages = append(stages, archiveintakeworkspace.StageView{ID: id, State: workflow.Completed})
		}
		return stages
	}
	stage := func(id string, state workflow.StepState, reason vocabulary.WaitReason) archiveintakeworkspace.StageView {
		return archiveintakeworkspace.StageView{ID: id, State: state, WaitReason: reason}
	}
	stageTwo := done("audio", "transcript", "subtitle", "transcript_text", "add")
	folder := func(name string, disposition vocabulary.OrganizationDisposition, videos int, gigabytes uint64, current string, state workflow.StepState, stages ...archiveintakeworkspace.StageView) archiveintakeworkspace.FolderView {
		for index := range stages {
			stages[index].TotalCount = videos
			if stages[index].State == workflow.Completed {
				stages[index].CompletedCount = videos
			}
		}
		view := archiveintakeworkspace.FolderView{
			Folder: name, DecisionID: "DECISION-" + strings.ToUpper(name), InputRevision: 7, DecisionSequence: 1,
			Disposition: disposition, VideoCount: videos, EstimatedOutputBytes: gigabytes << 30,
			JobID: "JOB-" + strings.ToUpper(name), CurrentStepID: current, StepState: state, Stages: stages,
		}
		if decision, ok := workspace.latest[name]; ok && decision.Disposition == disposition {
			view.DecisionID, view.DecisionSequence, view.HoldReasons = decision.ID, decision.Sequence, decision.HoldReasons
			if disposition == vocabulary.OrganizationHold {
				for _, option := range organization.AvailableWorkOptions(decision) {
					view.WorkOptions = append(view.WorkOptions, archiveintakeworkspace.WorkOptionView{
						Disposition: option.Disposition, TargetVideoBitRateKbps: option.TargetVideoBitRateKbps,
					})
				}
			}
		}
		if disposition == vocabulary.OrganizationHold {
			view.JobID = ""
		}
		return view
	}
	stored := func(view archiveintakeworkspace.FolderView, state archive.PackageState) archiveintakeworkspace.FolderView {
		view.ArchivePackageID, view.ArchiveState = "VP-20261030-"+strings.ToUpper(view.Folder), state
		view.ArchiveDate = "2026-10-30"
		view.ArchiveVideoFileName = view.Folder + ".mp4"
		view.ArchiveDisplayName = view.ArchiveDate + " · " + view.Folder + " · " + view.ArchiveVideoFileName
		view.CanAddTranscription = state == archive.PackageStored &&
			(view.Disposition == vocabulary.OrganizationAsIs || view.Disposition == vocabulary.OrganizationRenderRecordings)
		return view
	}
	transcribed := func(view archiveintakeworkspace.FolderView) archiveintakeworkspace.FolderView {
		view.TranscriptionJobID, view.CurrentStepID = "STT-"+strings.ToUpper(view.Folder), "archive_catalog"
		view.Stages = append(view.Stages, stageTwo...)
		view.CanAddTranscription = false
		return view
	}
	renderDone := func(name string, videos int, gigabytes uint64) archiveintakeworkspace.FolderView {
		return stored(folder(name, vocabulary.OrganizationRenderRecordings, videos, gigabytes, "organize", workflow.Completed,
			done("classification", "render", "qc", "archive")...), archive.PackageStored)
	}
	broken := folder("broken", vocabulary.OrganizationRenderRecordings, 2, 1, "render", workflow.Failed,
		stage("classification", workflow.Completed, ""), stage("render", workflow.Failed, ""))
	hold := folder("hold", vocabulary.OrganizationHold, 3, 0, "resolve_classification", workflow.Waiting,
		stage("classification", workflow.Waiting, workflow.WaitInput))
	ready := stored(folder("ready", vocabulary.OrganizationAsIs, 105, 42, "organize", workflow.Completed,
		done("classification", "qc", "archive")...), archive.PackageStored)
	source := stored(folder("source", vocabulary.OrganizationCompileSourceSequence, 86, 28, "organize", workflow.Completed,
		done("classification", "markers", "render", "qc", "archive")...), archive.PackageCommitted)
	sourceB := stored(folder("source-b", vocabulary.OrganizationCompileSourceSequence, 322, 30, "organize", workflow.Completed,
		done("classification", "markers", "render", "qc", "archive")...), archive.PackageStored)
	folders := []archiveintakeworkspace.FolderView{broken, renderDone("event-b", 64, 12), hold, renderDone("intro", 12, 2), ready, renderDone("render", 201, 96), source, sourceB}
	drive := archiveintakeworkspace.DriveView{
		WorkspaceDriveID: "VOLUME-1", ConnectionState: vocabulary.VolumeConnected,
		ConnectionObservedAt: now, ConnectionWorkerID: "WORKER-1",
		ArchiveRunID: "ARCHIVE-RUN-1", ArchiveRunRevision: 1,
		CandidateRevision: 7, FolderDiscoveryRevision: 7,
		SessionID: "INTAKE-1", SelectionSessionID: "INTAKE-1", WorkerID: "WORKER-1", SourceVolumeID: "VOLUME-1", SourceStorageID: "source-main",
		ArchiveStorageID: "archive-main", State: "HAS_HOLD", InputRevision: 7, FolderDiscoveryState: vocabulary.FolderDiscoveryReady, FileCount: 795, FolderCount: len(folders),
		HoldCount: 1, CatalogRowCount: 795,
		StorageCandidates: []archiveintakeworkspace.StorageView{
			{StorageID: "source-main", Role: vocabulary.StorageSourceMedia, WorkspaceDriveID: "VOLUME-1", AccessKind: vocabulary.StorageAccessAttachedVolume, Accessible: true, CapacityBytes: 1 << 40, FreeBytes: 420 << 30},
			{StorageID: "archive-main", Role: vocabulary.StorageArchivePrimary, WorkspaceDriveID: "ARCHIVE-VOLUME-1", AccessKind: vocabulary.StorageAccessLocal, Accessible: true, CapacityBytes: 2 << 40, FreeBytes: 860 << 30},
			{StorageID: "archive-nas", Role: vocabulary.StorageArchivePrimary, WorkspaceDriveID: "ARCHIVE-NETWORK-VOLUME-1", AccessKind: vocabulary.StorageAccessMountedNetwork, Accessible: true, CapacityBytes: 8 << 40, FreeBytes: 3 << 40},
			{StorageID: "archive-new-drive", Role: vocabulary.StorageArchivePrimary, WorkspaceDriveID: "ARCHIVE-VOLUME-2", AccessKind: vocabulary.StorageAccessAttachedVolume, Accessible: true, Model: "TOSHIBA EXTERNAL", Label: "새 아카이브", FileSystem: "NTFS", CapacityBytes: 4 << 40, FreeBytes: 3 << 40},
			{StorageID: "archive-ftp", Role: vocabulary.StorageArchivePrimary, AccessKind: vocabulary.StorageAccessFTP, Accessible: false},
			{StorageID: "archive-sftp", Role: vocabulary.StorageArchivePrimary, AccessKind: vocabulary.StorageAccessSFTP, Accessible: false},
		},
	}
	if workspace.scene == scenePreRun {
		folders = []archiveintakeworkspace.FolderView{
			{Folder: "event-a", InputRevision: 7, VideoCount: 105, EstimatedOutputBytes: 42 << 30},
			{Folder: "event-b", InputRevision: 7, VideoCount: 64, EstimatedOutputBytes: 12 << 30},
			{Folder: "source-a", InputRevision: 7, VideoCount: 86, EstimatedOutputBytes: 28 << 30},
			{Folder: "source-b", InputRevision: 7, VideoCount: 322, EstimatedOutputBytes: 30 << 30},
		}
		// The person already fixed this volume to an intake session; no run yet.
		drive.ArchiveRunID, drive.ArchiveRunRevision, drive.ArchiveStorageID = "", 0, ""
		drive.State, drive.ArchiveRunState, drive.FileCount, drive.FolderCount = "UNCLASSIFIED", "", 577, len(folders)
		drive.StageOneRemaining, drive.StageTwoRemaining, drive.HoldCount, drive.EligibleTranscriptionCount = 0, 0, 0, 0
		drive.CatalogRowCount, drive.CatalogUpdatedAt = 0, time.Time{}
	}
	switch workspace.scene {
	case scenePreRun:
	case sceneStageOne:
		folders[1] = folder("event-b", vocabulary.OrganizationRenderRecordings, 64, 12, "render", workflow.Waiting,
			stage("classification", workflow.Completed, ""), stage("render", workflow.Waiting, workflow.WaitMemory),
			stage("qc", workflow.Queued, ""), stage("archive", workflow.Queued, ""))
		folders[1].Stages[1].CompletedCount = 18
		folders[3] = folder("intro", vocabulary.OrganizationRenderRecordings, 12, 2, "job_pending", workflow.Queued,
			stage("classification", workflow.Completed, ""), stage("job_pending", workflow.Queued, ""))
		folders[3].JobID = ""
		folders[5] = folder("render", vocabulary.OrganizationRenderRecordings, 201, 96, "render", workflow.Running,
			stage("classification", workflow.Completed, ""), stage("render", workflow.Running, ""),
			stage("qc", workflow.Queued, ""), stage("archive", workflow.Queued, ""))
		folders[5].Stages[1].CompletedCount = 127
		folders[7] = folder("source-b", vocabulary.OrganizationCompileSourceSequence, 322, 30, "organize", workflow.Waiting,
			append(done("classification", "markers", "render", "qc"), stage("archive", workflow.Waiting, workflow.WaitDevice))...)
		folders[7].Stages[4].WaitTargetStorageID = "archive-main"
		drive.State, drive.ArchiveRunState, drive.StageOneRemaining, drive.EligibleTranscriptionCount = "IN_PROGRESS", "STAGE_ONE_RUNNING", 4, 1
		drive.CatalogUpdatedAt = now
	case sceneTranscription:
		drive.ArchiveRunState, drive.EligibleTranscriptionCount, drive.CatalogUpdatedAt = "TRANSCRIPTION_READY", 4, now.Add(40*time.Minute)
	default:
		for _, index := range []int{1, 3, 4, 5} {
			folders[index] = transcribed(folders[index])
		}
		folders[4].TranscriptQualities = []archiveintakeworkspace.TranscriptQualityView{{
			TranscriptID: "TRANSCRIPT-ASSET-READY", OriginalAssetID: "ASSET-READY",
			State:          transcription.QualityReviewRecommended,
			Reasons:        []transcription.QualityReason{transcription.QualityReasonSegmentTooLong, transcription.QualityReasonLowTextDensity},
			DurationMillis: 19_120, SegmentCount: 1, TextCharacterCount: 9,
		}}
		drive.ArchiveRunState, drive.EligibleTranscriptionCount, drive.CatalogUpdatedAt = "COMPLETED", 4, now.Add(3*time.Hour)
		for index, completed := range folders {
			if !completed.ArchiveState.Valid() {
				continue
			}
			drive.CompletedWork = append(drive.CompletedWork, archiveintakeworkspace.CompletedWorkView{
				ArchiveRunID: "ARCHIVE-RUN-1", Folder: completed.Folder, JobID: completed.JobID,
				Disposition: completed.Disposition, ProfileReference: completed.ProfileReference,
				SourceVideoCount: completed.VideoCount, ArchiveStorageID: "archive-main",
				ArchivePackageID: completed.ArchivePackageID, ArchiveDate: completed.ArchiveDate,
				ArchiveDisplayName: completed.ArchiveDisplayName, ArchiveVideoFileName: completed.ArchiveVideoFileName,
				ArchiveRelativePath: "packages/2026/10/30/" + completed.ArchivePackageID + "/revisions/000001",
				ArchiveState:        completed.ArchiveState, CompletedAt: now.Add(time.Duration(180-index) * time.Minute),
			})
		}
		drive.CompletedWork = append(drive.CompletedWork, archiveintakeworkspace.CompletedWorkView{
			ArchiveRunID: "ARCHIVE-RUN-0", Folder: "2026-10-29/lecture", JobID: "JOB-LECTURE",
			Disposition: vocabulary.OrganizationRenderRecordings, ProfileReference: "h264-1080p-6mbps",
			SourceVideoCount: 1, ArchiveStorageID: "archive-main", ArchivePackageID: "VP-20261029-LECTURE",
			ArchiveDate: "2026-10-29", ArchiveDisplayName: "2026-10-29 · 2026-10-29/lecture · lecture.mov",
			ArchiveVideoFileName: "lecture.mp4", ArchiveRelativePath: "packages/2026/10/29/VP-20261029-LECTURE/revisions/000001",
			ArchiveState: archive.PackageStored, CompletedAt: now.Add(-21 * time.Hour),
		})
	}
	for index := range folders {
		folders[index].Videos = []archiveintakeworkspace.SourceVideoView{
			{RelativePath: folders[index].Folder + "/C0001.MP4", FileName: "C0001.MP4", SizeBytes: 3 << 30, ModifiedAt: now.Add(-2 * time.Hour)},
			{RelativePath: folders[index].Folder + "/C0002.MOV", FileName: "C0002.MOV", SizeBytes: 2 << 30, ModifiedAt: now.Add(-time.Hour)},
		}
	}
	drive.Folders = folders
	drive.DiscoveredFolders = append([]archiveintakeworkspace.FolderView(nil), folders...)
	drive.DiscoveredFolderCount = len(folders)
	for _, folder := range folders {
		drive.DiscoveredFileCount += folder.VideoCount
	}
	reviewA := transcribed(renderDone("review-a", 64, 12))
	reviewB := sourceB
	reviewB.Folder, reviewB.ArchivePackageID = "review-b", "VP-20261029-REVIEW-B"
	reviewA.InputRevision, reviewB.InputRevision = 3, 3
	return []archiveintakeworkspace.DriveView{drive, {
		WorkspaceDriveID: "VOLUME-2", ConnectionState: vocabulary.VolumeUnknown,
		ConnectionObservedAt: now.Add(-50 * time.Minute), ConnectionWorkerID: "WORKER-2",
		ArchiveRunID: "ARCHIVE-RUN-2", ArchiveRunRevision: 1,
		CandidateRevision: 3, FolderDiscoveryRevision: 3,
		SessionID: "INTAKE-2", SelectionSessionID: "INTAKE-2", WorkerID: "WORKER-2", SourceVolumeID: "VOLUME-2", SourceStorageID: "source-review",
		ArchiveStorageID: "archive-nas", State: "COMPLETED", ArchiveRunState: "COMPLETED", FolderDiscoveryState: vocabulary.FolderDiscoveryReady,
		InputRevision: 3, FileCount: 386, FolderCount: 2, EligibleTranscriptionCount: 1,
		CatalogRowCount: 386, CatalogUpdatedAt: now.Add(-20 * time.Hour), Folders: []archiveintakeworkspace.FolderView{reviewA, reviewB},
		DiscoveredFileCount: 386, DiscoveredFolderCount: 2, DiscoveredFolders: []archiveintakeworkspace.FolderView{reviewA, reviewB},
		StorageCandidates: []archiveintakeworkspace.StorageView{
			{StorageID: "source-review", Role: vocabulary.StorageSourceMedia, WorkspaceDriveID: "VOLUME-2", AccessKind: vocabulary.StorageAccessAttachedVolume, Accessible: true, CapacityBytes: 1 << 40, FreeBytes: 500 << 30},
			{StorageID: "archive-nas", Role: vocabulary.StorageArchivePrimary, WorkspaceDriveID: "ARCHIVE-NETWORK-VOLUME-1", AccessKind: vocabulary.StorageAccessMountedNetwork, Accessible: true, CapacityBytes: 8 << 40, FreeBytes: 3 << 40},
		},
	}}, nil
}

func (workspace *archiveIntakeWorkspaceFixture) Override(_ context.Context, request archiveintakeworkspace.OverrideCommand) (organization.OrganizationDecision, error) {
	if request.Actor.Type != vocabulary.ActorHuman || request.WorkspaceDriveID != "VOLUME-1" {
		return organization.OrganizationDecision{}, organization.ErrInvalidDecision
	}
	if replay, ok := workspace.receipts[request.IdempotencyKey]; ok {
		return replay, nil
	}
	current, ok := workspace.latest[request.Folder]
	if !ok {
		return organization.OrganizationDecision{}, organization.ErrInvalidDecision
	}
	history, err := organization.NewDecisionHistory(current)
	if err != nil {
		return organization.OrganizationDecision{}, err
	}
	decision, err := history.RecordOverride(organization.OverrideRequest{
		DecisionID: request.DecisionID, ExpectedInputRevision: request.ExpectedInputRevision,
		Disposition: request.Disposition, TargetVideoBitRateKbps: request.TargetVideoBitRateKbps,
		DecidedByType: request.Actor.Type, DecidedBy: request.Actor.ID,
		Reason: "explicit operator classification", Basis: []string{"previous-decision:" + current.ID}, DecidedAt: request.OccurredAt,
	})
	if err != nil {
		return organization.OrganizationDecision{}, err
	}
	workspace.latest[request.Folder] = decision
	workspace.receipts[request.IdempotencyKey] = decision
	return decision, nil
}

func (workspace fixedArchivistWorkspace) CreateSession(context.Context, archivistruntime.SessionSpec, string) (archivistcontrol.SessionRecord, error) {
	return archivistcontrol.SessionRecord{}, errors.New("fixture is read-only")
}

func (workspace fixedArchivistWorkspace) CreateArchivist(context.Context, string, string, string, string, workflow.ControlMode) (archivistcontrol.SessionRecord, error) {
	return archivistcontrol.SessionRecord{}, errors.New("fixture is read-only")
}

func (workspace fixedArchivistWorkspace) SubmitTurnWithDriveSelection(context.Context, string, string, string, string, *archivistruntime.DriveSelection) (archivistcontrol.TurnRecord, error) {
	return archivistcontrol.TurnRecord{}, errors.New("fixture is read-only")
}

func (workspace fixedArchivistWorkspace) Projection(context.Context) ([]archivistcontrol.SessionProjection, error) {
	return workspace.projections, nil
}

func (workspace fixedArchivistWorkspace) HostStatuses() []archivistruntime.HostStatus {
	return workspace.statuses
}

func (workspace fixedArchivistWorkspace) HostProfiles() []archivistruntime.HostProfile {
	profiles := make([]archivistruntime.HostProfile, 0, len(workspace.statuses))
	for _, status := range workspace.statuses {
		profiles = append(profiles, archivistruntime.HostProfile{HostStatus: status, DetectedCLIIDs: workspace.detected[status.ArchivistHostID]})
	}
	return profiles
}

func (workspace fixedArchivistWorkspace) PrepareActionCard(context.Context, string, string) (command.Envelope, error) {
	return command.Envelope{}, errors.New("fixture is read-only")
}

func (workspace fixedArchivistWorkspace) CompleteActionCard(context.Context, string) error {
	return errors.New("fixture is read-only")
}

func (workspace fixedArchivistWorkspace) ReleaseActionCard(context.Context, string) error {
	return errors.New("fixture is read-only")
}

func (workspace fixedArchivistWorkspace) BindActionCardArchiveRun(context.Context, string, string, uint64) error {
	return errors.New("fixture is read-only")
}

func (workspace fixedArchivistWorkspace) RejectActionCard(context.Context, string, string, string) error {
	return errors.New("fixture is read-only")
}

func assertArchiveWorkspaceGolden(t *testing.T, handler http.Handler, scene archiveWorkspaceScene) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()

	testdata, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	actualPath := filepath.Join(t.TempDir(), "archiveworkspace.actual.png")
	python := os.Getenv("ARCHIVE_WORKSPACE_GOLDEN_PYTHON")
	if python == "" {
		python = "python3"
	}
	renderContext, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(renderContext, python, filepath.Join(testdata, "render_archive_workspace.py"), server.URL, actualPath, string(scene))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("render Archive Workspace golden with Playwright: %v\n%s\nrun `make visual-deps` before the test", err, output)
	}

	goldenName := fmt.Sprintf("archive_workspace_%s_%s", scene, runtime.GOOS)
	goldenPath := filepath.Join(testdata, goldenName+".golden.png")
	if os.Getenv("UPDATE_ARCHIVE_WORKSPACE_GOLDEN") == "1" {
		payload, readErr := os.ReadFile(actualPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if writeErr := os.WriteFile(goldenPath, payload, 0o644); writeErr != nil {
			t.Fatal(writeErr)
		}
		t.Logf("updated Archive Workspace golden: %s", goldenPath)
		return
	}

	expected, err := decodePNG(goldenPath)
	if err != nil {
		artifactDir := preserveArchiveWorkspaceActual(t, actualPath, goldenName)
		t.Fatalf("read Archive Workspace golden %s: %v; actual image: %s", goldenPath, err, artifactDir)
	}
	actual, err := decodePNG(actualPath)
	if err != nil {
		t.Fatalf("decode rendered Archive Workspace: %v", err)
	}
	if expected.Bounds() != actual.Bounds() {
		artifactDir := preserveArchiveWorkspaceActual(t, actualPath, goldenName)
		t.Fatalf("Archive Workspace dimensions changed: golden=%v actual=%v; artifacts: %s", expected.Bounds(), actual.Bounds(), artifactDir)
	}

	diff, changed, maximumChannelDelta := archiveWorkspaceDiff(expected, actual)
	// Chromium on the same host alternates about a hundred rounded 1px panel-corner
	// samples by one or two channel values. Keep the approved PNG as the oracle
	// while ignoring only this bounded subpixel rasterization noise.
	if changed == 0 || (changed <= 128 && maximumChannelDelta <= 2) {
		preserveArchiveWorkspaceGoldenMatch(t, actualPath, goldenPath)
		return
	}
	artifactDir := preserveArchiveWorkspaceActual(t, actualPath, goldenName)
	diffPath := filepath.Join(artifactDir, goldenName+".diff.png")
	file, createErr := os.Create(diffPath)
	if createErr != nil {
		t.Fatalf("create Archive Workspace diff: %v", createErr)
	}
	encodeErr := png.Encode(file, diff)
	closeErr := file.Close()
	if encodeErr != nil || closeErr != nil {
		t.Fatalf("write Archive Workspace diff: %v %v", encodeErr, closeErr)
	}
	t.Fatalf("Archive Workspace differs from %s at %d pixels (maximum channel delta %d); artifacts: %s; review and run `make archive-workspace-golden-update` when intentional", goldenPath, changed, maximumChannelDelta, artifactDir)
}

func preserveArchiveWorkspaceGoldenMatch(t *testing.T, actualPath, goldenPath string) {
	t.Helper()
	artifactDir := strings.TrimSpace(os.Getenv("ARCHIVE_WORKSPACE_GOLDEN_EVIDENCE_DIR"))
	if artifactDir == "" {
		return
	}
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("create Archive Workspace golden evidence directory: %v", err)
	}
	baseName := strings.NewReplacer("/", "_", "\\", "_", " ", "_").Replace(t.Name())
	for source, suffix := range map[string]string{
		actualPath: ".actual.png",
		goldenPath: ".golden.png",
	} {
		payload, err := os.ReadFile(source)
		if err != nil {
			t.Fatalf("read Archive Workspace golden evidence %s: %v", source, err)
		}
		destination := filepath.Join(artifactDir, baseName+suffix)
		if err := os.WriteFile(destination, payload, 0o644); err != nil {
			t.Fatalf("write Archive Workspace golden evidence %s: %v", destination, err)
		}
	}
}

func decodePNG(path string) (image.Image, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return png.Decode(file)
}

func archiveWorkspaceDiff(expected, actual image.Image) (*image.RGBA, int, uint8) {
	bounds := expected.Bounds()
	diff := image.NewRGBA(bounds)
	changed := 0
	var maximumChannelDelta uint8
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			want := color.RGBAModel.Convert(expected.At(x, y)).(color.RGBA)
			got := color.RGBAModel.Convert(actual.At(x, y)).(color.RGBA)
			if want == got {
				diff.SetRGBA(x, y, color.RGBA{R: want.R / 4, G: want.G / 4, B: want.B / 4, A: 255})
				continue
			}
			changed++
			for _, delta := range []uint8{
				absoluteChannelDelta(want.R, got.R), absoluteChannelDelta(want.G, got.G),
				absoluteChannelDelta(want.B, got.B), absoluteChannelDelta(want.A, got.A),
			} {
				if delta > maximumChannelDelta {
					maximumChannelDelta = delta
				}
			}
			diff.SetRGBA(x, y, color.RGBA{R: 255, G: 32, B: 112, A: 255})
		}
	}
	return diff, changed, maximumChannelDelta
}

func absoluteChannelDelta(left, right uint8) uint8 {
	if left >= right {
		return left - right
	}
	return right - left
}

func preserveArchiveWorkspaceActual(t *testing.T, actualPath, goldenName string) string {
	t.Helper()
	artifactDir := os.Getenv("EVIDENCE_DIR")
	if artifactDir == "" {
		var err error
		artifactDir, err = os.MkdirTemp("", "archive-workspace-golden-")
		if err != nil {
			t.Fatalf("create Archive Workspace artifact directory: %v", err)
		}
	} else if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("create Archive Workspace evidence directory: %v", err)
	}
	payload, err := os.ReadFile(actualPath)
	if err != nil {
		t.Fatalf("read rendered Archive Workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, goldenName+".actual.png"), payload, 0o644); err != nil {
		t.Fatalf("preserve rendered Archive Workspace: %v", err)
	}
	return artifactDir
}
