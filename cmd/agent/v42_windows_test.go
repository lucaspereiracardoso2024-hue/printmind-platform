//go:build windows

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

func TestNotifyIconDataWStructSizeMatchesWin64(t *testing.T) {
	// Vista+ NOTIFYICONDATAW size on 64-bit Windows must be 976 bytes.
	if got := unsafe.Sizeof(notifyIconDataW{}); got != 976 {
		t.Fatalf("unexpected notifyIconDataW size on windows/amd64: got %d, want 976", got)
	}
}

func TestShouldUseCrossSessionTrayLaunch(t *testing.T) {
	if !shouldUseCrossSessionTrayLaunch(true, 1) {
		t.Fatalf("expected Windows service context to use cross-session tray launch")
	}
	if !shouldUseCrossSessionTrayLaunch(false, 0) {
		t.Fatalf("expected Session 0 CLI helper (e.g. restart-service) to use cross-session tray launch")
	}
	if shouldUseCrossSessionTrayLaunch(false, 1) {
		t.Fatalf("did not expect interactive desktop session (Session 1) to force WTSQueryUserToken cross-session launch")
	}
}

func TestShouldRunWindowsBootstrapGUI(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{`C:\Users\Test\Downloads\printmind-agent__pm_live_abc.exe`}
	if !shouldRunWindowsBootstrapGUI("", false, Config{}) {
		t.Fatalf("expected double-click plain binary launch (len(os.Args)==1) to open Windows bootstrap GUI")
	}

	os.Args = []string{`C:\Program Files\PrintMind\printmind-agent.exe`, `-install-gui`}
	if !shouldRunWindowsBootstrapGUI("", true, Config{}) {
		t.Fatalf("expected -install-gui flag to open Windows bootstrap GUI")
	}

	if shouldRunWindowsBootstrapGUI("install", false, Config{}) {
		t.Fatalf("did not expect CLI subcommand 'install' to enter bootstrap GUI")
	}
}

func TestBuildDesiredServiceConfigSetsNonZeroServiceType(t *testing.T) {
	exe := `C:\Program Files\PrintMind\printmind-agent.exe`
	cfg := buildDesiredServiceConfig(exe, mgr.Config{})
	if cfg.ServiceType != windows.SERVICE_WIN32_OWN_PROCESS {
		t.Fatalf("expected ServiceType SERVICE_WIN32_OWN_PROCESS, got %d", cfg.ServiceType)
	}
	if cfg.ErrorControl != mgr.ErrorNormal {
		t.Fatalf("expected ErrorControl ErrorNormal, got %d", cfg.ErrorControl)
	}
	if cfg.BinaryPathName != exe || cfg.StartType != mgr.StartAutomatic || cfg.ServiceStartName != "LocalSystem" || !cfg.DelayedAutoStart {
		t.Fatalf("unexpected service config fields: %+v", cfg)
	}
}

func TestVerifyPairingTokenWithBackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("apikey") == "reject" {
			_, _ = w.Write([]byte(`{"status":"error","message":"Token de pareamento inválido ou não encontrado"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 3 * time.Second}
	if err := verifyPairingTokenWithBackend(client, Config{ServerURL: srv.URL + "/rest/v1/rpc/sync_agent_telemetry", APIKey: "ok"}, "pm_live_valid"); err != nil {
		t.Fatalf("expected valid token to pass, got %v", err)
	}
	if err := verifyPairingTokenWithBackend(client, Config{ServerURL: srv.URL + "/rest/v1/rpc/sync_agent_telemetry", APIKey: "reject"}, "pm_live_dead"); err == nil {
		t.Fatalf("expected rejected token to return error")
	}
	if err := verifyPairingTokenWithBackend(client, Config{ServerURL: "http://127.0.0.1:9/rest/v1/rpc/sync_agent_telemetry"}, "pm_live_offline"); err != nil {
		t.Fatalf("expected transient offline error not to block install, got %v", err)
	}
}
