package mediafolder

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"sort"
	"strings"
)

var ErrScanFailed = errors.New("media folder scan failed")

type Folder struct {
	RelativePath string
	VideoPaths   []string
}

// IncompleteScan counts unreadable directories and identifies the first in traversal order.
type IncompleteScan struct {
	Count       int
	FirstFolder string
	Cause       string
}

func (missing *IncompleteScan) record(directory string, err error) {
	missing.Count++
	if missing.Count == 1 {
		missing.FirstFolder = directory
		missing.Cause = "DIRECTORY_READ_FAILED"
		if errors.Is(err, fs.ErrPermission) {
			missing.Cause = "DIRECTORY_PERMISSION_DENIED"
		}
	}
}
func (e *IncompleteScan) Error() string { return "media folder inventory incomplete" }
func (e *IncompleteScan) Unwrap() error { return ErrScanFailed }

type Scanner struct {
	root directoryReader
}

type directoryReader interface {
	ReadDir(string) ([]fs.DirEntry, error)
}

func NewScanner(root directoryReader) (*Scanner, error) {
	if root == nil {
		return nil, ErrScanFailed
	}
	return &Scanner{root: root}, nil
}

func (scanner *Scanner) Discover() ([]Folder, error) {
	return scanner.DiscoverWithProgressContext(context.Background(), nil)
}

// DiscoverWithProgress reports immutable snapshots after each directory that
// adds supported video files. A caller may expose those snapshots as partial
// discovery, but only the returned complete value is suitable for revisioning.
func (scanner *Scanner) DiscoverWithProgress(report func([]Folder)) ([]Folder, error) {
	return scanner.DiscoverWithProgressContext(context.Background(), report)
}

// DiscoverWithProgressContext stops before reading another directory when the
// owning volume observation is no longer current. Progress snapshots are
// spaced geometrically after the first 64 changed directories so a large
// source does not repeatedly copy its entire inventory at a linear cadence.
func (scanner *Scanner) DiscoverWithProgressContext(ctx context.Context, report func([]Folder)) ([]Folder, error) {
	return scanner.DiscoverInventory(ctx, report, nil)
}

// DiscoverInventory observes all visible directories during the same protected walk.
func (scanner *Scanner) DiscoverInventory(ctx context.Context, report func([]Folder), directoryFacts func(string, map[string]int)) ([]Folder, error) {
	if ctx == nil {
		return nil, ErrScanFailed
	}
	grouped := make(map[string][]string)
	changedDirectories, lastReported, nextReportAt := 0, 0, 1
	var missing IncompleteScan
	if err := scanner.walk(ctx, "", grouped, &missing, report, &changedDirectories, &lastReported, &nextReportAt, directoryFacts); err != nil {
		return nil, err
	}
	if report != nil && changedDirectories > lastReported {
		report(snapshot(grouped))
	}
	if missing.Count > 0 {
		return nil, &missing
	}
	return snapshot(grouped), nil
}

func snapshot(grouped map[string][]string) []Folder {
	folders := make([]Folder, 0, len(grouped))
	for folder, videoPaths := range grouped {
		videoPaths = append([]string(nil), videoPaths...)
		sort.Slice(videoPaths, func(i, j int) bool { return strings.ToLower(videoPaths[i]) < strings.ToLower(videoPaths[j]) })
		folders = append(folders, Folder{RelativePath: folder, VideoPaths: videoPaths})
	}
	sort.Slice(folders, func(i, j int) bool { return folders[i].RelativePath < folders[j].RelativePath })
	return folders
}

func (scanner *Scanner) walk(ctx context.Context, directory string, grouped map[string][]string, missing *IncompleteScan, report func([]Folder), changedDirectories, lastReported, nextReportAt *int, directoryFacts func(string, map[string]int)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := scanner.root.ReadDir(directory)
	if err != nil {
		if directory != "" {
			missing.record(directory, err)
			return nil
		}
		return ErrScanFailed
	}
	children := make([]string, 0, len(entries))
	changed := false
	var extensions map[string]int
	if directoryFacts != nil {
		extensions = make(map[string]int)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			continue
		}
		excluded, metadataErr := hidden(entry)
		if metadataErr != nil {
			if entry.IsDir() {
				missing.record(path.Join(directory, entry.Name()), metadataErr)
				continue
			}
			return ErrScanFailed
		}
		if excluded {
			continue
		}
		relative := path.Join(directory, entry.Name())
		if entry.IsDir() {
			children = append(children, relative)
			continue
		}
		if !entry.Type().IsRegular() {
			continue
		}
		ext := strings.ToLower(path.Ext(entry.Name()))
		if extensions != nil {
			extensions[ext]++
		}
		if !SupportedVideoExtension(ext) {
			continue
		}
		folder := path.Dir(relative)
		if folder == "." {
			folder = "root"
		}
		grouped[folder] = append(grouped[folder], relative)
		changed = true
	}
	if directoryFacts != nil {
		directoryFacts(directory, extensions)
	}
	if changed && report != nil {
		(*changedDirectories)++
		if *changedDirectories >= *nextReportAt {
			report(snapshot(grouped))
			*lastReported = *changedDirectories
			if *nextReportAt < 64 {
				*nextReportAt = 64
			} else {
				*nextReportAt *= 2
			}
		}
	}
	for _, child := range children {
		if err := scanner.walk(ctx, child, grouped, missing, report, changedDirectories, lastReported, nextReportAt, directoryFacts); err != nil {
			return err
		}
	}
	return nil
}

func SupportedVideoExtension(extension string) bool {
	switch strings.ToLower(extension) {
	case ".mp4", ".mov", ".mkv", ".mxf", ".avi", ".mts", ".m2ts":
		return true
	default:
		return false
	}
}
