//go:build !linux

package client

import "io/fs"

func inode(fs.FileInfo) uint64 { return 0 }
