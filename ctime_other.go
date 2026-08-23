//go:build !unix

package main

import "os"

// fileCTime falls back to mtime on platforms without a portable ctime.
func fileCTime(st os.FileInfo) int64 {
	return st.ModTime().Unix()
}
