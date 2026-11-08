//go:build windows

package hub

import "golang.org/x/sys/windows"

func storageVolume(path string) (total, free int64, err error) {
	p, e := windows.UTF16PtrFromString(path)
	if e != nil {
		return 0, 0, e
	}
	var available, size, unused uint64
	err = windows.GetDiskFreeSpaceEx(p, &available, &size, &unused)
	return int64(size), int64(available), err
}
