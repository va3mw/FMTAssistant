//go:build !windows

package fmtdata

import (
	"os"
	"syscall"
)

// fileID identifies a file by device and inode.
type fileID struct{ dev, ino uint64 }

// openShared opens path read-only. On non-Windows systems a plain open
// already lets other programs write, rename and delete the file. The app
// itself only ships for Windows; this lets the package's tests run anywhere.
func openShared(path string) (*os.File, fileID, int64, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, fileID{}, 0, err
	}
	fi, err := fh.Stat()
	if err != nil {
		fh.Close()
		return nil, fileID{}, 0, err
	}
	var id fileID
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		id = fileID{uint64(st.Dev), uint64(st.Ino)}
	}
	return fh, id, fi.Size(), nil
}
