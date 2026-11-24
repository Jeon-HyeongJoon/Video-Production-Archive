package organization

import (
	"errors"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/rendering"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidDecision    = errors.New("organization decision is incomplete or invalid")
	ErrStaleInputRevision = errors.New("organization input revision is stale")
)

type Policy struct {
	Version                       uint32
	ShortClipMaxDurationMillis    int64
	SequenceMinFiles              int
	ReadyBitRateTolerancePermille int64
}

func DefaultPolicy() Policy {
	return Policy{Version: 2, ShortClipMaxDurationMillis: 180_000, SequenceMinFiles: 2, ReadyBitRateTolerancePermille: 150}
}

func (policy Policy) validate() error {
	if policy.Version == 0 || policy.Version > DrivePolicyVersion || policy.ShortClipMaxDurationMillis <= 0 || policy.SequenceMinFiles < 2 ||
		policy.ReadyBitRateTolerancePermille < 0 || policy.ReadyBitRateTolerancePermille > 500 {
		return ErrInvalidDecision
	}
	return nil
}

type Input struct {
	RenderSelection *RenderSelection `json:",omitempty"`
	MatchedOriginal string           `json:",omitempty"`
	OriginalAssetID string
	RelativePath    string
	Inspection      mediaanalysis.MediaInspection
}

type AutoRequest struct {
	EventContext     EventContext
	CameraOriginals  CameraOriginalIndex
	DecisionID       string
	WorkspaceDriveID string
	Folder           string
	InputRevision    uint64
	Inputs           []Input
	Policy           Policy
	DecidedAt        time.Time
}

type Action string

const (
	ActionOrganize     Action = "ORGANIZE"
	ActionRender       Action = "RENDER"
	ActionSequence     Action = "SEQUENCE_RENDER"
	ActionAudioExtract Action = "AUDIO_EXTRACT"
	ActionTranscribe   Action = "TRANSCRIBE"
	ActionSubtitle     Action = "SUBTITLE"
)

type OrganizationDecision struct {
	ClassificationEvidence  *ClassificationEvidence `json:",omitempty"`
	EventPreview            *EventPreview           `json:",omitempty"`
	SourceRole              *SourceRoleEvidence     `json:",omitempty"`
	ID                      string
	Sequence                uint64
	WorkspaceDriveID        string
	Folder                  string
	InputRevision           uint64
	PolicyVersion           uint32
	Disposition             vocabulary.OrganizationDisposition
	ProfileReference        string
	OrderedOriginalAssetIDs []string
	Inputs                  []Input
	Actions                 []Action
	HoldReasons             []vocabulary.OrganizationHoldReason
	DecidedByType           vocabulary.ActorType
	DecidedBy               string
	DecisionReason          string
	DecisionBasis           []string
	DecidedAt               time.Time
}

func Classify(request AutoRequest) (decision OrganizationDecision, err error) {
	if request.Policy.validate() != nil || strings.TrimSpace(request.DecisionID) == "" || strings.TrimSpace(request.WorkspaceDriveID) == "" ||
		request.InputRevision == 0 || request.DecidedAt.IsZero() {
		return OrganizationDecision{}, ErrInvalidDecision
	}
	folder, ok := normalizeRelative(request.Folder)
	if !ok {
		return OrganizationDecision{}, ErrInvalidDecision
	}
	decision = OrganizationDecision{
		ID: strings.TrimSpace(request.DecisionID), Sequence: 1, WorkspaceDriveID: strings.TrimSpace(request.WorkspaceDriveID),
		Folder: folder, InputRevision: request.InputRevision, PolicyVersion: request.Policy.Version,
		Disposition: vocabulary.OrganizationHold, DecidedByType: vocabulary.ActorSystem, DecidedBy: "SYSTEM",
		DecisionReason: "versioned automatic organization policy", DecisionBasis: []string{"policy:" + strconv.FormatUint(uint64(request.Policy.Version), 10)},
		DecidedAt: request.DecidedAt.UTC(),
	}
	if request.Policy.Version >= ContextPolicyVersion {
		request.EventContext.ListingChanged = !request.EventContext.matchesInputs(folder, request.Inputs)
		ctx := request.EventContext
		if ctx.InputRevision != 0 && ctx.InputRevision != request.InputRevision {
			return OrganizationDecision{}, ErrInvalidDecision
		}
		decision.EventPreview = ctx.Preview
		defer func() {
			if err == nil {
				decision.recordHold(ctx, request.Inputs)
			}
		}()
	}
	if len(request.Inputs) == 0 {
		decision.HoldReasons = []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldNoMedia}
		return decision, nil
	}
	inputs, valid := normalizeInputs(request.Inputs)
	if !valid {
		decision.HoldReasons = []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldMissingInspection}
		return decision, nil
	}
	decision.Inputs = cloneInputs(inputs)
	for _, input := range inputs {
		decision.OrderedOriginalAssetIDs = append(decision.OrderedOriginalAssetIDs, input.OriginalAssetID)
	}

	if request.Policy.Version >= 2 {
		names := make([]string, 0, len(inputs))
		for _, input := range inputs {
			parent := path.Dir(input.RelativePath)
			if parent != folder && !(parent == "." && folder == "root") {
				return OrganizationDecision{}, ErrInvalidDecision
			}
			names = append(names, path.Base(input.RelativePath))
		}
		role := AssessSourceRole(folder, names)
		if role.Role == RoleProxy {
			for i, input := range inputs {
				if original := request.CameraOriginals.Match(input.RelativePath, uint64(input.Inspection.SizeBytes)); original != "" {
					decision.Inputs[i].MatchedOriginal = original
					role.MatchedProxies++
				}
			}
			role.UnpairedProxies = len(inputs) - role.MatchedProxies
		}
		decision.SourceRole = &role
		decision.DecisionBasis = append(decision.DecisionBasis, role.Basis)
		positionVerified := false
		if request.Policy.Version >= ContextPolicyVersion {
			ctx := request.EventContext
			if NoiseCandidateKind(folder, &role) != "" {
				decision.HoldReasons = []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldSourceRole}
				return decision, nil
			}
			missingEvent := ctx.InputRevision != 0 && ctx.EventFolder == ""
			if request.Policy.Version == DrivePolicyVersion && (sourceLabel.MatchString(folderKey(folder)) || recordingName.MatchString(folderKey(folder)) || captureLabel.MatchString(folderKey(folder))) {
				missingEvent = false
			}
			if missingEvent || ctx.ListingChanged || len(ctx.ConflictingEventFolders) > 0 || len(ctx.OverlappingPositionFolders) > 0 || ctx.EditorialWitness != "" || editorialPath(folder) {
				decision.HoldReasons = []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldSourceRole}
				return decision, nil
			}
			if pos, _ := positionBranch(folder, ctx.EventFolder); pos != "" {
				positionVerified = role.Role == RoleRecording && ctx.InputRevision == request.InputRevision && corroboratedPosition(folder, ctx, inputs)
				if !positionVerified {
					decision.HoldReasons = []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldSourceRole}
					return decision, nil
				}
				decision.DecisionBasis = append(decision.DecisionBasis, ctx.EventFolder)
				decision.DecisionBasis = append(decision.DecisionBasis, ctx.ChannelFolders...)
			}
		}
		if request.Policy.Version == DrivePolicyVersion {
			applyRoleProfiles(&decision, request.Policy, positionVerified)
			return decision, nil
		}
		// Multiple recorder files may be consecutive parts or separate channels.
		if (role.Role == RoleRecording && role.Strength == roleExplicit && len(inputs) == 1) || positionVerified {
			decision.Disposition = vocabulary.OrganizationRenderRecordings
			decision.ProfileReference = mustOrganizationProfile(6_000).Reference()
			decision.Actions = []Action{ActionRender, ActionOrganize}
			return decision, nil
		}
		sourceEvidence := role.Role == RoleSource && role.Strength == roleExplicit
		if role.Role == RoleSource && role.Strength == roleSuggested && len(inputs) >= request.Policy.SequenceMinFiles {
			sourceEvidence = true
			for _, input := range inputs {
				if !rawCameraName.MatchString(strings.ToLower(path.Base(input.RelativePath))) || SourceMetadataHint(folder, input.Inspection) != ShortCameraHint {
					sourceEvidence = false
					break
				}
			}
			if sourceEvidence {
				decision.DecisionBasis = append(decision.DecisionBasis, ShortCameraHint)
			}
		}
		if (role.Strength != roleUnknown || request.Policy.Version >= ContextPolicyVersion) && !sourceEvidence {
			decision.HoldReasons = []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldSourceRole}
			return decision, nil
		}
	}

	if len(inputs) >= request.Policy.SequenceMinFiles && allShort(inputs, request.Policy.ShortClipMaxDurationMillis) {
		ordered, nameStatus := orderNumberedSequence(inputs)
		switch nameStatus {
		case sequenceNonconsecutive:
			decision.HoldReasons = []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldNonconsecutiveSequence}
			return decision, nil
		case sequenceOrdered:
			if !technicallyCompatible(ordered) {
				decision.HoldReasons = []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldIncompatibleSequence}
				return decision, nil
			}
			decision.Disposition = vocabulary.OrganizationCompileSourceSequence
			decision.ProfileReference = mustOrganizationProfile(20_000).Reference()
			decision.Inputs = cloneInputs(ordered)
			decision.OrderedOriginalAssetIDs = decision.OrderedOriginalAssetIDs[:0]
			for _, input := range ordered {
				decision.OrderedOriginalAssetIDs = append(decision.OrderedOriginalAssetIDs, input.OriginalAssetID)
			}
			decision.Actions = []Action{ActionSequence, ActionOrganize}
			return decision, nil
		}
	}

	if decision.SourceRole != nil && decision.SourceRole.Role == RoleSource {
		decision.HoldReasons = []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldSourceRole}
		return decision, nil
	}
	readyCount := 0
	for _, input := range inputs {
		if readyProfile(input.Inspection, request.Policy) != "" {
			readyCount++
		}
	}
	if readyCount == len(inputs) {
		decision.Disposition = vocabulary.OrganizationAsIs
		decision.Actions = []Action{ActionOrganize}
		if len(inputs) == 1 {
			decision.ProfileReference = readyProfile(inputs[0].Inspection, request.Policy)
		}
		return decision, nil
	}
	if len(inputs) == 1 {
		decision.Disposition = vocabulary.OrganizationRenderRecordings
		decision.ProfileReference = mustOrganizationProfile(6_000).Reference()
		decision.Actions = []Action{ActionRender, ActionOrganize}
		return decision, nil
	}
	if readyCount > 0 {
		decision.HoldReasons = []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldMixedReadyState}
	} else {
		decision.HoldReasons = []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldAmbiguousMultipleVideos}
	}
	return decision, nil
}

func readyProfile(inspection mediaanalysis.MediaInspection, policy Policy) string {
	if len(inspection.VideoStreams) == 0 || !contains(inspection.Containers, "mp4") {
		return ""
	}
	video := inspection.VideoStreams[0]
	for _, candidate := range inspection.VideoStreams[1:] {
		if candidate.Index < video.Index {
			video = candidate
		}
	}
	if video.Codec != "h264" || video.Width > 1920 || video.Height != 1080 || video.BitRate <= 0 {
		return ""
	}
	if len(inspection.AudioStreams) > 0 {
		audio := inspection.AudioStreams[0]
		for _, candidate := range inspection.AudioStreams[1:] {
			if candidate.Index < audio.Index {
				audio = candidate
			}
		}
		if audio.Codec != "aac" {
			return ""
		}
	}
	for _, target := range []int64{6_000_000, 20_000_000} {
		lower := target * (1000 - policy.ReadyBitRateTolerancePermille) / 1000
		upper := target * (1000 + policy.ReadyBitRateTolerancePermille) / 1000
		if video.BitRate >= lower && video.BitRate <= upper {
			return mustOrganizationProfile(int(target / 1000)).Reference()
		}
	}
	return ""
}

func normalizeInputs(source []Input) ([]Input, bool) {
	inputs := append([]Input(nil), source...)
	seen := make(map[string]bool, len(inputs))
	for index := range inputs {
		input := &inputs[index]
		input.MatchedOriginal = ""
		input.RenderSelection = nil
		input.OriginalAssetID = strings.TrimSpace(input.OriginalAssetID)
		input.RelativePath = path.Clean(strings.ReplaceAll(strings.TrimSpace(input.RelativePath), "\\", "/"))
		normalized, err := mediaanalysis.NewMediaInspection(input.Inspection)
		if input.OriginalAssetID == "" || seen[input.OriginalAssetID] || !safeRelative(input.RelativePath) ||
			err != nil || normalized.OriginalAssetID != input.OriginalAssetID {
			return nil, false
		}
		seen[input.OriginalAssetID] = true
		input.Inspection = normalized
	}
	sort.Slice(inputs, func(i, j int) bool {
		return strings.ToLower(inputs[i].RelativePath) < strings.ToLower(inputs[j].RelativePath)
	})
	return inputs, true
}

func allShort(inputs []Input, maximum int64) bool {
	for _, input := range inputs {
		if input.Inspection.DurationMillis > maximum {
			return false
		}
	}
	return true
}

type sequenceNameStatus int

const (
	sequenceNotNumbered sequenceNameStatus = iota
	sequenceNonconsecutive
	sequenceOrdered
)

var numberedStemPattern = regexp.MustCompile(`^(.*?)([0-9]+)$`)

func orderNumberedSequence(inputs []Input) ([]Input, sequenceNameStatus) {
	type numbered struct {
		input  Input
		prefix string
		number int64
	}
	values := make([]numbered, 0, len(inputs))
	for _, input := range inputs {
		stem := strings.TrimSuffix(path.Base(input.RelativePath), path.Ext(input.RelativePath))
		match := numberedStemPattern.FindStringSubmatch(strings.ToLower(stem))
		if len(match) != 3 {
			return nil, sequenceNotNumbered
		}
		number, err := strconv.ParseInt(match[2], 10, 64)
		if err != nil {
			return nil, sequenceNotNumbered
		}
		values = append(values, numbered{input: input, prefix: match[1], number: number})
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].prefix != values[j].prefix {
			return values[i].prefix < values[j].prefix
		}
		return values[i].number < values[j].number
	})
	for index := 1; index < len(values); index++ {
		if values[index].prefix != values[0].prefix || values[index].number != values[index-1].number+1 {
			return nil, sequenceNonconsecutive
		}
	}
	ordered := make([]Input, len(values))
	for index, value := range values {
		ordered[index] = value.input
	}
	return ordered, sequenceOrdered
}

func technicallyCompatible(inputs []Input) bool {
	if len(inputs) < 2 {
		return false
	}
	firstVideo := primaryVideo(inputs[0].Inspection)
	firstAudio := primaryAudio(inputs[0].Inspection)
	for _, input := range inputs[1:] {
		video := primaryVideo(input.Inspection)
		audio := primaryAudio(input.Inspection)
		if video == nil || firstVideo == nil || video.Codec != firstVideo.Codec || video.Width != firstVideo.Width ||
			video.Height != firstVideo.Height || video.FrameRate != firstVideo.FrameRate || !sameAudio(audio, firstAudio) {
			return false
		}
	}
	return true
}

func primaryVideo(inspection mediaanalysis.MediaInspection) *mediaanalysis.VideoStream {
	if len(inspection.VideoStreams) == 0 {
		return nil
	}
	value := inspection.VideoStreams[0]
	for _, candidate := range inspection.VideoStreams[1:] {
		if candidate.Index < value.Index {
			value = candidate
		}
	}
	return &value
}

func primaryAudio(inspection mediaanalysis.MediaInspection) *mediaanalysis.AudioStream {
	if len(inspection.AudioStreams) == 0 {
		return nil
	}
	value := inspection.AudioStreams[0]
	for _, candidate := range inspection.AudioStreams[1:] {
		if candidate.Index < value.Index {
			value = candidate
		}
	}
	return &value
}

func sameAudio(left, right *mediaanalysis.AudioStream) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Codec == right.Codec && left.SampleRateHz == right.SampleRateHz && left.Channels == right.Channels
}

func mustOrganizationProfile(target int) rendering.Profile {
	profile, err := rendering.OrganizationProfile(target)
	if err != nil {
		panic(err)
	}
	return profile
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), expected) {
			return true
		}
	}
	return false
}

func normalizeRelative(value string) (string, bool) {
	value = path.Clean(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"))
	return value, safeRelative(value)
}

func safeRelative(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.HasPrefix(value, "../") &&
		!strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") &&
		!(len(value) >= 3 && value[1] == ':' && value[2] == '/')
}

func cloneInputs(source []Input) []Input {
	values := append([]Input(nil), source...)
	for index := range values {
		if source[index].RenderSelection != nil {
			selection := *source[index].RenderSelection
			values[index].RenderSelection = &selection
		}
		values[index].Inspection.Containers = append([]string(nil), values[index].Inspection.Containers...)
		values[index].Inspection.VideoStreams = append([]mediaanalysis.VideoStream(nil), values[index].Inspection.VideoStreams...)
		values[index].Inspection.AudioStreams = append([]mediaanalysis.AudioStream(nil), values[index].Inspection.AudioStreams...)
	}
	return values
}

func PlanFollowUp(decision OrganizationDecision, qcPassed bool) []Action {
	if !TranscriptionEligible(decision, qcPassed) {
		return nil
	}
	return []Action{ActionAudioExtract, ActionTranscribe, ActionSubtitle}
}

func TranscriptionEligible(decision OrganizationDecision, qcPassed bool) bool {
	if !qcPassed || (decision.Disposition != vocabulary.OrganizationAsIs && decision.Disposition != vocabulary.OrganizationRenderRecordings) {
		return false
	}
	for _, input := range decision.Inputs {
		if len(input.Inspection.AudioStreams) > 0 {
			return true
		}
	}
	return false
}
