package workerclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
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

type fixedStorageProbe struct{ observations []worker.StorageObservation }

type sequenceVolumeProbe struct {
	mu        sync.Mutex
	snapshots [][]device.ConnectedVolume
	current   []device.ConnectedVolume
}

type selectedProbeCall struct {
	selected []string
	volumes  []device.ConnectedVolume
}

type recordingSelectedProbe struct{ calls chan selectedProbeCall }

func (probe fixedStorageProbe) Observe(context.Context) ([]worker.StorageObservation, error) {
	return append([]worker.StorageObservation(nil), probe.observations...), nil
}

func (probe fixedVolumeProbe) Poll(context.Context, string, time.Time) ([]device.VolumeAttachmentEvent, error) {
	return nil, nil
}

func (probe fixedVolumeProbe) Connected() []device.ConnectedVolume {
	return append([]device.ConnectedVolume(nil), probe.volumes...)
}

func (probe *sequenceVolumeProbe) Poll(context.Context, string, time.Time) ([]device.VolumeAttachmentEvent, error) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if len(probe.snapshots) > 0 {
		probe.current = append([]device.ConnectedVolume(nil), probe.snapshots[0]...)
		probe.snapshots = probe.snapshots[1:]
	}
	return nil, nil
}

func (probe *sequenceVolumeProbe) Connected() []device.ConnectedVolume {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	return append([]device.ConnectedVolume(nil), probe.current...)
}

func (probe recordingSelectedProbe) Observe(_ context.Context, selected []string, volumes []device.ConnectedVolume) ([]worker.StorageObservation, error) {
	probe.calls <- selectedProbeCall{selected: append([]string(nil), selected...), volumes: append([]device.ConnectedVolume(nil), volumes...)}
	return nil, nil
}

// Requirement: WRK-006
func TestWorkerClientResolvesCredentialAndSendsOutboundHeartbeat(t *testing.T) {
	received := make(chan map[string]any, 1)
	inventoryReceived := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer service-secret" ||
			request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected heartbeat request: path=%s headers=%v", request.URL.Path, request.Header)
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		switch request.URL.Path {
		case "/worker/v1/heartbeat":
			received <- payload
		case "/worker/v1/storage-inventory":
			inventoryReceived <- payload
		default:
			t.Fatalf("unexpected Worker request path: %s", request.URL.Path)
		}
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
	}}, func() time.Time { return time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC) }, nil, fixedStorageProbe{observations: []worker.StorageObservation{{
		StorageID: "source-main", Connected: true, DiscoveryState: "READY", DiscoveryRevision: 9,
		Folders: []worker.MediaFolderObservation{{Folder: "event-a", VideoCount: 700, InputBytes: 10 << 30}},
	}}}, fixedVolumeProbe{volumes: []device.ConnectedVolume{{
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
	heartbeatObservations, ok := payload["storage_observations"].([]any)
	if !ok || len(heartbeatObservations) != 1 || heartbeatObservations[0].(map[string]any)["folders"] != nil {
		t.Fatalf("heartbeat carried the unbounded folder inventory: %+v", heartbeatObservations)
	}
	inventory := <-inventoryReceived
	inventoryObservations, ok := inventory["storage_observations"].([]any)
	if !ok || len(inventoryObservations) != 1 {
		t.Fatalf("separate storage inventory is incomplete: %+v", inventory)
	}
	folders, ok := inventoryObservations[0].(map[string]any)["folders"].([]any)
	if !ok || len(folders) != 1 || folders[0].(map[string]any)["video_count"] != float64(700) {
		t.Fatalf("separate storage inventory lost folder details: %+v", inventory)
	}
}

func TestWorkerClientReconcilesSelectedInventoryAfterEveryVolumeDetaches(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte(`{"data":{"state":"ONLINE","selected_source_storage_ids":["source-old"]}}`))
	}))
	defer server.Close()
	controlURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	oldVolume := device.ConnectedVolume{PhysicalDeviceID: "physical-old", VolumeID: "volume-old", StorageID: "source-old", ArchiveStorageID: "archive-old"}
	volumes := &sequenceVolumeProbe{snapshots: [][]device.ConnectedVolume{{oldVolume}, {}}}
	selected := recordingSelectedProbe{calls: make(chan selectedProbeCall, 2)}
	client, err := workerclient.NewWithExecutorAndInventoryProbes(workerconfig.Configuration{
		SchemaVersion: 1, ID: "worker-main", ControlURL: controlURL,
		CredentialReference: "wincred://archive/workers/worker-main", HeartbeatInterval: 10 * time.Second,
		AccessibleStorageIDs: []string{"scratch-main"},
	}, fixedSecret("service-secret"), server.Client(), fixedProbe{metrics: worker.Metrics{AcceptingWork: true}}, time.Now,
		nil, nil, volumes, selected, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, second := <-selected.calls, <-selected.calls
	if len(first.selected) != 0 || len(first.volumes) != 1 || len(second.selected) != 1 || second.selected[0] != "source-old" || len(second.volumes) != 0 {
		t.Fatalf("selected source probe did not receive the all-detached reconciliation: first=%+v second=%+v", first, second)
	}
}
