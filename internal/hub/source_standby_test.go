package hub

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func standbySourceChallengeFixture(t *testing.T) (*Hub, *http.Request, commandReq, SourceSnapshot, SourceChallenge) {
	t.Helper()
	h, pair, req, snapshot, private := sourceContractFixture(t)
	h.mu.Lock()
	dev := h.accounts["receipt-owner"].devices[req.DeviceID]
	dev.conn = nil
	dev.standby = &bridgeConn{cmds: make(chan []byte, 1), closed: make(chan struct{})}
	dev.standbyAt, dev.standbyComputerID = nowMs(), "physical-pc"
	h.cfg.PublicURL = "https://station.test/salcara-hub"
	h.mu.Unlock()
	req.Command = json.RawMessage(`{"type":"remote.station.switch","targetHubUrl":"https://station.test/salcara-hub","targetDeviceId":"pc","targetComputerId":"physical-pc","operationId":"10000000-0000-4000-8000-000000000001","agent":"codex","accountId":"credential-1"}`)
	challenge := issueSourceChallengeFixture(t, h, pair, req)
	snapshot.CommandHash, snapshot.ChallengeID, snapshot.Model = challenge.CommandHash, challenge.ChallengeID, ""
	signSourceFixture(t, &req, snapshot, private)
	return h, pair, req, snapshot, challenge
}

func TestStandbyAcquiresOnlyItsActiveHandoverChallenge(t *testing.T) {
	h, pair, req, _, challenge := standbySourceChallengeFixture(t)
	body, _ := json.Marshal(map[string]string{"challengeId": challenge.ChallengeID})
	r := httptest.NewRequest(http.MethodPost, "/app/source/grant", bytes.NewReader(body))
	r.Header = pair.Header.Clone()
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.handleSourceGrant(w, r, "receipt-owner"); close(done) }()
	h.mu.Lock()
	conn := h.accounts["receipt-owner"].devices[req.DeviceID].standby
	h.mu.Unlock()
	var env commandEnvelope
	select {
	case raw := <-conn.cmds:
		if json.Unmarshal(raw, &env) != nil || !env.Phone || env.BindingID != challenge.BindingID || env.Source != nil || string(env.Command) != `{"challengeId":"`+challenge.ChallengeID+`","type":"source.grant.acquire"}` {
			t.Fatal("standby did not receive the narrow internal acquisition")
		}
	case <-time.After(time.Second):
		t.Fatal("standby acquisition was not delivered")
	}
	grant, _ := json.Marshal(req.SourceGrant)
	h.mu.Lock()
	pending := h.pending[env.CommandID]
	h.mu.Unlock()
	pending.ch <- replyBody{OK: true, Result: grant}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("standby grant did not finish")
	}
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"sourceGrant"`)) {
		t.Fatalf("standby grant refused: %d %s", w.Code, w.Body)
	}
	// The grant then admits exactly the original standby handover, and its
	// completed receipt remains recoverable after the challenge is retired.
	commandBody, _ := json.Marshal(req)
	post := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/app/commands", bytes.NewReader(commandBody))
		request.Header = pair.Header.Clone()
		result := httptest.NewRecorder()
		h.handleAppCommands(result, request, "receipt-owner")
		return result
	}
	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() { completed <- post() }()
	select {
	case raw := <-conn.cmds:
		if json.Unmarshal(raw, &env) != nil || env.Source == nil || env.Source.Mode != "restricted" || env.Source.CredentialHandle != "credential-1" || !bytes.Equal(env.Command, req.Command) {
			t.Fatal("grant did not authorize exactly the original handover")
		}
	case <-time.After(time.Second):
		t.Fatal("authorized standby handover was not delivered")
	}
	h.mu.Lock()
	pending = h.pending[env.CommandID]
	h.mu.Unlock()
	pending.ch <- replyBody{OK: true, Result: json.RawMessage(`{"switched":true}`)}
	select {
	case result := <-completed:
		if result.Code != 200 || !strings.Contains(result.Body.String(), `"switched":true`) {
			t.Fatalf("standby handover reply lost: %d %s", result.Code, result.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("standby handover did not finish")
	}
	if result := post(); result.Code != 200 || !strings.Contains(result.Body.String(), `"switched":true`) {
		t.Fatal("completed standby handover could not be recovered")
	}
	select {
	case <-conn.cmds:
		t.Fatal("completed standby handover was dispatched twice")
	default:
	}
}

func TestStandbySourceAcquisitionRejectsWrongOriginalCommandAndState(t *testing.T) {
	for _, name := range []string{"inactive", "different-device", "different-computer", "different-station", "ordinary-task", "stale-policy", "rebound", "extra-field", "case-alias", "invalid-source-only"} {
		t.Run(name, func(t *testing.T) {
			h, _, req, _, challenge := standbySourceChallengeFixture(t)
			raw := json.RawMessage(`{"type":"source.grant.acquire","challengeId":"` + challenge.ChallengeID + `"}`)
			h.mu.Lock()
			defer h.mu.Unlock()
			dev := h.accounts["receipt-owner"].devices[req.DeviceID]
			state := h.sourceChallenges[challenge.ChallengeID]
			state.acquiring = true
			switch name {
			case "inactive":
				state.acquiring = false
			case "different-device":
				state.Command = bytes.ReplaceAll(state.Command, []byte(`"targetDeviceId":"pc"`), []byte(`"targetDeviceId":"other"`))
			case "different-computer":
				state.Command = bytes.ReplaceAll(state.Command, []byte(`physical-pc`), []byte(`other-pc`))
			case "different-station":
				state.Command = bytes.ReplaceAll(state.Command, []byte(`station.test`), []byte(`other.test`))
			case "ordinary-task":
				state.Command = json.RawMessage(`{"type":"session.send","agent":"codex","accountId":"credential-1"}`)
			case "stale-policy":
				state.PolicyRevision = "other"
			case "rebound":
				dev.bindingID = "other"
			case "extra-field":
				raw = json.RawMessage(`{"type":"source.grant.acquire","challengeId":"` + challenge.ChallengeID + `","issuerURL":"https://other.test"}`)
			case "case-alias":
				raw = json.RawMessage(`{"type":"source.grant.acquire","ChallengeId":"` + challenge.ChallengeID + `"}`)
			case "invalid-source-only":
				state.Command = bytes.ReplaceAll(state.Command, []byte(`"agent":"codex"`), []byte(`"sourceOnly":"true","agent":"codex"`))
			}
			if h.standbyCommandLocked(dev, raw) != nil {
				t.Fatal("unrelated standby acquisition was admitted")
			}
		})
	}
}

// Shorten the real connection's deadline after checking the production value,
// so this exercises a TCP body read without waiting for the full 60 seconds.
type sourceGrantDeadlineWriter struct {
	http.ResponseWriter
	deadlines chan<- time.Time
}

func (w sourceGrantDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	w.deadlines <- deadline
	if !deadline.IsZero() {
		deadline = time.Now().Add(80 * time.Millisecond)
	}
	return http.NewResponseController(w.ResponseWriter).SetReadDeadline(deadline)
}

func TestSourceGrantSlowTCPBodyHasReadDeadline(t *testing.T) {
	h, id, _, token := standbyFixture(t)
	deadlines := make(chan time.Time, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(sourceGrantDeadlineWriter{w, deadlines}, r)
	}))
	t.Cleanup(server.Close)
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(conn, "POST /salcara-hub/v1/app/source/grant HTTP/1.1\r\nHost: station.test\r\nX-Salcara-Pair-Token: %s\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{", token)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case deadline := <-deadlines:
		if remaining := time.Until(deadline); remaining < 59*time.Second || remaining > 61*time.Second {
			t.Fatalf("unexpected production read deadline: %v", remaining)
		}
	case <-time.After(time.Second):
		t.Fatal("grant route did not set a read deadline")
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 400 || !strings.Contains(string(body), "source_challenge_invalid") {
		t.Fatalf("slow body not rejected: status=%d body=%s err=%v", response.StatusCode, body, err)
	}
	select {
	case deadline := <-deadlines:
		if !deadline.IsZero() {
			t.Fatal("body deadline was not cleared")
		}
	case <-time.After(time.Second):
		t.Fatal("grant handler remained occupied")
	}
	h.mu.Lock()
	pending := len(h.pending)
	dev := h.accounts[deviceAccount(id)].devices[id]
	paired := dev.hasPair
	h.mu.Unlock()
	if pending != 0 || !paired {
		t.Fatal("slow body changed authorization or dispatched work")
	}
}
