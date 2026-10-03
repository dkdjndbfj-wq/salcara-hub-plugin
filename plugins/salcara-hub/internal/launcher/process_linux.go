//go:build linux

package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

type managedChild struct {
	cmd  *exec.Cmd
	done chan struct{}
}
type managedRunner struct {
	cfg   Config
	mu    sync.Mutex
	child *managedChild
}

func (p *managedRunner) Start(ctx context.Context, r release) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.child != nil {
		select {
		case <-p.child.done:
			p.child = nil
		default:
			return errors.New("refusing a second Hub writer")
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	file, err := openVerified(r)
	if err != nil {
		return err
	}
	// Execute the already-open verified inode, not a disk pointer which could
	// be swapped between hashing and exec. FD 3 is assigned by ExtraFiles.
	cmd := exec.Command("/proc/self/fd/3")
	cmd.Args[0] = r.Path
	cmd.ExtraFiles = []*os.File{file}
	cmd.Env = childEnvironment(os.Environ(), p.cfg)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	child := &managedChild{cmd: cmd, done: make(chan struct{})}
	started := make(chan error, 1)
	go func() {
		// Pdeathsig belongs to the spawning Linux thread, so keep that thread
		// alive until Wait completes rather than relying on Go's thread pool.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := cmd.Start()
		file.Close()
		started <- err
		if err != nil {
			return
		}
		_ = cmd.Wait()
		close(child.done)
	}()
	if err = <-started; err != nil {
		return errors.New("cannot start verified Hub executable")
	}
	p.child = child
	if err = p.waitHealthy(ctx, child, r.Version); err != nil {
		if stopErr := p.stopLocked(); stopErr != nil {
			return errors.New("new Hub was unhealthy and could not be confirmed stopped")
		}
		return err
	}
	return nil
}
func childEnvironment(original []string, cfg Config) []string {
	drop := map[string]bool{"GOMEMLIMIT": true, "GOMAXPROCS": true, "SALCARA_HUB_CONTROL_SOCKET": true, "SALCARA_HUB_CONTROL_TOKEN_FILE": true, "SALCARA_HUB_DATA_DIR": true, "SALCARA_HUB_LISTEN": true}
	out := make([]string, 0, len(original)+6)
	for _, item := range original {
		name, _, _ := strings.Cut(item, "=")
		if !drop[name] {
			out = append(out, item)
		}
	}
	return append(out, "GOMEMLIMIT=320MiB", "GOMAXPROCS=2", "SALCARA_HUB_CONTROL_SOCKET="+ControlSocket, "SALCARA_HUB_CONTROL_TOKEN_FILE="+ControlTokenFile, "SALCARA_HUB_DATA_DIR="+cfg.DataDir, "SALCARA_HUB_LISTEN="+cfg.Listen)
}
func (p *managedRunner) waitHealthy(ctx context.Context, child *managedChild, version string) error {
	host, port, err := net.SplitHostPort(p.cfg.Listen)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	} else if host == "::" {
		host = "::1"
	}
	endpoint := "http://" + net.JoinHostPort(host, port) + "/healthz"
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("health redirect forbidden") }}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-child.done:
			return errors.New("verified Hub exited before becoming healthy")
		case <-ctx.Done():
			return errors.New("Hub health/version verification timed out")
		default:
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		res, e := client.Do(req)
		if e == nil {
			var state struct {
				OK      bool   `json:"ok"`
				Service string `json:"service"`
				Product string `json:"product"`
				Version string `json:"version"`
				PID     int    `json:"pid"`
			}
			e = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&state)
			res.Body.Close()
			if e == nil && res.StatusCode == 200 && state.OK && state.Service == "salcara-hub" && state.Product == Product && state.Version == version && state.PID == child.cmd.Process.Pid {
				select {
				case <-child.done:
					return errors.New("Hub exited before health acceptance")
				default:
					return nil
				}
			}
		}
		select {
		case <-child.done:
			return errors.New("Hub exited during health check")
		case <-ctx.Done():
			return errors.New("Hub health/version verification timed out")
		case <-ticker.C:
		}
	}
}
func (p *managedRunner) Stop() error { p.mu.Lock(); defer p.mu.Unlock(); return p.stopLocked() }
func (p *managedRunner) stopLocked() error {
	c := p.child
	if c == nil {
		return nil
	}
	select {
	case <-c.done:
		p.child = nil
		return nil
	default:
	}
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return errors.New("cannot signal Hub shutdown")
	}
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case <-c.done:
		p.child = nil
		return nil
	case <-timer.C:
	}
	if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return errors.New("Hub shutdown failed; no second writer started")
	}
	select {
	case <-c.done:
		p.child = nil
		return errors.New("Hub required forced termination; update not continued")
	case <-time.After(5 * time.Second):
		return errors.New("cannot confirm Hub process termination")
	}
}
func (p *managedRunner) exited() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.child == nil {
		return true
	}
	select {
	case <-p.child.done:
		return true
	default:
		return false
	}
}
