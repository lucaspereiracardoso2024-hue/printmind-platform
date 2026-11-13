//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

const systemdUnitPath = "/etc/systemd/system/printmind-agent.service"

func openPlatformEventSink(_ bool) EventSink {
	return nil
}

func isRunningAsService() bool {
	return os.Getenv("INVOCATION_ID") != "" || os.Getppid() == 1
}

func runWindowsService(_ func(ctx context.Context)) error {
	return fmt.Errorf("not supported on linux")
}

func buildSystemdUnit(execPath string) string {
	return fmt.Sprintf(`[Unit]
Description=%s
After=network-online.target cups.service
Wants=network-online.target

[Service]
Type=simple
User=lp
Group=lp
ExecStart=%s
Restart=always
RestartSec=5s
StandardOutput=journal
StandardError=journal
StateDirectory=printmind
WorkingDirectory=/var/lib/printmind

[Install]
WantedBy=multi-user.target
`, ServiceDesc, execPath)
}

func handleServiceSubcommand(cmd string, cfg Config, paths OSPaths) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(cmd)) {
	case "install":
		if err := os.MkdirAll("/var/lib/printmind/logs", 0755); err != nil {
			return true, err
		}
		saveConfig(paths.ConfigPath, cfg)
		exePath, err := os.Executable()
		if err != nil {
			return true, err
		}
		unit := buildSystemdUnit(exePath)
		if err := os.WriteFile(systemdUnitPath, []byte(unit), 0644); err != nil {
			return true, err
		}
		_ = exec.Command("systemctl", "daemon-reload").Run()
		_ = exec.Command("systemctl", "enable", "printmind-agent.service").Run()
		fmt.Printf("Unidade systemd gravada em %s.\n", systemdUnitPath)
		return true, nil

	case "uninstall":
		_ = exec.Command("systemctl", "stop", "printmind-agent.service").Run()
		_ = exec.Command("systemctl", "disable", "printmind-agent.service").Run()
		if err := os.Remove(systemdUnitPath); err != nil && !os.IsNotExist(err) {
			return true, err
		}
		_ = exec.Command("systemctl", "daemon-reload").Run()
		fmt.Println("Unidade systemd printmind-agent.service removida.")
		return true, nil

	case "start":
		out, err := exec.Command("systemctl", "start", "printmind-agent.service").CombinedOutput()
		if err != nil {
			return true, fmt.Errorf("%v: %s", err, string(out))
		}
		fmt.Println("Servico printmind-agent iniciado.")
		return true, nil

	case "stop":
		out, err := exec.Command("systemctl", "stop", "printmind-agent.service").CombinedOutput()
		if err != nil {
			return true, fmt.Errorf("%v: %s", err, string(out))
		}
		fmt.Println("Servico printmind-agent parado.")
		return true, nil

	case "status":
		out, err := exec.Command("systemctl", "status", "--no-pager", "printmind-agent.service").CombinedOutput()
		fmt.Print(string(out))
		return true, err

	case "run-console":
		return false, nil
	}
	return false, nil
}

func onPlatformServiceStartup() {
	// No Windows EventLog channel to enable on Linux.
}

func waitForOSSignals(cancel context.CancelFunc) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
}
