//go:build !windows

package archive

import "os"

func syncStorageFile(file *os.File, _ bool) error {
	return file.Sync()
}
