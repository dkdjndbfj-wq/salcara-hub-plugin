package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"salcara/hub/internal/standalone"
)

func main() {
	initAccount := flag.Bool("init", false, "Create absent management-key credentials and a random initial key in a private file; never print or replace them")
	resetKey := flag.Bool("reset-admin-key", false, "Recover an existing management key offline; generate a new random key in the private initial-login file")
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
	if (*initAccount && *check) || (*resetKey && (*initAccount || *check)) {
		slog.Error("choose only one of -init, -reset-admin-key and -healthcheck")
		os.Exit(1)
	}
	if *initAccount {
		err = standalone.InitAdminAccount(cfg)
		if err != nil {
			slog.Error("Hub administrator initialization failed", "error", err)
			os.Exit(1)
		}
		slog.Info("Hub management key initialized; read admin-initial-login.txt privately; key was not printed")
		return
	}
	if *resetKey {
		warning, err := standalone.ResetAdminKey(cfg)
		if err != nil {
			slog.Error("Hub management key recovery failed", "error", err)
			os.Exit(1)
		}
		if warning {
			slog.Warn("management key was replaced but directory durability is uncertain; inspect storage")
		}
		slog.Info("management key replaced; read admin-initial-login.txt privately; key was not printed")
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
