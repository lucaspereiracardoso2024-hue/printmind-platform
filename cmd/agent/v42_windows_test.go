//go:build windows

package main

import (
	"os"
	"testing"
	"unsafe"
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
