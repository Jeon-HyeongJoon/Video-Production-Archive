package archive

import (
	"errors"
	"syscall"
)

func storageIOError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.ENOSPC) {
		return errors.Join(ErrStorageFull, ErrStorageUnavailable, err)
	}
	return errors.Join(ErrStorageUnavailable, err)
}
