package organization

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
)

// EventContext contains listing evidence pinned before inspecting a selection.
type EventContext struct {
	ListingDigest              string
	ListingChanged             bool `json:",omitempty"`
	InputRevision              uint64
	EventFolder                string
	ChannelFolders             []string
	ConflictingEventFolders    []string
	OverlappingPositionFolders []string
	EditorialWitness           string
	Preview                    *EventPreview
}

type EventPreview struct {
	Directory string `json:"directory"`
	Video     string `json:"video"`
	Subtitle  string `json:"subtitle"`
	Metadata  string `json:"metadata"`
}

type ListingContext struct {
	EventContext
	CopyCandidates []string
}

var eventDate = regexp.MustCompile(`^([0-9]{2})([-.]?)([0-9]{2})([-.]?)([0-9]{2})(.*)$`)
var positionName = regexp.MustCompile(`고정\s*캠|천장\s*(?:캠|[0-9]+번)|메인\s*캠|측면\s*캠|우측카메라|^[123]번\s*캠$`)

func eventAnchor(folder string) (string, []string) {
	parts := strings.Split(folder, "/")
	anchor, date := "", ""
	for i, part := range parts {
		match := eventDate.FindStringSubmatch(part)
		if match == nil || match[2] != match[4] || (len(match[6]) > 0 && match[6][0] >= '0' && match[6][0] <= '9') {
			continue
		}
		month, _ := strconv.Atoi(match[3])
		day, _ := strconv.Atoi(match[5])
		maxDay := [...]int{0, 31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
		if month < 1 || month > 12 || day < 1 || day > maxDay[month] {
			continue
		}
		token := match[1] + match[3] + match[5]
		if date != "" && date != token {
			return anchor, []string{strings.Join(parts[:i+1], "/")}
		}
		if date == "" && strings.Trim(match[6], " _-.[]") != "" {
			anchor, date = strings.Join(parts[:i+1], "/"), token
		}
	}
	return anchor, nil
}

// SelectedEventTitle uses only the event ancestor shared by every pinned source
// folder. A drive can contain many events, even ones with the same display name.
func SelectedEventTitle(folders []string) string {
	var shared string
	for _, folder := range folders {
		anchor, conflict := eventAnchor(strings.ReplaceAll(folder, "\\", "/"))
		if anchor == "" || len(conflict) > 0 || (shared != "" && shared != anchor) {
			return ""
		}
		shared = anchor
	}
	if shared == "" {
		return ""
	}
	match := eventDate.FindStringSubmatch(path.Base(shared))
	name := recordingSuffix.ReplaceAllString(match[6], "")
	return strings.Join(strings.Fields(strings.NewReplacer("-", " ", "_", " ").Replace(strings.Trim(name, " ._-"))), " ")
}

// An explicit capture label can carry a location or camera after the event name.
var recordingSuffix = regexp.MustCompile(`(?i)[_-](촬영|녹화|PGM|CLN)(?:[-_ ].*)?$`)

func positionBranch(folder, anchor string) (name, parent string) {
	if anchor == "" || !strings.HasPrefix(folder, anchor+"/") {
		return "", ""
	}
	parent = anchor
	for _, part := range strings.Split(strings.TrimPrefix(folder, anchor+"/"), "/") {
		if positionName.MatchString(strings.ToLower(part)) {
			return part, parent
		}
		parent = path.Join(parent, part)
	}
	return "", ""
}

func programBranch(folder, anchor string) (name, parent string) {
	if anchor == "" || !strings.HasPrefix(folder, anchor+"/") {
		return "", ""
	}
	parent = anchor
	for _, part := range strings.Split(strings.TrimPrefix(folder, anchor+"/"), "/") {
		switch strings.ToLower(strings.Trim(part, " -_#().[]")) {
		case "pgm":
			return "PGM", parent
		case "cln", "클린본":
			return "CLN", parent
		}
		parent = path.Join(parent, part)
	}
	return "", ""
}

func siblingOutput(folder string, dirs map[string]string) string {
	for p := folder; p != "." && p != "/"; p = path.Dir(p) {
		if strings.Trim(strings.ToLower(path.Base(p)), " -_#().[]") == "source" {
			return dirs[strings.ToLower(path.Dir(p))]
		}
	}
	return ""
}

func copyKey(f worker.MediaFolderObservation) string {
	type entry struct {
		Name string
		Size uint64
	}
	if len(f.Videos) == 0 || len(f.Videos) != f.VideoCount {
		return ""
	}
	list := make([]entry, 0, len(f.Videos))
	for _, v := range f.Videos {
		if v.FileName == "" {
			return ""
		}
		list = append(list, entry{strings.ToLower(v.FileName), v.SizeBytes})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Name == list[j].Name {
			return list[i].Size < list[j].Size
		}
		return list[i].Name < list[j].Name
	})
	encoded, _ := json.Marshal(list)
	return string(encoded)
}

func uniqueStem(stem, key string, ambiguous bool) string {
	if !ambiguous {
		return stem
	}
	sum := sha256.Sum256([]byte(key))
	return stem + "__" + hex.EncodeToString(sum[:6])
}

// InspectListing compares the entire READY revision, including folders outside
// the subsequent selection. A partial snapshot cannot establish a missing peer.
func InspectListing(obs worker.StorageObservation) map[string]*ListingContext {
	if obs.DiscoveryState != vocabulary.FolderDiscoveryReady || obs.DiscoveryRevision == 0 {
		return nil
	}
	contexts := make(map[string]*ListingContext, len(obs.Folders))
	copies := map[string][]string{}
	outputs := map[string]string{}
	for _, d := range obs.Directories {
		if strings.Trim(strings.ToLower(path.Base(d.RelativePath)), " -_#().[]") == "output" {
			parent := strings.ToLower(path.Dir(d.RelativePath))
			if _, seen := outputs[parent]; seen {
				outputs[parent] = ""
			} else {
				outputs[parent] = d.RelativePath
			}
		}
	}
	programs := map[string][]string{}
	positions := map[string][]string{}
	stems := map[string]string{}
	roleCounts := map[[2]string]int{}
	titleGroups := map[string]map[string]bool{}
	for _, f := range obs.Folders {
		anchor, conflict := eventAnchor(f.Folder)
		c := &ListingContext{EventContext: EventContext{InputRevision: obs.DiscoveryRevision, EventFolder: anchor, ConflictingEventFolders: conflict, EditorialWitness: siblingOutput(f.Folder, outputs)}}
		contexts[f.Folder] = c
		if key := copyKey(f); key != "" {
			sum := sha256.Sum256([]byte(key))
			c.ListingDigest = hex.EncodeToString(sum[:])
			copies[key] = append(copies[key], f.Folder)
		}
		if anchor == "" || len(conflict) > 0 {
			continue
		}
		names := make([]string, 0, len(f.Videos))
		for _, v := range f.Videos {
			names = append(names, v.FileName)
		}
		role := AssessSourceRole(f.Folder, names)
		if editorialPath(f.Folder) || c.EditorialWitness != "" {
			continue
		}
		pos, _ := positionBranch(f.Folder, anchor)
		prog, progParent := programBranch(f.Folder, anchor)
		stem := ""
		switch {
		case pos != "" && role.Role == RoleRecording:
			positions[anchor+"\x00"+strings.ToLower(pos)] = append(positions[anchor+"\x00"+strings.ToLower(pos)], f.Folder)
			stem = pos
		case role.Role == RoleRecording && role.Strength == roleExplicit:
			stem = prog
			if stem == "" {
				stem = "풀녹화"
			} else {
				programs[progParent] = append(programs[progParent], f.Folder)
			}
		case role.Role == RoleSource:
			stem = "VJ-소스컷"
		}
		if role.Role == RoleRecording && f.VideoCount != 1 {
			continue
		}
		if role.Role == RoleSource {
			if f.VideoCount < 2 {
				continue
			}
			inputs := make([]Input, len(f.Videos))
			for i, v := range f.Videos {
				inputs[i].RelativePath = v.RelativePath
			}
			if _, order := orderNumberedSequence(inputs); order != sequenceOrdered {
				continue
			}
		}
		if stem == "" {
			continue
		}
		stems[f.Folder] = archive.SafeWindowsFileStem(stem)
		roleCounts[[2]string{anchor, strings.ToLower(stems[f.Folder])}]++
		title := strings.ToLower(archive.SafeWindowsFileStem(path.Base(anchor)))
		if titleGroups[title] == nil {
			titleGroups[title] = map[string]bool{}
		}
		titleGroups[title][anchor] = true
	}
	for _, group := range copies {
		if len(group) < 2 {
			continue
		}
		slices.Sort(group)
		for _, p := range group {
			c := contexts[p]
			for _, q := range group {
				if q == p {
					continue
				}
				c.CopyCandidates = append(c.CopyCandidates, q)
				if c.EventFolder != "" && contexts[q].EventFolder != c.EventFolder {
					c.ConflictingEventFolders = append(c.ConflictingEventFolders, q)
				}
			}
		}
	}
	for p, stem := range stems {
		c := contexts[p]
		anchor := c.EventFolder
		if pos, parent := positionBranch(p, anchor); pos != "" {
			for _, q := range programs[parent] {
				if len(contexts[q].ConflictingEventFolders) == 0 {
					c.ChannelFolders = append(c.ChannelFolders, q)
				}
			}
			slices.Sort(c.ChannelFolders)
			for _, q := range positions[anchor+"\x00"+strings.ToLower(pos)] {
				if q != p {
					c.OverlappingPositionFolders = append(c.OverlappingPositionFolders, q)
				}
			}
			slices.Sort(c.OverlappingPositionFolders)
			if len(c.ChannelFolders) == 0 || len(c.OverlappingPositionFolders) > 0 {
				continue
			}
		}
		if len(c.ConflictingEventFolders) > 0 {
			continue
		}
		title := archive.SafeWindowsFileStem(path.Base(anchor))
		title = uniqueStem(title, anchor, len(titleGroups[strings.ToLower(title)]) > 1)
		stem = uniqueStem(stem, p, roleCounts[[2]string{anchor, strings.ToLower(stem)}] > 1)
		c.Preview = &EventPreview{Directory: title, Video: stem + ".mp4", Subtitle: stem + ".srt", Metadata: stem + ".json"}
	}
	return contexts
}

func editorialPath(folder string) bool {
	for _, p := range strings.Split(folderKey(folder), "/") {
		s := strings.Trim(p, " -_#().[]")
		if s == "source" || s == "output" || s == "렌더링" || strings.Contains(s, "편집") || strings.Contains(s, "참고자료") || strings.Contains(s, "리허설") || strings.Contains(s, "사전준비") || editedLabel.MatchString(s) {
			return true
		}
	}
	return false
}

// matchesInputs detects additions, removals and renames since selection, including a missing end cut.
func (c EventContext) matchesInputs(folder string, inputs []Input) bool {
	if c.ListingDigest == "" {
		return true
	}
	f := worker.MediaFolderObservation{Folder: folder, VideoCount: len(inputs)}
	for _, in := range inputs {
		f.Videos = append(f.Videos, worker.MediaFileObservation{FileName: path.Base(in.RelativePath), SizeBytes: uint64(in.Inspection.SizeBytes)})
	}
	digest := sha256.Sum256([]byte(copyKey(f)))
	return c.ListingDigest == hex.EncodeToString(digest[:])
}
