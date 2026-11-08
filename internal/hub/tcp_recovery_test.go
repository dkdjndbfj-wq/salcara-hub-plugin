package hub

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The listener only observes native TCP writes and shortens an already-set
// deadline. There is no synthetic write gate or ResponseRecorder on this path.
type recoveryTCPConn struct {
	*net.TCPConn
	writeLimit time.Duration
	mu         sync.Mutex
	requested  time.Duration
	writing    *recoveryTCPWrite
}

type recoveryTCPWrite struct {
	started time.Time
	done    chan error
}

func (c *recoveryTCPConn) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		c.mu.Lock()
		c.requested = time.Until(deadline)
		c.mu.Unlock()
		if c.writeLimit > 0 && time.Until(deadline) > c.writeLimit {
			deadline = time.Now().Add(c.writeLimit)
		}
	}
	return c.TCPConn.SetWriteDeadline(deadline)
}

func (c *recoveryTCPConn) Write(data []byte) (int, error) {
	write := &recoveryTCPWrite{started: time.Now(), done: make(chan error, 1)}
	c.mu.Lock()
	c.writing = write
	c.mu.Unlock()
	n, err := c.TCPConn.Write(data)
	write.done <- err
	c.mu.Lock()
	c.writing = nil
	c.mu.Unlock()
	return n, err
}

type recoveryTCPListener struct {
	net.Listener
	accepted   chan *recoveryTCPConn
	writeLimit time.Duration
}

func (l *recoveryTCPListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tcp := c.(*net.TCPConn)
	if err := tcp.SetWriteBuffer(1024); err != nil {
		_ = tcp.Close()
		return nil, err
	}
	observed := &recoveryTCPConn{TCPConn: tcp, writeLimit: l.writeLimit}
	l.accepted <- observed
	return observed, nil
}

type recoveryTCPServer struct {
	*httptest.Server
	listener *recoveryTCPListener
}

func newRecoveryTCPServer(t *testing.T, h *Hub, writeLimit time.Duration) *recoveryTCPServer {
	t.Helper()
	s := httptest.NewUnstartedServer(h)
	l := &recoveryTCPListener{Listener: s.Listener, accepted: make(chan *recoveryTCPConn, 1), writeLimit: writeLimit}
	s.Listener = l
	s.Start()
	t.Cleanup(s.Close)
	return &recoveryTCPServer{Server: s, listener: l}
}

type recoveryTCPRead struct {
	client   *net.TCPConn
	server   *recoveryTCPConn
	reader   *bufio.Reader
	response *http.Response
	frames   *bufio.Reader
}

func (s *recoveryTCPServer) request(t *testing.T, p concurrentPhone, path string, bridge, slow bool, body any) *recoveryTCPRead {
	t.Helper()
	address, err := net.ResolveTCPAddr("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTCP("tcp", nil, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if slow {
		if err := c.SetReadBuffer(1024); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var observed *recoveryTCPConn
	select {
	case observed = <-s.listener.accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("loopback connection was not accepted")
	}
	method := http.MethodGet
	var data []byte
	if body != nil {
		method = http.MethodPost
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := http.NewRequest(method, s.URL+"/salcara-hub/v1"+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if bridge {
		r.Header.Set("X-Salcara-Device-Id", p.id)
		r.Header.Set("X-Salcara-Device-Secret", p.secret)
		r.Header.Set("X-Salcara-Binding-Id", p.binding)
		r.Header.Set("X-Salcara-Phone-Hash", p.phoneHash)
	} else {
		r.Header.Set("X-Salcara-Pair-Token", p.token)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if err := r.Write(c); err != nil {
		t.Fatal(err)
	}
	return &recoveryTCPRead{client: c, server: observed, reader: bufio.NewReader(c)}
}

func (r *recoveryTCPRead) status(t *testing.T, want int) {
	t.Helper()
	response, err := http.ReadResponse(r.reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("TCP response status = %d, want %d", response.StatusCode, want)
	}
	r.response = response
	r.frames = bufio.NewReader(response.Body)
}

func (r *recoveryTCPRead) frame(t *testing.T, want string) []byte {
	t.Helper()
	if err := r.client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var event string
	var data []byte
	for {
		line, err := r.frames.ReadString('\n')
		if err != nil {
			t.Fatalf("read TCP SSE frame %q: %v", want, err)
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if event == want {
				return data
			}
			if event != "" {
				t.Fatalf("SSE event = %q, want %q", event, want)
			}
			continue // SSE comments do not carry an event.
		}
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") {
			data = []byte(strings.TrimPrefix(line, "data: "))
		}
	}
}

func (s *recoveryTCPServer) stream(t *testing.T, p concurrentPhone, after int64, slow bool) *recoveryTCPRead {
	t.Helper()
	r := s.request(t, p, fmt.Sprintf("/app/stream?after=%d", after), false, slow, nil)
	r.status(t, http.StatusOK)
	r.frame(t, "device")
	return r
}

func (s *recoveryTCPServer) upload(t *testing.T, p concurrentPhone, events ...any) int64 {
	t.Helper()
	r := s.request(t, p, "/bridge/events", true, false, map[string]any{"deviceId": p.id, "events": events})
	defer r.client.Close()
	r.status(t, http.StatusOK)
	var result struct {
		LastSeq int64 `json:"lastSeq"`
	}
	if err := json.NewDecoder(r.response.Body).Decode(&result); err != nil || result.LastSeq == 0 {
		t.Fatalf("TCP event upload failed: result=%+v err=%v", result, err)
	}
	return result.LastSeq
}

func recoveryEvent(id, text string) any {
	return map[string]any{"type": "message", "sessionKey": "codex:s", "id": id, "text": text, "role": "assistant"}
}

func requireRecoveryEvent(t *testing.T, r *recoveryTCPRead, p concurrentPhone, id string, seq int64) {
	t.Helper()
	var event struct {
		ID       string `json:"id"`
		DeviceID string `json:"deviceId"`
		Seq      int64  `json:"seq"`
	}
	if err := json.Unmarshal(r.frame(t, "event"), &event); err != nil {
		t.Fatal(err)
	}
	if event.ID != id || event.DeviceID != p.id || event.Seq != seq {
		t.Fatalf("TCP event = %+v, want id=%q device=%q seq=%d", event, id, p.id, seq)
	}
}

func saturateRecoveryTCP(t *testing.T, h *Hub, p concurrentPhone, r *recoveryTCPRead) *recoveryTCPWrite {
	t.Helper()
	// Windows loopback may buffer much more than SO_SNDBUF/SO_RCVBUF. Feed
	// bounded, valid-sized frames until an observed native Write stays pending;
	// a large payload alone is not evidence of a slow network consumer. These
	// transport-only filler frames do not evict the cache being tested next.
	payload, err := json.Marshal(recoveryEvent("tcp-window-fill", strings.Repeat("x", 256<<10)))
	if err != nil {
		t.Fatal(err)
	}
	for range 128 {
		h.mu.Lock()
		h.accounts[deviceAccount(p.id)].sendLocked(sseMsg{event: "event", deviceID: p.id, data: payload})
		h.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		r.server.mu.Lock()
		write := r.server.writing
		r.server.mu.Unlock()
		if write != nil && time.Since(write.started) >= 25*time.Millisecond {
			return write
		}
	}
	t.Fatal("32 MiB of bounded frames did not saturate the loopback connection")
	return nil
}

func TestTCPRecoverySlowAppTimeoutRetainsBudgetAndReplays(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "economy", PingInterval: time.Hour})
	phones := concurrentPhones(t, h, 2)
	first, second := phones[0], phones[1]
	h.mu.Lock()
	h.resource.MaxAppStreams = 1
	h.resource.MaxGlobalAppStreams = 3
	owner := h.accounts[deviceAccount(first.id)]
	h.mu.Unlock()
	s := newRecoveryTCPServer(t, h, 2*time.Second)
	slow := s.stream(t, first, 0, true)
	healthy := s.stream(t, second, 0, false)
	cursor := s.upload(t, first, recoveryEvent("before-stall", "received"))
	requireRecoveryEvent(t, slow, first, "before-stall", cursor)
	write := saturateRecoveryTCP(t, h, first, slow)
	s.upload(t, first, recoveryEvent("large", strings.Repeat("x", 256<<10)))
	// The writer is blocked in the kernel, so the bounded delivery queue can
	// overflow while the admitted handler still owns its slot.
	queued := make([]any, 33)
	for i := range queued {
		queued[i] = recoveryEvent(fmt.Sprintf("queued-%d", i), "pending")
	}
	s.upload(t, first, queued...)
	awaitReadState(t, h, func() bool { return len(owner.apps) == 0 && owner.appStreams == 1 && h.appStreams == 2 })
	for _, path := range []string{"/app/stream", "/app/events?deviceId=" + first.id + "&sessionKey=codex:empty&wait=1"} {
		rejected := s.request(t, first, path, false, false, nil)
		rejected.status(t, http.StatusTooManyRequests)
		var body struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		}
		if err := json.NewDecoder(rejected.response.Body).Decode(&body); err != nil || body.Code != "app_read_capacity" || !body.Retryable || rejected.response.Header.Get("Retry-After") != "2" {
			t.Fatalf("TCP capacity response lost retry contract: %+v err=%v", body, err)
		}
		_ = rejected.client.Close()
	}
	seq := s.upload(t, second, recoveryEvent("other-owner", "still receiving"))
	requireRecoveryEvent(t, healthy, second, "other-owner", seq)
	select {
	case err := <-write.done:
		var networkError net.Error
		if !errors.As(err, &networkError) || !networkError.Timeout() {
			t.Fatalf("slow TCP write ended without a native deadline timeout: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("native TCP write did not obey the configured deadline")
	}
	slow.server.mu.Lock()
	requested := slow.server.requested
	slow.server.mu.Unlock()
	if requested < 29*time.Second || requested > 30*time.Second || sseWriteTimeout != 30*time.Second {
		t.Fatalf("Hub did not set its production 30-second write deadline: %v", requested)
	}
	awaitReadState(t, h, func() bool { return owner.appStreams == 0 && h.appStreams == 1 })
	reconnected := s.stream(t, first, cursor, false)
	requireRecoveryEvent(t, reconnected, first, "large", cursor+1)
	for i := range queued {
		requireRecoveryEvent(t, reconnected, first, fmt.Sprintf("queued-%d", i), cursor+int64(i)+2)
	}
	seq = s.upload(t, first, recoveryEvent("live-again", "received"))
	requireRecoveryEvent(t, reconnected, first, "live-again", seq)
	_ = reconnected.client.Close()
	_ = healthy.client.Close()
	awaitReadState(t, h, func() bool { return h.appStreams == 0 && h.eventWaiters == 0 })
}

func TestTCPRecoveryDisconnectReleasesAndReconnects(t *testing.T) {
	for _, transport := range []string{"app", "bridge", "long-poll"} {
		for _, disconnect := range []string{"fin", "reset"} {
			t.Run(transport+"/"+disconnect, func(t *testing.T) {
				h := newDeviceHub(t, Config{ResourceMode: "economy", PingInterval: time.Hour})
				phones := concurrentPhones(t, h, 2)
				first, second := phones[0], phones[1]
				h.mu.Lock()
				h.resource.MaxAppStreams = 1
				h.resource.MaxGlobalAppStreams = 2
				h.resource.MaxBridgeStreams = 1
				owner := h.accounts[deviceAccount(first.id)]
				dev := owner.devices[first.id]
				h.mu.Unlock()
				s := newRecoveryTCPServer(t, h, 0)
				healthy := s.stream(t, second, 0, false)
				path := "/app/stream"
				if transport == "bridge" {
					path = "/bridge/stream?deviceId=" + first.id
				} else if transport == "long-poll" {
					path = emptyWaitPath(first, 25)
				}
				open := func() *recoveryTCPRead {
					r := s.request(t, first, path, transport == "bridge", false, nil)
					if transport != "long-poll" {
						r.status(t, http.StatusOK)
						if transport == "app" {
							r.frame(t, "device")
						} else {
							var hello struct {
								EventScope string `json:"eventScope"`
							}
							if err := json.Unmarshal(r.frame(t, "hello"), &hello); err != nil || hello.EventScope == "" {
								t.Fatalf("bridge handshake has no binding scope: %+v, %v", hello, err)
							}
							r.frame(t, "pair.status")
						}
					}
					awaitReadState(t, h, func() bool {
						return owner.appStreams+owner.eventWaiters+h.bridgeStreams == 1
					})
					return r
				}
				original := open()
				if disconnect == "reset" {
					if err := original.client.SetLinger(0); err != nil {
						t.Fatal(err)
					}
				}
				if err := original.client.Close(); err != nil {
					t.Fatal(err)
				}
				awaitReadState(t, h, func() bool {
					return owner.appStreams == 0 && owner.eventWaiters == 0 && h.bridgeStreams == 0 && h.appStreams == 1 && (transport != "bridge" || dev.conn == nil)
				})
				reconnected := open()
				seq := s.upload(t, second, recoveryEvent("other-owner", "not disconnected"))
				requireRecoveryEvent(t, healthy, second, "other-owner", seq)
				if transport == "long-poll" {
					s.upload(t, first, recoveryEvent("wakes-reconnected-poll", "received"))
					reconnected.status(t, http.StatusOK)
					body, err := io.ReadAll(reconnected.response.Body)
					if err != nil || !bytes.Contains(body, []byte(`"id":"wakes-reconnected-poll"`)) || bytes.Contains(body, []byte(`"id":"other-owner"`)) {
						t.Fatalf("reconnected TCP poll did not receive only its owner's event: %s, %v", body, err)
					}
				}
				_ = reconnected.client.Close()
				_ = healthy.client.Close()
				awaitReadState(t, h, func() bool { return h.appStreams == 0 && h.eventWaiters == 0 && h.bridgeStreams == 0 })
			})
		}
	}
}
