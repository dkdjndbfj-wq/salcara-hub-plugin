package edgepool

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFixedOriginLoopFailsBeforeExhaustingCapacity(t *testing.T) {
	var incoming atomic.Int32
	var handler atomic.Value
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		incoming.Add(1)
		handler.Load().(http.Handler).ServeHTTP(w, r)
	}))
	p := testPool(t, "http://"+server.Listener.Addr().String())
	handler.Store(p.Handler())
	server.Start()
	defer server.Close()
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Post(server.URL+"/salcara-hub/v1/app/command", "application/json", strings.NewReader(`{"type":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 508 || !strings.Contains(string(body), "origin_loop_detected") || incoming.Load() != 2 {
		t.Fatalf("origin loop was not stopped: status=%d hops=%d", response.StatusCode, incoming.Load())
	}
}
