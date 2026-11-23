//go:build classification_contract

package organization

import (
	"encoding/json"
	"path"
	"testing"
	"time"

	"videoarchive/internal/core/vocabulary"
)

// Requirement: ORG-021
func TestV3RoutesOnlyRoleSupportedInputs(t *testing.T) {
	const hold = vocabulary.OrganizationHold
	for _, tc := range []struct {
		folder         string
		names          []string
		duration, rate int64
		version        uint32
		want           vocabulary.OrganizationDisposition
		profile        string
	}{
		{"행사/VJ", []string{"C0001.MP4", "C0002.MP4"}, 40_000, 30_000_000, 3, vocabulary.OrganizationCompileSourceSequence, "archive-1080p-20mbps@v1"},
		{"391216-행사/VJ", []string{"C0001.MP4", "C0002.MP4"}, 40_000, 30_000_000, 3, hold, ""},
		{"행사/PGM", []string{"event.mp4"}, 1_500_000, 30_000_000, 3, vocabulary.OrganizationRenderRecordings, "archive-1080p-6mbps@v1"},
		{"미지/묶음", []string{"C0001.MP4", "C0002.MP4"}, 40_000, 30_000_000, 3, hold, ""},
		{"미지/단독", []string{"event.mp4"}, 1_500_000, 6_000_000, 3, hold, ""},
		{"미지/장시간", []string{"event.mp4"}, 1_500_000, 30_000_000, 3, hold, ""},
		{"복구/영상_mov/PGM", []string{"event.mp4"}, 1_500_000, 30_000_000, 3, hold, ""},
		{"편집/Output/PGM", []string{"event.mp4"}, 1_500_000, 30_000_000, 3, hold, ""},
		{"미지/장시간", []string{"event.mp4"}, 1_500_000, 30_000_000, 2, vocabulary.OrganizationRenderRecordings, "archive-1080p-6mbps@v1"},
	} {
		policy := DefaultPolicy()
		policy.Version = tc.version
		var inputs []Input
		for _, name := range tc.names {
			inputs = append(inputs, organizationInput(name, path.Join(tc.folder, name), tc.duration, tc.rate, 1920, 1080))
		}
		req := AutoRequest{DecisionID: "D", WorkspaceDriveID: "W", Folder: tc.folder, Inputs: inputs, InputRevision: 7, Policy: policy, DecidedAt: time.Unix(1, 0)}
		if tc.folder == "391216-행사/VJ" {
			if err := json.Unmarshal([]byte(`{"EventContext":{"EventFolder":"391216-행사","InputRevision":7,"ConflictingEventFolders":["391206-행사/CLIP"]}}`), &req); err != nil {
				t.Fatal(err)
			}
		}
		decision, err := Classify(req)
		if err != nil || decision.Disposition != tc.want || decision.ProfileReference != tc.profile ||
			(tc.want == hold && (len(decision.Actions) != 0 || len(decision.HoldReasons) == 0)) {
			t.Errorf("policy v%d %s: want %s %q, got %s %q actions=%v reasons=%v err=%v", tc.version, tc.folder, tc.want, tc.profile, decision.Disposition, decision.ProfileReference, decision.Actions, decision.HoldReasons, err)
		}
	}
}

// Requirement: ORG-025
func TestV3PreservesEachCorroboratedPositionRecording(t *testing.T) {
	const event = "촬영물/391216-행사-본관"
	for _, tc := range []struct {
		label   string
		count   int
		sibling string
		allowed bool
	}{
		{"천장캠", 1, event + "/PGM", true}, {"측면캠", 1, event + "/PGM", true},
		{"우측카메라", 1, event + "/PGM", false}, {"측면캠", 1, "촬영물/다른행사/PGM", false}, {"천장캠", 1, "", false}, {"천장캠", 2, event + "/PGM", false},
	} {
		folder := event + "/" + tc.label
		req := AutoRequest{DecisionID: "D", WorkspaceDriveID: "W", Folder: folder, Policy: DefaultPolicy(), InputRevision: 7, DecidedAt: time.Unix(1, 0)}
		req.Policy.Version = 3
		for _, name := range []string{"channel-01.mp4", "channel-02.mp4"}[:tc.count] {
			req.Inputs = append(req.Inputs, organizationInput(name, path.Join(folder, name), 1_600_000, 18_000_000, 1920, 1080))
		}
		var peers []string
		if tc.sibling != "" {
			peers = []string{tc.sibling}
		}
		contextFacts := map[string]any{"EventFolder": event, "ChannelFolders": peers, "InputRevision": 7}
		if tc.label == "우측카메라" {
			contextFacts["OverlappingPositionFolders"] = []string{folder + "/본촬영/PRIVATE/XDROOT/Clip"}
		}
		ctx, err := json.Marshal(map[string]any{"EventContext": contextFacts})
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(ctx, &req); err != nil {
			t.Fatal(err)
		}
		d, err := Classify(req)
		if err != nil {
			t.Fatal(err)
		}
		if tc.allowed {
			if d.Disposition != vocabulary.OrganizationRenderRecordings || d.ProfileReference != "archive-1080p-6mbps@v1" || len(d.OrderedOriginalAssetIDs) != 1 {
				t.Errorf("%s with sibling %s: got %v %s", folder, tc.sibling, d.Disposition, d.ProfileReference)
			}
		} else if d.Disposition != vocabulary.OrganizationHold || len(d.Actions) > 0 {
			t.Errorf("unverified position rendered: %+v", tc)
		}
	}
}
