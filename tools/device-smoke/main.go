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
	"net/url"
	"os"
	"os/signal"
	"time"

	"salcara/hub/internal/hub"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:47837", "loopback listen address; use :0 for an available port")
	dataDir := flag.String("data-dir", "", "explicit fixture state directory; empty disables persistence")
	publicURL := flag.String("public-url", "", "optional loopback station URL for synthetic API switching tests")
	flag.Parse()
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		log.Fatal("listen must be an explicit loopback address")
	}
	if *publicURL != "" {
		u, parseErr := url.Parse(*publicURL)
		if parseErr != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
			log.Fatal("public-url must be an explicit loopback HTTP station URL")
		}
	}
	service, err := hub.New(hub.Config{DataDir: *dataDir, TrustProxy: false, PublicURL: *publicURL})
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
