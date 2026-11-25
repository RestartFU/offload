package client

import (
	"io/fs"
	"syscall"
)

func inode(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

// changeTime is the inode change time, which (unlike mtime) no ordinary write can set back.
func changeTime(fi fs.FileInfo) (int64, bool) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ctim.Sec*1e9 + st.Ctim.Nsec, true
	}
	return 0, false
}
