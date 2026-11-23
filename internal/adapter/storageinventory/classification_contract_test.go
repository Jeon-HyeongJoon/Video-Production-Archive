package storageinventory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"videoarchive/internal/core/device"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: ORG-019
func TestDiscoveryVersionsDirectoryEvidence(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"card/CLIP", "card/SUB", "recovery", ".private"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"card/CLIP/C0001.MP4", "card/CLIP/C0001.xml", "recovery/a.M4V", ".private/a.mov"} {
		if err := os.WriteFile(filepath.Join(root, file), []byte{1}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	volume := device.ConnectedVolume{StorageID: "S", Root: root, CapacityBytes: 10000, FreeBytes: 5000, ObservedAt: time.Unix(1, 0)}
	scan := func() (uint64, map[string]map[string]int) {
		t.Helper()
		obs := waitForDiscoveryState(t, NewAttachedProbe(), volume, vocabulary.FolderDiscoveryReady)[0]
		if len(obs.Folders) != 1 || obs.Folders[0].Folder != "card/CLIP" || obs.Folders[0].VideoCount != 1 {
			t.Fatalf("unexpected selectable media folders: %+v", obs.Folders)
		}
		payload, err := json.Marshal(obs)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Directories []struct {
				Path       string         `json:"relative_path"`
				Extensions map[string]int `json:"direct_extensions"`
			} `json:"directories"`
		}
		if err := json.Unmarshal(payload, &result); err != nil {
			t.Fatal(err)
		}
		dirs := make(map[string]map[string]int)
		for _, dir := range result.Directories {
			if _, exists := dirs[dir.Path]; exists {
				t.Fatalf("repeated directory %q", dir.Path)
			}
			dirs[dir.Path] = dir.Extensions
		}
		return obs.DiscoveryRevision, dirs
	}
	before, dirs := scan()
	_, empty := dirs["card/SUB"]
	_, protected := dirs[".private"]
	if !empty || len(dirs["card/SUB"]) != 0 || protected ||
		!reflect.DeepEqual(dirs["card/CLIP"], map[string]int{".mp4": 1, ".xml": 1}) || dirs["recovery"][".m4v"] != 1 {
		t.Errorf("missing or inaccurate directory evidence: %v", dirs)
	}
	if err := os.Mkdir(filepath.Join(root, "Output"), 0o700); err != nil {
		t.Fatal(err)
	}
	after, dirs := scan()
	if _, exists := dirs["Output"]; !exists || after == before || before == 0 {
		t.Errorf("empty sibling did not change the inventory: revisions %d / %d, %v", before, after, dirs)
	}
}
