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
)

type fixedSecret string

func (secret fixedSecret) Resolve(reference string) (string, error) {
	return string(secret), nil
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
	client, err := workerclient.New(workerconfig.Configuration{
		SchemaVersion: 1, ID: "worker-main", ControlURL: controlURL,
		CredentialReference: "wincred://video-archive/workers/worker-main", HeartbeatInterval: 10 * time.Second,
		AccessibleStorageIDs: []string{"source-main", "scratch-main"},
	}, fixedSecret("service-secret"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	payload := <-received
	storageIDs, ok := payload["accessible_storage_ids"].([]any)
	if payload["worker_id"] != "worker-main" || payload["accepting_work"] != true || !ok || len(storageIDs) != 2 {
		t.Fatalf("heartbeat payload is incomplete: %+v", payload)
	}
}
