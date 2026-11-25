package ffmpeg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/rendering"
)

// Requirement: RND-006
func TestSequenceRendererPublishesTimelineOrderedTwentyMbpsVideoAndMarkerSidecar(t *testing.T) {
	directory := t.TempDir()
	argumentsPath := filepath.Join(directory, "arguments.txt")
	payload := []byte("verified-sequence-video")
	executablePath := ffmpegFixtureExecutable(t, argumentsPath, payload)
	markerRoot, err := archive.OpenStorageRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer markerRoot.Close()
	if err := markerRoot.MkdirAll("organized/SOURCE-1", 0o755); err != nil {
		t.Fatal(err)
	}
	renderer, err := NewSequenceRenderer(
		executablePath, "ffmpeg-8.0", "WORKER-1", OSRunner{}, payloadVerifier{expected: payload}, AtomicPublisher{}, markerRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := rendering.OrganizationProfile(20_000)
	if err != nil {
		t.Fatal(err)
	}
	timeline := organization.SourceTimeline{
		ID: "TIMELINE-1", Revision: 1, DecisionID: "DECISION-1",
		Clips: []organization.SourceClip{
			{Sequence: 1, OriginalAssetID: "ASSET-1", InMillis: 0, OutMillis: 10_000},
			{Sequence: 2, OriginalAssetID: "ASSET-2", InMillis: 0, OutMillis: 12_000},
			{Sequence: 3, OriginalAssetID: "ASSET-3", InMillis: 0, OutMillis: 8_000},
		},
		Markers: []organization.CutMarker{
			{BeforeClipID: "ASSET-1", AfterClipID: "ASSET-2", AtMillis: 10_000},
			{BeforeClipID: "ASSET-2", AfterClipID: "ASSET-3", AtMillis: 22_000},
		},
	}
	inputPaths := map[string]string{
		"ASSET-1": filepath.Join(directory, "input", "C0001.MP4"),
		"ASSET-2": filepath.Join(directory, "input", "C0002.MP4"),
		"ASSET-3": filepath.Join(directory, "input", "C0003.MP4"),
	}
	temporaryVideo := filepath.Join(directory, "organized", "SOURCE-1", "video.mp4.partial")
	publishedVideo := filepath.Join(directory, "organized", "SOURCE-1", "video.mp4")
	request := SequenceRequest{
		RenditionID: "RENDITION-SEQUENCE", MarkerArtifactID: "MARKERS-1", Timeline: timeline,
		ResolvedInputPaths: inputPaths, HasAudio: true, TemporaryOutputPath: temporaryVideo,
		PublishedOutputPath: publishedVideo, ArtifactPath: "organized/SOURCE-1/video.mp4",
		TemporaryMarkerPath: "organized/SOURCE-1/cut-markers.json.partial",
		PublishedMarkerPath: "organized/SOURCE-1/cut-markers.json", Profile: profile,
	}
	result, err := renderer.Render(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	argumentPayload, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	arguments := strings.Split(strings.TrimSuffix(string(argumentPayload), "\n"), "\n")
	positions := make([]int, 3)
	for index, inputPath := range []string{inputPaths["ASSET-1"], inputPaths["ASSET-2"], inputPaths["ASSET-3"]} {
		positions[index] = slices.Index(arguments, inputPath)
	}
	if positions[0] < 0 || positions[1] <= positions[0] || positions[2] <= positions[1] ||
		!containsArgumentPair(arguments, "-b:v", "20000k") {
		t.Fatalf("timeline order or 20Mbps argument missing: positions=%v args=%q", positions, arguments)
	}
	markerPayload, err := markerRoot.ReadFile("organized/SOURCE-1/cut-markers.json")
	if err != nil || !strings.Contains(string(markerPayload), `"at_millis":10000`) || !strings.Contains(string(markerPayload), `"at_millis":22000`) {
		t.Fatalf("marker sidecar missing timeline boundaries: payload=%s err=%v", markerPayload, err)
	}
	if result.Rendition.Artifact.Origin.InputTimelineID != timeline.ID ||
		!slices.Equal(result.Rendition.Artifact.Origin.RootOriginalAssetIDs, []string{"ASSET-1", "ASSET-2", "ASSET-3"}) ||
		result.MarkerArtifact.Path != "organized/SOURCE-1/cut-markers.json" {
		t.Fatalf("sequence provenance is incomplete: %+v", result)
	}

	verificationRenderer, err := NewSequenceRenderer(
		executablePath, "ffmpeg-8.0", "WORKER-1", OSRunner{}, payloadVerifier{expected: []byte("rejected")}, AtomicPublisher{}, markerRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verificationRenderer.Render(context.Background(), request); err == nil {
		t.Fatal("rejected replacement sequence was accepted")
	}
	priorVideo, err := os.ReadFile(publishedVideo)
	if err != nil || string(priorVideo) != string(payload) {
		t.Fatalf("prior sequence changed: %v", err)
	}
	priorMarker, err := markerRoot.ReadFile(request.PublishedMarkerPath)
	if err != nil || string(priorMarker) != string(markerPayload) {
		t.Fatalf("prior markers changed: %v", err)
	}
	t.Run("conflicting replacement preserves both published files", func(t *testing.T) {
		t.Setenv("FFMPEG_FIXTURE_PAYLOAD", "different verified sequence")
		replacement, err := NewSequenceRenderer(executablePath, "ffmpeg-8.0", "WORKER-1", OSRunner{},
			payloadVerifier{expected: []byte("different verified sequence")}, AtomicPublisher{}, markerRoot)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = replacement.Render(context.Background(), request); !errors.Is(err, ErrSequenceOutputConflict) {
			t.Fatal(err)
		}
		video, err := os.ReadFile(publishedVideo)
		if err != nil || string(video) != string(priorVideo) {
			t.Fatal("previous video changed", err)
		}
		markers, err := markerRoot.ReadFile(request.PublishedMarkerPath)
		if err != nil || string(markers) != string(priorMarker) {
			t.Fatal("previous markers changed", err)
		}
	})
	for _, missing := range []string{"neither", "markers", "video"} {
		t.Run("retry with missing "+missing, func(t *testing.T) {
			paths := map[string]string{"video": publishedVideo, "markers": filepath.Join(directory, request.PublishedMarkerPath)}
			existing := map[string]os.FileInfo{}
			for name, file := range paths {
				if name == missing {
					if err := os.Remove(file); err != nil {
						t.Fatal(err)
					}
					continue
				}
				info, err := os.Stat(file)
				if err != nil {
					t.Fatal(err)
				}
				existing[name] = info
			}
			retry := request
			retry.TemporaryOutputPath += ".2"
			retry.TemporaryMarkerPath += ".2"
			replayed, err := renderer.Render(context.Background(), retry)
			if err != nil || replayed.Rendition.Artifact.Checksum != result.Rendition.Artifact.Checksum ||
				replayed.MarkerArtifact.Checksum != result.MarkerArtifact.Checksum {
				t.Fatalf("retry returned a different sequence: %+v, %v", replayed, err)
			}
			for name, prior := range existing {
				current, err := os.Stat(paths[name])
				if err != nil || !os.SameFile(prior, current) || !prior.ModTime().Equal(current.ModTime()) {
					t.Errorf("retry replaced existing %s: %v", name, err)
				}
			}
			for _, temp := range []string{retry.TemporaryOutputPath, filepath.Join(directory, retry.TemporaryMarkerPath)} {
				if _, err := os.Stat(temp); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("retry left temporary file %s: %v", temp, err)
				}
			}
		})
	}
	verificationFailure := request
	verificationFailure.RenditionID = "RENDITION-VERIFY-FAIL"
	verificationFailure.TemporaryOutputPath = filepath.Join(directory, "organized", "SOURCE-1", "verify-fail.mp4.partial")
	verificationFailure.PublishedOutputPath = filepath.Join(directory, "organized", "SOURCE-1", "verify-fail.mp4")
	verificationFailure.ArtifactPath = "organized/SOURCE-1/verify-fail.mp4"
	verificationFailure.TemporaryMarkerPath = "organized/SOURCE-1/verify-fail.json.partial"
	verificationFailure.PublishedMarkerPath = "organized/SOURCE-1/verify-fail.json"
	if failed, err := verificationRenderer.Render(context.Background(), verificationFailure); err == nil || failed.Rendition.Artifact.ID != "" {
		t.Fatalf("verification failure returned a completed rendition: result=%+v err=%v", failed, err)
	}

	publishRenderer, err := NewSequenceRenderer(
		executablePath, "ffmpeg-8.0", "WORKER-1", OSRunner{}, payloadVerifier{expected: payload}, failingSequencePublisher{}, markerRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	publishFailure := verificationFailure
	publishFailure.RenditionID = "RENDITION-PUBLISH-FAIL"
	publishFailure.TemporaryOutputPath = filepath.Join(directory, "organized", "SOURCE-1", "publish-fail.mp4.partial")
	publishFailure.PublishedOutputPath = filepath.Join(directory, "organized", "SOURCE-1", "publish-fail.mp4")
	publishFailure.ArtifactPath = "organized/SOURCE-1/publish-fail.mp4"
	if failed, err := publishRenderer.Render(context.Background(), publishFailure); err == nil || failed.Rendition.Artifact.ID != "" {
		t.Fatalf("video publish failure returned a completed rendition: result=%+v err=%v", failed, err)
	}

	if err := markerRoot.MkdirAll("organized/SOURCE-1/marker-target", 0o755); err != nil {
		t.Fatal(err)
	}
	markerFailure := verificationFailure
	markerFailure.RenditionID = "RENDITION-MARKER-FAIL"
	markerFailure.TemporaryOutputPath = filepath.Join(directory, "organized", "SOURCE-1", "marker-fail.mp4.partial")
	markerFailure.PublishedOutputPath = filepath.Join(directory, "organized", "SOURCE-1", "marker-fail.mp4")
	markerFailure.ArtifactPath = "organized/SOURCE-1/marker-fail.mp4"
	markerFailure.TemporaryMarkerPath = "organized/SOURCE-1/marker-fail.json.partial"
	markerFailure.PublishedMarkerPath = "organized/SOURCE-1/marker-target"
	if failed, err := renderer.Render(context.Background(), markerFailure); err == nil || failed.Rendition.Artifact.ID != "" {
		t.Fatalf("marker publish failure returned a completed rendition: result=%+v err=%v", failed, err)
	}
	for _, attempt := range []SequenceRequest{request, verificationFailure, publishFailure, markerFailure} {
		if _, err := os.Stat(attempt.TemporaryOutputPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("left temporary sequence %s: %v", attempt.TemporaryOutputPath, err)
		}
		if exists, err := markerRoot.Exists(attempt.TemporaryMarkerPath); err != nil || exists {
			t.Fatalf("left temporary marker %s: %v", attempt.TemporaryMarkerPath, err)
		}
	}
}

type failingSequencePublisher struct{}

func (failingSequencePublisher) Publish(string, string) error {
	return errors.New("publish failed")
}

func containsArgumentPair(arguments []string, key, value string) bool {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == key && arguments[index+1] == value {
			return true
		}
	}
	return false
}
