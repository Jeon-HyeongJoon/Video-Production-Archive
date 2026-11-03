package organization

import (
	"errors"
	"reflect"
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
	if !reflect.DeepEqual(first, second) || first.Disposition != vocabulary.OrganizationHold ||
		!reflect.DeepEqual(first.HoldReasons, []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldAmbiguousMultipleVideos}) ||
		len(first.Actions) != 0 {
		t.Fatalf("ambiguous inputs were inferred or classification changed: first=%+v second=%+v", first, second)
	}
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
	if err != nil || decision.Disposition != vocabulary.OrganizationCompileSourceSequence ||
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
	decision, err := Classify(AutoRequest{
		DecisionID: "DECISION-SOURCE", WorkspaceDriveID: "DRIVE-1", Folder: "source", InputRevision: 1,
		Inputs: []Input{
			organizationInput("ASSET-2", "source/C0002.MP4", 12_000, 30_000_000, 3840, 2160),
			organizationInput("ASSET-1", "source/C0001.MP4", 10_000, 30_000_000, 3840, 2160),
			organizationInput("ASSET-3", "source/C0003.MP4", 8_000, 30_000_000, 3840, 2160),
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
	if len(timeline.Clips) != 3 || timeline.Clips[0].OriginalAssetID != "ASSET-1" || timeline.Clips[0].InMillis != 0 || timeline.Clips[0].OutMillis != 10_000 ||
		len(timeline.Markers) != 2 || timeline.Markers[0] != (CutMarker{BeforeClipID: "ASSET-1", AfterClipID: "ASSET-2", AtMillis: 10_000}) ||
		timeline.Markers[1] != (CutMarker{BeforeClipID: "ASSET-2", AfterClipID: "ASSET-3", AtMillis: 22_000}) || timeline.DurationMillis() != 30_000 {
		t.Fatalf("timeline order or markers are invalid: %+v", timeline)
	}
}

// Requirement: ORG-007
func TestTranscriptionPlanIncludesOnlyQCPassedRecordings(t *testing.T) {
	recording := OrganizationDecision{Disposition: vocabulary.OrganizationRenderRecordings}
	asIs := OrganizationDecision{Disposition: vocabulary.OrganizationAsIs}
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
