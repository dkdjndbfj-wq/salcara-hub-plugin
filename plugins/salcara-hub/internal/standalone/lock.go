package standalone

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type dataLock struct {
	file *os.File
	once sync.Once
	err  error
}

func prepareDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("cannot prepare Hub data directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Hub data directory must be a real directory, not a symlink")
	}
	return nil
}

func acquireDataLock(dir string) (*dataLock, error) {
	if err := prepareDataDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(filepath.Clean(dir), ".salcara-hub.lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("Hub lock path must be a regular file; do not remove any lock until all Hub processes using this volume have stopped")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("cannot inspect Hub data lock")
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, errors.New("cannot open Hub data lock")
	}
	info, statErr := f.Stat()
	current, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil || !info.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		f.Close()
		return nil, errors.New("Hub lock path changed or is not a regular file")
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, errors.New("Hub data directory is already in use; run exactly one Hub writer per local persistent volume")
	}
	// Keep the file path forever: removing it while locked would let a second
	// process lock a new inode. The OS releases the lock on exit, including a
	// crash, so a stale lock file never requires deletion or secret inspection.
	return &dataLock{file: f}, nil
}

func (l *dataLock) release() error {
	l.once.Do(func() {
		l.err = l.file.Close()
	})
	return l.err
}
