//go:build linux

package launcher

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	s, err := newStore(cfg, fixedPublicKey())
	if err != nil {
		return err
	}
	lock, err := launcherLock(s.dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	state, err := s.load()
	if err != nil {
		return err
	}
	if err = prepareControlPaths(); err != nil {
		return err
	}
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		return errors.New("cannot create private control identity")
	}
	token := hex.EncodeToString(random[:])
	file, err := os.OpenFile(ControlTokenFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("cannot create private control token file")
	}
	_, writeErr := file.WriteString(token + "\n")
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("cannot save private control identity")
	}
	tokenInfo, _ := os.Lstat(ControlTokenFile)
	defer removeSameFile(ControlTokenFile, tokenInfo)
	listener, err := net.Listen("unix", ControlSocket)
	if err != nil {
		return errors.New("cannot bind private launcher control socket")
	}
	if err = os.Chmod(ControlSocket, 0600); err != nil {
		listener.Close()
		return errors.New("cannot restrict private launcher control socket")
	}
	socketInfo, _ := os.Lstat(ControlSocket)
	defer removeSameFile(ControlSocket, socketInfo)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	runner := &managedRunner{cfg: cfg}
	state, err = s.startConfirmed(ctx, state, runner)
	if err != nil {
		listener.Close()
		return err
	}
	m := newManager(ctx, cfg, s, state, runner)
	m.mu.Lock()
	m.ready = true
	m.mu.Unlock()
	server := &http.Server{Handler: &control{manager: m, tokenDigest: sha256.Sum256([]byte(token))}, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 4096}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	logger.Info("Hub supervisor ready", "protocol", 1, "version", m.snapshot().CurrentVersion)
	var runErr error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case err = <-served:
			if !errors.Is(err, http.ErrServerClosed) {
				runErr = errors.New("private launcher control stopped")
			}
			break loop
		case <-tick.C:
			if m.operation.TryLock() {
				if runner.exited() {
					r, e := s.resolve(m.state.Current)
					if e == nil {
						e = runner.Start(ctx, r)
					}
					if e != nil {
						runErr = errors.New("confirmed Hub process exited and could not be safely restarted")
						m.operation.Unlock()
						break loop
					}
					logger.Warn("Hub process restarted from verified last-confirmed executable", "version", r.Version)
				}
				m.operation.Unlock()
			}
		}
	}
	// Serialize closed + WaitGroup.Add under the manager mutex before waiting.
	m.mu.Lock()
	m.closed = true
	m.ready = false
	m.mu.Unlock()
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = server.Shutdown(shutdownCtx)
	shutdownCancel()
	m.job.Wait()
	if e := runner.Stop(); e != nil && runErr == nil {
		runErr = e
	}
	return runErr
}
func launcherLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, ".launcher.lock")
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, errors.New("cannot open safe launcher lock")
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("invalid launcher lock")
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another Hub launcher owns this data volume")
	}
	return f, nil
}
func prepareControlPaths() error {
	if info, err := os.Lstat(ControlSocket); err == nil {
		if !ownedByUs(info) || info.Mode()&os.ModeSocket == 0 {
			return errors.New("private control socket path has unknown ownership/type")
		}
		conn, e := net.DialTimeout("unix", ControlSocket, 200*time.Millisecond)
		if e == nil {
			conn.Close()
			return errors.New("another launcher control socket is active")
		}
		if !errors.Is(e, syscall.ECONNREFUSED) && !errors.Is(e, os.ErrNotExist) {
			return errors.New("cannot prove private control socket is stale")
		}
		if err = os.Remove(ControlSocket); err != nil {
			return errors.New("cannot remove owned stale control socket")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect private control socket")
	}
	if info, err := os.Lstat(ControlTokenFile); err == nil {
		if !ownedByUs(info) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return errors.New("private control identity has unknown ownership/type")
		}
		if err = os.Remove(ControlTokenFile); err != nil {
			return errors.New("cannot remove owned stale ephemeral control identity")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect private control identity")
	}
	return nil
}
func ownedByUs(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}
func removeSameFile(path string, expected os.FileInfo) {
	if expected == nil {
		return
	}
	current, err := os.Lstat(path)
	if err == nil && os.SameFile(current, expected) && ownedByUs(current) {
		_ = os.Remove(path)
	}
}
