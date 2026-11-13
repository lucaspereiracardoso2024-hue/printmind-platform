//go:build linux

package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

func checkAndApplyUpdate(ctx context.Context, client *http.Client, cfg Config, paths OSPaths, rc RemoteConfig) {
	url := strings.TrimSpace(rc.UpdateLinuxURL)
	expectedHash := strings.ToLower(strings.TrimSpace(rc.UpdateLinuxSHA256))
	if url == "" || expectedHash == "" {
		return
	}
	if rc.UpdateVersion != "" && rc.UpdateVersion == AgentVersion {
		return
	}
	if cfg.DisableAutoUpdate {
		slog.Info("skipping Linux auto-update because disable_auto_update is configured")
		return
	}

	exePath, err := os.Executable()
	if err != nil || exePath == "" {
		slog.Warn("failed to resolve executable path for Linux auto-update", "error", err)
		return
	}

	if err := stageAndVerifyBinaryUpdate(ctx, client, url, expectedHash, paths.StagingDir, exePath, true); err != nil {
		slog.Error("Linux auto-update failed; keeping existing binary", "error", err)
		return
	}

	slog.Info("Linux binary updated atomically", "version", rc.UpdateVersion, "path", exePath)
	if isRunningAsService() {
		if os.Geteuid() == 0 {
			if err := exec.Command("systemctl", "restart", "printmind-agent.service").Start(); err == nil {
				return
			}
		}
		// Under systemd Restart=always, clean exit respawns the updated binary even as unprivileged user lp.
		slog.Info("exiting cleanly so systemd restarts PrintMind Agent into updated binary")
		os.Exit(0)
	}
}
