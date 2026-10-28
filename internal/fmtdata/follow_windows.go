package fmtdata

import (
	"os"

	"golang.org/x/sys/windows"
)

type fileID struct{ vol, hi, lo uint32 }

// openShared opens path read-only without locking it. os.Open does not share
// delete access on Windows, which would make renaming fmt.all fail while it
// is open, so the file is opened with every share flag.
func openShared(path string) (*os.File, fileID, int64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fileID{}, 0, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if err == windows.ERROR_FILE_NOT_FOUND || err == windows.ERROR_PATH_NOT_FOUND {
			return nil, fileID{}, 0, os.ErrNotExist
		}
		return nil, fileID{}, 0, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		windows.CloseHandle(h)
		return nil, fileID{}, 0, err
	}
	id := fileID{fi.VolumeSerialNumber, fi.FileIndexHigh, fi.FileIndexLow}
	size := int64(fi.FileSizeHigh)<<32 | int64(fi.FileSizeLow)
	return os.NewFile(uintptr(h), path), id, size, nil
}
