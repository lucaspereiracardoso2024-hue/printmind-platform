//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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

func handleServiceSubcommand(cmd string, cfg Config, paths OSPaths) (handled bool, err error) {
	switch strings.ToLower(strings.TrimSpace(cmd)) {
	case "install":
		saveConfig(paths.ConfigPath, cfg)
		exePath, err := os.Executable()
		if err != nil {
			return true, err
		}
		m, err := mgr.Connect()
		if err != nil {
			return true, fmt.Errorf("falha ao conectar ao SCM (execute como Administrador): %w", err)
		}
		defer m.Disconnect()

		if s, oErr := m.OpenService(ServiceName); oErr == nil {
			_ = s.Close()
			return true, fmt.Errorf("servico %s ja esta instalado", ServiceName)
		}

		s, err := m.CreateService(ServiceName, exePath, mgr.Config{
			DisplayName:      ServiceDisplay,
			Description:      ServiceDesc,
			StartType:        mgr.StartAutomatic,
			DelayedAutoStart: true,
			ServiceStartName: "LocalSystem",
		})
		if err != nil {
			return true, err
		}
		defer s.Close()

		_ = s.SetRecoveryActions([]mgr.RecoveryAction{
			{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
			{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
			{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		}, 86400)

		_ = eventlog.InstallAsEventCreate(ServiceName, eventlog.Error|eventlog.Warning|eventlog.Info)
		fmt.Printf("Servico %s instalado com sucesso (%s).\n", ServiceName, paths.ConfigPath)
		return true, nil

	case "uninstall":
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
		_, _ = s.Control(svc.Stop)
		time.Sleep(1500 * time.Millisecond)
		if err := s.Delete(); err != nil {
			return true, err
		}
		_ = eventlog.Remove(ServiceName)
		fmt.Printf("Servico %s removido.\n", ServiceName)
		return true, nil

	case "start":
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
		if err := s.Start(); err != nil {
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

func onPlatformServiceStartup() {
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
