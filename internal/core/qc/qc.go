package qc

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/rendering"
)

var ErrInvalidAssessment = errors.New("qc assessment input is incomplete or inconsistent")

type Outcome string

const (
	Passed Outcome = "PASSED"
	Failed Outcome = "FAILED"
)

type CheckKind string

const (
	CheckDecodable  CheckKind = "DECODABLE"
	CheckChecksum   CheckKind = "CHECKSUM"
	CheckFileSize   CheckKind = "FILE_SIZE"
	CheckContainer  CheckKind = "CONTAINER"
	CheckVideoCodec CheckKind = "VIDEO_CODEC"
	CheckAudioCodec CheckKind = "AUDIO_CODEC"
	CheckDimensions CheckKind = "DIMENSIONS"
	CheckDuration   CheckKind = "DURATION"
)

type OutputObservation struct {
	Checksum       string   `json:"checksum"`
	SizeBytes      int64    `json:"size_bytes"`
	Containers     []string `json:"containers"`
	DurationMillis int64    `json:"duration_millis"`
	VideoCodec     string   `json:"video_codec"`
	AudioCodec     string   `json:"audio_codec"`
	Width          int      `json:"width"`
	Height         int      `json:"height"`
	Decodable      bool     `json:"decodable"`
}

type Policy struct {
	DurationToleranceMillis int64 `json:"duration_tolerance_millis"`
}

type Check struct {
	Kind     CheckKind `json:"kind"`
	Passed   bool      `json:"passed"`
	Expected string    `json:"expected"`
	Actual   string    `json:"actual"`
}

type QCAssessment struct {
	ID                 string    `json:"id"`
	TargetArtifactID   string    `json:"target_artifact_id"`
	TargetChecksum     string    `json:"target_checksum"`
	SourceInspectionID string    `json:"source_inspection_id"`
	ProfileReference   string    `json:"profile_reference"`
	Outcome            Outcome   `json:"outcome"`
	Checks             []Check   `json:"checks"`
	CreatedBy          string    `json:"created_by"`
	CreatedAt          time.Time `json:"created_at"`
}

func Evaluate(assessmentID string, rendition rendering.Rendition, profile rendering.Profile, source mediaanalysis.MediaInspection, output OutputObservation, policy Policy, workerID string, createdAt time.Time) (QCAssessment, error) {
	normalizedProfile, err := rendering.NewProfile(profile)
	if err != nil {
		return QCAssessment{}, ErrInvalidAssessment
	}
	validatedRendition, err := rendering.NewRendition(rendition.Artifact, normalizedProfile)
	if err != nil || validatedRendition.ProfileID != rendition.ProfileID || validatedRendition.ProfileVersion != rendition.ProfileVersion {
		return QCAssessment{}, ErrInvalidAssessment
	}
	normalizedSource, err := mediaanalysis.NewMediaInspection(source)
	if err != nil || normalizedSource.OriginalAssetID != rendition.Artifact.Origin.OriginalAssetID {
		return QCAssessment{}, ErrInvalidAssessment
	}
	output = normalizeOutput(output)
	if strings.TrimSpace(assessmentID) == "" || strings.TrimSpace(workerID) == "" || createdAt.IsZero() ||
		policy.DurationToleranceMillis < 0 || output.Checksum == "" || output.SizeBytes <= 0 ||
		len(output.Containers) == 0 || output.DurationMillis <= 0 {
		return QCAssessment{}, ErrInvalidAssessment
	}

	durationDelta := output.DurationMillis - normalizedSource.DurationMillis
	if durationDelta < 0 {
		durationDelta = -durationDelta
	}
	checks := []Check{
		{Kind: CheckDecodable, Passed: output.Decodable, Expected: "true", Actual: strconv.FormatBool(output.Decodable)},
		{Kind: CheckChecksum, Passed: output.Checksum == rendition.Artifact.Checksum, Expected: rendition.Artifact.Checksum, Actual: output.Checksum},
		{Kind: CheckFileSize, Passed: output.SizeBytes == rendition.Artifact.Size, Expected: strconv.FormatInt(rendition.Artifact.Size, 10), Actual: strconv.FormatInt(output.SizeBytes, 10)},
		{Kind: CheckContainer, Passed: contains(output.Containers, normalizedProfile.Container), Expected: normalizedProfile.Container, Actual: strings.Join(output.Containers, ",")},
		{Kind: CheckVideoCodec, Passed: output.VideoCodec == "h264", Expected: "h264", Actual: output.VideoCodec},
		{Kind: CheckAudioCodec, Passed: output.AudioCodec == "aac", Expected: "aac", Actual: output.AudioCodec},
		{Kind: CheckDimensions, Passed: output.Width <= normalizedProfile.MaxWidth && output.Height <= normalizedProfile.MaxHeight && output.Width%2 == 0 && output.Height%2 == 0,
			Expected: fmt.Sprintf("even <= %dx%d", normalizedProfile.MaxWidth, normalizedProfile.MaxHeight), Actual: fmt.Sprintf("%dx%d", output.Width, output.Height)},
		{Kind: CheckDuration, Passed: durationDelta <= policy.DurationToleranceMillis,
			Expected: fmt.Sprintf("%d±%dms", normalizedSource.DurationMillis, policy.DurationToleranceMillis), Actual: strconv.FormatInt(output.DurationMillis, 10) + "ms"},
	}
	outcome := Passed
	for _, check := range checks {
		if !check.Passed {
			outcome = Failed
			break
		}
	}
	return QCAssessment{
		ID: strings.TrimSpace(assessmentID), TargetArtifactID: rendition.Artifact.ID, TargetChecksum: rendition.Artifact.Checksum,
		SourceInspectionID: normalizedSource.ID, ProfileReference: normalizedProfile.Reference(), Outcome: outcome,
		Checks: checks, CreatedBy: strings.TrimSpace(workerID), CreatedAt: createdAt,
	}, nil
}

func normalizeOutput(output OutputObservation) OutputObservation {
	output.Checksum = strings.ToLower(strings.TrimSpace(output.Checksum))
	output.VideoCodec = strings.ToLower(strings.TrimSpace(output.VideoCodec))
	output.AudioCodec = strings.ToLower(strings.TrimSpace(output.AudioCodec))
	uniqueContainers := make(map[string]bool, len(output.Containers))
	for _, container := range output.Containers {
		container = strings.ToLower(strings.TrimSpace(container))
		if container != "" {
			uniqueContainers[container] = true
		}
	}
	output.Containers = make([]string, 0, len(uniqueContainers))
	for container := range uniqueContainers {
		output.Containers = append(output.Containers, container)
	}
	sort.Strings(output.Containers)
	return output
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
