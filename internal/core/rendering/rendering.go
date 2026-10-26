package rendering

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidProfile     = errors.New("render profile is incomplete or invalid")
	ErrUnsupportedProfile = errors.New("render profile uses an unsupported v1 codec")
	ErrInvalidRendition   = errors.New("rendition does not match its render profile")
)

const (
	ContainerMP4       = "mp4"
	VideoEncoderX264   = "libx264"
	AudioEncoderAAC    = "aac"
	PixelFormatYUV420P = "yuv420p"
)

var profileIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Profile is a versioned, resource-bounded FFmpeg parameter set. V1 begins
// with the universally decodable H.264/AAC MP4 path; hardware profiles can be
// added as separately validated profiles after Windows acceptance evidence.
type Profile struct {
	ID                 string `json:"id"`
	Version            uint32 `json:"version"`
	Container          string `json:"container"`
	VideoEncoder       string `json:"video_encoder"`
	AudioEncoder       string `json:"audio_encoder"`
	PixelFormat        string `json:"pixel_format"`
	MaxWidth           int    `json:"max_width"`
	MaxHeight          int    `json:"max_height"`
	ConstantRateFactor int    `json:"constant_rate_factor"`
	AudioBitRateKbps   int    `json:"audio_bit_rate_kbps"`
	ThreadCount        int    `json:"thread_count"`
}

func NewProfile(source Profile) (Profile, error) {
	profile := source
	profile.ID = strings.ToLower(strings.TrimSpace(profile.ID))
	profile.Container = strings.ToLower(strings.TrimSpace(profile.Container))
	profile.VideoEncoder = strings.ToLower(strings.TrimSpace(profile.VideoEncoder))
	profile.AudioEncoder = strings.ToLower(strings.TrimSpace(profile.AudioEncoder))
	profile.PixelFormat = strings.ToLower(strings.TrimSpace(profile.PixelFormat))
	if !profileIDPattern.MatchString(profile.ID) || profile.Version == 0 ||
		profile.MaxWidth <= 0 || profile.MaxHeight <= 0 || profile.MaxWidth%2 != 0 || profile.MaxHeight%2 != 0 ||
		profile.ConstantRateFactor < 0 || profile.ConstantRateFactor > 51 ||
		profile.AudioBitRateKbps < 32 || profile.AudioBitRateKbps > 512 ||
		profile.ThreadCount < 1 || profile.ThreadCount > 16 {
		return Profile{}, ErrInvalidProfile
	}
	if profile.Container != ContainerMP4 || profile.VideoEncoder != VideoEncoderX264 ||
		profile.AudioEncoder != AudioEncoderAAC || profile.PixelFormat != PixelFormatYUV420P {
		return Profile{}, ErrUnsupportedProfile
	}
	return profile, nil
}

func (profile Profile) Reference() string {
	return fmt.Sprintf("%s@v%d", profile.ID, profile.Version)
}

type Rendition struct {
	Artifact       archive.Artifact `json:"artifact"`
	ProfileID      string           `json:"profile_id"`
	ProfileVersion uint32           `json:"profile_version"`
}

func NewRendition(artifact archive.Artifact, profile Profile) (Rendition, error) {
	normalized, err := NewProfile(profile)
	if err != nil || artifact.ValidateDerived() != nil || artifact.Origin.ProcessingKind != vocabulary.ProcessingRender ||
		artifact.Origin.ProfileVersion != normalized.Reference() {
		return Rendition{}, ErrInvalidRendition
	}
	return Rendition{Artifact: artifact, ProfileID: normalized.ID, ProfileVersion: normalized.Version}, nil
}
