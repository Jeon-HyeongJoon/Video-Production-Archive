package organization

import (
	"path"
	"regexp"
	"strings"

	"videoarchive/internal/core/vocabulary"
)

// RenderSelection records a target, independent of the measured source bitrate.
// The enclosing input links this decision to its checksum and MediaInspection.
type RenderSelection struct {
	Role             string `json:"role"`
	ProfileReference string `json:"profile_reference"`
	Basis            string `json:"basis"`
}

const RoleMixed = "MIXED_SOURCE_AND_RECORDING"

var captureLabel = regexp.MustCompile(`촬영물|(^|[ /_(-])촬영($|[ /_)\-])|(^|/)footage(/|$)`)
var recordingName = regexp.MustCompile(`녹화|(^|[^a-z])(pgm|cln|recordings?)([^a-z]|$)`)

// applyRoleProfiles is reached after checking noise, pinned listing conflicts,
// editorial context and position/channel evidence. No partial folder is dispatched.
func applyRoleProfiles(d *OrganizationDecision, policy Policy, positionVerified bool) {
	folder := folderKey(d.Folder)
	role := *d.SourceRole
	source := sourceLabel.MatchString(folder)
	recording := recordLabel.MatchString(folder) || recordingName.MatchString(folder)
	fixed := fixedCamera.MatchString(folder) || strings.Contains("/"+folder+"/", "/aja/")
	capture := captureLabel.MatchString(folder)
	unknown := func() { d.HoldReasons = []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldSourceRole} }
	if role.Role == RoleEditorial || role.Role == RoleProxy || (source && (recording || fixed)) ||
		strings.Contains(folder, "타임랩스") || strings.Contains(folder, "인터벌") {
		unknown()
		return
	}
	for _, segment := range strings.Split(folder, "/") {
		if strings.HasPrefix(segment, "영상_") || segment == "디렉터리 완전한 파일" {
			unknown()
			return
		}
	}
	selections := make([]RenderSelection, len(d.Inputs))
	allCuts, allRecordings := true, true
	for i, input := range d.Inputs {
		name := strings.ToLower(path.Base(input.RelativePath))
		fileSource := sourceLabel.MatchString(name)
		fileRecording := recordingName.MatchString(name)
		video := primaryVideo(input.Inspection)
		if video == nil || video.Width <= 0 || video.Height <= 0 || editedLabel.MatchString(name) || (fileSource && fileRecording) {
			unknown()
			return
		}
		short := input.Inspection.DurationMillis <= policy.ShortClipMaxDurationMillis
		assigned, basis := "", ""
		switch {
		case positionVerified:
			assigned, basis = RoleRecording, "행사 안에서 대조된 위치별 녹화 채널"
		case fixed && role.Strength != roleExplicit:
			// A fixed camera label alone does not establish its channel.
		case (source && fileRecording) || (recording && fileSource):
			// Explicit role conflict within the selected leaf.
		case recording || fileRecording:
			assigned, basis = RoleRecording, "폴더 또는 파일명의 녹화 채널 표기"
		case source || fileSource:
			assigned, basis = RoleSource, "VJ·소스컷 역할 표기와 원본 영상 검사"
		case capture && short && !strings.HasPrefix(name, "파일명 손실된 파일"):
			assigned, basis = RoleSource, "촬영물 경로·원본 파일명·검사된 컷 길이"
		case (capture || role.Role == RoleSource) && SourceMetadataHint(folder, input.Inspection) == ShortCameraHint:
			assigned, basis = RoleSource, "촬영 경로와 3분 이하·HD 1080 이상·영상 스트림 25 Mbps 이상"
		case capture && !source && input.Inspection.DurationMillis >= 1_200_000 && len(input.Inspection.AudioStreams) > 0:
			assigned, basis = RoleRecording, "촬영물 경로에서 검사된 20분 이상 유음 영상"
		}
		if assigned == "" {
			unknown()
			return
		}
		target := 6_000
		if assigned == RoleSource {
			target = 20_000
		}
		selections[i] = RenderSelection{Role: assigned, ProfileReference: mustOrganizationProfile(target).Reference(), Basis: basis}
		allCuts = allCuts && assigned == RoleSource
		allRecordings = allRecordings && assigned == RoleRecording
	}
	for i := range d.Inputs {
		d.Inputs[i].RenderSelection = &selections[i]
	}
	d.Disposition = vocabulary.OrganizationRenderRecordings
	d.Actions = []Action{ActionRender, ActionOrganize}
	folderRole := RoleMixed
	if allCuts || allRecordings {
		folderRole = selections[0].Role
		d.ProfileReference = selections[0].ProfileReference
	}
	d.SourceRole = &SourceRoleEvidence{Role: folderRole, Strength: roleExplicit, Basis: "원본별 폴더·파일명과 검사 기록으로 렌더 규격 결정"}
	if allCuts && len(d.Inputs) >= policy.SequenceMinFiles && allShort(d.Inputs, policy.ShortClipMaxDurationMillis) {
		ordered, status := orderNumberedSequence(d.Inputs)
		if status == sequenceOrdered && technicallyCompatible(ordered) {
			d.Inputs = ordered
			for i := range ordered {
				d.OrderedOriginalAssetIDs[i] = ordered[i].OriginalAssetID
			}
			d.Disposition = vocabulary.OrganizationCompileSourceSequence
			d.Actions = []Action{ActionSequence, ActionOrganize}
			d.DecisionBasis = append(d.DecisionBasis, "연속 컷 번호와 호환된 스트림 규격")
		} else {
			d.DecisionBasis = append(d.DecisionBasis, "합본 순서 또는 스트림 호환성 미확인, VJ 컷별 20 Mbps 보존")
		}
	}
	d.DecisionBasis = append(d.DecisionBasis, d.SourceRole.Basis)
}
