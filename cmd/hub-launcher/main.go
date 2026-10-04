package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"salcara/hub/internal/launcher"
	"salcara/hub/internal/standalone"
	"syscall"
)

func main() {
	initAccount := flag.Bool("init", false, "Create absent management-key credentials and a random initial key in a private file without printing or replacing them")
	resetKey := flag.Bool("reset-admin-key", false, "Recover an existing management key offline; generate a new random key in the private initial-login file")
	check := flag.Bool("healthcheck", false, "Check local Hub without loading credentials")
	version := flag.Bool("version", false, "Print launcher protocol and initial Hub version")
	flag.Parse()
	if *version {
		fmt.Printf("launcher_protocol=1 bootstrap_hub=%s\n", standalone.Version)
		return
	}
	if *initAccount || *check || *resetKey {
		cfg, err := standalone.ConfigFromEnv(os.Getenv)
		if err != nil {
			slog.Error("invalid Hub configuration", "error", err)
			os.Exit(1)
		}
		if (*initAccount && *check) || (*resetKey && (*initAccount || *check)) {
			slog.Error("choose only one operation")
			os.Exit(1)
		}
		if *initAccount {
			err = standalone.InitAdminAccount(cfg)
		} else if *resetKey {
			var warning bool
			warning, err = standalone.ResetAdminKey(cfg)
			if err == nil {
				if warning {
					slog.Warn("management key was replaced but directory durability is uncertain; inspect storage")
				}
				slog.Info("management key replaced; read admin-initial-login.txt privately; key was not printed")
			}
		} else {
			err = standalone.Healthcheck(context.Background(), cfg.Listen)
		}
		if err != nil {
			slog.Error("Hub initialization, recovery or health check failed", "error", err)
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
