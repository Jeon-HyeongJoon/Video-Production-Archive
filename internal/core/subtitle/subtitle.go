package subtitle

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidProfile  = errors.New("subtitle profile is incomplete or invalid")
	ErrInvalidSubtitle = errors.New("subtitle does not match its transcript provenance")
)

var profileIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

type Profile struct {
	ID      string `json:"id"`
	Version uint32 `json:"version"`
}

func NewProfile(source Profile) (Profile, error) {
	profile := source
	profile.ID = strings.ToLower(strings.TrimSpace(profile.ID))
	if !profileIDPattern.MatchString(profile.ID) || profile.Version == 0 {
		return Profile{}, ErrInvalidProfile
	}
	return profile, nil
}

func (profile Profile) Reference() string {
	return fmt.Sprintf("%s@v%d", profile.ID, profile.Version)
}

type Subtitle struct {
	Artifact       archive.Artifact        `json:"artifact"`
	TranscriptID   string                  `json:"transcript_id"`
	ProfileID      string                  `json:"profile_id"`
	ProfileVersion uint32                  `json:"profile_version"`
	Language       string                  `json:"language"`
	Segments       []transcription.Segment `json:"segments"`
}

func NewSubtitle(artifact archive.Artifact, profile Profile, transcript transcription.Transcript) (Subtitle, error) {
	normalizedProfile, err := NewProfile(profile)
	if err != nil {
		return Subtitle{}, err
	}
	if transcript.Validate() != nil || artifact.ValidateDerived() != nil ||
		artifact.Origin.ProcessingKind != vocabulary.ProcessingSTT ||
		artifact.Origin.OriginalAssetID != transcript.Artifact.Origin.OriginalAssetID ||
		artifact.Origin.ProfileVersion != normalizedProfile.Reference() ||
		!reflect.DeepEqual(artifact.Origin.InputArtifactIDs, []string{transcript.Artifact.ID}) {
		return Subtitle{}, ErrInvalidSubtitle
	}
	return Subtitle{
		Artifact: artifact, TranscriptID: transcript.Artifact.ID,
		ProfileID: normalizedProfile.ID, ProfileVersion: normalizedProfile.Version,
		Language: transcript.Content.Language,
		Segments: append([]transcription.Segment(nil), transcript.Content.Segments...),
	}, nil
}

func (subtitle Subtitle) Validate() error {
	if subtitle.Artifact.ValidateDerived() != nil || subtitle.Artifact.Origin.ProcessingKind != vocabulary.ProcessingSTT ||
		subtitle.TranscriptID == "" || !profileIDPattern.MatchString(subtitle.ProfileID) || subtitle.ProfileVersion == 0 ||
		subtitle.Artifact.Origin.ProfileVersion != fmt.Sprintf("%s@v%d", subtitle.ProfileID, subtitle.ProfileVersion) ||
		!reflect.DeepEqual(subtitle.Artifact.Origin.InputArtifactIDs, []string{subtitle.TranscriptID}) {
		return ErrInvalidSubtitle
	}
	content, err := transcription.NewContent(subtitle.Language, subtitle.Segments)
	if err != nil || !reflect.DeepEqual(content.Segments, subtitle.Segments) || content.Language != subtitle.Language {
		return ErrInvalidSubtitle
	}
	return nil
}

func RenderSRT(segments []transcription.Segment) ([]byte, error) {
	content, err := transcription.NewContent("und", segments)
	if err != nil {
		return nil, ErrInvalidSubtitle
	}
	var builder strings.Builder
	for _, segment := range content.Segments {
		fmt.Fprintf(&builder, "%d\n%s --> %s\n%s\n\n", segment.Sequence, timestamp(segment.StartMillis), timestamp(segment.EndMillis), segment.Text)
	}
	return []byte(builder.String()), nil
}

func timestamp(milliseconds int64) string {
	hours := milliseconds / 3_600_000
	milliseconds %= 3_600_000
	minutes := milliseconds / 60_000
	milliseconds %= 60_000
	seconds := milliseconds / 1_000
	milliseconds %= 1_000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", hours, minutes, seconds, milliseconds)
}
