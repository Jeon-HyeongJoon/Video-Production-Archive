package organization

import (
	"path"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/core/vocabulary"
)

func classifyRoleFolder(t *testing.T, folder string, names []string, rate int64) OrganizationDecision {
	t.Helper()
	var inputs []Input
	for _, name := range names {
		inputs = append(inputs, organizationInput(name, path.Join(folder, name), 60_000, rate, 1920, 1080))
	}
	result, err := Classify(AutoRequest{DecisionID: "D", WorkspaceDriveID: "W", Folder: folder, InputRevision: 2, Inputs: inputs, Policy: DefaultPolicy(), DecidedAt: time.Unix(1, 0)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// Requirement: ORG-016
func TestSourceRoleRoutesOriginalsAndPreservesReviewFolders(t *testing.T) {
	for _, tc := range []struct {
		folder      string
		names       []string
		role        string
		disposition vocabulary.OrganizationDisposition
		profile     string
	}{
		{"행사/origin/Vj/PRIVATE/M4ROOT/CLIP", []string{"C0002.MP4", "C0001.MP4"}, RoleSource, vocabulary.OrganizationCompileSourceSequence, "archive-1080p-20mbps@v1"},
		{"행사/origin/Vj/PRIVATE/M4ROOT/CLIP", []string{"C0001.MP4", "C0003.MP4"}, RoleSource, vocabulary.OrganizationHold, ""},
		{"행사/VJ", []string{"C0001.MP4"}, RoleSource, vocabulary.OrganizationHold, ""},
		{"행사/PGM/AJA", []string{"event.mp4"}, RoleRecording, vocabulary.OrganizationRenderRecordings, "archive-1080p-6mbps@v1"},
		{"행사/PGM", []string{"part01.mp4", "part02.mp4"}, RoleRecording, vocabulary.OrganizationHold, ""},
		{"행사/메인캠/PRIVATE/XDROOT/Clip", []string{"Clip0001.MXF", "Clip0002.MXF"}, RoleRecording, vocabulary.OrganizationHold, ""},
		{"행사/VJ/PGM", []string{"C0001.MP4", "C0002.MP4"}, RoleUnresolved, vocabulary.OrganizationHold, ""},
		{"행사/VJ/-Output", []string{"C0001.MP4", "C0002.MP4"}, RoleEditorial, vocabulary.OrganizationHold, ""},
		{"행사/ok 컷/vj", []string{"셀렉 컷.mp4"}, RoleEditorial, vocabulary.OrganizationHold, ""},
		{"자료/# 3. 참고자료", []string{"PGM.mp4"}, RoleEditorial, vocabulary.OrganizationHold, ""},
		{"37년도/영상_mov/PGM", []string{"C0001.MP4"}, RoleUnresolved, vocabulary.OrganizationHold, ""},
		{"37년도/영상_CANON", []string{"C0001.MP4", "C0002.MP4"}, RoleUnresolved, vocabulary.OrganizationHold, ""},
	} {
		d := classifyRoleFolder(t, tc.folder, tc.names, 20_000_000)
		if d.PolicyVersion != 2 || d.SourceRole == nil || d.SourceRole.Role != tc.role || d.Disposition != tc.disposition || d.ProfileReference != tc.profile || len(d.DecisionBasis) < 2 {
			t.Fatalf("%s: %+v", tc.folder, d)
		}
		if strings.Contains(tc.folder, "영상_") && strings.Contains(d.SourceRole.Basis, "복구") {
			t.Fatal("folder name alone asserted a recovery history")
		}
		if tc.disposition == vocabulary.OrganizationCompileSourceSequence {
			timeline, err := BuildSourceTimeline("T", 1, d)
			if err != nil || len(timeline.Markers) != 1 || timeline.Markers[0].AtMillis != 60_000 || timeline.Clips[0].OriginalAssetID != "C0001.MP4" {
				t.Fatalf("invalid markers: %+v %v", timeline, err)
			}
		}
	}
	_, err := Classify(AutoRequest{DecisionID: "D", WorkspaceDriveID: "W", Folder: "행사/PGM", InputRevision: 1, Policy: DefaultPolicy(), DecidedAt: time.Unix(1, 0), Inputs: []Input{organizationInput("A", "행사/VJ/C0001.MP4", 10000, 25000000, 1920, 1080)}})
	if err != ErrInvalidDecision {
		t.Fatalf("accepted a file outside the direct folder: %v", err)
	}
}

// Requirement: ORG-018
func TestMeasuredHintsSeparateReviewFromExecutableSequences(t *testing.T) {
	inspection := organizationInput("A", "C0001.MP4", 180_000, 25_000_000, 1920, 1080).Inspection
	folder := "37년도/영상_mov"
	if got := SourceMetadataHint(folder, inspection); got != ShortCameraHint {
		t.Fatal(got)
	}
	inspection.VideoStreams[0].BitRate = 0
	inspection.BitRate = 50_000_000
	if got := SourceMetadataHint(folder, inspection); got != "" {
		t.Fatalf("container bitrate supplied missing stream rate: %s", got)
	}
	inspection.DurationMillis = 1_200_000
	if got := SourceMetadataHint(folder, inspection); got != LongRecordingHint {
		t.Fatal(got)
	}
	if got := SourceMetadataHint("card/M4ROOT/SUB", inspection); got != "" {
		t.Fatal(got)
	}
	inspection.AudioStreams = nil
	if got := SourceMetadataHint(folder, inspection); got != "" {
		t.Fatal(got)
	}
	names := []string{"C0001.MP4", "C0002.MP4"}
	for _, tc := range []struct {
		folder string
		rate   int64
		want   vocabulary.OrganizationDisposition
	}{
		{"행사/M4ROOT/Clip", 25_000_000, vocabulary.OrganizationCompileSourceSequence},
		{"행사/M4ROOT/Clip", 24_999_999, vocabulary.OrganizationHold},
		{folder, 25_000_000, vocabulary.OrganizationHold},
		{"행사/메인캠/M4ROOT/Clip", 25_000_000, vocabulary.OrganizationHold},
	} {
		d := classifyRoleFolder(t, tc.folder, names, tc.rate)
		if d.Disposition != tc.want {
			t.Fatalf("unexpected automatic result: %+v", d)
		}
	}
}
