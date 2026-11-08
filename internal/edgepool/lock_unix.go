//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package edgepool

import (
	"os"
	"syscall"
)

func lockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) }
