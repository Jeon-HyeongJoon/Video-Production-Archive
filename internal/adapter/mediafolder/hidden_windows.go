//go:build windows

package mediafolder

import (
	"io/fs"
	"strings"
	"syscall"
)

const (
	windowsFileAttributeHidden = 0x2
	windowsFileAttributeSystem = 0x4
)

func hidden(entry fs.DirEntry) bool {
	if entry == nil || strings.HasPrefix(entry.Name(), ".") {
		return true
	}
	info, err := entry.Info()
	if err != nil {
		return true
	}
	attributes, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && attributes.FileAttributes&(windowsFileAttributeHidden|windowsFileAttributeSystem) != 0
}
