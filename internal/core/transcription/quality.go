package transcription

import (
	"errors"
	"strings"
	"unicode"
)

var ErrInvalidQualityAssessment = errors.New("transcript quality assessment is incomplete or invalid")

const QualityPolicyReference = "transcript-quality@v1"

const (
	qualityMaximumSegmentMillis       int64 = 15_000
	qualityMinimumDurationMillis      int64 = 5_000
	qualityMinimumCharactersPerSecond int64 = 1
)

type QualityState string

const (
	QualityPassed            QualityState = "PASSED"
	QualityReviewRecommended QualityState = "REVIEW_RECOMMENDED"
)

func (state QualityState) Valid() bool {
	return state == QualityPassed || state == QualityReviewRecommended
}

type QualityReason string

const (
	QualityReasonSegmentTooLong QualityReason = "SEGMENT_TOO_LONG"
	QualityReasonLowTextDensity QualityReason = "LOW_TEXT_DENSITY"
)

func (reason QualityReason) Valid() bool {
	return reason == QualityReasonSegmentTooLong || reason == QualityReasonLowTextDensity
}

// QualityAssessment is a deterministic signal that a published Transcript
// should be checked by a person. It is not a ContentReview and does not change
// successful STT workflow completion.
type QualityAssessment struct {
	IssueCount         int             `json:"issue_count,omitempty"`
	PolicyReference    string          `json:"policy_reference"`
	State              QualityState    `json:"state"`
	Reasons            []QualityReason `json:"reasons"`
	DurationMillis     int64           `json:"duration_millis"`
	SegmentCount       int             `json:"segment_count"`
	TextCharacterCount int             `json:"text_character_count"`
}

func AssessQuality(source Content) (QualityAssessment, error) {
	content, err := NewContent(source.Language, source.Segments)
	if err != nil {
		return QualityAssessment{}, ErrInvalidQualityAssessment
	}
	assessment := QualityAssessment{
		PolicyReference: QualityPolicyReference,
		State:           QualityPassed,
		DurationMillis:  content.Segments[len(content.Segments)-1].EndMillis,
		SegmentCount:    len(content.Segments),
	}
	segmentTooLong := false
	for _, segment := range content.Segments {
		if segment.EndMillis-segment.StartMillis > qualityMaximumSegmentMillis {
			segmentTooLong = true
			assessment.IssueCount++
		}
		for _, character := range strings.TrimSpace(segment.Text) {
			if unicode.IsLetter(character) || unicode.IsDigit(character) {
				assessment.TextCharacterCount++
			}
		}
	}
	if segmentTooLong {
		assessment.Reasons = append(assessment.Reasons, QualityReasonSegmentTooLong)
	}
	if assessment.DurationMillis >= qualityMinimumDurationMillis &&
		int64(assessment.TextCharacterCount)*1_000 < assessment.DurationMillis*qualityMinimumCharactersPerSecond {
		assessment.IssueCount++
		assessment.Reasons = append(assessment.Reasons, QualityReasonLowTextDensity)
	}
	if len(assessment.Reasons) > 0 {
		assessment.State = QualityReviewRecommended
	}
	if assessment.Validate() != nil {
		return QualityAssessment{}, ErrInvalidQualityAssessment
	}
	return assessment, nil
}

func (assessment QualityAssessment) Validate() error {
	if assessment.PolicyReference != QualityPolicyReference || !assessment.State.Valid() || assessment.DurationMillis <= 0 ||
		assessment.SegmentCount <= 0 || assessment.TextCharacterCount < 0 || assessment.IssueCount < 0 || (assessment.State == QualityPassed && assessment.IssueCount > 0) {
		return ErrInvalidQualityAssessment
	}
	if (assessment.State == QualityPassed) != (len(assessment.Reasons) == 0) {
		return ErrInvalidQualityAssessment
	}
	seen := make(map[QualityReason]bool, len(assessment.Reasons))
	for _, reason := range assessment.Reasons {
		if !reason.Valid() || seen[reason] {
			return ErrInvalidQualityAssessment
		}
		seen[reason] = true
	}
	return nil
}
