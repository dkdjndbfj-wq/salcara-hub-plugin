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
	backup := flag.String("backup", "", "Create an encrypted offline full-volume backup at a new absolute path")
	verifyBackup := flag.String("verify-backup", "", "Verify an encrypted backup without restoring")
	restore := flag.String("restore-backup", "", "Restore an encrypted backup into a new, absent SALCARA_HUB_DATA_DIR")
	backupKey := flag.String("backup-key-file", "", "Private encryption key outside the data volume; generated only for a new backup if absent")
	flag.Parse()
	if *version {
		fmt.Println(standalone.Version)
		return
	}
	selected := 0
	for _, enabled := range []bool{*initAccount, *check, *resetKey, *backup != "", *restore != "", *verifyBackup != ""} {
		if enabled {
			selected++
		}
	}
	if selected > 1 || *backupKey != "" && *backup == "" && *restore == "" && *verifyBackup == "" {
		slog.Error("choose exactly one initialization, maintenance or health operation")
		os.Exit(1)
	}
	if *verifyBackup != "" {
		if *backupKey == "" {
			slog.Error("-backup-key-file is required")
			os.Exit(1)
		}
		if _, err := standalone.VerifyBackup(*verifyBackup, *backupKey); err != nil {
			slog.Error("Hub backup verification failed", "error", err)
			os.Exit(1)
		}
		slog.Info("Hub encrypted backup verified")
		return
	}
	cfg, err := standalone.ConfigFromEnv(os.Getenv)
	if err != nil {
		slog.Error("Hub configuration is invalid", "error", err)
		os.Exit(1)
	}
	if *backup != "" || *restore != "" || *verifyBackup != "" {
		if *backupKey == "" {
			slog.Error("-backup-key-file is required")
			os.Exit(1)
		}
		if *backup != "" {
			_, err = standalone.CreateBackup(cfg, *backup, *backupKey)
		} else if *restore != "" {
			err = standalone.RestoreBackup(cfg, *restore, *backupKey)
		} else {
			_, err = standalone.VerifyBackup(*verifyBackup, *backupKey)
		}
		if err != nil {
			slog.Error("Hub backup operation failed", "error", err)
			os.Exit(1)
		}
		slog.Info("Hub backup operation completed and verified; keep the encryption key separately and privately")
		return
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
