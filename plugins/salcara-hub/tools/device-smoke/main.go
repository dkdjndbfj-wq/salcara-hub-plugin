// device-smoke runs only a loopback Hub fixture. No relay Key or model endpoint
// is consulted. It is for local interoperability tests, not public deployment.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"salcara/hubplugin/internal/hub"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:47837", "loopback listen address; use :0 for an available port")
	dataDir := flag.String("data-dir", "", "explicit fixture state directory; empty disables persistence")
	flag.Parse()
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		log.Fatal("listen must be an explicit loopback address")
	}
	service, err := hub.New(hub.Config{DataDir: *dataDir, TrustProxy: false})
	if err != nil {
		log.Fatal(err)
	}
	defer service.Close()
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: service, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	defer signal.Stop(stop)
	go func() {
		<-stop
		service.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	fmt.Printf("Hub fixture: http://%s/salcara-hub (device pairing only; no model API requests)\n", listener.Addr())
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
