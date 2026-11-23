package organization

import (
	"strings"
	"time"

	"videoarchive/internal/core/rendering"
	"videoarchive/internal/core/vocabulary"
)

type OverrideRequest struct {
	DecisionID             string
	ExpectedInputRevision  uint64
	Disposition            vocabulary.OrganizationDisposition
	TargetVideoBitRateKbps int
	DecidedByType          vocabulary.ActorType
	DecidedBy              string
	Reason                 string
	Basis                  []string
	DecidedAt              time.Time
}

// OrganizationWorkOption is one executable first-stage path the current structured media
// evidence can safely support. It is projected to people and Archivists so an
// unavailable disposition is not offered and rejected only after submission.
type OrganizationWorkOption struct {
	Disposition            vocabulary.OrganizationDisposition
	TargetVideoBitRateKbps int
}

func AvailableWorkOptions(decision OrganizationDecision) []OrganizationWorkOption {
	options := make([]OrganizationWorkOption, 0, 4)
	if allReadyForOrganization(decision.Inputs) {
		options = append(options, OrganizationWorkOption{Disposition: vocabulary.OrganizationAsIs})
	}
	if renderableInputs(decision.Inputs) {
		options = append(options,
			OrganizationWorkOption{Disposition: vocabulary.OrganizationRenderRecordings, TargetVideoBitRateKbps: 6_000},
			OrganizationWorkOption{Disposition: vocabulary.OrganizationRenderRecordings, TargetVideoBitRateKbps: 20_000},
		)
	}
	if explicitlySequenceable(decision.Inputs) {
		options = append(options, OrganizationWorkOption{Disposition: vocabulary.OrganizationCompileSourceSequence})
	}
	return options
}

type DecisionHistory struct {
	decisions []OrganizationDecision
}

func NewDecisionHistory(initial OrganizationDecision) (*DecisionHistory, error) {
	if initial.ID == "" || initial.Sequence == 0 || initial.InputRevision == 0 || !initial.Disposition.Valid() {
		return nil, ErrInvalidDecision
	}
	return &DecisionHistory{decisions: []OrganizationDecision{cloneDecision(initial)}}, nil
}

func (history *DecisionHistory) RecordOverride(request OverrideRequest) (OrganizationDecision, error) {
	if history == nil || len(history.decisions) == 0 {
		return OrganizationDecision{}, ErrInvalidDecision
	}
	latest := history.decisions[len(history.decisions)-1]
	if request.ExpectedInputRevision != latest.InputRevision {
		return OrganizationDecision{}, ErrStaleInputRevision
	}
	basis := normalizeDecisionBasis(request.Basis)
	if strings.TrimSpace(request.DecisionID) == "" ||
		(request.DecidedByType != vocabulary.ActorHuman && request.DecidedByType != vocabulary.ActorArchivist) ||
		strings.TrimSpace(request.DecidedBy) == "" || strings.TrimSpace(request.Reason) == "" || len(basis) == 0 ||
		request.DecidedAt.IsZero() || !request.Disposition.Valid() {
		return OrganizationDecision{}, ErrInvalidDecision
	}
	decision := cloneDecision(latest)
	decision.ID = strings.TrimSpace(request.DecisionID)
	decision.Sequence++
	decision.Disposition = request.Disposition
	decision.ProfileReference = ""
	decision.Actions = nil
	decision.HoldReasons = nil
	decision.DecidedByType = request.DecidedByType
	decision.DecidedBy = strings.TrimSpace(request.DecidedBy)
	decision.DecisionReason = strings.TrimSpace(request.Reason)
	decision.DecisionBasis = basis
	decision.DecidedAt = request.DecidedAt.UTC()

	switch request.Disposition {
	case vocabulary.OrganizationRenderRecordings:
		if !availableWorkOption(latest, request.Disposition, request.TargetVideoBitRateKbps) {
			return OrganizationDecision{}, ErrInvalidDecision
		}
		profile, err := rendering.OrganizationProfile(request.TargetVideoBitRateKbps)
		if err != nil {
			return OrganizationDecision{}, ErrInvalidDecision
		}
		decision.ProfileReference = profile.Reference()
		decision.Actions = []Action{ActionRender, ActionOrganize}
	case vocabulary.OrganizationCompileSourceSequence:
		if !availableWorkOption(latest, request.Disposition, request.TargetVideoBitRateKbps) {
			return OrganizationDecision{}, ErrInvalidDecision
		}
		decision.ProfileReference = mustOrganizationProfile(20_000).Reference()
		decision.Actions = []Action{ActionSequence, ActionOrganize}
	case vocabulary.OrganizationAsIs:
		if !availableWorkOption(latest, request.Disposition, request.TargetVideoBitRateKbps) {
			return OrganizationDecision{}, ErrInvalidDecision
		}
		decision.Actions = []Action{ActionOrganize}
	case vocabulary.OrganizationHold:
		decision.HoldReasons = []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldInvalidOverride}
	}
	history.decisions = append(history.decisions, cloneDecision(decision))
	return cloneDecision(decision), nil
}

func availableWorkOption(decision OrganizationDecision, disposition vocabulary.OrganizationDisposition, targetVideoBitRateKbps int) bool {
	for _, option := range AvailableWorkOptions(decision) {
		if option.Disposition == disposition && option.TargetVideoBitRateKbps == targetVideoBitRateKbps {
			return true
		}
	}
	return false
}

func allReadyForOrganization(inputs []Input) bool {
	if len(inputs) == 0 {
		return false
	}
	for _, input := range inputs {
		if readyProfile(input.Inspection, DefaultPolicy()) == "" {
			return false
		}
	}
	return true
}

// Explicit selection may join decoded clips with different codecs or frame
// rates because the sequence renderer re-encodes one normalized rendition.
// The concat filter still requires matching frame dimensions and audio shape.
func explicitlySequenceable(inputs []Input) bool {
	if len(inputs) < 2 || !allShort(inputs, DefaultPolicy().ShortClipMaxDurationMillis) {
		return false
	}
	firstVideo := primaryVideo(inputs[0].Inspection)
	firstAudio := primaryAudio(inputs[0].Inspection)
	for _, input := range inputs[1:] {
		video := primaryVideo(input.Inspection)
		audio := primaryAudio(input.Inspection)
		if video == nil || firstVideo == nil || video.Width != firstVideo.Width || video.Height != firstVideo.Height ||
			!sameAudio(audio, firstAudio) {
			return false
		}
	}
	return true
}

func renderableInputs(inputs []Input) bool {
	if len(inputs) == 0 {
		return false
	}
	for _, input := range inputs {
		if primaryVideo(input.Inspection) == nil {
			return false
		}
	}
	return true
}

func (history *DecisionHistory) Decisions() []OrganizationDecision {
	if history == nil {
		return nil
	}
	values := make([]OrganizationDecision, len(history.decisions))
	for index, decision := range history.decisions {
		values[index] = cloneDecision(decision)
	}
	return values
}

func cloneDecision(source OrganizationDecision) OrganizationDecision {
	value := source
	if source.SourceRole != nil {
		role := *source.SourceRole
		value.SourceRole = &role
	}
	value.OrderedOriginalAssetIDs = append([]string(nil), source.OrderedOriginalAssetIDs...)
	value.Inputs = cloneInputs(source.Inputs)
	value.Actions = append([]Action(nil), source.Actions...)
	value.HoldReasons = append([]vocabulary.OrganizationHoldReason(nil), source.HoldReasons...)
	value.DecisionBasis = append([]string(nil), source.DecisionBasis...)
	return value
}

func normalizeDecisionBasis(source []string) []string {
	seen := make(map[string]bool, len(source))
	values := make([]string, 0, len(source))
	for _, item := range source {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		values = append(values, item)
	}
	return values
}
