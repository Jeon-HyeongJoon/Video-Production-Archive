package mediafolder

import (
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
	grouped := make(map[string][]string)
	if err := scanner.walk("", grouped); err != nil {
		return nil, err
	}
	folders := make([]Folder, 0, len(grouped))
	for folder, videoPaths := range grouped {
		sort.Slice(videoPaths, func(i, j int) bool { return strings.ToLower(videoPaths[i]) < strings.ToLower(videoPaths[j]) })
		folders = append(folders, Folder{RelativePath: folder, VideoPaths: videoPaths})
	}
	sort.Slice(folders, func(i, j int) bool { return folders[i].RelativePath < folders[j].RelativePath })
	return folders, nil
}

func (scanner *Scanner) walk(directory string, grouped map[string][]string) error {
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
	for _, entry := range entries {
		if entry.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			continue
		}
		if hidden(entry) {
			continue
		}
		relative := path.Join(directory, entry.Name())
		if entry.IsDir() {
			if err := scanner.walk(relative, grouped); err != nil {
				return err
			}
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
