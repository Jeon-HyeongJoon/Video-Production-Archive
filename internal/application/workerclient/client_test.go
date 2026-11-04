package workerclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/workerclient"
	"videoarchive/internal/core/device"
	"videoarchive/internal/core/worker"
)

type fixedSecret string

func (secret fixedSecret) Resolve(reference string) (string, error) {
	return string(secret), nil
}

type fixedProbe struct {
	metrics worker.Metrics
}

func (probe fixedProbe) Snapshot(context.Context) (worker.Metrics, error) { return probe.metrics, nil }

type fixedVolumeProbe struct{ volumes []device.ConnectedVolume }

func (probe fixedVolumeProbe) Poll(context.Context, string, time.Time) ([]device.VolumeAttachmentEvent, error) {
	return nil, nil
}

func (probe fixedVolumeProbe) Connected() []device.ConnectedVolume {
	return append([]device.ConnectedVolume(nil), probe.volumes...)
}

// Requirement: WRK-006
func TestWorkerClientResolvesCredentialAndSendsOutboundHeartbeat(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/worker/v1/heartbeat" || request.Header.Get("Authorization") != "Bearer service-secret" ||
			request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected heartbeat request: path=%s headers=%v", request.URL.Path, request.Header)
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		received <- payload
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte(`{"data":{"state":"ONLINE"}}`))
	}))
	defer server.Close()
	controlURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	volumeID := "VOLUME-USB-1"
	archiveStorageID := device.AttachedArchiveStorageID(volumeID)
	client, err := workerclient.NewWithExecutorAndInventoryProbes(workerconfig.Configuration{
		SchemaVersion: 1, ID: "worker-main", ControlURL: controlURL,
		CredentialReference: "wincred://archive/workers/worker-main", HeartbeatInterval: 10 * time.Second,
		AccessibleStorageIDs: []string{"source-main", "scratch-main"},
	}, fixedSecret("service-secret"), server.Client(), fixedProbe{metrics: worker.Metrics{
		AverageCPU5m: 12.5, AverageGPU5m: 3, IdleStableFor: 6 * time.Minute,
		AvailableMemoryBytes: 24 << 30, AvailableVRAMBytes: 8 << 30, ScratchFreeBytes: 512 << 30,
		HeavyRunning: 0, LightRunning: 1, BusyPhysicalDeviceIDs: map[string]bool{"disk-2": true}, AcceptingWork: true,
	}}, func() time.Time { return time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC) }, nil, nil, fixedVolumeProbe{volumes: []device.ConnectedVolume{{
		PhysicalDeviceID: "PHYSICAL-USB-1", VolumeID: volumeID, StorageID: "source-usb-1", ArchiveStorageID: archiveStorageID,
		Model: "USB DRIVE", FileSystem: "NTFS", CapacityBytes: 4 << 30, FreeBytes: 3 << 30,
	}}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	payload := <-received
	storageIDs, ok := payload["accessible_storage_ids"].([]any)
	attached, attachedOK := payload["attached_volumes"].([]any)
	if payload["worker_id"] != "worker-main" || payload["accepting_work"] != true || !ok || len(storageIDs) != 4 ||
		payload["cpu_average_5m"] != 12.5 || payload["available_memory_bytes"] != float64(24<<30) ||
		payload["light_running"] != float64(1) || payload["snapshot_id"] == float64(0) || !attachedOK || len(attached) != 1 {
		t.Fatalf("heartbeat payload is incomplete: %+v", payload)
	}
	attachedVolume, ok := attached[0].(map[string]any)
	if !ok || attachedVolume["storage_id"] != "source-usb-1" || attachedVolume["archive_storage_id"] != archiveStorageID {
		t.Fatalf("heartbeat did not preserve role-separated attached Storage identities: %+v", attached)
	}
}
