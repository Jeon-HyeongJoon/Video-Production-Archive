package archiveintakeworkspace_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"videoarchive/internal/adapter/storageinventory"
	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/core/device"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
)

type classificationPreview struct {
	Folder         string
	CopyCandidates []string
	EventPreview   struct{ Directory, Video, Subtitle, Metadata string }
	Videos         []struct {
		RelativePath    string
		ContextEvidence struct {
			Case    string
			Witness string
		}
	}
}

// Exercise the real Worker discovery and Control workspace projection with the same snapshot.
func classificationPreviews(t *testing.T, files map[string]string, state vocabulary.FolderDiscoveryState) map[string]classificationPreview {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	probe := storageinventory.NewAttachedProbe()
	volume := device.ConnectedVolume{StorageID: "S", VolumeID: "V", Root: root, CapacityBytes: 10000, FreeBytes: 5000, ObservedAt: now}
	var obs worker.StorageObservation
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		found, err := probe.Observe(context.Background(), []string{"S"}, []device.ConnectedVolume{volume})
		if err != nil || len(found) != 1 {
			t.Fatalf("source inventory: %+v %v", found, err)
		}
		obs = found[0]
		if obs.DiscoveryState == vocabulary.FolderDiscoveryReady {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if obs.DiscoveryState != vocabulary.FolderDiscoveryReady {
		t.Fatal("source discovery did not finish")
	}
	if state == vocabulary.FolderDiscoveryScanning {
		obs.DiscoveryState = state
		obs.DiscoveryRevision = 0
	}
	statuses := statusInventory{{WorkerID: "W", State: "ONLINE", Metrics: worker.Metrics{ObservedAt: now, StorageObservations: []worker.StorageObservation{obs}}}}
	service, err := archiveintakeworkspace.New(workstationStore{}, workstationJobs{}, archiveintakeworkspace.WithInventory(candidateInventory{{VolumeID: "V", Revision: 1, WorkerID: "W", StorageID: "S", Connected: true}}, storageInventory{}, statuses, func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	views, err := service.Projection(context.Background())
	if err != nil || len(views) != 1 {
		t.Fatalf("drive projection: %v %v", views, err)
	}
	payload, err := json.Marshal(views[0].DiscoveredFolders)
	if err != nil {
		t.Fatal(err)
	}
	var folders []classificationPreview
	if err = json.Unmarshal(payload, &folders); err != nil {
		t.Fatal(err)
	}
	byPath := map[string]classificationPreview{}
	for _, folder := range folders {
		byPath[folder.Folder] = folder
	}
	return byPath
}

// Requirement: ORG-020
func TestInventoryExplainsFileContextWithinItsParent(t *testing.T) {
	files := map[string]string{
		"project/Source/input.mov": "a", "project/Output/title.xml": "b",
		"other/Source/input.mov": "c", "unrelated/Output/final.mov": "d",
		"card/M4ROOT/CLIP/C0001.MP4":   "primary",
		"card/M4ROOT/SUB/C0001S03.MP4": "p", "card/M4ROOT/SUB/C0002S03.MP4": "q",
	}
	for _, state := range []vocabulary.FolderDiscoveryState{vocabulary.FolderDiscoveryReady, vocabulary.FolderDiscoveryScanning} {
		folders := classificationPreviews(t, files, state)
		for _, tc := range []struct{ folder, file, role, witness string }{
			{"project/Source", "input.mov", "EDITORIAL_INPUT", "project/Output"},
			{"other/Source", "input.mov", "", ""},
			{"card/M4ROOT/SUB", "C0001S03.MP4", "MATCHED_PROXY", "card/M4ROOT/CLIP/C0001.MP4"},
			{"card/M4ROOT/SUB", "C0002S03.MP4", "UNPAIRED_PROXY", "card/M4ROOT/SUB"},
		} {
			seen := false
			for _, v := range folders[tc.folder].Videos {
				if v.RelativePath != tc.folder+"/"+tc.file {
					continue
				}
				seen = true
				got := v.ContextEvidence
				switch {
				case state == vocabulary.FolderDiscoveryScanning:
					if got.Case == "EDITORIAL_INPUT" || got.Case == "MATCHED_PROXY" || got.Case == "UNPAIRED_PROXY" {
						t.Errorf("partial inventory finalized %s: %+v", v.RelativePath, got)
					}
				case tc.role == "":
					if got.Case == "EDITORIAL_INPUT" {
						t.Errorf("unrelated sibling attached: %+v", v)
					}
				default:
					if got.Case != tc.role || got.Witness != tc.witness {
						t.Errorf("%s: want %s / %s, got %+v", v.RelativePath, tc.role, tc.witness, got)
					}
				}
			}
			if !seen && state == vocabulary.FolderDiscoveryReady {
				t.Errorf("missing file %s/%s", tc.folder, tc.file)
			}
		}
	}
}

// Requirement: ORG-022
func TestInventoryLinksOnlyCompleteFolderCopyCandidates(t *testing.T) {
	files := map[string]string{
		"shoot/a.mov": "x", "shoot/b.mov": "yy", "edit/a.mov": "z", "edit/b.mov": "ww",
		"subset/a.mov": "x", "resized/a.mov": "x", "resized/b.mov": "yyy",
	}
	for _, state := range []vocabulary.FolderDiscoveryState{vocabulary.FolderDiscoveryReady, vocabulary.FolderDiscoveryScanning} {
		folders := classificationPreviews(t, files, state)
		for dir, peer := range map[string]string{"shoot": "edit", "edit": "shoot", "subset": "", "resized": ""} {
			row := folders[dir]
			var want []string
			if state == vocabulary.FolderDiscoveryReady && peer != "" {
				want = []string{peer}
			}
			if !slices.Equal(row.CopyCandidates, want) {
				t.Errorf("%s %s: expected %v, got %v", state, dir, want, row.CopyCandidates)
			}
			if state == vocabulary.FolderDiscoveryReady {
				var paths []string
				for _, v := range row.Videos {
					paths = append(paths, v.RelativePath)
				}
				slices.Sort(paths)
				expected := []string{dir + "/a.mov"}
				if dir != "subset" {
					expected = append(expected, dir+"/b.mov")
				}
				if !slices.Equal(paths, expected) {
					t.Errorf("lost original paths for %s: %v", dir, paths)
				}
			}
		}
	}
}

// Requirement: ORG-024
func TestInventoryPreviewsOneDirectoryPerSourceEvent(t *testing.T) {
	const a = "391216-촬-기념식-본관"
	const b = "391216-촬-교육-별관"
	prefix := "39년도/# 1. 촬영물/"
	files := map[string]string{
		prefix + a + "/vj/C0001.MP4": "a", prefix + a + "/vj/C0002.MP4": "bb",
		prefix + a + "/PGM/AJA/main.mp4": "ccc", prefix + a + "/천장캠/cam.mp4": "dddd",
		prefix + b + "/PGM/AJA/main.mp4": "eeeee",
	}
	folders := classificationPreviews(t, files, vocabulary.FolderDiscoveryReady)
	for source, want := range map[string][2]string{a + "/vj": {a, "VJ-소스컷"}, a + "/PGM/AJA": {a, "PGM"}, a + "/천장캠": {a, "천장캠"}, b + "/PGM/AJA": {b, "PGM"}} {
		got := folders[prefix+source].EventPreview
		if got.Directory != want[0] || got.Video != want[1]+".mp4" || got.Subtitle != want[1]+".srt" || got.Metadata != want[1]+".json" {
			t.Errorf("%s: want flat event %s and stem %s, got %+v", source, want[0], want[1], got)
		}
	}
}
