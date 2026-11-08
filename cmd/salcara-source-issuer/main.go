// salcara-source-issuer runs behind the station's same-origin HTTPS proxy.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"salcara/hub/internal/sourceissuer"
)

func load(configPath, keyPath string) (*sourceissuer.Service, error) {
	configFile, err := os.Open(configPath)
	if err != nil {
		return nil, errors.New("cannot open issuer configuration")
	}
	raw, readErr := io.ReadAll(io.LimitReader(configFile, (64<<10)+1))
	_ = configFile.Close()
	if readErr != nil {
		return nil, errors.New("cannot read issuer configuration")
	}
	cfg, err := sourceissuer.ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	keyFile, err := os.Open(keyPath)
	if err != nil {
		return nil, errors.New("cannot open issuer signing key")
	}
	key, err := sourceissuer.ReadPrivateKey(keyFile)
	_ = keyFile.Close()
	if err != nil {
		return nil, err
	}
	return sourceissuer.New(cfg, key, nil)
}

func loopback(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("salcara-source-issuer", flag.ContinueOnError)
	flags.SetOutput(out)
	configPath := flags.String("config", "", "issuer configuration file")
	keyPath := flags.String("key", "", "private Ed25519 seed file")
	listen := flags.String("listen", "127.0.0.1:9418", "loopback listener behind HTTPS proxy")
	public := flags.Bool("public-config", false, "print the public Hub issuer entry and exit")
	check := flags.Bool("check", false, "validate configuration and signing key without network access")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*configPath) == "" || strings.TrimSpace(*keyPath) == "" || !loopback(*listen) {
		return errors.New("config, key and a loopback listen address are required")
	}
	service, err := load(*configPath, *keyPath)
	if err != nil {
		return err
	}
	if *public {
		return json.NewEncoder(out).Encode(service.PublicIssuer())
	}
	if *check {
		_, err = fmt.Fprintln(out, "source issuer configuration valid")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: *listen, Handler: service, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 12 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return errors.New("cannot start issuer listener")
	}
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	_, _ = fmt.Fprintln(out, "source issuer ready")
	select {
	case err = <-result:
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if server.Shutdown(shutdown) != nil {
			_ = server.Close()
		}
		err = <-result
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("issuer listener stopped unexpectedly")
	}
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
