package httpcontrol_test

import (
	"context"
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
	"videoarchive/internal/adapter/sqlitecatalog"
	"videoarchive/internal/adapter/sqlitedashboard"
	"videoarchive/internal/application/dashboard"
	"videoarchive/internal/core/asset"
	"videoarchive/internal/core/catalog"
	"videoarchive/internal/core/intake"
	"videoarchive/internal/core/notification"
	"videoarchive/internal/core/security"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
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

	dashboardStore, err := sqlitedashboard.Open(filepath.Join(directory, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dashboardStore.Close()
	view, err := dashboard.Project(dashboard.ProjectionInput{
		Job: dashboard.Job{
			ID: "JOB-RUNNING", Revision: 3, TemplateName: "occasion-recording", TargetTitle: "가을 행사 편집",
			ControlMode: workflow.AssistedMode, CurrentStepID: "render",
		},
		Steps: []dashboard.StepStatus{{
			ID: "render", State: workflow.Running, Progress: 0.64, ExecutorKind: vocabulary.ExecutorWorker,
			WorkerID: "worker-main", LastEventAt: observation.ObservedAt,
		}},
		Workers: []dashboard.WorkerResource{{
			WorkerID: "worker-main", CPUPercent: 38, GPUPercent: 71,
			AvailableMemoryBytes: 18 << 30, AvailableVRAMBytes: 7 << 30, ScratchFreeBytes: 420 << 30,
			HeavyRunning: 1, AcceptingWork: true, AccessibleStorageIDs: []string{"archive-main", "source-main"},
			HeartbeatAt: observation.ObservedAt,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dashboardStore.Replace(ctx, view); err != nil {
		t.Fatal(err)
	}
	waitingView, err := dashboard.Project(dashboard.ProjectionInput{
		Job: dashboard.Job{
			ID: "JOB-REVIEW", Revision: 2, TemplateName: "final-edit", TargetTitle: "인터뷰 요약 검수",
			ControlMode: workflow.AgentMode, CurrentStepID: "approve-summary",
		},
		Steps: []dashboard.StepStatus{{
			ID: "approve-summary", State: workflow.Waiting, Progress: 0.82, ExecutorKind: vocabulary.ExecutorHuman,
			WaitReason: vocabulary.WaitApproval, LastEventAt: observation.ObservedAt.Add(time.Minute),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dashboardStore.Replace(ctx, waitingView); err != nil {
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
		WorkerAPI: http.NotFoundHandler(),
		Workers: fixedWorkerReader{{
			WorkerID: "worker-main", State: "BUSY", LastHeartbeat: observation.ObservedAt,
			Metrics: worker.Metrics{
				AcceptingWork: true, HeavyRunning: 1,
				AccessibleStorageIDs: map[string]bool{"archive-main": true, "source-main": true},
			},
		}, {
			WorkerID: "worker-review", State: "OFFLINE",
		}},
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

	assertDashboardGolden(t, handler)
}

type fixedWorkerReader []worker.Status

func (reader fixedWorkerReader) Statuses(context.Context) ([]worker.Status, error) {
	return append([]worker.Status(nil), reader...), nil
}

func assertDashboardGolden(t *testing.T, handler http.Handler) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()

	testdata, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	actualPath := filepath.Join(t.TempDir(), "dashboard.actual.png")
	python := os.Getenv("DASHBOARD_GOLDEN_PYTHON")
	if python == "" {
		python = "python3"
	}
	renderContext, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(renderContext, python, filepath.Join(testdata, "render_dashboard.py"), server.URL, actualPath)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("render dashboard golden with Playwright: %v\n%s\nrun `make visual-deps` before the test", err, output)
	}

	goldenPath := filepath.Join(testdata, fmt.Sprintf("dashboard_%s.golden.png", runtime.GOOS))
	if os.Getenv("UPDATE_DASHBOARD_GOLDEN") == "1" {
		payload, readErr := os.ReadFile(actualPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if writeErr := os.WriteFile(goldenPath, payload, 0o644); writeErr != nil {
			t.Fatal(writeErr)
		}
		t.Logf("updated dashboard golden: %s", goldenPath)
		return
	}

	expected, err := decodePNG(goldenPath)
	if err != nil {
		artifactDir := preserveDashboardActual(t, actualPath)
		t.Fatalf("read dashboard golden %s: %v; actual image: %s", goldenPath, err, artifactDir)
	}
	actual, err := decodePNG(actualPath)
	if err != nil {
		t.Fatalf("decode rendered dashboard: %v", err)
	}
	if expected.Bounds() != actual.Bounds() {
		artifactDir := preserveDashboardActual(t, actualPath)
		t.Fatalf("dashboard dimensions changed: golden=%v actual=%v; artifacts: %s", expected.Bounds(), actual.Bounds(), artifactDir)
	}

	diff, changed := dashboardDiff(expected, actual)
	if changed == 0 {
		return
	}
	artifactDir := preserveDashboardActual(t, actualPath)
	diffPath := filepath.Join(artifactDir, "dashboard.diff.png")
	file, createErr := os.Create(diffPath)
	if createErr != nil {
		t.Fatalf("create dashboard diff: %v", createErr)
	}
	encodeErr := png.Encode(file, diff)
	closeErr := file.Close()
	if encodeErr != nil || closeErr != nil {
		t.Fatalf("write dashboard diff: %v %v", encodeErr, closeErr)
	}
	t.Fatalf("dashboard differs from %s at %d pixels; artifacts: %s; review and run `make dashboard-golden-update` when intentional", goldenPath, changed, artifactDir)
}

func decodePNG(path string) (image.Image, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return png.Decode(file)
}

func dashboardDiff(expected, actual image.Image) (*image.RGBA, int) {
	bounds := expected.Bounds()
	diff := image.NewRGBA(bounds)
	changed := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			want := color.RGBAModel.Convert(expected.At(x, y)).(color.RGBA)
			got := color.RGBAModel.Convert(actual.At(x, y)).(color.RGBA)
			if want == got {
				diff.SetRGBA(x, y, color.RGBA{R: want.R / 4, G: want.G / 4, B: want.B / 4, A: 255})
				continue
			}
			changed++
			diff.SetRGBA(x, y, color.RGBA{R: 255, G: 32, B: 112, A: 255})
		}
	}
	return diff, changed
}

func preserveDashboardActual(t *testing.T, actualPath string) string {
	t.Helper()
	artifactDir := os.Getenv("EVIDENCE_DIR")
	if artifactDir == "" {
		var err error
		artifactDir, err = os.MkdirTemp("", "video-archive-dashboard-golden-")
		if err != nil {
			t.Fatalf("create dashboard artifact directory: %v", err)
		}
	} else if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("create dashboard evidence directory: %v", err)
	}
	payload, err := os.ReadFile(actualPath)
	if err != nil {
		t.Fatalf("read rendered dashboard: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "dashboard.actual.png"), payload, 0o644); err != nil {
		t.Fatalf("preserve rendered dashboard: %v", err)
	}
	return artifactDir
}
