package qc

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/organization"
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
	CheckDecodable     CheckKind = "DECODABLE"
	CheckChecksum      CheckKind = "CHECKSUM"
	CheckFileSize      CheckKind = "FILE_SIZE"
	CheckContainer     CheckKind = "CONTAINER"
	CheckVideoCodec    CheckKind = "VIDEO_CODEC"
	CheckAudioCodec    CheckKind = "AUDIO_CODEC"
	CheckDimensions    CheckKind = "DIMENSIONS"
	CheckDuration      CheckKind = "DURATION"
	CheckVideoDuration CheckKind = "VIDEO_DURATION"
)

type OutputObservation struct {
	Checksum            string   `json:"checksum"`
	SizeBytes           int64    `json:"size_bytes"`
	Containers          []string `json:"containers"`
	DurationMillis      int64    `json:"duration_millis"`
	VideoDurationMillis int64    `json:"video_duration_millis,omitempty"`
	VideoCodec          string   `json:"video_codec"`
	AudioCodec          string   `json:"audio_codec"`
	Width               int      `json:"width"`
	Height              int      `json:"height"`
	Decodable           bool     `json:"decodable"`
}

// Failure carries the failed measurements without publishing an accepted QC output.
type Failure struct {
	AssessmentID       string  `json:"assessment_id"`
	TargetArtifactID   string  `json:"target_artifact_id"`
	SourceInspectionID string  `json:"source_inspection_id,omitempty"`
	SourceTimelineID   string  `json:"source_timeline_id,omitempty"`
	Checks             []Check `json:"checks"`
}

func (failure *Failure) Error() string { return "rendition failed measured QC checks" }

var evidenceID = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,160}$`)
var measurement = regexp.MustCompile(`^[a-zA-Z0-9±=<> ,._-]{0,128}$`)

func (failure *Failure) Safe() *Failure {
	if failure == nil || !evidenceID.MatchString(failure.AssessmentID) || !evidenceID.MatchString(failure.TargetArtifactID) ||
		(failure.SourceInspectionID != "" && !evidenceID.MatchString(failure.SourceInspectionID)) || (failure.SourceTimelineID != "" && !evidenceID.MatchString(failure.SourceTimelineID)) || len(failure.Checks) == 0 || len(failure.Checks) > 9 {
		return nil
	}
	safe := *failure
	safe.Checks = make([]Check, 0, len(failure.Checks))
	seen := map[CheckKind]bool{}
	for _, check := range failure.Checks {
		switch check.Kind {
		case CheckDecodable, CheckChecksum, CheckFileSize, CheckContainer, CheckVideoCodec, CheckAudioCodec, CheckDimensions, CheckDuration, CheckVideoDuration:
		default:
			return nil
		}
		if seen[check.Kind] || check.Passed {
			return nil
		}
		seen[check.Kind] = true
		for _, value := range []*string{&check.Expected, &check.Actual} {
			lower := strings.ToLower(*value)
			if !measurement.MatchString(*value) || strings.Contains(lower, "password") || strings.Contains(lower, "secret") || strings.Contains(lower, "credential") || strings.Contains(lower, "token") {
				*value = "withheld"
			}
		}
		safe.Checks = append(safe.Checks, check)
	}
	return &safe
}
func (assessment QCAssessment) Failure() *Failure {
	failure := Failure{AssessmentID: assessment.ID, TargetArtifactID: assessment.TargetArtifactID, SourceInspectionID: assessment.SourceInspectionID, SourceTimelineID: assessment.SourceTimelineID}
	for _, check := range assessment.Checks {
		if !check.Passed {
			failure.Checks = append(failure.Checks, check)
		}
	}
	return failure.Safe()
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
	SourceTimelineID   string    `json:"source_timeline_id,omitempty"`
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
	return evaluateExpectedDuration(
		assessmentID, rendition, normalizedProfile, normalizedSource.ID, "", normalizedSource.DurationMillis, normalizedSource.VideoStreams[0].DurationMillis,
		len(normalizedSource.AudioStreams) > 0, output, policy, workerID, createdAt,
	)
}

func EvaluateTimeline(assessmentID string, rendition rendering.Rendition, profile rendering.Profile, timeline organization.SourceTimeline, expectedAudio bool, output OutputObservation, policy Policy, workerID string, createdAt time.Time) (QCAssessment, error) {
	return EvaluateTimelineWithVideoDurations(assessmentID, rendition, profile, timeline, expectedAudio, nil, output, policy, workerID, createdAt)
}

func EvaluateTimelineWithVideoDurations(assessmentID string, rendition rendering.Rendition, profile rendering.Profile, timeline organization.SourceTimeline, expectedAudio bool, videoDurations map[string]int64, output OutputObservation, policy Policy, workerID string, createdAt time.Time) (QCAssessment, error) {
	normalizedProfile, err := rendering.NewProfile(profile)
	if err != nil {
		return QCAssessment{}, ErrInvalidAssessment
	}
	validatedRendition, err := rendering.NewRendition(rendition.Artifact, normalizedProfile)
	if err != nil || validatedRendition.ProfileID != rendition.ProfileID || validatedRendition.ProfileVersion != rendition.ProfileVersion || timeline.Validate() != nil {
		return QCAssessment{}, ErrInvalidAssessment
	}
	rootIDs := make(map[string]bool, len(rendition.Artifact.Origin.RootOriginalAssetIDs))
	for _, assetID := range rendition.Artifact.Origin.RootOriginalAssetIDs {
		rootIDs[assetID] = true
	}
	if rendition.Artifact.Origin.InputTimelineID != timeline.ID || len(rootIDs) != len(timeline.Clips) {
		return QCAssessment{}, ErrInvalidAssessment
	}
	for _, clip := range timeline.Clips {
		if !rootIDs[clip.OriginalAssetID] {
			return QCAssessment{}, ErrInvalidAssessment
		}
	}
	expectedVideo := timelineVideoDuration(timeline, expectedAudio, videoDurations)
	return evaluateExpectedDuration(assessmentID, rendition, normalizedProfile, "", timeline.ID, timeline.DurationMillis(), expectedVideo, expectedAudio, output, policy, workerID, createdAt)
}

func timelineVideoDuration(timeline organization.SourceTimeline, hasAudio bool, durations map[string]int64) int64 {
	var end int64
	for i, clip := range timeline.Clips {
		// The A/V concat filter starts the next clip at the segment boundary.
		// The last clip can end with audio only. For video-only concat each
		// segment ends with its mapped video stream.
		if hasAudio && i < len(timeline.Clips)-1 {
			end += clip.OutMillis - clip.InMillis
			continue
		}
		video := durations[clip.OriginalAssetID]
		if video <= clip.InMillis {
			return 0
		}
		end += min(video, clip.OutMillis) - clip.InMillis
	}
	return end
}

func evaluateExpectedDuration(assessmentID string, rendition rendering.Rendition, profile rendering.Profile, sourceInspectionID, sourceTimelineID string, expectedDuration, expectedVideo int64, expectedAudio bool, output OutputObservation, policy Policy, workerID string, createdAt time.Time) (QCAssessment, error) {
	output = normalizeOutput(output)
	if strings.TrimSpace(assessmentID) == "" || strings.TrimSpace(workerID) == "" || createdAt.IsZero() ||
		policy.DurationToleranceMillis < 0 || expectedDuration <= 0 || output.Checksum == "" || output.SizeBytes <= 0 ||
		len(output.Containers) == 0 || output.DurationMillis <= 0 {
		return QCAssessment{}, ErrInvalidAssessment
	}

	audioExpected := "aac"
	audioActual := output.AudioCodec
	audioPassed := output.AudioCodec == "aac"
	if !expectedAudio {
		audioExpected = "absent"
		audioPassed = output.AudioCodec == ""
		if audioActual == "" {
			audioActual = "absent"
		}
	}
	checks := []Check{
		{Kind: CheckDecodable, Passed: output.Decodable, Expected: "true", Actual: strconv.FormatBool(output.Decodable)},
		{Kind: CheckChecksum, Passed: output.Checksum == rendition.Artifact.Checksum, Expected: rendition.Artifact.Checksum, Actual: output.Checksum},
		{Kind: CheckFileSize, Passed: output.SizeBytes == rendition.Artifact.Size, Expected: strconv.FormatInt(rendition.Artifact.Size, 10), Actual: strconv.FormatInt(output.SizeBytes, 10)},
		{Kind: CheckContainer, Passed: contains(output.Containers, profile.Container), Expected: profile.Container, Actual: strings.Join(output.Containers, ",")},
		{Kind: CheckVideoCodec, Passed: output.VideoCodec == "h264", Expected: "h264", Actual: output.VideoCodec},
		{Kind: CheckAudioCodec, Passed: audioPassed, Expected: audioExpected, Actual: audioActual},
		{Kind: CheckDimensions, Passed: output.Width <= profile.MaxWidth && output.Height <= profile.MaxHeight && output.Width%2 == 0 && output.Height%2 == 0,
			Expected: fmt.Sprintf("even <= %dx%d", profile.MaxWidth, profile.MaxHeight), Actual: fmt.Sprintf("%dx%d", output.Width, output.Height)},
		durationCheck(CheckDuration, expectedDuration, output.DurationMillis, policy.DurationToleranceMillis),
	}
	if expectedVideo > 0 {
		checks = append(checks, durationCheck(CheckVideoDuration, expectedVideo, output.VideoDurationMillis, policy.DurationToleranceMillis))
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
		SourceInspectionID: sourceInspectionID, SourceTimelineID: sourceTimelineID, ProfileReference: profile.Reference(), Outcome: outcome,
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

func durationCheck(kind CheckKind, expected, observed, tolerance int64) Check {
	check := Check{Kind: kind, Expected: fmt.Sprintf("%d±%dms", expected, tolerance), Actual: "unavailable"}
	if observed > 0 {
		check.Actual = strconv.FormatInt(observed, 10) + "ms"
		check.Passed = max(observed, expected)-min(observed, expected) <= tolerance
	}
	return check
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
