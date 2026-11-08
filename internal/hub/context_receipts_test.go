package hub

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var transientSourceReadTypes = []string{"session.context", "models.list", "features.list", "session.actions", "sessions.archived", "session.workflows", "session.side-question.catalog", "session.side-question.read"}

type sourceReadReceiptFixture struct {
	h        *Hub
	pair     *http.Request
	req      commandReq
	snapshot SourceSnapshot
	private  ed25519.PrivateKey
	conn     *bridgeConn
}

func newSourceReadReceiptFixture(t *testing.T, typ string) *sourceReadReceiptFixture {
	t.Helper()
	h, pair, req, snapshot, private := sourceContractFixture(t)
	req.Command = json.RawMessage(fmt.Sprintf(`{"type":%q,"accountId":"credential-1","tool":"codex","controlSurface":"cli","sessionKey":"codex:s"}`, typ))
	h.mu.Lock()
	conn := h.accounts["receipt-owner"].devices["pc"].conn
	h.mu.Unlock()
	return &sourceReadReceiptFixture{h: h, pair: pair, req: req, snapshot: snapshot, private: private, conn: conn}
}

func (f *sourceReadReceiptFixture) authorize(t *testing.T) SourceChallenge {
	t.Helper()
	canonical, err := canonicalCommand(f.req.Command)
	if err != nil {
		t.Fatal(err)
	}
	f.snapshot.CommandHash, f.snapshot.Model, f.snapshot.RequestID = shaHex(canonical), "", f.req.RequestID
	challenge := issueSourceChallengeFixture(t, f.h, f.pair, f.req)
	f.snapshot.ChallengeID = challenge.ChallengeID
	// Each poll obtains a fresh grant, just as the source signer does. A long
	// race run must not reuse the fixture's original sixty-second grant.
	f.snapshot.IssuedAt = time.Now().Unix()
	f.snapshot.ExpiresAt = min(f.snapshot.IssuedAt+60, challenge.ExpiresAt)
	signSourceFixture(t, &f.req, f.snapshot, f.private)
	return challenge
}

func (f *sourceReadReceiptFixture) request(t *testing.T, ctx context.Context, req commandReq) *http.Request {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/app/commands", bytes.NewReader(body)).WithContext(ctx)
	r.Header = f.pair.Header.Clone()
	return r
}

func (f *sourceReadReceiptFixture) start(t *testing.T, ctx context.Context, req commandReq) <-chan *httptest.ResponseRecorder {
	t.Helper()
	r := f.request(t, ctx, req)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		f.h.handleAppCommands(w, r, "receipt-owner")
		done <- w
	}()
	return done
}

func awaitSourceReadResponse(t *testing.T, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-done:
		return w
	case <-time.After(2 * time.Second):
		t.Fatal("source read did not finish")
		return nil
	}
}

func (f *sourceReadReceiptFixture) pending(t *testing.T) (commandEnvelope, *pendingCmd) {
	t.Helper()
	var env commandEnvelope
	select {
	case raw := <-f.conn.cmds:
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("source read was not dispatched")
	}
	f.h.mu.Lock()
	pending := f.h.pending[env.CommandID]
	f.h.mu.Unlock()
	if pending == nil {
		t.Fatal("source read has no pending receiver")
	}
	return env, pending
}

func assertSourceReadReceiptCount(t *testing.T, h *Hub, want int) {
	t.Helper()
	h.receipts.mu.Lock()
	count, cachedBytes := len(h.receipts.entries), h.receipts.bytes
	h.receipts.mu.Unlock()
	if count != want || want == 0 && cachedBytes != 0 {
		t.Fatalf("receipt entries=%d bytes=%d, want %d entries", count, cachedBytes, want)
	}
	files, err := os.ReadDir(filepath.Join(h.cfg.DataDir, "command-receipts"))
	if err != nil || len(files) != want {
		t.Fatalf("receipt files=%d, want %d: %v", len(files), want, err)
	}
}

func assertSourceReadReleased(t *testing.T, h *Hub) {
	t.Helper()
	h.mu.Lock()
	pending, challenges := len(h.pending), len(h.sourceChallenges)
	h.mu.Unlock()
	if pending != 0 || challenges != 0 {
		t.Fatalf("read retained pending=%d challenges=%d", pending, challenges)
	}
}

func TestSourceReadPollingDoesNotConsumeReceiptsOrChallenges(t *testing.T) {
	for _, typ := range transientSourceReadTypes {
		t.Run(typ, func(t *testing.T) {
			f := newSourceReadReceiptFixture(t, typ)
			for i := 0; i < maxOwnerCommandReceipts+2; i++ {
				f.req.RequestID = fmt.Sprintf("20000000-0000-4000-8000-%012x", i+1)
				f.authorize(t)
				done := f.start(t, context.Background(), f.req)
				env, pending := f.pending(t)
				if env.Source == nil || env.Source.Mode != "restricted" || env.Source.CredentialHandle != "credential-1" {
					t.Fatal("source read lost its verified source envelope")
				}
				result := fmt.Sprintf(`{"observation":%d}`, i)
				pending.ch <- replyBody{OK: true, Result: json.RawMessage(result)}
				w := awaitSourceReadResponse(t, done)
				if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(result)) {
					t.Fatalf("read %d: status=%d body=%s", i, w.Code, w.Body.String())
				}
				assertSourceReadReleased(t, f.h)
			}
			assertSourceReadReceiptCount(t, f.h, 0)
		})
	}
}

func TestSourceReadRequestIDNeverCachesARead(t *testing.T) {
	for _, typ := range transientSourceReadTypes {
		t.Run(typ, func(t *testing.T) {
			f := newSourceReadReceiptFixture(t, typ)
			for i := 0; i < 2; i++ {
				f.authorize(t) // Same request ID, fresh verified challenge.
				done := f.start(t, context.Background(), f.req)
				result := fmt.Sprintf(`{"observation":%d}`, i)
				replyReceipt(t, f.h, f.conn, result)
				w := awaitSourceReadResponse(t, done)
				if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(result)) {
					t.Fatalf("read reused an old result: %d %s", w.Code, w.Body.String())
				}
				assertSourceReadReleased(t, f.h)
			}
			w := awaitSourceReadResponse(t, f.start(t, context.Background(), f.req))
			if w.Code != http.StatusForbidden || !bytes.Contains(w.Body.Bytes(), []byte("source_challenge_expired")) || len(f.conn.cmds) != 0 {
				t.Fatalf("consumed nonce was reused: %d %s", w.Code, w.Body.String())
			}
			assertSourceReadReceiptCount(t, f.h, 0)
		})
	}
}

func TestSourceReadRequiresValidRequestAndSourceGrant(t *testing.T) {
	scenarios := []struct {
		name         string
		status       int
		code         string
		alter        func(*commandReq, *SourceSnapshot)
		missingGrant bool
		badSignature bool
	}{
		{name: "malformed-request-id", status: 400, code: "invalid_request_id", alter: func(req *commandReq, snapshot *SourceSnapshot) {
			req.RequestID, snapshot.RequestID, snapshot.ChallengeID = "not-a-uuid", "not-a-uuid", ""
		}},
		{name: "missing-request-id", status: 403, code: "source_policy_stale", alter: func(req *commandReq, _ *SourceSnapshot) { req.RequestID = "" }},
		{name: "missing-grant", status: 403, code: "source_grant_required", missingGrant: true},
		{name: "bad-signature", status: 403, code: "source_grant_invalid", badSignature: true},
		{name: "wrong-request-id", status: 403, code: "source_policy_stale", alter: func(req *commandReq, _ *SourceSnapshot) {
			req.RequestID = "30000000-0000-4000-8000-000000000002"
		}},
		{name: "wrong-command", status: 403, code: "source_command_mismatch", alter: func(req *commandReq, _ *SourceSnapshot) {
			req.Command = bytes.Replace(req.Command, []byte("codex:s"), []byte("codex:other"), 1)
		}},
		{name: "wrong-credential", status: 403, code: "source_credential_mismatch", alter: func(_ *commandReq, snapshot *SourceSnapshot) {
			snapshot.CredentialHandle = "credential-other"
		}},
		{name: "wrong-binding", status: 403, code: "source_binding_mismatch", alter: func(_ *commandReq, snapshot *SourceSnapshot) {
			snapshot.BindingID = "binding-other"
		}},
		{name: "expired-grant", status: 403, code: "source_grant_expired", alter: func(_ *commandReq, snapshot *SourceSnapshot) {
			snapshot.IssuedAt, snapshot.ExpiresAt = time.Now().Unix()-120, time.Now().Unix()-60
		}},
		{name: "wrong-nonce", status: 403, code: "source_challenge_expired", alter: func(_ *commandReq, snapshot *SourceSnapshot) {
			snapshot.ChallengeID = "challenge-not-issued"
		}},
	}
	for _, typ := range transientSourceReadTypes {
		for _, scenario := range scenarios {
			t.Run(typ+"/"+scenario.name, func(t *testing.T) {
				f := newSourceReadReceiptFixture(t, typ)
				challenge := f.authorize(t)
				f.h.mu.Lock()
				original := f.h.sourceChallenges[challenge.ChallengeID]
				f.h.mu.Unlock()
				if scenario.alter != nil {
					scenario.alter(&f.req, &f.snapshot)
				}
				signSourceFixture(t, &f.req, f.snapshot, f.private)
				if scenario.missingGrant {
					f.req.SourceGrant = nil
				} else if scenario.badSignature {
					f.req.SourceGrant.Signature = "invalid"
				}
				w := awaitSourceReadResponse(t, f.start(t, context.Background(), f.req))
				if w.Code != scenario.status || !bytes.Contains(w.Body.Bytes(), []byte(scenario.code)) || len(f.conn.cmds) != 0 {
					t.Fatalf("invalid source read: %d %s", w.Code, w.Body.String())
				}
				f.h.mu.Lock()
				retained := f.h.sourceChallenges[challenge.ChallengeID] == original
				f.h.mu.Unlock()
				if !retained {
					t.Fatal("unverified request retired a valid source nonce")
				}
				assertSourceReadReceiptCount(t, f.h, 0)
			})
		}
	}
}

func TestSourceReadOpenModeCannotRetireAnUnverifiedNonce(t *testing.T) {
	for _, typ := range transientSourceReadTypes {
		t.Run(typ, func(t *testing.T) {
			f := newSourceReadReceiptFixture(t, typ)
			challenge := f.authorize(t)
			f.h.mu.Lock()
			original := f.h.sourceChallenges[challenge.ChallengeID]
			f.h.cfg.RemoteAccess.Mode = "open"
			f.h.mu.Unlock()
			f.req.SourceGrant.Signature = "invalid"
			for _, withID := range []bool{true, false} {
				if !withID {
					f.req.RequestID, f.req.SourceGrant = "", nil
					// The read remains available when durable retry is disabled.
					f.h.receipts.dir = ""
				}
				done := f.start(t, context.Background(), f.req)
				env, pending := f.pending(t)
				if env.Source == nil || env.Source.Mode != "open" {
					t.Fatal("open read lost its station source contract")
				}
				pending.ch <- replyBody{OK: true, Result: json.RawMessage(`{"available":true}`)}
				w := awaitSourceReadResponse(t, done)
				if w.Code != http.StatusOK {
					t.Fatalf("open read denied: %d %s", w.Code, w.Body.String())
				}
			}
			f.h.mu.Lock()
			retained := f.h.sourceChallenges[challenge.ChallengeID] == original
			f.h.mu.Unlock()
			if !retained {
				t.Fatal("open mode trusted an unverified grant to retire a nonce")
			}
			assertSourceReadReceiptCount(t, f.h, 0)
		})
	}
}

func TestSourceReadCancellationReleasesPendingAndNonce(t *testing.T) {
	for _, typ := range transientSourceReadTypes {
		for _, beforeDispatch := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/before-dispatch=%t", typ, beforeDispatch), func(t *testing.T) {
				f := newSourceReadReceiptFixture(t, typ)
				for i := 0; i < 34; i++ { // Exceed the per-device nonce limit without waiting for expiry.
					f.req.RequestID = fmt.Sprintf("40000000-0000-4000-8000-%012x", i+1)
					f.authorize(t)
					ctx, cancel := context.WithCancel(context.Background())
					if beforeDispatch {
						cancel()
					}
					done := f.start(t, ctx, f.req)
					var pending *pendingCmd
					if !beforeDispatch {
						_, pending = f.pending(t)
						cancel()
					}
					w := awaitSourceReadResponse(t, done)
					cancel()
					assertSourceReadReleased(t, f.h)
					if pending != nil {
						// A native reply after cancellation has no registered receiver.
						pending.ch <- replyBody{OK: true, Result: json.RawMessage(`{"private":"late-read-result"}`)}
					}
					if w.Body.Len() != 0 || len(f.conn.cmds) != 0 {
						t.Fatalf("cancelled read returned or dispatched content: %s", w.Body.String())
					}
				}
				assertSourceReadReceiptCount(t, f.h, 0)
			})
		}
	}
}

func TestSourceReadCancellationDuringFinalSourceCheckDropsReply(t *testing.T) {
	f := newSourceReadReceiptFixture(t, "session.context")
	f.authorize(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := f.request(t, ctx, f.req)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	checks := 0
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		f.h.handleReceiptCommand(w, r, "receipt-owner", f.req, func() (string, *ExecutionSource) {
			checks++
			if checks == 3 {
				close(entered)
				<-release
			}
			return f.h.executionSourceFor("receipt-owner", r, f.req)
		})
		done <- w
	}()
	replyReceipt(t, f.h, f.conn, `{"private":"completed-read-result"}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("read did not recheck its source before releasing the reply")
	}
	cancel()
	unblock()
	w := awaitSourceReadResponse(t, done)
	if w.Body.Len() != 0 {
		t.Fatalf("final validation released a cancelled read: %s", w.Body.String())
	}
	assertSourceReadReleased(t, f.h)
	assertSourceReadReceiptCount(t, f.h, 0)
}

func TestSourceReadRevocationDropsPendingResult(t *testing.T) {
	for _, typ := range transientSourceReadTypes {
		t.Run(typ, func(t *testing.T) {
			f := newSourceReadReceiptFixture(t, typ)
			f.authorize(t)
			done := f.start(t, context.Background(), f.req)
			_, pending := f.pending(t)
			f.h.remoteAccessGate.Lock()
			f.h.mu.Lock()
			f.h.cfg.RemoteAccess.Mode, f.h.cfg.RemoteAccess.Revision = "disabled", "revoked"
			f.h.mu.Unlock()
			f.h.remoteAccessGate.Unlock()
			pending.ch <- replyBody{OK: true, Result: json.RawMessage(`{"private":"revoked-read-result"}`)}
			w := awaitSourceReadResponse(t, done)
			if w.Code != http.StatusForbidden || !bytes.Contains(w.Body.Bytes(), []byte("source_policy_changed")) || bytes.Contains(w.Body.Bytes(), []byte("revoked-read-result")) {
				t.Fatalf("revoked source released a read result: %d %s", w.Code, w.Body.String())
			}
			assertSourceReadReleased(t, f.h)
			assertSourceReadReceiptCount(t, f.h, 0)
		})
	}
}

func TestSourceReadCleanupPreservesReplacementChallenge(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%t", cancelled), func(t *testing.T) {
			f := newSourceReadReceiptFixture(t, "session.context")
			challenge := f.authorize(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := f.start(t, ctx, f.req)
			_, pending := f.pending(t)
			f.h.mu.Lock()
			replacement := *f.h.sourceChallenges[challenge.ChallengeID]
			f.h.sourceChallenges[challenge.ChallengeID] = &replacement
			f.h.mu.Unlock()
			if cancelled {
				cancel()
			} else {
				pending.ch <- replyBody{OK: true, Result: json.RawMessage(`{"available":true}`)}
			}
			w := awaitSourceReadResponse(t, done)
			if cancelled && w.Body.Len() != 0 || !cancelled && w.Code != http.StatusOK {
				t.Fatalf("unexpected source read response: %d %s", w.Code, w.Body.String())
			}
			f.h.mu.Lock()
			retained := f.h.sourceChallenges[challenge.ChallengeID] == &replacement
			f.h.mu.Unlock()
			if !retained {
				t.Fatal("old read retired a replacement source challenge")
			}
			assertSourceReadReceiptCount(t, f.h, 0)
		})
	}
}

func TestSourceReadChallengeReplacementDuringValidationIsRetained(t *testing.T) {
	f := newSourceReadReceiptFixture(t, "session.context")
	challenge := f.authorize(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := f.request(t, ctx, f.req)
	var replacement *sourceChallengeState
	_, status := f.h.executeTransientSourceCommand(r, "receipt-owner", f.req, func() (string, *ExecutionSource) {
		code, source := f.h.executionSourceFor("receipt-owner", r, f.req)
		f.h.mu.Lock()
		copy := *f.h.sourceChallenges[challenge.ChallengeID]
		replacement = &copy
		f.h.sourceChallenges[challenge.ChallengeID] = replacement
		f.h.mu.Unlock()
		return code, source
	})
	f.h.mu.Lock()
	retained := f.h.sourceChallenges[challenge.ChallengeID] == replacement
	f.h.mu.Unlock()
	if status != 499 || !retained || len(f.conn.cmds) != 0 {
		t.Fatalf("validation retired an unverified replacement: status=%d retained=%t", status, retained)
	}
	assertSourceReadReceiptCount(t, f.h, 0)
}

func TestSourceReadLegacyReceiptCannotBypassSourceCheck(t *testing.T) {
	for _, typ := range transientSourceReadTypes {
		t.Run(typ, func(t *testing.T) {
			f := newSourceReadReceiptFixture(t, typ)
			f.authorize(t)
			credential := f.h.receiptCredential("receipt-owner", f.pair)
			seeded := make(chan int, 1)
			go func() {
				_, status, _ := f.h.executeReceipt(context.Background(), "receipt-owner", "pc", credential, f.req.RequestID, f.req.Command, nil)
				seeded <- status
			}()
			replyReceipt(t, f.h, f.conn, `{"private":"legacy-cached-result"}`)
			select {
			case status := <-seeded:
				if status != http.StatusOK {
					t.Fatal("legacy receipt fixture failed")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("legacy receipt fixture stalled")
			}
			if f.h.cachedReceiptReplay("receipt-owner", f.pair, f.req) {
				t.Fatal("legacy source read receipt authorized a cached replay")
			}
			for _, expired := range []bool{false, true} {
				req := f.req
				if expired {
					snapshot := f.snapshot
					snapshot.IssuedAt, snapshot.ExpiresAt = time.Now().Unix()-120, time.Now().Unix()-60
					signSourceFixture(t, &req, snapshot, f.private)
				} else {
					req.SourceGrant = nil
				}
				w := awaitSourceReadResponse(t, f.start(t, context.Background(), req))
				if w.Code != http.StatusForbidden || bytes.Contains(w.Body.Bytes(), []byte("legacy-cached-result")) || len(f.conn.cmds) != 0 {
					t.Fatalf("legacy receipt bypassed source validation: %d %s", w.Code, w.Body.String())
				}
			}
			done := f.start(t, context.Background(), f.req)
			replyReceipt(t, f.h, f.conn, `{"fresh":true}`)
			w := awaitSourceReadResponse(t, done)
			if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"fresh":true`)) || bytes.Contains(w.Body.Bytes(), []byte("legacy-cached-result")) {
				t.Fatalf("fresh read reused its legacy receipt: %d %s", w.Code, w.Body.String())
			}
			assertSourceReadReleased(t, f.h)
			assertSourceReadReceiptCount(t, f.h, 1)
		})
	}
}

func TestSessionCompactKeepsDurableReceiptAfterPhoneCancellation(t *testing.T) {
	for _, typ := range []string{"session.compact", "session.action", "session.workflow", "session.side-question.ask", "session.side-question.open", "session.side-question.send", "session.side-question.delete"} {
		t.Run(typ, func(t *testing.T) { testNativeMutationKeepsDurableReceipt(t, typ) })
	}
}

func testNativeMutationKeepsDurableReceipt(t *testing.T, typ string) {
	f := newSourceReadReceiptFixture(t, "session.compact")
	f.req.Command, _ = json.Marshal(map[string]any{"type": typ, "accountId": "credential-1", "tool": "codex", "controlSurface": "cli", "sessionKey": "codex:s", "expectedRevision": "runtime-1", "operationId": "11111111-2222-4333-8444-555555555555"})
	f.authorize(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := f.start(t, ctx, f.req)
	env, pending := f.pending(t)
	cancel()
	w := awaitSourceReadResponse(t, first)
	if w.Body.Len() != 0 {
		t.Fatalf("cancelled compact HTTP request returned a body: %s", w.Body.String())
	}
	f.h.mu.Lock()
	retained := f.h.pending[env.CommandID] == pending
	f.h.mu.Unlock()
	if !retained {
		t.Fatal("phone cancellation cancelled a durable compact operation")
	}
	assertSourceReadReceiptCount(t, f.h, 1)
	retry := f.start(t, context.Background(), f.req)
	pending.ch <- replyBody{OK: true, Result: json.RawMessage(`{"compacted":true}`)}
	w = awaitSourceReadResponse(t, retry)
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"compacted":true`)) || len(f.conn.cmds) != 0 {
		t.Fatalf("compact retry did not join its receipt: %d %s", w.Code, w.Body.String())
	}
	assertSourceReadReleased(t, f.h)
	f.req.SourceGrant = nil
	w = awaitSourceReadResponse(t, f.start(t, context.Background(), f.req))
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"compacted":true`)) || len(f.conn.cmds) != 0 {
		t.Fatalf("compact lost cached replay after nonce retirement: %d %s", w.Code, w.Body.String())
	}
	assertSourceReadReceiptCount(t, f.h, 1)
}
