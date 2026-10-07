package hub

import (
	"container/list"
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// Device is what the bridge registers (PROTOCOL.md §2). Tools and projects are
// kept as raw JSON so newer bridges can add fields without a hub upgrade.
type Device struct {
	DeviceID string          `json:"deviceId"`
	Name     string          `json:"name"`
	OS       string          `json:"os"`
	Version  string          `json:"version"`
	Tools    json.RawMessage `json:"tools"`
	Projects json.RawMessage `json:"projects"`
}

// DeviceStatus = Device + online + lastSeen (ms).
type DeviceStatus struct {
	Device
	Online   bool  `json:"online"`
	LastSeen int64 `json:"lastSeen"`
}

type storedEvent struct {
	seq        int64
	deviceID   string
	sessionKey string
	data       []byte // full JSON including seq and deviceId
}

// ring is a fixed-capacity FIFO of events ordered by seq.
type ring struct {
	capacity       int
	bytes          int64
	maxBytes       int64
	buf            []*storedEvent
	start          int
	n              int
	evictedThrough int64
}

func newRing(size int) *ring { return &ring{buf: make([]*storedEvent, size), capacity: size} }

func (r *ring) clearCache() {
	for r.n > 0 {
		r.pop()
	}
	r.buf = nil
	r.start = 0
}

func newBoundedRing(size int, maxBytes int64) *ring {
	return &ring{capacity: size, maxBytes: maxBytes}
}

func (r *ring) pop() {
	if r.n == 0 {
		return
	}
	e := r.buf[r.start]
	if e.seq > r.evictedThrough {
		r.evictedThrough = e.seq
	}
	r.bytes -= int64(len(e.data))
	r.buf[r.start] = nil
	r.start = (r.start + 1) % len(r.buf)
	r.n--
}

func (r *ring) resize(size int, maxBytes int64) {
	next := newBoundedRing(size, maxBytes)
	next.evictedThrough = r.evictedThrough
	for i := 0; i < r.n; i++ {
		next.push(r.buf[(r.start+i)%len(r.buf)])
	}
	*r = *next
}

func (r *ring) push(e *storedEvent) {
	for r.n > 0 && (r.n >= len(r.buf) || r.maxBytes > 0 && r.bytes+int64(len(e.data)) > r.maxBytes) {
		r.pop()
	}
	if r.maxBytes > 0 && int64(len(e.data)) > r.maxBytes {
		if e.seq > r.evictedThrough {
			r.evictedThrough = e.seq
		}
		return
	}
	if len(r.buf) == 0 {
		r.buf = make([]*storedEvent, r.capacity)
	}
	r.buf[(r.start+r.n)%len(r.buf)] = e
	r.n++
	r.bytes += int64(len(e.data))
}

// after returns the buffered events with seq > after, oldest first.
func (r *ring) after(after int64) []*storedEvent {
	var out []*storedEvent
	for i := 0; i < r.n; i++ {
		e := r.buf[(r.start+i)%len(r.buf)]
		if e.seq > after {
			out = append(out, e)
		}
	}
	return out
}

type sessionBuf struct {
	ring    *ring
	updated int64 // seq of the last event, for eviction
	status  string
	tool    string
}

type sseMsg struct {
	event    string
	data     []byte
	seq      int64 // >0 for timeline events
	deviceID string
}

type appSub struct {
	queuedBytes    int64 // guarded by Hub.mu
	maxQueuedBytes int64
	ch             chan sseMsg
	closed         chan struct{}
	once           sync.Once
	deviceID       string
}

func (s *appSub) close() { s.once.Do(func() { close(s.closed) }) }

type bridgeConn struct {
	cmds        chan []byte
	pairChanged chan struct{} // coalesced state, separate from command capacity
	closed      chan struct{}
	once        sync.Once
}

func (c *bridgeConn) close() { c.once.Do(func() { close(c.closed) }) }

type device struct {
	metadataBytes     int64 // conservative retained/decoded metadata charge, guarded by Hub.mu
	info              Device
	lastSeen          int64
	conn              *bridgeConn // nil while offline
	standby           *bridgeConn // handover only; never reports this station online
	standbyAt         int64
	standbyComputerID string
	secretHash        [32]byte
	hasSecret         bool
	pairHash          [32]byte
	hasPair           bool
	pairRevision      int64
	phoneHash         string
	bindingID         string
	pairAttemptID     string
	lastAppSeen       int64 // last request from the paired phone (memory only); push waits while it is active
	banned            bool
	banReason         string
	bannedAt          int64
	latency           latencyState
}

func (d *device) status() DeviceStatus {
	st := DeviceStatus{Device: d.info, Online: d.conn != nil, LastSeen: d.lastSeen}
	if st.Online {
		st.LastSeen = nowMs()
	}
	if len(st.Tools) == 0 || string(st.Tools) == "null" {
		st.Tools = json.RawMessage("[]")
	}
	if len(st.Projects) == 0 || string(st.Projects) == "null" {
		st.Projects = json.RawMessage("[]")
	}
	return st
}

type account struct {
	metadataCharged       bool // account map/identity reserve is charged only once
	owner                 *Hub
	cacheBytes            int64
	cacheEntry            *list.Element
	resource              ResourcePreset
	sessionEvictedThrough int64
	id                    string
	devices               map[string]*device
	seq                   int64
	events                *ring
	sessions              map[string]*sessionBuf
	apps                  map[*appSub]struct{}
	// wake is closed (and replaced) whenever an event is appended, waking
	// phones long-polling /app/events. Lazily created under h.mu.
	wake chan struct{}
	// Event upload batches are acknowledged in memory so a lost HTTP response
	// can be retried without appending the same timeline events twice. The
	// payload digest prevents reusing one batch ID for different content.
	eventBatches    map[string]eventBatchReceipt
	eventBatchOrder []string
}

type eventBatchReceipt struct {
	digest   string
	accepted int
	lastSeq  int64
}

// wakeChanLocked returns the channel closed by the next appended event.
func (a *account) wakeChanLocked() chan struct{} {
	if a.wake == nil {
		a.wake = make(chan struct{})
	}
	return a.wake
}

// wakeEventWaitersLocked wakes phones that are holding an empty /app/events
// long-poll even when no conversation event was appended. Pair/revoke state is
// authorization state too: without this notification a revoked phone could
// remain blocked for the full wait timeout before learning that it was
// detached.
func (a *account) wakeEventWaitersLocked() {
	if a == nil || a.wake == nil {
		return
	}
	close(a.wake)
	a.wake = nil
}

func sessionID(deviceID, sessionKey string) string { return deviceID + "\x00" + sessionKey }

func nowMs() int64 { return time.Now().UnixMilli() }

// accountLocked returns (creating if needed) the account. Caller holds h.mu.
func (h *Hub) accountLocked(id string) *account {
	a := h.accounts[id]
	if a == nil {
		a = &account{
			owner:        h,
			resource:     h.resource,
			id:           id,
			devices:      map[string]*device{},
			seq:          h.seqBase,
			sessions:     map[string]*sessionBuf{},
			apps:         map[*appSub]struct{}{},
			eventBatches: map[string]eventBatchReceipt{},
		}
		h.accounts[id] = a
	}
	return a
}

// sendLocked delivers msg to every app stream of the account; a stream whose
// queue is full is dropped (the app reconnects with ?after=).
func (a *account) sendLocked(msg sseMsg) {
	for s := range a.apps {
		if msg.deviceID != "" && s.deviceID != msg.deviceID {
			continue
		}
		if s.maxQueuedBytes > 0 && s.queuedBytes+int64(len(msg.data)) > s.maxQueuedBytes {
			delete(a.apps, s)
			s.close()
			continue
		}
		select {
		case s.ch <- msg:
			s.queuedBytes += int64(len(msg.data))
		default:
			delete(a.apps, s)
			s.close()
		}
	}
}

func (a *account) broadcastDeviceLocked(d *device) {
	data, err := json.Marshal(d.status())
	if err != nil {
		return
	}
	a.sendLocked(sseMsg{event: "device", data: data, deviceID: d.info.DeviceID})
}

func (a *account) deviceStatusesLocked() []DeviceStatus {
	out := make([]DeviceStatus, 0, len(a.devices))
	for _, d := range a.devices {
		out = append(out, d.status())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		if out[i].LastSeen != out[j].LastSeen {
			return out[i].LastSeen > out[j].LastSeen
		}
		return out[i].DeviceID < out[j].DeviceID
	})
	return out
}

// appendEventLocked assigns the next seq, stores the event in the account and
// session buffers and fans it out. fields is the decoded event object.
func (a *account) appendEventLocked(deviceID string, fields map[string]json.RawMessage) (*storedEvent, error) {
	if a.events == nil {
		a.events = newBoundedRing(a.resource.AccountEvents, a.resource.AccountEventBytes)
	}
	seq := a.seq + 1
	fields["seq"], _ = json.Marshal(seq)
	fields["deviceId"], _ = json.Marshal(deviceID)
	if _, ok := fields["ts"]; !ok {
		fields["ts"], _ = json.Marshal(nowMs())
	}
	var sk string
	if raw, ok := fields["sessionKey"]; ok {
		_ = json.Unmarshal(raw, &sk)
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	a.seq = seq
	e := &storedEvent{seq: seq, deviceID: deviceID, sessionKey: sk, data: data}
	a.events.push(e)
	if a.wake != nil {
		close(a.wake)
		a.wake = nil
	}
	if sk != "" {
		id := sessionID(deviceID, sk)
		sb := a.sessions[id]
		if sb == nil {
			if len(a.sessions) >= a.resource.MaxCachedSessions {
				a.evictSessionLocked()
			}
			sb = &sessionBuf{ring: newBoundedRing(a.resource.SessionEvents, a.resource.SessionEventBytes)}
			sb.ring.evictedThrough = a.sessionEvictedThrough
			a.sessions[id] = sb
		}
		sb.ring.push(e)
		sb.updated = seq
		var typ string
		_ = json.Unmarshal(fields["type"], &typ)
		if typ == "session.updated" {
			var session struct {
				Status string `json:"status"`
				Tool   string `json:"tool"`
			}
			if json.Unmarshal(fields["session"], &session) == nil {
				switch session.Status {
				case "running", "idle", "waiting_approval", "failed":
					sb.status, sb.tool = session.Status, session.Tool
				}
			}
		}
	}
	a.trimSessionBytesLocked()
	a.syncCacheBytesLocked(true)
	if a.owner != nil {
		a.owner.trimGlobalCacheLocked()
	}
	a.sendLocked(sseMsg{event: "event", data: data, seq: seq, deviceID: deviceID})
	return e, nil
}

func (a *account) evictSessionLocked() {
	var oldest string
	var min int64 = -1
	for id, sb := range a.sessions {
		if min < 0 || sb.updated < min {
			oldest, min = id, sb.updated
		}
	}
	if sb := a.sessions[oldest]; sb != nil && sb.updated > a.sessionEvictedThrough {
		a.sessionEvictedThrough = sb.updated
	}
	delete(a.sessions, oldest)
}
