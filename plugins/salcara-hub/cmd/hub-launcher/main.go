package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"salcara/hubplugin/internal/launcher"
	"salcara/hubplugin/internal/standalone"
	"syscall"
)

func main() {
	initToken := flag.Bool("init", false, "Create absent independent admin token without printing or replacing it")
	check := flag.Bool("healthcheck", false, "Check local Hub without loading credentials")
	version := flag.Bool("version", false, "Print launcher protocol and initial Hub version")
	flag.Parse()
	if *version {
		fmt.Printf("launcher_protocol=1 bootstrap_hub=%s\n", standalone.Version)
		return
	}
	if *initToken || *check {
		cfg, err := standalone.ConfigFromEnv(os.Getenv)
		if err != nil {
			slog.Error("invalid Hub configuration", "error", err)
			os.Exit(1)
		}
		if *initToken && *check {
			slog.Error("choose only one operation")
			os.Exit(1)
		}
		if *initToken {
			if err = os.MkdirAll(cfg.DataDir, 0700); err == nil {
				err = standalone.InitAdminToken(cfg.AdminTokenFile)
			}
		} else {
			err = standalone.Healthcheck(context.Background(), cfg.Listen)
		}
		if err != nil {
			slog.Error("Hub initialization or health check failed", "error", err)
			os.Exit(1)
		}
		return
	}
	cfg, err := launcher.ConfigFromEnv(os.Getenv, standalone.Version)
	if err != nil {
		slog.Error("invalid launcher configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err = launcher.Run(ctx, cfg, logger); err != nil {
		logger.Error("Hub supervisor stopped", "error", err)
		os.Exit(1)
	}
}
