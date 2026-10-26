package audio

import (
	"errors"
	"testing"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/vocabulary"
)

const extractedAudioChecksum = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// Requirement: AUD-001
func TestSpeechAudioProfileCreatesVersionedPCMArtifact(t *testing.T) {
	profile, err := NewProfile(Profile{
		ID: " Speech-PCM-16K ", Version: 1, Container: " WAV ", Codec: " PCM_S16LE ",
		SampleRateHz: SpeechRateHz, Channels: SpeechChannels, ThreadCount: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact := archive.Artifact{
		ID: "AUDIO-1", Path: "audio/event.wav", Size: 2_048, Checksum: extractedAudioChecksum,
		Origin: archive.Provenance{
			OriginalAssetID: "ASSET-1", InputArtifactIDs: []string{}, ProcessingKind: vocabulary.ProcessingAudioExtract,
			ProcessorVersion: "ffmpeg-8.0", ProfileVersion: profile.Reference(), CreatedBy: "WORKER-1", CreatedAt: time.Now().UTC(),
		},
	}
	audio, err := NewAudioArtifact(artifact, profile)
	if err != nil {
		t.Fatal(err)
	}
	if profile.ID != "speech-pcm-16k" || profile.Container != ContainerWAV || profile.Codec != CodecPCM16LE ||
		profile.Reference() != "speech-pcm-16k@v1" || audio.ProfileID != profile.ID || audio.ProfileVersion != profile.Version ||
		audio.SampleRateHz != SpeechRateHz || audio.Channels != SpeechChannels || audio.Validate() != nil {
		t.Fatalf("speech audio profile or artifact was not normalized: profile=%+v audio=%+v", profile, audio)
	}
	_, err = NewProfile(Profile{
		ID: "speech-lossy", Version: 1, Container: "mp3", Codec: "mp3", SampleRateHz: SpeechRateHz, Channels: SpeechChannels, ThreadCount: 2,
	})
	if !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("unsupported STT input profile was accepted: %v", err)
	}
}
