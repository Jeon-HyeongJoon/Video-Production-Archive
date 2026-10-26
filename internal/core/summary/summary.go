package summary

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

const (
	KeywordLimit = 12
	ChapterLimit = 24
)

var (
	ErrInvalidProfile = errors.New("summary profile is incomplete or invalid")
	ErrInvalidContent = errors.New("summary content is incomplete or invalid")
	ErrInvalidSummary = errors.New("summary does not match its transcript provenance")
)

var (
	profileIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	languagePattern  = regexp.MustCompile(`^[a-z]{2,3}$`)
)

type Profile struct {
	ID          string `json:"id"`
	Version     uint32 `json:"version"`
	Language    string `json:"language"`
	MaxKeywords int    `json:"max_keywords"`
	MaxChapters int    `json:"max_chapters"`
}

func NewProfile(source Profile) (Profile, error) {
	profile := source
	profile.ID = strings.ToLower(strings.TrimSpace(profile.ID))
	profile.Language = strings.ToLower(strings.TrimSpace(profile.Language))
	if !profileIDPattern.MatchString(profile.ID) || profile.Version == 0 || !languagePattern.MatchString(profile.Language) ||
		profile.MaxKeywords != KeywordLimit || profile.MaxChapters != ChapterLimit {
		return Profile{}, ErrInvalidProfile
	}
	return profile, nil
}

func (profile Profile) Reference() string {
	return fmt.Sprintf("%s@v%d", profile.ID, profile.Version)
}

type Chapter struct {
	Sequence    uint32 `json:"sequence"`
	StartMillis int64  `json:"start_millis"`
	EndMillis   int64  `json:"end_millis"`
	Title       string `json:"title"`
}

type Content struct {
	Text     string    `json:"text"`
	Keywords []string  `json:"keywords"`
	Chapters []Chapter `json:"chapters"`
}

func NewContent(profile Profile, transcript transcription.Transcript, source Content) (Content, error) {
	normalizedProfile, err := NewProfile(profile)
	if err != nil {
		return Content{}, err
	}
	if transcript.Validate() != nil || transcript.Content.Language != normalizedProfile.Language {
		return Content{}, ErrInvalidContent
	}
	return normalizeContent(source, transcript.Content.Segments[len(transcript.Content.Segments)-1].EndMillis)
}

func normalizeContent(source Content, transcriptEndMillis int64) (Content, error) {
	content := Content{Text: strings.TrimSpace(source.Text)}
	seenKeywords := make(map[string]bool, len(source.Keywords))
	for _, value := range source.Keywords {
		keyword := strings.ToLower(strings.TrimSpace(value))
		if keyword != "" && !seenKeywords[keyword] {
			seenKeywords[keyword] = true
			content.Keywords = append(content.Keywords, keyword)
		}
	}
	sort.Strings(content.Keywords)
	content.Chapters = append([]Chapter(nil), source.Chapters...)
	var previousEnd int64
	for index := range content.Chapters {
		chapter := &content.Chapters[index]
		chapter.Title = strings.TrimSpace(chapter.Title)
		if chapter.Sequence != uint32(index+1) || chapter.StartMillis < 0 || chapter.EndMillis <= chapter.StartMillis ||
			(index > 0 && chapter.StartMillis < previousEnd) || chapter.EndMillis > transcriptEndMillis || chapter.Title == "" {
			return Content{}, ErrInvalidContent
		}
		previousEnd = chapter.EndMillis
	}
	if content.Text == "" || len(content.Keywords) == 0 || len(content.Keywords) > KeywordLimit ||
		len(content.Chapters) == 0 || len(content.Chapters) > ChapterLimit || transcriptEndMillis <= 0 {
		return Content{}, ErrInvalidContent
	}
	return content, nil
}

type Summary struct {
	ID                       string               `json:"id"`
	Revision                 uint64               `json:"revision"`
	Artifact                 archive.Artifact     `json:"artifact"`
	TranscriptID             string               `json:"transcript_id"`
	TranscriptDurationMillis int64                `json:"transcript_duration_millis"`
	ProfileID                string               `json:"profile_id"`
	ProfileVersion           uint32               `json:"profile_version"`
	Language                 string               `json:"language"`
	Content                  Content              `json:"content"`
	AuthorType               vocabulary.ActorType `json:"author_type"`
	AuthorID                 string               `json:"author_id"`
}

func NewSummary(id string, revision uint64, artifact archive.Artifact, profile Profile, transcript transcription.Transcript, authorType vocabulary.ActorType, authorID string, source Content) (Summary, error) {
	normalizedProfile, err := NewProfile(profile)
	if err != nil {
		return Summary{}, err
	}
	content, err := NewContent(normalizedProfile, transcript, source)
	authorID = strings.TrimSpace(authorID)
	if err != nil || strings.TrimSpace(id) == "" || revision == 0 ||
		(authorType != vocabulary.ActorHuman && authorType != vocabulary.ActorAgent) || authorID == "" ||
		artifact.ValidateDerived() != nil || artifact.Origin.ProcessingKind != vocabulary.ProcessingSummary ||
		artifact.Origin.OriginalAssetID != transcript.Artifact.Origin.OriginalAssetID ||
		artifact.Origin.ProfileVersion != normalizedProfile.Reference() || artifact.Origin.CreatedBy != authorID ||
		!reflect.DeepEqual(artifact.Origin.InputArtifactIDs, []string{transcript.Artifact.ID}) {
		return Summary{}, ErrInvalidSummary
	}
	return Summary{
		ID: strings.TrimSpace(id), Revision: revision, Artifact: artifact, TranscriptID: transcript.Artifact.ID,
		TranscriptDurationMillis: transcript.Content.Segments[len(transcript.Content.Segments)-1].EndMillis,
		ProfileID:                normalizedProfile.ID, ProfileVersion: normalizedProfile.Version, Language: normalizedProfile.Language,
		Content: content, AuthorType: authorType, AuthorID: authorID,
	}, nil
}

func (summary Summary) Validate() error {
	content, err := normalizeContent(summary.Content, summary.TranscriptDurationMillis)
	if err != nil || strings.TrimSpace(summary.ID) == "" || summary.ID != strings.TrimSpace(summary.ID) || summary.Revision == 0 ||
		summary.TranscriptID == "" || !profileIDPattern.MatchString(summary.ProfileID) || summary.ProfileVersion == 0 ||
		!languagePattern.MatchString(summary.Language) ||
		(summary.AuthorType != vocabulary.ActorHuman && summary.AuthorType != vocabulary.ActorAgent) ||
		summary.AuthorID == "" || summary.AuthorID != strings.TrimSpace(summary.AuthorID) ||
		summary.Artifact.ValidateDerived() != nil || summary.Artifact.Origin.ProcessingKind != vocabulary.ProcessingSummary ||
		summary.Artifact.Origin.ProfileVersion != fmt.Sprintf("%s@v%d", summary.ProfileID, summary.ProfileVersion) ||
		summary.Artifact.Origin.CreatedBy != summary.AuthorID ||
		!reflect.DeepEqual(summary.Artifact.Origin.InputArtifactIDs, []string{summary.TranscriptID}) ||
		!reflect.DeepEqual(content, summary.Content) {
		return ErrInvalidSummary
	}
	return nil
}
