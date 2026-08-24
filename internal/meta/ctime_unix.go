//go:build unix && !darwin

package meta

import (
	"os"
	"syscall"
)

// fileCTime returns the file's inode change time (ctime).
func fileCTime(st os.FileInfo) int64 {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return sys.Ctim.Sec
	}
	return st.ModTime().Unix()
}
