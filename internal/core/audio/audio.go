package audio

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidProfile       = errors.New("audio extraction profile is incomplete or invalid")
	ErrUnsupportedProfile   = errors.New("audio extraction profile is unsupported in v1")
	ErrInvalidAudioArtifact = errors.New("audio artifact does not match its extraction profile")
)

const (
	ContainerWAV   = "wav"
	CodecPCM16LE   = "pcm_s16le"
	SpeechRateHz   = 16_000
	SpeechChannels = 1
)

var profileIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Profile defines the stable speech-audio format handed from FFmpeg to STT.
// V1 deliberately uses uncompressed mono PCM so the recognition Adapter does
// not need to infer codec or resampling behavior.
type Profile struct {
	ID           string `json:"id"`
	Version      uint32 `json:"version"`
	Container    string `json:"container"`
	Codec        string `json:"codec"`
	SampleRateHz int    `json:"sample_rate_hz"`
	Channels     int    `json:"channels"`
	ThreadCount  int    `json:"thread_count"`
}

func NewProfile(source Profile) (Profile, error) {
	profile := source
	profile.ID = strings.ToLower(strings.TrimSpace(source.ID))
	profile.Container = strings.ToLower(strings.TrimSpace(source.Container))
	profile.Codec = strings.ToLower(strings.TrimSpace(source.Codec))
	if !profileIDPattern.MatchString(profile.ID) || profile.Version == 0 || profile.ThreadCount < 1 || profile.ThreadCount > 4 {
		return Profile{}, ErrInvalidProfile
	}
	if profile.Container != ContainerWAV || profile.Codec != CodecPCM16LE ||
		profile.SampleRateHz != SpeechRateHz || profile.Channels != SpeechChannels {
		return Profile{}, ErrUnsupportedProfile
	}
	return profile, nil
}

func (profile Profile) Reference() string {
	return fmt.Sprintf("%s@v%d", profile.ID, profile.Version)
}

type AudioArtifact struct {
	Artifact       archive.Artifact `json:"artifact"`
	ProfileID      string           `json:"profile_id"`
	ProfileVersion uint32           `json:"profile_version"`
	Container      string           `json:"container"`
	Codec          string           `json:"codec"`
	SampleRateHz   int              `json:"sample_rate_hz"`
	Channels       int              `json:"channels"`
}

func NewAudioArtifact(artifact archive.Artifact, source Profile) (AudioArtifact, error) {
	profile, err := NewProfile(source)
	if err != nil || artifact.ValidateDerived() != nil || artifact.Origin.ProcessingKind != vocabulary.ProcessingAudioExtract ||
		artifact.Origin.ProfileVersion != profile.Reference() {
		return AudioArtifact{}, ErrInvalidAudioArtifact
	}
	return AudioArtifact{
		Artifact: artifact, ProfileID: profile.ID, ProfileVersion: profile.Version,
		Container: profile.Container, Codec: profile.Codec, SampleRateHz: profile.SampleRateHz, Channels: profile.Channels,
	}, nil
}

func (artifact AudioArtifact) Validate() error {
	profile := Profile{
		ID: artifact.ProfileID, Version: artifact.ProfileVersion, Container: artifact.Container, Codec: artifact.Codec,
		SampleRateHz: artifact.SampleRateHz, Channels: artifact.Channels, ThreadCount: 1,
	}
	_, err := NewAudioArtifact(artifact.Artifact, profile)
	return err
}
