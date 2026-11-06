//go:build windows

package archive

import "os"

func syncStorageFile(file *os.File, directory bool) error {
	// Go opens directories with backup semantics on Windows, but
	// FlushFileBuffers does not accept every filesystem's directory handle.
	// Files are always flushed; atomic rename provides the directory boundary.
	if directory {
		return nil
	}
	return file.Sync()
}
