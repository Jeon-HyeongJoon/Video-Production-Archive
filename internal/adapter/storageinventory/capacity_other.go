//go:build !windows

package storageinventory

import "syscall"

func storageCapacity(root string) (uint64, uint64, error) {
	var filesystem syscall.Statfs_t
	if err := syscall.Statfs(root, &filesystem); err != nil {
		return 0, 0, err
	}
	blockSize := uint64(filesystem.Bsize)
	return uint64(filesystem.Blocks) * blockSize, uint64(filesystem.Bavail) * blockSize, nil
}
