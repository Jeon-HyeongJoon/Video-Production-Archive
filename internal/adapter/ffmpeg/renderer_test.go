package ffmpeg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"videoarchive/internal/core/rendering"
)

type payloadVerifier struct {
	expected []byte
}

func (verifier payloadVerifier) Verify(filePath string) error {
	payload, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(payload, verifier.expected) {
		return errors.New("rendered payload did not pass verification")
	}
	return nil
}

// Requirement: RND-002
func TestFFmpegAdapterRendersVerifiedProfileWithoutShellAndPublishesRendition(t *testing.T) {
	directory := t.TempDir()
	argumentsPath := filepath.Join(directory, "arguments.txt")
	executablePath := filepath.Join(directory, "ffmpeg-fixture")
	payload := []byte("verified-rendered-video")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$FFMPEG_ARGUMENTS\"\nfor argument in \"$@\"; do output=\"$argument\"; done\nprintf '%s' \"$FFMPEG_PAYLOAD\" > \"$output\"\n"
	if err := os.WriteFile(executablePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FFMPEG_ARGUMENTS", argumentsPath)
	t.Setenv("FFMPEG_PAYLOAD", string(payload))

	renderer, err := NewRenderer(executablePath, "ffmpeg-8.0", "WORKER-1", OSRunner{}, payloadVerifier{expected: payload}, AtomicPublisher{})
	if err != nil {
		t.Fatal(err)
	}
	profile := rendering.Profile{
		ID: "archive-event-1080p", Version: 1,
		Container: rendering.ContainerMP4, VideoEncoder: rendering.VideoEncoderX264,
		AudioEncoder: rendering.AudioEncoderAAC, PixelFormat: rendering.PixelFormatYUV420P,
		MaxWidth: 1920, MaxHeight: 1080, ConstantRateFactor: 20, AudioBitRateKbps: 192, ThreadCount: 4,
	}
	inputPath := filepath.Join(directory, "camera", "clip & keep.mov")
	temporaryPath := filepath.Join(directory, "output", "proxy.mp4.partial")
	publishedPath := filepath.Join(directory, "output", "proxy.mp4")
	rendition, err := renderer.Render(context.Background(), Request{
		RenditionID: "RENDITION-1", OriginalAssetID: "ASSET-1", ResolvedInputPath: inputPath,
		TemporaryOutputPath: temporaryPath, PublishedOutputPath: publishedPath,
		ArtifactPath: "proxy/clip.mp4", Profile: profile,
	})
	if err != nil {
		t.Fatal(err)
	}

	wantArguments := []string{
		"-hide_banner", "-nostdin", "-y", "-i", inputPath,
		"-map", "0:v:0", "-map", "0:a?",
		"-vf", "scale=1920:1080:force_original_aspect_ratio=decrease:force_divisible_by=2",
		"-c:v", "libx264", "-crf", "20", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "192k", "-threads", "4",
		"-movflags", "+faststart", "-f", "mp4", temporaryPath,
	}
	argumentPayload, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	gotArguments := strings.Split(strings.TrimSuffix(string(argumentPayload), "\n"), "\n")
	if !reflect.DeepEqual(gotArguments, wantArguments) {
		t.Fatalf("ffmpeg arguments=%q want=%q", gotArguments, wantArguments)
	}
	if _, err := os.Stat(temporaryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary output remained public: %v", err)
	}
	published, err := os.ReadFile(publishedPath)
	if err != nil || !reflect.DeepEqual(published, payload) {
		t.Fatalf("verified output was not published: payload=%q err=%v", published, err)
	}
	digest := sha256.Sum256(payload)
	artifact := rendition.Artifact
	if rendition.ProfileID != profile.ID || rendition.ProfileVersion != profile.Version || artifact.ID != "RENDITION-1" ||
		artifact.Path != "proxy/clip.mp4" || artifact.Size != int64(len(payload)) || artifact.Checksum != hex.EncodeToString(digest[:]) ||
		artifact.Origin.OriginalAssetID != "ASSET-1" || artifact.Origin.ProfileVersion != "archive-event-1080p@v1" ||
		artifact.Origin.ProcessorVersion != "ffmpeg-8.0" || artifact.Origin.CreatedBy != "WORKER-1" || artifact.Origin.CreatedAt.IsZero() {
		t.Fatalf("rendition provenance is incomplete: %+v", rendition)
	}
}
