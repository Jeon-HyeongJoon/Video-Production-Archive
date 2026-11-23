package organization

import (
	"path"
	"regexp"
	"strings"

	"videoarchive/internal/core/mediaanalysis"
)

// SourceRoleEvidence describes a directory listing. It never grants permission
// to render or discard a file.
type SourceRoleEvidence struct {
	Role            string `json:"role"`
	Strength        string `json:"strength"`
	Basis           string `json:"basis"`
	MatchedProxies  int    `json:"matched_proxies,omitempty"`
	UnpairedProxies int    `json:"unpaired_proxies,omitempty"`
}

const (
	roleUnknown       = "UNKNOWN"
	ShortCameraHint   = "SHORT_HIGH_BITRATE_CAMERA_CANDIDATE"
	LongRecordingHint = "LONG_HD_WITH_AUDIO"
	RoleSource        = "VJ_SOURCE"
	RoleRecording     = "RECORDING"
	RoleProxy         = "CAMERA_PROXY"
	RoleEditorial     = "EDITORIAL_OR_REFERENCE"
	RoleUnresolved    = "UNRESOLVED"
	roleExplicit      = "EXPLICIT"
	roleSuggested     = "SUGGESTED"
)

var (
	recordLabel   = regexp.MustCompile(`(^|[^a-z])(pgm|cln)([^a-z]|$)|클린본|[-_]녹[-_]|풀[ ]*(녹화|영상)|전체[ ]*녹화|갤러리녹화`)
	sourceLabel   = regexp.MustCompile(`(^|[^a-z])vj([^a-z]|$)|소스[ ]*(촬영|컷)`)
	fixedCamera   = regexp.MustCompile(`고정[ ]*캠|천장[ ]*(캠|[0-9]+번)|메인[ ]*캠|측면[ ]*캠|우측카메라|(^|/)[123]번[ ]*캠(/|$)|(^|[_/])(main|c[123])(/|$)`)
	editedLabel   = regexp.MustCompile(`ok[ ]*(컷|cut)|셀렉|셀랙|샐랙|샐렉|보고용|요약본|색보정|하이라이트|타이틀|컷[ ]*편집|수정본`)
	rawCameraName = regexp.MustCompile(`^(c|clip|h[0-9]{2}a|dji_|img_|[0-9]+a_|v[0-9]+_|cbh_)[0-9]+\.(mp4|mov|mxf|mts)$`)
	cardDirectory = regexp.MustCompile(`(^|/)((m4root|xdroot)/clip|bdmv/stream|[0-9]{3}eos[a-z0-9]*)$`)
	subFile       = regexp.MustCompile(`^(c[0-9]+)s[0-9]{2}\.mp4$`)
)

func folderKey(folder string) string { return strings.ToLower(strings.ReplaceAll(folder, "\\", "/")) }

func CameraProxyDirectory(folder string) bool {
	p := "/" + folderKey(folder)
	return strings.HasSuffix(p, "/m4root/sub")
}

func proxyOriginalPath(relative string) string {
	folder, name := path.Split(folderKey(relative))
	folder = strings.TrimSuffix(folder, "/")
	m := subFile.FindStringSubmatch(name)
	if !CameraProxyDirectory(folder) || m == nil {
		return ""
	}
	return path.Join(path.Dir(folder), "clip", m[1]+".mp4")
}

// CameraOriginalIndex contains only primary directory entries from one volume
// and one observation. Case-colliding entries cannot establish a unique match.
type CameraOriginalIndex map[string]originalEntry
type originalEntry struct {
	path string
	size uint64
}

func (index CameraOriginalIndex) Observe(relative string, size uint64) {
	key := folderKey(relative)
	if !strings.HasSuffix("/"+path.Dir(key), "/m4root/clip") {
		return
	}
	if _, exists := index[key]; exists {
		index[key] = originalEntry{}
		return
	}
	index[key] = originalEntry{relative, size}
}

func (index CameraOriginalIndex) Match(relative string, size uint64) string {
	original := index[proxyOriginalPath(relative)]
	if size > 0 && size < original.size {
		return original.path
	}
	return ""
}

// AssessSourceRole evaluates the directory and its directly listed video files.
func AssessSourceRole(folder string, names []string) SourceRoleEvidence {
	p := folderKey(folder)
	parts := strings.Split(p, "/")
	last := parts[len(parts)-1]
	seg := map[string]bool{}
	recovery := false
	for _, s := range parts {
		seg[strings.Trim(s, " -_#().[]")] = true
		recovery = recovery || strings.HasPrefix(s, "영상_") || s == "디렉터리 완전한 파일"
	}
	evidence := func(role, strength, basis string) SourceRoleEvidence {
		return SourceRoleEvidence{Role: role, Strength: strength, Basis: basis}
	}
	if CameraProxyDirectory(p) {
		return evidence(RoleProxy, "REVIEW", "카메라 SUB 경로, 동일 카드의 CLIP 원본 대조 필요")
	}
	if recovery {
		return evidence(RoleUnresolved, "REVIEW", "복구·형식별 폴더로 행사와 컷 순서 미확인")
	}
	if seg["output"] || seg["렌더링"] || seg["자료"] || seg["서류"] || strings.Contains(p, "참고자료") || strings.Contains(p, ".fcpbundle/") || strings.Contains(p, "(footage)") || strings.Contains(p, ". 서류") {
		return evidence(RoleEditorial, roleExplicit, "편집 출력·프로젝트·참고자료 경로")
	}
	allCamera := len(names) > 0
	anyEdited := false
	allRecorded := len(names) > 0
	for _, name := range names {
		s := strings.ToLower(name)
		if strings.HasPrefix(s, "파일명 손실된 파일") {
			return evidence(RoleUnresolved, "REVIEW", "손실된 원본 파일명으로 행사와 컷 순서 미확인")
		}
		allCamera = allCamera && rawCameraName.MatchString(s)
		anyEdited = anyEdited || editedLabel.MatchString(s)
		allRecorded = allRecorded && (strings.Contains(s, "녹화") || strings.Contains(s, "screen_recording"))
	}
	source := sourceLabel.MatchString(p)
	recording := recordLabel.MatchString(p)
	fixed := fixedCamera.MatchString(p) || seg["aja"]
	card := cardDirectory.MatchString(p)
	rawContext := (seg["origin"] || seg["원본"] || card || source) && allCamera && len(names) >= 2
	if editedLabel.MatchString(last) || ((editedLabel.MatchString(p) || anyEdited) && !rawContext) {
		return evidence(RoleEditorial, roleSuggested, "선별·편집 표기, 원본과 별도 확인")
	}
	if strings.Contains(p, "리허설") || strings.Contains(p, "타임랩스") || strings.Contains(p, "인터벌") || (source && (recording || fixed)) {
		return evidence(RoleUnresolved, "REVIEW", "리허설·특수 촬영 또는 소스와 녹화 역할 충돌")
	}
	if source && rawContext {
		return evidence(RoleSource, roleExplicit, "VJ·소스촬영 경로와 카메라 원본 파일명")
	}
	if source {
		return evidence(RoleSource, roleSuggested, "VJ·소스촬영 표기, 개별 원본 여부 확인")
	}
	if recording {
		if (recordLabel.MatchString(last) && !strings.Contains(last, "편집")) || seg["pgm"] || seg["cln"] || seg["클린본"] {
			return evidence(RoleRecording, roleExplicit, "PGM·CLN·클린본 또는 녹화 역할 표기")
		}
		return evidence(RoleRecording, roleSuggested, "상위 녹화 표기, 해당 채널·편집 여부 확인")
	}
	if fixed {
		return evidence(RoleRecording, roleSuggested, "고정·메인·측면 카메라 또는 AJA, 채널·분할 확인")
	}
	if len(names) >= 2 && allCamera && (card || seg["origin"] || seg["원본"] || strings.Contains(p, "촬영물") || strings.Contains(p, "드론촬영")) {
		return evidence(RoleSource, roleSuggested, "카메라 파일명과 촬영 경로, 연속 녹화 분할 여부 확인")
	}
	if seg["source"] && allRecorded {
		return evidence(RoleRecording, roleSuggested, "편집 입력 파일명의 녹화 표기")
	}
	if seg["편집"] || strings.Contains(p, ". 편집") || strings.Contains(p, "편집컴") || (len(parts) > 1 && seg["source"]) {
		return evidence(RoleEditorial, roleSuggested, "편집 작업 트리 또는 편집 입력 경로")
	}
	if strings.Contains(p, "촬영물") || card {
		return evidence(RoleUnresolved, "REVIEW", "행사명·카드 경로만으로 촬영 역할 미확인")
	}
	return evidence(RoleUnresolved, roleUnknown, "명시적 폴더 역할 없음")
}

// SourceMetadataHint is a review cue for one inspected asset, independent of the
// ordering and identity evidence a folder needs for automatic processing.
func SourceMetadataHint(folder string, inspection mediaanalysis.MediaInspection) string {
	if CameraProxyDirectory(folder) {
		return ""
	}
	v := primaryVideo(inspection)
	if v == nil || inspection.DurationMillis <= 0 || v.Width <= 0 || v.Height <= 0 {
		return ""
	}
	if v.Width >= 1280 && v.Height >= 720 && inspection.DurationMillis >= 1_200_000 && len(inspection.AudioStreams) > 0 {
		return LongRecordingHint
	}
	if v.Width >= 1920 && v.Height >= 1080 && inspection.DurationMillis <= 180_000 && v.BitRate >= 25_000_000 {
		return ShortCameraHint
	}
	return ""
}
