//go:build !windows

package hub

import "golang.org/x/sys/unix"

func storageVolume(path string) (total, free int64, err error) {
	var st unix.Statfs_t
	err = unix.Statfs(path, &st)
	return int64(st.Blocks) * int64(st.Bsize), int64(st.Bavail) * int64(st.Bsize), err
}
