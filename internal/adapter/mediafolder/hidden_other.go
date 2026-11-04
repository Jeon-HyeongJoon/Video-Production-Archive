//go:build !windows

package mediafolder

import (
	"io/fs"
	"strings"
)

func hidden(entry fs.DirEntry) bool {
	return entry == nil || strings.HasPrefix(entry.Name(), ".")
}
