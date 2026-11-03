package ffmpeg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"videoarchive/internal/core/rendering"
)

const ffmpegFixtureProcess = "ARCHIVE_FFMPEG_FIXTURE_PROCESS"

func TestMain(m *testing.M) {
	if os.Getenv(ffmpegFixtureProcess) == "1" {
		arguments := os.Args[1:]
		if err := os.WriteFile(os.Getenv("FFMPEG_FIXTURE_ARGUMENTS"), []byte(strings.Join(arguments, "\n")+"\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if len(arguments) == 0 {
			fmt.Fprintln(os.Stderr, "ffmpeg fixture received no output argument")
			os.Exit(2)
		}
		if err := os.WriteFile(arguments[len(arguments)-1], []byte(os.Getenv("FFMPEG_FIXTURE_PAYLOAD")), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func ffmpegFixtureExecutable(t *testing.T, argumentsPath string, payload []byte) string {
	t.Helper()
	executablePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(ffmpegFixtureProcess, "1")
	t.Setenv("FFMPEG_FIXTURE_ARGUMENTS", argumentsPath)
	t.Setenv("FFMPEG_FIXTURE_PAYLOAD", string(payload))
	return executablePath
}

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
	payload := []byte("verified-rendered-video")
	executablePath := ffmpegFixtureExecutable(t, argumentsPath, payload)

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

// Requirement: RND-005
func TestFFmpegAdapterUsesFixedTargetBitrateArgumentsForOrganizationProfiles(t *testing.T) {
	for _, target := range []int{6_000, 20_000} {
		t.Run(fmt.Sprintf("%dkbps", target), func(t *testing.T) {
			directory := t.TempDir()
			argumentsPath := filepath.Join(directory, "arguments.txt")
			payload := []byte(fmt.Sprintf("verified-%d", target))
			executablePath := ffmpegFixtureExecutable(t, argumentsPath, payload)
			renderer, err := NewRenderer(executablePath, "ffmpeg-8.0", "WORKER-1", OSRunner{}, payloadVerifier{expected: payload}, AtomicPublisher{})
			if err != nil {
				t.Fatal(err)
			}
			profile, err := rendering.OrganizationProfile(target)
			if err != nil {
				t.Fatal(err)
			}
			temporaryPath := filepath.Join(directory, "output.mp4.partial")
			publishedPath := filepath.Join(directory, "output.mp4")
			result, err := renderer.Render(context.Background(), Request{
				RenditionID: fmt.Sprintf("RENDITION-%d", target), OriginalAssetID: "ASSET-1",
				ResolvedInputPath: filepath.Join(directory, "input.mov"), TemporaryOutputPath: temporaryPath,
				PublishedOutputPath: publishedPath, ArtifactPath: "organized/MEDIA-1/video.mp4", Profile: profile,
			})
			if err != nil {
				t.Fatal(err)
			}
			argumentPayload, err := os.ReadFile(argumentsPath)
			if err != nil {
				t.Fatal(err)
			}
			arguments := strings.Split(strings.TrimSuffix(string(argumentPayload), "\n"), "\n")
			bitrate := fmt.Sprintf("%dk", target)
			bufsize := fmt.Sprintf("%dk", target*2)
			for _, pair := range [][2]string{{"-b:v", bitrate}, {"-maxrate", bitrate}, {"-bufsize", bufsize}} {
				found := false
				for index := 0; index+1 < len(arguments); index++ {
					if arguments[index] == pair[0] && arguments[index+1] == pair[1] {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("missing fixed rate control %q in %q", pair, arguments)
				}
			}
			if result.Artifact.Origin.ProfileVersion != profile.Reference() {
				t.Fatalf("published rendition lost profile provenance: %+v", result)
			}
		})
	}
}
