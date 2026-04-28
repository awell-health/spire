//go:build unix

package logexport

import (
	"os"
	"syscall"
)

// fileInodeDevice returns the (inode, device) pair that uniquely
// identifies a file across rotations on Unix systems. A new (inode,
// device) means the file the agent is now writing to is not the one
// the tailer was tracking.
func fileInodeDevice(info os.FileInfo) (uint64, uint64) {
	if info == nil {
		return 0, 0
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino), uint64(st.Dev)
	}
	return 0, 0
}
