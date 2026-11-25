package repositorycheck

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestWalkIncludesLocalFilesAndExcludesChildCheckouts(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".git", "new", "nested/checkout/.git", "workers/feature"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"new/rule.md", "nested/checkout/rule.md", "workers/feature/rule.md", "workers/feature/.git"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var found []string
	err := WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			found = append(found, path)
		}
		return err
	})
	if err != nil || !slices.Equal(found, []string{filepath.Join(root, "new", "rule.md")}) {
		t.Fatal(found, err)
	}
}
