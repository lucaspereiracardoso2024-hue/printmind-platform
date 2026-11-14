//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

type windowsEventLogSink struct {
	el *eventlog.Log
}

func openPlatformEventSink(isService bool) EventSink {
	if !isService {
		return nil
	}
	el, err := eventlog.Open(ServiceName)
	if err != nil {
		return nil
	}
	return &windowsEventLogSink{el: el}
}

func (s *windowsEventLogSink) LogEvent(level slog.Level, msg string) {
	if s == nil || s.el == nil {
		return
	}
	if level >= slog.LevelError {
		_ = s.el.Error(1, msg)
	} else if level >= slog.LevelWarn {
		_ = s.el.Warning(2, msg)
	}
}

func (s *windowsEventLogSink) Close() {
	if s != nil && s.el != nil {
		_ = s.el.Close()
	}
}

func isRunningAsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

type printMindServiceHandler struct {
	runLoop func(ctx context.Context)
}

func (h *printMindServiceHandler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	// Immediately inform SCM that the service is running before scanning begins (avoids Error 1053).
	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	go func() {
		defer close(done)
		h.runLoop(ctx)
	}()

	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
				}
				return false, 0
			}
		case <-done:
			cancel()
			return false, 0
		}
	}
}

func runWindowsService(runLoop func(ctx context.Context)) error {
	return svc.Run(ServiceName, &printMindServiceHandler{runLoop: runLoop})
}

func queryPrintMindServicePID() uint32 {
	m, err := mgr.Connect()
	if err != nil {
		return 0
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return 0
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil || st.State == svc.Stopped {
		return 0
	}
	return st.ProcessId
}

func stopExistingPrintMindService(timeout time.Duration) {
	m, err := mgr.Connect()
	if err != nil {
		return
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return
	}
	defer s.Close()

	st, qErr := s.Query()
	if qErr == nil && st.State == svc.Stopped {
		return
	}
	_, _ = s.Control(svc.Stop)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		st, err := s.Query()
		if err != nil || st.State == svc.Stopped {
			return
		}
	}
}

func registerWindowsAutoStartAndUninstall(exePath string) {
	trayCmd := fmt.Sprintf(`"%s" -tray`, exePath)
	if k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE); err == nil {
		_ = k.SetStringValue("PrintMindTray", trayCmd)
		_ = k.Close()
	}

	uninstallKey := `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\PrintMindAgent`
	if uk, _, err := registry.CreateKey(registry.LOCAL_MACHINE, uninstallKey, registry.SET_VALUE); err == nil {
		_ = uk.SetStringValue("DisplayName", "PrintMind Agente de Telemetria")
		_ = uk.SetStringValue("DisplayVersion", AgentVersion)
		_ = uk.SetStringValue("Publisher", "PrintMind Tecnologia")
		_ = uk.SetStringValue("InstallLocation", filepath.Dir(exePath))
		_ = uk.SetStringValue("UninstallString", fmt.Sprintf(`"%s" uninstall`, exePath))
		_ = uk.SetDWordValue("NoModify", 1)
		_ = uk.SetDWordValue("NoRepair", 1)
		_ = uk.Close()
	}
}

func unregisterWindowsAutoStartAndUninstall() {
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE); err == nil {
		_ = k.DeleteValue("PrintMindTray")
		_ = k.Close()
	}
	_ = registry.DeleteKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\PrintMindAgent`)
}

func buildDesiredServiceConfig(exePath string, base mgr.Config) mgr.Config {
	cfg := base
	if cfg.ServiceType == 0 {
		cfg.ServiceType = windows.SERVICE_WIN32_OWN_PROCESS
	}
	if cfg.ErrorControl == 0 {
		cfg.ErrorControl = mgr.ErrorNormal
	}
	cfg.DisplayName = ServiceDisplay
	cfg.Description = ServiceDesc
	cfg.StartType = mgr.StartAutomatic
	cfg.DelayedAutoStart = true
	cfg.ServiceStartName = "LocalSystem"
	cfg.BinaryPathName = exePath
	return cfg
}

func installOrUpdateWindowsService(exePath string, paths OSPaths) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("falha ao conectar ao SCM (execute como Administrador): %w", err)
	}
	defer m.Disconnect()

	var s *mgr.Service
	if existing, oErr := m.OpenService(ServiceName); oErr == nil {
		s = existing
		baseCfg, cfgErr := s.Config()
		if cfgErr != nil {
			baseCfg = mgr.Config{}
		}
		desiredCfg := buildDesiredServiceConfig(exePath, baseCfg)
		if uErr := s.UpdateConfig(desiredCfg); uErr != nil {
			_ = s.Close()
			return fmt.Errorf("falha ao atualizar servico existente: %w", uErr)
		}
	} else {
		desiredCfg := buildDesiredServiceConfig(exePath, mgr.Config{})
		created, cErr := m.CreateService(ServiceName, exePath, desiredCfg)
		if cErr != nil {
			return cErr
		}
		s = created
	}
	defer s.Close()

	_ = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 86400)

	_ = eventlog.InstallAsEventCreate(ServiceName, eventlog.Error|eventlog.Warning|eventlog.Info)
	_ = EnablePrintServiceOperationalLog()
	registerWindowsAutoStartAndUninstall(exePath)
	slog.Info("Windows service configured", "service", ServiceName, "exe", exePath, "config", paths.ConfigPath)
	return nil
}

func startInstalledWindowsService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Start(); err != nil && err != windows.ERROR_SERVICE_ALREADY_RUNNING {
		return err
	}
	return nil
}

func handleServiceSubcommand(cmd string, cfg Config, paths OSPaths) (handled bool, err error) {
	switch strings.ToLower(strings.TrimSpace(cmd)) {
	case "install":
		saveConfig(paths.ConfigPath, cfg)
		exePath, err := os.Executable()
		if err != nil {
			return true, err
		}
		if err := installOrUpdateWindowsService(exePath, paths); err != nil {
			return true, err
		}
		fmt.Printf("Servico %s instalado/atualizado com sucesso (%s).\n", ServiceName, paths.ConfigPath)
		return true, nil

	case "uninstall":
		exePath, _ := os.Executable()
		stopAllTrayProcesses(exePath)
		unregisterWindowsAutoStartAndUninstall()
		stopExistingPrintMindService(10 * time.Second)

		m, err := mgr.Connect()
		if err != nil {
			return true, err
		}
		defer m.Disconnect()
		s, err := m.OpenService(ServiceName)
		if err != nil {
			return true, err
		}
		defer s.Close()
		if err := s.Delete(); err != nil {
			return true, err
		}
		_ = eventlog.Remove(ServiceName)
		fmt.Printf("Servico %s removido.\n", ServiceName)
		return true, nil

	case "restart-service":
		exePath, _ := os.Executable()
		stopExistingPrintMindService(15 * time.Second)
		if snap, sErr := ReadStatusSnapshot(paths.StatusPath); sErr != nil || snap.State == "updating" || snap.State == "" {
			_ = WriteStatusSnapshot(paths.StatusPath, AgentStatusSnapshot{
				AgentVersion: AgentVersion,
				State:        "starting",
			})
		}
		if err := startInstalledWindowsService(); err != nil {
			return true, err
		}
		time.Sleep(1 * time.Second)
		relaunchTrayInActiveSession(exePath)
		return true, nil

	case "start":
		if err := startInstalledWindowsService(); err != nil {
			return true, err
		}
		fmt.Printf("Servico %s iniciado.\n", ServiceName)
		return true, nil

	case "stop":
		m, err := mgr.Connect()
		if err != nil {
			return true, err
		}
		defer m.Disconnect()
		s, err := m.OpenService(ServiceName)
		if err != nil {
			return true, err
		}
		defer s.Close()
		if _, err := s.Control(svc.Stop); err != nil {
			return true, err
		}
		fmt.Printf("Comando de parada enviado para %s.\n", ServiceName)
		return true, nil

	case "status":
		m, err := mgr.Connect()
		if err != nil {
			return true, err
		}
		defer m.Disconnect()
		s, err := m.OpenService(ServiceName)
		if err != nil {
			return true, err
		}
		defer s.Close()
		st, err := s.Query()
		if err != nil {
			return true, err
		}
		stateStr := "UNKNOWN"
		switch st.State {
		case svc.Stopped:
			stateStr = "STOPPED"
		case svc.StartPending:
			stateStr = "START_PENDING"
		case svc.StopPending:
			stateStr = "STOP_PENDING"
		case svc.Running:
			stateStr = "RUNNING"
		}
		fmt.Printf("Servico %s: %s (PID %d)\n", ServiceName, stateStr, st.ProcessId)
		return true, nil

	case "run-console":
		return false, nil
	}
	return false, nil
}

func onPlatformServiceStartup(paths OSPaths) {
	cleanupOrphanUpdateFiles(paths)
	if err := EnablePrintServiceOperationalLog(); err != nil {
		slog.Warn("could not enable Microsoft-Windows-PrintService/Operational log", "error", err)
	}
}

func waitForOSSignals(cancel context.CancelFunc) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
}
