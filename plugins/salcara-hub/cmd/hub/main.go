package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"salcara/hubplugin/internal/standalone"
)

func main() {
	initToken := flag.Bool("init", false, "Create an absent independent admin token; never print or replace it")
	check := flag.Bool("healthcheck", false, "Check the local Hub without loading secrets")
	version := flag.Bool("version", false, "Print standalone Hub version")
	flag.Parse()
	if *version {
		fmt.Println(standalone.Version)
		return
	}
	cfg, err := standalone.ConfigFromEnv(os.Getenv)
	if err != nil {
		slog.Error("Hub configuration is invalid", "error", err)
		os.Exit(1)
	}
	if *initToken && *check {
		slog.Error("choose only one of -init and -healthcheck")
		os.Exit(1)
	}
	if *initToken {
		// Only the explicitly configured directory is prepared. O_EXCL keeps
		// an existing token unchanged; its content is never sent to stdout.
		if err = os.MkdirAll(cfg.DataDir, 0700); err == nil {
			err = standalone.InitAdminToken(cfg.AdminTokenFile)
		}
		if err != nil {
			slog.Error("Hub token initialization failed", "error", err)
			os.Exit(1)
		}
		slog.Info("independent Hub admin token created; token value was not printed")
		return
	}
	if *check {
		if err = standalone.Healthcheck(context.Background(), cfg.Listen); err != nil {
			slog.Error("Hub health check failed", "error", err)
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	logger.Info("starting standalone Hub", "listen", cfg.Listen, "version", standalone.Version)
	if err = standalone.Run(ctx, cfg, logger); err != nil {
		logger.Error("Hub stopped", "error", err)
		os.Exit(1)
	}
}
