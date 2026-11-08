package edgepool

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

func privateMode(info os.FileInfo) bool {
	return runtime.GOOS == "windows" || info.Mode().Perm()&0077 == 0
}

func acquireLock(dir string) (*os.File, error) {
	if os.MkdirAll(dir, 0700) != nil {
		return nil, errors.New("cannot create private data directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !privateMode(info) {
		return nil, errors.New("data directory must be private and must not be a symlink")
	}
	path := filepath.Join(dir, ".edge-pool.lock")
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || !privateMode(info)) {
		return nil, errors.New("invalid data lock file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("cannot inspect data lock")
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, errors.New("cannot open data lock")
	}
	current, err := os.Lstat(path)
	opened, statErr := f.Stat()
	if err != nil || statErr != nil || !current.Mode().IsRegular() || !os.SameFile(current, opened) || !privateMode(current) {
		f.Close()
		return nil, errors.New("data lock changed")
	}
	if lockFile(f) != nil {
		f.Close()
		return nil, errors.New("pool data directory is already in use")
	}
	// Never unlink the lock inode. The OS releases the lock even after a crash.
	return f, nil
}
