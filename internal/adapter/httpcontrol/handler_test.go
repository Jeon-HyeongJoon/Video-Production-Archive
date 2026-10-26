package httpcontrol_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/adapter/httpcontrol"
	"videoarchive/internal/adapter/notificationsse"
	"videoarchive/internal/adapter/sqlitecatalog"
	"videoarchive/internal/adapter/sqlitedashboard"
	"videoarchive/internal/application/dashboard"
	"videoarchive/internal/core/asset"
	"videoarchive/internal/core/catalog"
	"videoarchive/internal/core/intake"
	"videoarchive/internal/core/notification"
	"videoarchive/internal/core/security"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

const checksumA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// Requirement: CTL-001
func TestControlHTTPServesSQLiteDashboardAndSearchOnlyToAllowedNetwork(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	catalogStore, err := sqlitecatalog.Open(filepath.Join(directory, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer catalogStore.Close()
	observation, err := intake.NewInputFileObservation(intake.InputFileObservation{
		ID: "OBSERVATION-1", StorageID: "source-main", WorkspaceDriveID: "drive-1", RelativePath: "camera/clip.mov",
		OriginalName: "clip.mov", SizeBytes: 42, Checksum: checksumA, ObservedBy: "WORKER-1", ObservedAt: time.Now().UTC(),
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

	dashboardStore, err := sqlitedashboard.Open(filepath.Join(directory, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dashboardStore.Close()
	view, err := dashboard.Project(dashboard.ProjectionInput{
		Job: dashboard.Job{
			ID: "JOB-1", Revision: 1, TemplateName: "occasion-recording", TargetTitle: "가을 행사",
			ControlMode: workflow.ManualMode, CurrentStepID: "intake",
		},
		Steps: []dashboard.StepStatus{{
			ID: "intake", State: workflow.Queued, ExecutorKind: vocabulary.ExecutorWorker,
			LastEventAt: observation.ObservedAt,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dashboardStore.Replace(ctx, view); err != nil {
		t.Fatal(err)
	}
	queries, err := dashboard.NewQueryService(dashboardStore)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpcontrol.New(httpcontrol.Configuration{
		Access: security.AccessConfig{
			Mode: security.AuthDisabled, AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		},
		Catalog: catalogStore, Dashboard: queries, Notifications: notificationsse.New(&notification.Feed{}), Version: "v1-test",
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, request := range []struct {
		path       string
		wantStatus int
		want       string
	}{
		{path: "/", wantStatus: http.StatusOK, want: "영상 기록 제어실"},
		{path: "/healthz", wantStatus: http.StatusOK, want: `"version":"v1-test"`},
		{path: "/api/v1/assets?text=가을", wantStatus: http.StatusOK, want: `"title":"가을 행사"`},
		{path: "/api/v1/jobs", wantStatus: http.StatusOK, want: `"job_id":"JOB-1"`},
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
}
