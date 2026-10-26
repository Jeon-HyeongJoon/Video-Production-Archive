package transcription

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidProfile    = errors.New("transcription profile is incomplete or invalid")
	ErrInvalidContent    = errors.New("transcript content is incomplete or invalid")
	ErrInvalidTranscript = errors.New("transcript does not match its STT provenance")
)

var (
	profileIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	modelIDPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	languagePattern  = regexp.MustCompile(`^[a-z]{2,3}$`)
)

// Profile is a versioned, resource-bounded whisper.cpp parameter set. The
// resolved model file path belongs to the Adapter and is not part of this
// portable domain value.
type Profile struct {
	ID          string `json:"id"`
	Version     uint32 `json:"version"`
	ModelID     string `json:"model_id"`
	Language    string `json:"language"`
	ThreadCount int    `json:"thread_count"`
}

func NewProfile(source Profile) (Profile, error) {
	profile := source
	profile.ID = strings.ToLower(strings.TrimSpace(source.ID))
	profile.ModelID = strings.ToLower(strings.TrimSpace(source.ModelID))
	profile.Language = strings.ToLower(strings.TrimSpace(source.Language))
	if !profileIDPattern.MatchString(profile.ID) || profile.Version == 0 ||
		!modelIDPattern.MatchString(profile.ModelID) ||
		(profile.Language != "auto" && !languagePattern.MatchString(profile.Language)) ||
		profile.ThreadCount < 1 || profile.ThreadCount > 16 {
		return Profile{}, ErrInvalidProfile
	}
	return profile, nil
}

func (profile Profile) Reference() string {
	return fmt.Sprintf("%s@v%d", profile.ID, profile.Version)
}

type Segment struct {
	Sequence    uint32 `json:"sequence"`
	StartMillis int64  `json:"start_millis"`
	EndMillis   int64  `json:"end_millis"`
	Text        string `json:"text"`
}

type Content struct {
	Language string    `json:"language"`
	Segments []Segment `json:"segments"`
}

func NewContent(language string, source []Segment) (Content, error) {
	language = strings.ToLower(strings.TrimSpace(language))
	segments := append([]Segment(nil), source...)
	var previousEnd int64
	for index := range segments {
		segment := &segments[index]
		segment.Text = strings.TrimSpace(segment.Text)
		if segment.Sequence != uint32(index+1) || segment.StartMillis < 0 || segment.EndMillis <= segment.StartMillis ||
			(index > 0 && segment.StartMillis < previousEnd) || segment.Text == "" {
			return Content{}, ErrInvalidContent
		}
		previousEnd = segment.EndMillis
	}
	if !languagePattern.MatchString(language) || len(segments) == 0 {
		return Content{}, ErrInvalidContent
	}
	return Content{Language: language, Segments: segments}, nil
}

// Transcript is a published Artifact with a stable, searchable timed-content
// view. Artifact owns file lifecycle and provenance; Transcript owns STT
// language and segment meaning.
type Transcript struct {
	Artifact       archive.Artifact `json:"artifact"`
	ProfileID      string           `json:"profile_id"`
	ProfileVersion uint32           `json:"profile_version"`
	Content        Content          `json:"content"`
}

func NewTranscript(artifact archive.Artifact, profile Profile, source Content) (Transcript, error) {
	normalizedProfile, err := NewProfile(profile)
	if err != nil {
		return Transcript{}, err
	}
	content, err := NewContent(source.Language, source.Segments)
	if err != nil || artifact.ValidateDerived() != nil || artifact.Origin.ProcessingKind != vocabulary.ProcessingSTT ||
		artifact.Origin.ProfileVersion != normalizedProfile.Reference() || len(artifact.Origin.InputArtifactIDs) != 1 ||
		(normalizedProfile.Language != "auto" && content.Language != normalizedProfile.Language) {
		return Transcript{}, ErrInvalidTranscript
	}
	return Transcript{
		Artifact: artifact, ProfileID: normalizedProfile.ID, ProfileVersion: normalizedProfile.Version, Content: content,
	}, nil
}
