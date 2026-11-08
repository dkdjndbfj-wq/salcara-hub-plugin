// salcara-edge is an optional controlled ingress in front of one existing Hub.
// It is not a VPN for accepted Hub sockets and does not accelerate a route merely
// by being installed on the same VPS as the Hub.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"salcara/hub/internal/edgepool"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "edge stopped:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "recover" {
		return runRecovery(os.Args[2:], os.Stdout)
	}
	origin := flag.String("origin", os.Getenv("SALCARA_EDGE_ORIGIN"), "fixed Hub origin, for example https://origin.example.com")
	data := flag.String("data-dir", envOr("SALCARA_EDGE_DATA_DIR", "./edge-data"), "private persistent configuration directory")
	tokenFile := flag.String("control-token-file", os.Getenv("SALCARA_EDGE_CONTROL_TOKEN_FILE"), "private regular file containing an independent 64-character hexadecimal control token")
	listen := flag.String("listen", envOr("SALCARA_EDGE_LISTEN", "127.0.0.1:8790"), "HTTP ingress behind an HTTPS reverse proxy")
	control := flag.String("control-listen", envOr("SALCARA_EDGE_CONTROL_LISTEN", "127.0.0.1:8791"), "loopback management listener")
	trusted := flag.String("trusted-proxy-cidrs", os.Getenv("SALCARA_EDGE_TRUSTED_PROXY_CIDRS"), "comma-separated trusted HTTPS frontend CIDRs; empty ignores forwarded headers")
	flag.Parse()
	if *tokenFile == "" {
		return errors.New("control-token-file is required")
	}
	secret, err := readControlSecret(*tokenFile)
	if err != nil {
		return err
	}
	if err = loopback(*control); err != nil {
		return err
	}
	var trustedCIDRs []string
	if *trusted != "" {
		for _, value := range strings.Split(*trusted, ",") {
			trustedCIDRs = append(trustedCIDRs, strings.TrimSpace(value))
		}
	}
	pool, err := edgepool.New(edgepool.Config{Origin: *origin, DataDir: *data, ControlToken: strings.TrimSpace(string(secret)), TrustedProxyCIDRs: trustedCIDRs})
	if err != nil {
		return err
	}
	defer pool.Close()
	ingressListener, err := net.Listen("tcp", *listen)
	if err != nil {
		return errors.New("cannot listen for ingress")
	}
	defer ingressListener.Close()
	controlListener, err := net.Listen("tcp", *control)
	if err != nil {
		return errors.New("cannot listen for management")
	}
	defer controlListener.Close()
	ingress := &http.Server{Handler: pool.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 << 10}
	management := &http.Server{Handler: pool.ControlHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- ingress.Serve(ingressListener) }()
	go func() { errorsCh <- management.Serve(controlListener) }()
	fmt.Fprintln(os.Stderr, "edge ready; publish ingress through HTTPS and keep management private")
	select {
	case <-ctx.Done():
	case err = <-errorsCh:
	}
	pool.Close()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = ingress.Shutdown(shutdown)
	_ = management.Shutdown(shutdown)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("HTTP listener stopped unexpectedly")
	}
	return nil
}

func readControlSecret(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 64 || info.Size() > 66 || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("control token requires a private regular file containing 64 hexadecimal characters")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open control token file")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("control token file changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 67))
	secret := strings.TrimSpace(string(raw))
	decoded, decodeErr := hex.DecodeString(secret)
	if err != nil || len(raw) > 66 || len(secret) != 64 || decodeErr != nil || len(decoded) != 32 {
		return nil, errors.New("invalid control token file")
	}
	return []byte(secret), nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func loopback(address string) error {
	host, _, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		return errors.New("control listener must use a numeric loopback address")
	}
	return nil
}
