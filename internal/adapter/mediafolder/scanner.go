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
	if ctx == nil {
		return nil, ErrScanFailed
	}
	grouped := make(map[string][]string)
	changedDirectories, lastReported, nextReportAt := 0, 0, 1
	if err := scanner.walk(ctx, "", grouped, report, &changedDirectories, &lastReported, &nextReportAt); err != nil {
		return nil, err
	}
	if report != nil && changedDirectories > lastReported {
		report(snapshot(grouped))
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

func (scanner *Scanner) walk(ctx context.Context, directory string, grouped map[string][]string, report func([]Folder), changedDirectories, lastReported, nextReportAt *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := scanner.root.ReadDir(directory)
	if err != nil {
		// Protected operating-system folders on an otherwise readable source
		// drive are not media input. The selected root itself must remain
		// readable, but an inaccessible descendant is isolated and skipped.
		if directory != "" {
			return nil
		}
		return ErrScanFailed
	}
	children := make([]string, 0, len(entries))
	changed := false
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			continue
		}
		if hidden(entry) {
			continue
		}
		relative := path.Join(directory, entry.Name())
		if entry.IsDir() {
			children = append(children, relative)
			continue
		}
		if !entry.Type().IsRegular() || !supported(path.Ext(entry.Name())) {
			continue
		}
		folder := path.Dir(relative)
		if folder == "." {
			folder = "root"
		}
		grouped[folder] = append(grouped[folder], relative)
		changed = true
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
		if err := scanner.walk(ctx, child, grouped, report, changedDirectories, lastReported, nextReportAt); err != nil {
			return err
		}
	}
	return nil
}

func supported(extension string) bool {
	switch strings.ToLower(extension) {
	case ".mp4", ".mov", ".mkv", ".mxf", ".avi", ".mts", ".m2ts":
		return true
	default:
		return false
	}
}
