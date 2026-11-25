//go:build !linux

package client

import "io/fs"

func inode(fs.FileInfo) uint64 { return 0 }

// changeTime is unknown here: the hash cache then never trusts a file without rehashing.
func changeTime(fs.FileInfo) (int64, bool) { return 0, false }
