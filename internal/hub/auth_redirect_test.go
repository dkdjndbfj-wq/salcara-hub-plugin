package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLegacyAPIAuthenticationRejectsRedirectsWithoutCaching(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, destination := range []string{"relative", "same-origin", "same-host-other-port", "other-host"} {
			t.Run(fmt.Sprintf("%d/%s", status, destination), func(t *testing.T) {
				const key = "synthetic-legacy-redirect-key"
				var initialRequests, targetRequests, redirectHooks atomic.Int32
				var invalidInitialRequest, allowAuthentication atomic.Bool
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					targetRequests.Add(1)
					_, _ = io.WriteString(w, `{"group_id":7}`)
				}))
				defer target.Close()
				var location string
				verifier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v1/salcara-hub/key" {
						targetRequests.Add(1)
						_, _ = io.WriteString(w, `{"group_id":7}`)
						return
					}
					initialRequests.Add(1)
					if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+key {
						invalidInitialRequest.Store(true)
					}
					if allowAuthentication.Load() {
						_, _ = io.WriteString(w, `{"group_id":7}`)
						return
					}
					w.Header().Set("Location", location)
					w.WriteHeader(status)
				}))
				defer verifier.Close()
				switch destination {
				case "relative":
					location = "/credential-trap"
				case "same-origin":
					location = verifier.URL + "/credential-trap"
				case "same-host-other-port":
					location = target.URL + "/credential-trap"
				case "other-host":
					// A distinct loopback hostname keeps even a failing
					// regression entirely inside this synthetic fixture.
					location = strings.Replace(target.URL, "127.0.0.1", "localhost", 1) + "/credential-trap"
				}
				client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
					redirectHooks.Add(1)
					return nil
				}}
				cfg := Config{Sub2APIURL: verifier.URL, AllowedGroupIDs: []int64{7}, HTTPClient: client}
				cfg.setDefaults()
				auth := newAuthenticator(&cfg)
				for attempt := 0; attempt < 2; attempt++ {
					verdict, err := auth.check(context.Background(), key)
					if verdict != authInvalid || !errors.Is(err, errUpstream) {
						t.Fatalf("redirect verdict=%v err=%v", verdict, err)
					}
				}
				if initialRequests.Load() != 2 || targetRequests.Load() != 0 || redirectHooks.Load() != 0 || invalidInitialRequest.Load() {
					t.Fatalf("initial=%d targets=%d hooks=%d invalidInitial=%t", initialRequests.Load(), targetRequests.Load(), redirectHooks.Load(), invalidInitialRequest.Load())
				}
				if len(auth.cache) != 0 || auth.cachedValid(key) {
					t.Fatal("indeterminate redirect response was cached")
				}
				allowAuthentication.Store(true)
				for attempt := 0; attempt < 2; attempt++ {
					verdict, err := auth.check(context.Background(), key)
					if verdict != authAllowed || err != nil {
						t.Fatalf("recovered verifier verdict=%v err=%v", verdict, err)
					}
				}
				if initialRequests.Load() != 3 || targetRequests.Load() != 0 || redirectHooks.Load() != 0 {
					t.Fatalf("recovery initial=%d targets=%d hooks=%d", initialRequests.Load(), targetRequests.Load(), redirectHooks.Load())
				}
				// The query must neither replace the caller's hook nor invoke it.
				if err := client.CheckRedirect(&http.Request{}, nil); err != nil || redirectHooks.Load() != 1 {
					t.Fatal("caller HTTP client redirect hook was changed")
				}
			})
		}
	}
}

type legacyVerifierRoundTripper func(*http.Request) (*http.Response, error)

func (f legacyVerifierRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestLegacyAPIAuthenticationPreservesCallerClientSemantics(t *testing.T) {
	const endpoint = "https://configured.example.test/v1/salcara-hub/key"
	const key = "synthetic-legacy-client-key"
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(endpoint)
	jar.SetCookies(u, []*http.Cookie{{Name: "verifier-session", Value: "synthetic-session", Secure: true}})
	var requests atomic.Int32
	transport := legacyVerifierRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if r.URL.String() != endpoint || r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+key || r.Header.Get("Accept") != "application/json" {
			t.Errorf("request did not preserve the configured verifier identity")
		}
		cookie, err := r.Cookie("verifier-session")
		if err != nil || cookie.Value != "synthetic-session" {
			t.Error("caller cookie jar was not preserved")
		}
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 2100*time.Millisecond {
			t.Error("caller client timeout was not preserved")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"group_id":7}`)), Request: r}, nil
	})
	client := &http.Client{Transport: transport, Jar: jar, Timeout: 2 * time.Second}
	cfg := Config{Sub2APIURL: "https://configured.example.test", AllowedGroupIDs: []int64{7}, AuthTimeout: 5 * time.Second, HTTPClient: client}
	cfg.setDefaults()
	verdict, err := newAuthenticator(&cfg).query(key)
	if verdict != authAllowed || err != nil || requests.Load() != 1 {
		t.Fatalf("verdict=%v err=%v requests=%d", verdict, err, requests.Load())
	}
	if client.CheckRedirect != nil || client.Jar != jar || client.Timeout != 2*time.Second {
		t.Fatal("caller HTTP client was mutated")
	}
}
