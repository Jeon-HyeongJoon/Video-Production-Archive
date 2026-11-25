// Package repositorycheck scopes development checks to one source checkout.
package repositorycheck

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// WalkDir includes untracked work in root and leaves independent Git checkouts to their own checks.
func WalkDir(root string, visit fs.WalkDirFunc) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() && path != root {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			switch _, markerErr := os.Lstat(filepath.Join(path, ".git")); {
			case markerErr == nil:
				return filepath.SkipDir
			case !errors.Is(markerErr, fs.ErrNotExist):
				return markerErr
			}
		}
		return visit(path, entry, err)
	})
}
