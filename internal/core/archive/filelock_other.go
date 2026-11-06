//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package archive

import (
	"os"
	"sync"
)

var fallbackFileLocks sync.Map

func tryExclusiveFileLock(file *os.File) (bool, error) {
	_, loaded := fallbackFileLocks.LoadOrStore(file.Name(), file)
	return !loaded, nil
}

func unlockFile(file *os.File) error {
	fallbackFileLocks.Delete(file.Name())
	return nil
}
