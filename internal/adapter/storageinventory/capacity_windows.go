//go:build windows

package storageinventory

import "golang.org/x/sys/windows"

func storageCapacity(root string) (uint64, uint64, error) {
	pointer, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0, 0, err
	}
	var free, total uint64
	if err := windows.GetDiskFreeSpaceEx(pointer, &free, &total, nil); err != nil {
		return 0, 0, err
	}
	return total, free, nil
}
