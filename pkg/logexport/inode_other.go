//go:build !unix

package logexport

import "os"

// fileInodeDevice on non-Unix builds collapses inode tracking to size+
// modtime as a best-effort proxy. Cluster pods always run on Linux so
// this fallback is only exercised on developer Windows machines, where
// rotation detection is best-effort by design.
func fileInodeDevice(info os.FileInfo) (uint64, uint64) {
	if info == nil {
		return 0, 0
	}
	return uint64(info.Size()), uint64(info.ModTime().UnixNano())
}
