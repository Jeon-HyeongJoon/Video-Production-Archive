package organization

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/rendering"
	"videoarchive/internal/core/vocabulary"
)

func organizationInput(assetID, relativePath string, durationMillis, videoBitRate int64, width, height int) Input {
	return Input{
		OriginalAssetID: assetID,
		RelativePath:    relativePath,
		Inspection: mediaanalysis.MediaInspection{
			ID: "INSPECTION-" + assetID, OriginalAssetID: assetID,
			OriginalChecksum: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Containers:       []string{"mp4"}, DurationMillis: durationMillis, SizeBytes: 10_000_000, BitRate: videoBitRate + 192_000,
			VideoStreams: []mediaanalysis.VideoStream{{
				Index: 0, Codec: "h264", Width: width, Height: height,
				FrameRate: mediaanalysis.Rational{Numerator: 30, Denominator: 1}, BitRate: videoBitRate,
			}},
			AudioStreams:     []mediaanalysis.AudioStream{{Index: 1, Codec: "aac", SampleRateHz: 48_000, Channels: 2}},
			ProcessorVersion: "ffprobe-8.0", ObservedBy: "WORKER-1", InspectedAt: time.Now().UTC(),
		},
	}
}

// Requirement: ORG-002
func TestAutomaticOrganizationDecisionIsDeterministicAndHoldsAmbiguousInputs(t *testing.T) {
	request := AutoRequest{
		DecisionID: "DECISION-1", WorkspaceDriveID: "DRIVE-1", Folder: "event",
		InputRevision: 4, Inputs: []Input{
			organizationInput("ASSET-A", "event/interview.mov", 600_000, 50_000_000, 3840, 2160),
			organizationInput("ASSET-B", "event/presentation.mov", 900_000, 45_000_000, 3840, 2160),
		},
		Policy: DefaultPolicy(), DecidedAt: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC),
	}
	first, err := Classify(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Classify(request)
	if err != nil {
		t.Fatal(err)
	}
	if first.PolicyVersion != 2 || first.SourceRole == nil || first.SourceRole.Role != RoleUnresolved || !reflect.DeepEqual(first, second) || first.Disposition != vocabulary.OrganizationHold ||
		!reflect.DeepEqual(first.HoldReasons, []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldAmbiguousMultipleVideos}) ||
		len(first.Actions) != 0 {
		t.Fatalf("ambiguous inputs were inferred or classification changed: first=%+v second=%+v", first, second)
	}

	t.Run("case-distinct camera files in reversed inspection order", func(t *testing.T) {
		request.Folder = "촬영물/391216-행사/카드1"
		request.Policy.Version = DrivePolicyVersion
		request.Inputs = []Input{
			organizationInput("ASSET-UPPER", request.Folder+"/C0001.MP4", 12_000, 28_000_000, 1920, 1080),
			organizationInput("ASSET-LOWER", request.Folder+"/c0001.mp4", 12_000, 28_000_000, 1920, 1080),
		}
		first, err := Classify(request)
		if err != nil {
			t.Fatal(err)
		}
		slices.Reverse(request.Inputs)
		second, err := Classify(request)
		if err != nil {
			t.Fatal(err)
		}
		if first.Disposition != vocabulary.OrganizationRenderRecordings || !slices.Equal(first.OrderedOriginalAssetIDs, []string{"ASSET-UPPER", "ASSET-LOWER"}) ||
			!reflect.DeepEqual(first, second) {
			t.Fatalf("reordering separate cuts changed the decision: %v %v / %v %v", first.Disposition, first.OrderedOriginalAssetIDs, second.Disposition, second.OrderedOriginalAssetIDs)
		}
	})
}

// Requirement: ORG-003
func TestExplicitOrganizationClassificationAppendsActorEvidenceAndRejectsStaleRevision(t *testing.T) {
	initial, err := Classify(AutoRequest{
		DecisionID: "DECISION-1", WorkspaceDriveID: "DRIVE-1", Folder: "event", InputRevision: 7,
		Inputs: []Input{
			organizationInput("ASSET-A", "event/a.mov", 600_000, 50_000_000, 3840, 2160),
			organizationInput("ASSET-B", "event/b.mov", 600_000, 50_000_000, 3840, 2160),
		},
		Policy: DefaultPolicy(), DecidedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	history, err := NewDecisionHistory(initial)
	if err != nil {
		t.Fatal(err)
	}
	_, err = history.RecordOverride(OverrideRequest{
		DecisionID: "DECISION-STALE", ExpectedInputRevision: 6,
		Disposition: vocabulary.OrganizationRenderRecordings, TargetVideoBitRateKbps: 20_000,
		DecidedByType: vocabulary.ActorArchivist, DecidedBy: "archivist:session-1",
		Reason: "separate long recordings", Basis: []string{"ASSET-A", "ASSET-B"}, DecidedAt: time.Now().UTC(),
	})
	if !errors.Is(err, ErrStaleInputRevision) || len(history.Decisions()) != 1 {
		t.Fatalf("stale override changed history: decisions=%+v err=%v", history.Decisions(), err)
	}
	noise := initial
	noise.Folder = "card/M4ROOT/SUB"
	noiseHistory, err := NewDecisionHistory(noise)
	if err != nil {
		t.Fatal(err)
	}
	_, err = noiseHistory.RecordOverride(OverrideRequest{DecisionID: "agent-choice", ExpectedInputRevision: 7,
		Disposition: vocabulary.OrganizationRenderRecordings, TargetVideoBitRateKbps: 20_000, DecidedByType: vocabulary.ActorArchivist,
		DecidedBy: "archivist:session-1", Reason: "review", Basis: []string{initial.ID}, DecidedAt: time.Now().UTC()})
	if !errors.Is(err, ErrInvalidDecision) || len(noiseHistory.Decisions()) != 1 {
		t.Fatal("Archivist must leave the noise disposition to the operator", err)
	}
	actors := []struct {
		id        string
		actorType vocabulary.ActorType
		actorID   string
	}{
		{id: "DECISION-2", actorType: vocabulary.ActorHuman, actorID: "operator-1"},
		{id: "DECISION-3", actorType: vocabulary.ActorArchivist, actorID: "archivist:session-1"},
	}
	var added OrganizationDecision
	for _, actor := range actors {
		added, err = history.RecordOverride(OverrideRequest{
			DecisionID: actor.id, ExpectedInputRevision: 7,
			Disposition: vocabulary.OrganizationRenderRecordings, TargetVideoBitRateKbps: 20_000,
			DecidedByType: actor.actorType, DecidedBy: actor.actorID,
			Reason: "separate long recordings", Basis: []string{"ASSET-A", "ASSET-B", "ASSET-A"}, DecidedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if added.DecidedByType != actor.actorType || added.DecidedBy != actor.actorID ||
			added.DecisionReason != "separate long recordings" || !reflect.DeepEqual(added.DecisionBasis, []string{"ASSET-A", "ASSET-B"}) {
			t.Fatalf("classification provenance was not preserved: %+v", added)
		}
	}
	decisions := history.Decisions()
	if len(decisions) != 3 || decisions[0].ID != "DECISION-1" || decisions[0].Disposition != vocabulary.OrganizationHold ||
		decisions[2].ID != added.ID || added.Sequence != decisions[0].Sequence+2 ||
		added.Disposition != vocabulary.OrganizationRenderRecordings || added.ProfileReference != "archive-1080p-20mbps@v1" {
		t.Fatalf("override did not append a new decision: %+v", decisions)
	}

	// Automatic classification remains conservative when numbering or frame
	// rates differ. An explicit source-sequence selection is nevertheless safe
	// when the decoded dimensions and audio shape match because it is re-encoded.
	mixedFrameRateInputs := []Input{
		organizationInput("ASSET-MIXED-1", "source/C0001.MP4", 10_000, 50_000_000, 3840, 2160),
		organizationInput("ASSET-MIXED-3", "source/C0003.MP4", 12_000, 50_000_000, 3840, 2160),
	}
	mixedFrameRateInputs[1].Inspection.VideoStreams[0].FrameRate = mediaanalysis.Rational{Numerator: 120, Denominator: 1}
	mixed, err := Classify(AutoRequest{
		DecisionID: "DECISION-MIXED-HOLD", WorkspaceDriveID: "DRIVE-1", Folder: "source", InputRevision: 8,
		Inputs: mixedFrameRateInputs, Policy: DefaultPolicy(), DecidedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	options := AvailableWorkOptions(mixed)
	if len(options) != 3 || options[0] != (OrganizationWorkOption{Disposition: vocabulary.OrganizationRenderRecordings, TargetVideoBitRateKbps: 6_000}) ||
		options[1] != (OrganizationWorkOption{Disposition: vocabulary.OrganizationRenderRecordings, TargetVideoBitRateKbps: 20_000}) ||
		options[2] != (OrganizationWorkOption{Disposition: vocabulary.OrganizationCompileSourceSequence}) {
		t.Fatalf("work options exposed an unavailable as-is path or omitted executable work: %+v", options)
	}
	mixedHistory, err := NewDecisionHistory(mixed)
	if err != nil {
		t.Fatal(err)
	}
	mixedSource, err := mixedHistory.RecordOverride(OverrideRequest{
		DecisionID: "DECISION-MIXED-SOURCE", ExpectedInputRevision: 8,
		Disposition:   vocabulary.OrganizationCompileSourceSequence,
		DecidedByType: vocabulary.ActorHuman, DecidedBy: "operator-1", Reason: "join selected camera clips",
		Basis: []string{"ASSET-MIXED-1", "ASSET-MIXED-3"}, DecidedAt: time.Now().UTC(),
	})
	if err != nil || mixedSource.Disposition != vocabulary.OrganizationCompileSourceSequence ||
		mixedSource.ProfileReference != "archive-1080p-20mbps@v1" {
		t.Fatalf("explicit mixed-frame-rate source sequence was rejected: decision=%+v err=%v", mixedSource, err)
	}

	withoutEvidence, err := Classify(AutoRequest{
		DecisionID: "DECISION-NO-EVIDENCE", WorkspaceDriveID: "DRIVE-1", Folder: "empty", InputRevision: 7,
		Policy: DefaultPolicy(), DecidedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	insufficient, err := NewDecisionHistory(withoutEvidence)
	if err != nil {
		t.Fatal(err)
	}
	_, err = insufficient.RecordOverride(OverrideRequest{
		DecisionID: "DECISION-UNSUPPORTED", ExpectedInputRevision: 7,
		Disposition: vocabulary.OrganizationRenderRecordings, TargetVideoBitRateKbps: 6_000,
		DecidedByType: vocabulary.ActorArchivist, DecidedBy: "archivist:session-1",
		Reason: "no media evidence", Basis: []string{"HOLD:NO_MEDIA"}, DecidedAt: time.Now().UTC(),
	})
	if !errors.Is(err, ErrInvalidDecision) || len(insufficient.Decisions()) != 1 ||
		insufficient.Decisions()[0].Disposition != vocabulary.OrganizationHold {
		t.Fatalf("insufficient evidence changed HOLD: decisions=%+v err=%v", insufficient.Decisions(), err)
	}
}

// Requirement: ORG-004
func TestReadyRenditionRequiresEveryCodecDimensionAndBitrateCondition(t *testing.T) {
	policy := DefaultPolicy()
	for _, bitRate := range []int64{5_100_000, 6_000_000, 6_900_000, 17_000_000, 20_000_000, 23_000_000} {
		decision, err := Classify(AutoRequest{
			DecisionID: "DECISION-READY", WorkspaceDriveID: "DRIVE-1", Folder: "ready", InputRevision: 1,
			Inputs: []Input{organizationInput("ASSET-READY", "ready/final.mp4", 60_000, bitRate, 1920, 1080)},
			Policy: policy, DecidedAt: time.Now().UTC(),
		})
		if err != nil || decision.Disposition != vocabulary.OrganizationAsIs {
			t.Fatalf("ready boundary %d was not accepted: decision=%+v err=%v", bitRate, decision, err)
		}
	}
	invalid := []Input{
		organizationInput("ASSET-LOW", "ready/low.mp4", 60_000, 5_099_999, 1920, 1080),
		organizationInput("ASSET-ZERO", "ready/zero.mp4", 60_000, 0, 1920, 1080),
		organizationInput("ASSET-SIZE", "ready/size.mp4", 60_000, 6_000_000, 1920, 1078),
	}
	for _, input := range invalid {
		decision, err := Classify(AutoRequest{
			DecisionID: "DECISION-INVALID", WorkspaceDriveID: "DRIVE-1", Folder: "ready", InputRevision: 1,
			Inputs: []Input{input}, Policy: policy, DecidedAt: time.Now().UTC(),
		})
		if err != nil || decision.Disposition == vocabulary.OrganizationAsIs {
			t.Fatalf("invalid ready input passed through: input=%+v decision=%+v err=%v", input, decision, err)
		}
	}
}

// Requirement: ORG-005
func TestCompatibleConsecutiveShortClipsBecomeSourceSequenceBeforeReadyFiles(t *testing.T) {
	decision, err := Classify(AutoRequest{
		DecisionID: "DECISION-SOURCE", WorkspaceDriveID: "DRIVE-1", Folder: "source", InputRevision: 1,
		Inputs: []Input{
			organizationInput("ASSET-2", "source/C0002.MP4", 12_000, 20_000_000, 1920, 1080),
			organizationInput("ASSET-1", "source/C0001.MP4", 10_000, 20_000_000, 1920, 1080),
			organizationInput("ASSET-3", "source/C0003.MP4", 8_000, 20_000_000, 1920, 1080),
		},
		Policy: DefaultPolicy(), DecidedAt: time.Now().UTC(),
	})
	if err != nil || decision.PolicyVersion != 2 || decision.Disposition != vocabulary.OrganizationCompileSourceSequence ||
		!reflect.DeepEqual(decision.OrderedOriginalAssetIDs, []string{"ASSET-1", "ASSET-2", "ASSET-3"}) {
		t.Fatalf("ready camera clips were not classified as one source sequence: %+v err=%v", decision, err)
	}
	nonconsecutive := []Input{
		organizationInput("ASSET-1", "source/C0001.MP4", 10_000, 20_000_000, 1920, 1080),
		organizationInput("ASSET-3", "source/C0003.MP4", 10_000, 20_000_000, 1920, 1080),
	}
	held, err := Classify(AutoRequest{
		DecisionID: "DECISION-HOLD", WorkspaceDriveID: "DRIVE-1", Folder: "source", InputRevision: 1,
		Inputs: nonconsecutive, Policy: DefaultPolicy(), DecidedAt: time.Now().UTC(),
	})
	if err != nil || held.Disposition != vocabulary.OrganizationHold ||
		!reflect.DeepEqual(held.HoldReasons, []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldNonconsecutiveSequence}) {
		t.Fatalf("partial source sequence was not held: %+v err=%v", held, err)
	}
}

// Requirement: ORG-006
func TestSourceTimelinePreservesOrderedWholeClipsAndCumulativeBoundaryMarkers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		durations  [3]int64
		boundaries [2]int64
		total      int64
	}{
		{"unequal clips", [3]int64{10_000, 12_000, 8_000}, [2]int64{10_000, 22_000}, 30_000},
		{"three two-second cuts", [3]int64{2_000, 2_000, 2_000}, [2]int64{2_000, 4_000}, 6_000},
		{"subsecond boundaries", [3]int64{2_013, 1_994, 2_007}, [2]int64{2_013, 4_007}, 6_014},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := Classify(AutoRequest{
				DecisionID: "DECISION-SOURCE", WorkspaceDriveID: "DRIVE-1", Folder: "source", InputRevision: 1,
				Inputs: []Input{
					organizationInput("ASSET-2", "source/C0002.MP4", tc.durations[1], 30_000_000, 3840, 2160),
					organizationInput("ASSET-1", "source/C0001.MP4", tc.durations[0], 30_000_000, 3840, 2160),
					organizationInput("ASSET-3", "source/C0003.MP4", tc.durations[2], 30_000_000, 3840, 2160),
				},
				Policy: DefaultPolicy(), DecidedAt: time.Now().UTC(),
			})
			if err != nil {
				t.Fatal(err)
			}
			timeline, err := BuildSourceTimeline("TIMELINE-1", 1, decision)
			if err != nil {
				t.Fatal(err)
			}
			wantClips := []SourceClip{
				{Sequence: 1, OriginalAssetID: "ASSET-1", OutMillis: tc.durations[0]},
				{Sequence: 2, OriginalAssetID: "ASSET-2", OutMillis: tc.durations[1]},
				{Sequence: 3, OriginalAssetID: "ASSET-3", OutMillis: tc.durations[2]},
			}
			wantMarkers := []CutMarker{
				{BeforeClipID: "ASSET-1", AfterClipID: "ASSET-2", AtMillis: tc.boundaries[0]},
				{BeforeClipID: "ASSET-2", AfterClipID: "ASSET-3", AtMillis: tc.boundaries[1]},
			}
			if !reflect.DeepEqual(timeline.Clips, wantClips) || !reflect.DeepEqual(timeline.Markers, wantMarkers) || timeline.DurationMillis() != tc.total {
				t.Fatalf("got clips %v, markers %v, duration %d ms; want %v, %v, %d ms", timeline.Clips, timeline.Markers, timeline.DurationMillis(), wantClips, wantMarkers, tc.total)
			}
		})
	}
}

// Requirement: ORG-007
func TestTranscriptionPlanIncludesOnlyQCPassedRecordings(t *testing.T) {
	input := organizationInput("ASSET-STT", "recording.mp4", 60_000, 6_000_000, 1920, 1080)
	recording := OrganizationDecision{Disposition: vocabulary.OrganizationRenderRecordings, Inputs: []Input{input}}
	asIs := OrganizationDecision{Disposition: vocabulary.OrganizationAsIs, Inputs: []Input{input}}
	source := OrganizationDecision{Disposition: vocabulary.OrganizationCompileSourceSequence}
	hold := OrganizationDecision{Disposition: vocabulary.OrganizationHold}
	for _, value := range []OrganizationDecision{recording, asIs} {
		if got := PlanFollowUp(value, true); !reflect.DeepEqual(got, []Action{ActionAudioExtract, ActionTranscribe, ActionSubtitle}) {
			t.Fatalf("QC-passed recording lost STT plan: disposition=%s actions=%v", value.Disposition, got)
		}
		if got := PlanFollowUp(value, false); len(got) != 0 {
			t.Fatalf("QC-failed recording received STT plan: %v", got)
		}
	}
	silentInput := input
	silentInput.Inspection.AudioStreams = nil
	if got := PlanFollowUp(OrganizationDecision{Disposition: vocabulary.OrganizationRenderRecordings, Inputs: []Input{silentInput}}, true); len(got) != 0 {
		t.Fatalf("silent recording received STT plan: %v", got)
	}
	if got := PlanFollowUp(OrganizationDecision{Disposition: vocabulary.OrganizationAsIs, Inputs: []Input{silentInput, input}}, true); !reflect.DeepEqual(got, []Action{ActionAudioExtract, ActionTranscribe, ActionSubtitle}) {
		t.Fatalf("multi-recording folder with audible input lost STT plan: %v", got)
	}
	if got := PlanFollowUp(OrganizationDecision{Disposition: vocabulary.OrganizationAsIs, Inputs: []Input{silentInput, silentInput}}, true); len(got) != 0 {
		t.Fatalf("all-silent multi-recording folder received STT plan: %v", got)
	}
	for _, value := range []OrganizationDecision{source, hold} {
		if got := PlanFollowUp(value, true); len(got) != 0 {
			t.Fatalf("non-recording received STT plan: disposition=%s actions=%v", value.Disposition, got)
		}
	}
}

func TestOrganizationProfilesUsedByDecisionsRemainValid(t *testing.T) {
	for _, target := range []int{6_000, 20_000} {
		profile, err := rendering.OrganizationProfile(target)
		if err != nil || profile.Reference() == "" {
			t.Fatalf("organization profile %d invalid: %+v err=%v", target, profile, err)
		}
	}
}
