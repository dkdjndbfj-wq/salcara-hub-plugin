package hostfleet

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// This fixture exercises the real SSH handshake and channel protocol. Only the
// manager's dial function is replaced; production public-address checks remain
// enabled and no test needs an external host, account, or credential.
type sshFixture struct {
	listener     net.Listener
	signer       ssh.Signer
	clientKey    ed25519.PrivateKey
	password     string
	passwordAuth atomic.Int64
	keyAuth      atomic.Int64
	sessions     chan struct{}
	hangSession  bool
	mu           sync.Mutex
	connections  map[net.Conn]struct{}
	closing      bool
	wg           sync.WaitGroup
	closeOnce    sync.Once
}

func newSSHFixture(t *testing.T, hangSession bool) *sshFixture {
	t.Helper()
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	clientPublic, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientSSH, err := ssh.NewPublicKey(clientPublic)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &sshFixture{listener: listener, signer: signer, clientKey: clientKey, password: "fixture-only-password-76b1bd27", sessions: make(chan struct{}, 16), hangSession: hangSession, connections: map[net.Conn]struct{}{}}
	config := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			f.passwordAuth.Add(1)
			if meta.User() != "root" || string(password) != f.password {
				return nil, errors.New("invalid test password")
			}
			return nil, nil
		},
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			f.keyAuth.Add(1)
			if meta.User() != "root" || !bytes.Equal(key.Marshal(), clientSSH.Marshal()) {
				return nil, errors.New("invalid test key")
			}
			return nil, nil
		},
	}
	config.AddHostKey(signer)
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			if f.closing {
				f.mu.Unlock()
				conn.Close()
				return
			}
			f.connections[conn] = struct{}{}
			f.wg.Add(1)
			f.mu.Unlock()
			go f.serve(conn, config)
		}
	}()
	t.Cleanup(f.Close)
	return f
}

func (f *sshFixture) serve(raw net.Conn, config *ssh.ServerConfig) {
	defer f.wg.Done()
	defer func() { raw.Close(); f.mu.Lock(); delete(f.connections, raw); f.mu.Unlock() }()
	conn, channels, requests, err := ssh.NewServerConn(raw, config)
	if err != nil {
		return
	}
	defer conn.Close()
	f.wg.Add(1)
	go func() { defer f.wg.Done(); ssh.DiscardRequests(requests) }()
	for incoming := range channels {
		if incoming.ChannelType() != "session" {
			_ = incoming.Reject(ssh.UnknownChannelType, "fixture accepts sessions only")
			continue
		}
		select {
		case f.sessions <- struct{}{}:
		default:
		}
		if f.hangSession {
			continue
		} // Deliberately send no channel-open reply.
		channel, reqs, err := incoming.Accept()
		if err != nil {
			continue
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer channel.Close()
			for req := range reqs {
				var exec struct{ Command string }
				if req.Type != "exec" || ssh.Unmarshal(req.Payload, &exec) != nil {
					_ = req.Reply(false, nil)
					continue
				}
				_ = req.Reply(true, nil)
				status := uint32(1)
				if exec.Command == inspectCommand {
					_, _ = io.WriteString(channel, "Linux\nx86_64\nroot\nsystemd\ntools-ready\n")
					status = 0
				}
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
				return
			}
		}()
	}
}

func (f *sshFixture) Close() {
	f.closeOnce.Do(func() {
		f.mu.Lock()
		f.closing = true
		_ = f.listener.Close()
		for c := range f.connections {
			_ = c.Close()
		}
		f.mu.Unlock()
		f.wg.Wait()
	})
}

func (f *sshFixture) dial(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" || address != "fixture.example:22" {
		return nil, errors.New("unexpected SSH destination")
	}
	return (&net.Dialer{}).DialContext(ctx, network, f.listener.Addr().String())
}

func (f *sshFixture) input() Input {
	return Input{Name: "Fixture server", Host: "fixture.example", Port: 22, Username: "root", Fingerprint: ssh.FingerprintSHA256(f.signer.PublicKey()), Credentials: Credentials{AuthType: "password", Password: f.password}}
}

func newTestManager(t *testing.T, f *sshFixture) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if f != nil {
		m.dial = f.dial
	}
	t.Cleanup(func() {
		// Close fixture sockets first so even a cancellation regression cannot
		// strand the test process in Manager.Close.
		if f != nil {
			f.Close()
		}
		m.Close()
	})
	return m, dir
}

func TestDiscoverStopsBeforeAuthentication(t *testing.T) {
	f := newSSHFixture(t, false)
	m, _ := newTestManager(t, f)
	discovery, err := m.Discover(context.Background(), "fixture.example", 22)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Fingerprint != ssh.FingerprintSHA256(f.signer.PublicKey()) || discovery.Algorithm != ssh.KeyAlgoED25519 {
		t.Fatalf("unexpected discovery: %+v", discovery)
	}
	if f.passwordAuth.Load() != 0 || f.keyAuth.Load() != 0 {
		t.Fatal("discovery attempted authentication")
	}
}

func TestFingerprintMismatchDoesNotSendCredentials(t *testing.T) {
	f := newSSHFixture(t, false)
	m, _ := newTestManager(t, f)
	in := f.input()
	in.Fingerprint = "SHA256:not-the-server-key"
	if _, err := m.Check(context.Background(), in); err == nil || !strings.Contains(err.Error(), "指纹") {
		t.Fatalf("expected fingerprint mismatch, got %v", err)
	}
	if f.passwordAuth.Load() != 0 || f.keyAuth.Load() != 0 {
		t.Fatal("credentials were offered before host-key acceptance")
	}
	if len(m.State().Hosts) != 0 {
		t.Fatal("failed check persisted a host")
	}
}

func TestCheckSaveEncryptedRegistryAndRestart(t *testing.T) {
	for _, auth := range []string{"password", "key", "encrypted-key"} {
		t.Run(auth, func(t *testing.T) {
			f := newSSHFixture(t, false)
			m, dir := newTestManager(t, f)
			in := f.input()
			if auth == "password" {
				in.PrivateKey, in.Passphrase = "unused-private-key", "unused-passphrase"
			} else {
				in.AuthType = "key"
				var block *pem.Block
				var err error
				if auth == "encrypted-key" {
					in.Passphrase = "fixture-only-key-passphrase-936afc4e"
					block, err = ssh.MarshalPrivateKeyWithPassphrase(f.clientKey, "hostfleet-fixture", []byte(in.Passphrase))
				} else {
					block, err = ssh.MarshalPrivateKey(f.clientKey, "hostfleet-fixture")
				}
				if err != nil {
					t.Fatal(err)
				}
				in.PrivateKey = string(pem.EncodeToMemory(block))
			}
			check, err := m.Check(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if !check.CanDeploy || check.Platform != "linux" || check.Arch != "amd64" {
				t.Fatalf("unexpected inspection: %+v", check)
			}
			if len(m.State().Hosts) != 0 {
				t.Fatal("check must not save credentials")
			}
			state, err := m.Save(check.CheckID, 1)
			if err != nil {
				t.Fatal(err)
			}
			if state.Revision != 2 || len(state.Hosts) != 1 {
				t.Fatalf("unexpected save state: %+v", state)
			}
			if _, err := m.Save(check.CheckID, state.Revision); err == nil {
				t.Fatal("check was not consumed")
			}
			if auth == "password" && f.passwordAuth.Load() == 0 || auth != "password" && f.keyAuth.Load() == 0 {
				t.Fatal("expected authentication method was not exercised")
			}
			m.mu.Lock()
			next := m.saved
			next.Hosts = append([]record{}, next.Hosts...)
			next.Hosts[0].Token = strings.Repeat("ab", 32)
			err = m.persistLocked(next)
			m.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			public, err := json.Marshal(m.State())
			if err != nil {
				t.Fatal(err)
			}
			ciphertext, err := os.ReadFile(filepath.Join(dir, "servers.enc"))
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{in.Password, in.PrivateKey, in.Passphrase, strings.Repeat("ab", 32)} {
				if secret != "" && (bytes.Contains(public, []byte(secret)) || bytes.Contains(ciphertext, []byte(secret))) {
					t.Fatal("secret present in public state or plaintext registry")
				}
			}
			for _, field := range []string{`"password":`, `"private_key":`, `"passphrase":`, `"token":`, `"secret":`} {
				if bytes.Contains(public, []byte(field)) {
					t.Fatalf("public state exposes %s", field)
				}
			}
			if json.Valid(ciphertext) {
				t.Fatal("registry was saved as plaintext JSON")
			}
			key, err := os.ReadFile(filepath.Join(dir, "servers.key"))
			if err != nil || len(key) != 32 {
				t.Fatalf("invalid registry key: length %d, err %v", len(key), err)
			}
			m.Close()
			restarted, err := New(dir, "")
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			restarted.dial = f.dial
			if got := restarted.State(); got.Revision != state.Revision || len(got.Hosts) != 1 || got.Hosts[0] != state.Hosts[0] {
				t.Fatalf("restart lost registry: %+v", got)
			}
			restored := restarted.saved.Hosts[0]
			if restored.Token != strings.Repeat("ab", 32) {
				t.Fatal("restart lost controller credential")
			}
			if auth == "password" && (restored.Secret.PrivateKey != "" || restored.Secret.Passphrase != "") || auth != "password" && restored.Secret.Password != "" {
				t.Fatal("unused authentication material was retained")
			}
			if _, err := restarted.Check(context.Background(), inputFor(restored)); err != nil {
				t.Fatalf("restored credential cannot authenticate: %v", err)
			}
		})
	}
}

func TestCheckExpiryAndRevisionConflict(t *testing.T) {
	f := newSSHFixture(t, false)
	m, _ := newTestManager(t, f)
	check, err := m.Check(context.Background(), f.input())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Save(check.CheckID, 0); err == nil {
		t.Fatal("stale revision was accepted")
	}
	state, err := m.Save(check.CheckID, 1)
	if err != nil {
		t.Fatalf("revision conflict consumed valid check: %v", err)
	}
	check, err = m.Check(context.Background(), f.input())
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	expired := m.checks[check.CheckID]
	expired.expires = time.Now().Add(-time.Second)
	m.checks[check.CheckID] = expired
	m.mu.Unlock()
	if _, err := m.Save(check.CheckID, state.Revision); err == nil {
		t.Fatal("expired check was accepted")
	}
	if got := m.State(); got.Revision != state.Revision || len(got.Hosts) != 1 {
		t.Fatal("rejected save changed registry")
	}
}

func TestUnansweredSSHChannelOpenIsCancellable(t *testing.T) {
	for _, shutdown := range []string{"deadline", "manager-close"} {
		t.Run(shutdown, func(t *testing.T) {
			f := newSSHFixture(t, true)
			m, _ := newTestManager(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			if shutdown == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 500*time.Millisecond)
			}
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := m.Check(ctx, f.input()); result <- err }()
			select {
			case <-f.sessions:
			case <-time.After(3 * time.Second):
				t.Fatal("SSH session was not requested")
			}
			closed := make(chan struct{})
			if shutdown == "deadline" {
				close(closed)
			} else {
				go func() { m.Close(); close(closed) }()
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("unanswered channel unexpectedly succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("blocked channel-open ignored cancellation")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("Manager.Close did not finish")
			}
		})
	}
}
