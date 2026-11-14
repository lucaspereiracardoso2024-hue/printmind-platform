//go:build windows

package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	defaultDashboardURL    = "https://lucaspereiracardoso2024-hue.github.io/printmind-platform/"
	trayMutexName          = `Local\PrintMindAgentTrayMutex`
	trayShutdownEventName  = `Global\PrintMindTrayShutdown`
	trayShutdownLocalEvent = `Local\PrintMindTrayShutdown`
	trayShutdownMsgName    = "PrintMindTrayShutdownMsg"

	swShowNormal  = 1
	swHide        = 0
	mbOK          = 0x00000000
	mbIconInfo    = 0x00000040
	mbIconWarning = 0x00000030
	mbIconError   = 0x00000010
	mbTopMost     = 0x00040000

	wsOverlapped  = 0x00000000
	wsCaption     = 0x00C00000
	wsSysMenu     = 0x00080000
	wsVisible     = 0x10000000
	wsChild       = 0x40000000
	wsTabStop     = 0x00010000
	wsBorder      = 0x00800000
	esAutoHScroll = 0x0080
	bsPushButton  = 0x00000000
	bsDefPushBtn  = 0x00000001
	ssLeft        = 0x00000000

	wmDestroy     = 0x0002
	wmClose       = 0x0010
	wmSetFont     = 0x0030
	wmCommand     = 0x0111
	wmTimer       = 0x0113
	wmLButtonDbl  = 0x0203
	wmRButtonUp   = 0x0205
	wmContextMenu = 0x007B
	wmAppTray     = 0x8000 + 42

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	niifInfo    = 0x00000001
	niifWarning = 0x00000002

	mfString    = 0x00000000
	mfSeparator = 0x00000800
	mfGrayed    = 0x00000001

	tpmRightButton = 0x0002
	tpmBottomAlign = 0x0020

	idEditToken  = 101
	idBtnInstall = 1
	idBtnCancel  = 2

	menuStatusInfo = 2001
	menuScanNow    = 2002
	menuOpenPanel  = 2003
	menuOpenLogs   = 2004
	menuExitTray   = 2005
)

var (
	user32DLL              = windows.NewLazySystemDLL("user32.dll")
	procMessageBoxW        = user32DLL.NewProc("MessageBoxW")
	procRegisterClassExW   = user32DLL.NewProc("RegisterClassExW")
	procCreateWindowExW    = user32DLL.NewProc("CreateWindowExW")
	procDefWindowProcW     = user32DLL.NewProc("DefWindowProcW")
	procDestroyWindow      = user32DLL.NewProc("DestroyWindow")
	procShowWindow         = user32DLL.NewProc("ShowWindow")
	procUpdateWindow       = user32DLL.NewProc("UpdateWindow")
	procGetMessageW        = user32DLL.NewProc("GetMessageW")
	procTranslateMessage   = user32DLL.NewProc("TranslateMessage")
	procDispatchMessageW   = user32DLL.NewProc("DispatchMessageW")
	procIsDialogMessageW   = user32DLL.NewProc("IsDialogMessageW")
	procPostQuitMessage    = user32DLL.NewProc("PostQuitMessage")
	procPostMessageW       = user32DLL.NewProc("PostMessageW")
	procSendMessageW       = user32DLL.NewProc("SendMessageW")
	procGetWindowTextW     = user32DLL.NewProc("GetWindowTextW")
	procGetWindowTextLenW  = user32DLL.NewProc("GetWindowTextLengthW")
	procSetFocus           = user32DLL.NewProc("SetFocus")
	procLoadIconW          = user32DLL.NewProc("LoadIconW")
	procLoadCursorW        = user32DLL.NewProc("LoadCursorW")
	procGetSystemMetrics   = user32DLL.NewProc("GetSystemMetrics")
	procCreatePopupMenu    = user32DLL.NewProc("CreatePopupMenu")
	procAppendMenuW        = user32DLL.NewProc("AppendMenuW")
	procDestroyMenu        = user32DLL.NewProc("DestroyMenu")
	procTrackPopupMenu     = user32DLL.NewProc("TrackPopupMenu")
	procGetCursorPos       = user32DLL.NewProc("GetCursorPos")
	procSetForegroundWnd   = user32DLL.NewProc("SetForegroundWindow")
	procSetTimer           = user32DLL.NewProc("SetTimer")
	procRegisterWindowMsgW = user32DLL.NewProc("RegisterWindowMessageW")

	shell32DLL           = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteW    = shell32DLL.NewProc("ShellExecuteW")
	procShellNotifyIconW = shell32DLL.NewProc("Shell_NotifyIconW")
	procExtractIconW     = shell32DLL.NewProc("ExtractIconW")

	gdi32DLL        = windows.NewLazySystemDLL("gdi32.dll")
	procCreateFontW = gdi32DLL.NewProc("CreateFontW")
	procDeleteObj   = gdi32DLL.NewProc("DeleteObject")

	kernel32DLL         = windows.NewLazySystemDLL("kernel32.dll")
	procGetModuleHandle = kernel32DLL.NewProc("GetModuleHandleW")
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

type winPoint struct {
	X int32
	Y int32
}

type winMsg struct {
	Hwnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      winPoint
}

type notifyIconDataW struct {
	CbSize            uint32
	HWnd              windows.Handle
	UID               uint32
	UFlags            uint32
	UCallbackMessage  uint32
	HIcon             windows.Handle
	SzTip             [128]uint16
	DwState           uint32
	DwStateMask       uint32
	SzInfo            [256]uint16
	UVersionOrTimeout uint32
	SzInfoTitle       [64]uint16
	DwInfoFlags       uint32
	GuidItem          windows.GUID
	HBalloonIcon      windows.Handle
}

func showMessageBox(title, body string, flags uint32) {
	tPtr, _ := windows.UTF16PtrFromString(title)
	bPtr, _ := windows.UTF16PtrFromString(body)
	_, _, _ = procMessageBoxW.Call(0, uintptr(unsafe.Pointer(bPtr)), uintptr(unsafe.Pointer(tPtr)), uintptr(flags|mbTopMost))
}

func notifyGUIErrorIfInteractive(message string) {
	if isRunningAsService() {
		return
	}
	showMessageBox("PrintMind Agente", message, mbIconError)
}

func isProcessElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

func shouldRunWindowsBootstrapGUI(subcmd string, installGUIFlag bool, _ Config) bool {
	if installGUIFlag {
		return true
	}
	if subcmd != "" || isRunningAsService() {
		return false
	}
	return len(os.Args) == 1
}

func relaunchElevatedForGUIInstall(extractedToken string) error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	verbPtr, _ := windows.UTF16PtrFromString("runas")
	filePtr, _ := windows.UTF16PtrFromString(exePath)
	argStr := "-install-gui"
	if extractedToken != "" {
		argStr += " -token " + extractedToken
	}
	paramsPtr, _ := windows.UTF16PtrFromString(argStr)
	dirPtr, _ := windows.UTF16PtrFromString(filepath.Dir(exePath))

	r1, _, callErr := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verbPtr)),
		uintptr(unsafe.Pointer(filePtr)),
		uintptr(unsafe.Pointer(paramsPtr)),
		uintptr(unsafe.Pointer(dirPtr)),
		swShowNormal,
	)
	if r1 <= 32 {
		if callErr != nil && callErr != windows.ERROR_SUCCESS {
			return callErr
		}
		return fmt.Errorf("elevação UAC cancelada ou recusada (código %d)", r1)
	}
	return nil
}

func runWindowsBootstrapGUI(cfg Config, _ OSPaths) int {
	// Ensure token extracted from downloaded filename survives into elevated process.
	extractedTok := cfg.PairingToken
	if extractedTok == "" {
		if exePath, err := os.Executable(); err == nil {
			extractedTok = extractTokenFromExeName(exePath)
		}
	}

	if !isProcessElevated() {
		if err := relaunchElevatedForGUIInstall(extractedTok); err != nil {
			slog.Warn("UAC elevation aborted or denied", "error", err)
			return 1
		}
		return 0
	}

	// Elevated: always work against canonical ProgramData paths and Program Files binary.
	prodPaths := ResolvePaths("", true)
	_, logCloser := SetupLogger(prodPaths.LogPath, false, openPlatformEventSink(false))
	if logCloser != nil {
		defer logCloser.Close()
	}
	cfg = loadOrInitConfig(prodPaths.ConfigPath, cfg.ServerURL, extractedTok, "")

	if strings.TrimSpace(cfg.PairingToken) == "" {
		input, ok := promptPairingTokenDialog()
		if !ok || strings.TrimSpace(input) == "" {
			return 0
		}
		input = strings.TrimSpace(input)
		if strings.Contains(input, "@") {
			httpClient := &http.Client{Timeout: 20 * time.Second}
			resolved, err := resolveTokenByEmail(httpClient, cfg.ServerURL, cfg.APIKey, input)
			if err != nil {
				slog.Error("failed resolving pairing token via email in GUI", "error", err)
				showMessageBox(
					"PrintMind Agente — Pareamento",
					"Não foi possível localizar o token de pareamento para este e-mail.\n\nVerifique sua conexão com a internet ou cole diretamente o token pm_live_... gerado no painel PrintMind.",
					mbIconWarning,
				)
				return 1
			}
			cfg.PairingToken = resolved
		} else {
			if !strings.HasPrefix(input, "pm_live_") {
				showMessageBox(
					"PrintMind Agente — Token Inválido",
					"O token informado deve começar com 'pm_live_' ou ser o e-mail da sua conta PrintMind.",
					mbIconWarning,
				)
				return 1
			}
			cfg.PairingToken = input
		}
	}

	if err := performElevatedInstall(cfg, prodPaths); err != nil {
		slog.Error("elevated GUI install failed", "error", err)
		showMessageBox(
			"PrintMind Agente — Instalação",
			fmt.Sprintf("Não foi possível concluir a instalação do serviço em segundo plano.\n\nConsulte o registro de diagnóstico em:\n%s", prodPaths.LogPath),
			mbIconError,
		)
		return 1
	}

	showMessageBox(
		"PrintMind Agente v"+AgentVersion,
		"Instalação concluída com sucesso!\n\nO serviço PrintMind está rodando em segundo plano e o ícone já foi ativado na bandeja do relógio (perto do horário do Windows).",
		mbIconInfo,
	)
	return 0
}

func targetInstallExePath() string {
	pf := strings.TrimSpace(os.Getenv("ProgramFiles"))
	if pf == "" {
		pf = `C:\Program Files`
	}
	return filepath.Join(pf, "PrintMind", "printmind-agent.exe")
}

func copyFileAtomicWindows(src, dst string) error {
	if strings.EqualFold(filepath.Clean(src), filepath.Clean(dst)) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".staging"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	if _, statErr := os.Stat(dst); statErr == nil {
		old := dst + ".old"
		_ = os.Remove(old)
		if renErr := os.Rename(dst, old); renErr != nil {
			slog.Warn("could not move locked binary to .old; attempting direct replace", "error", renErr)
		} else {
			_ = os.Remove(old)
		}
	}
	return os.Rename(tmp, dst)
}

func performElevatedInstall(cfg Config, prodPaths OSPaths) error {
	srcExe, err := os.Executable()
	if err != nil {
		return err
	}
	dstExe := targetInstallExePath()

	// Stop existing tray and service instances before updating binary on disk.
	stopAllTrayProcesses(dstExe)
	stopExistingPrintMindService(10 * time.Second)

	if err := copyFileAtomicWindows(srcExe, dstExe); err != nil {
		return fmt.Errorf("copiar executável para Program Files: %w", err)
	}

	saveConfig(prodPaths.ConfigPath, cfg)

	if err := installOrUpdateWindowsService(dstExe, prodPaths); err != nil {
		return err
	}

	if err := startInstalledWindowsService(); err != nil {
		return err
	}

	relaunchTrayInActiveSession(dstExe)
	return nil
}

// --- Native Win32 Dialog for Pairing Token ---

type tokenDialogState struct {
	hwnd      windows.Handle
	hwndEdit  windows.Handle
	hFont     windows.Handle
	submitted bool
	value     string
}

func copyUTF16ToSlice(dst []uint16, s string) {
	u, _ := windows.UTF16FromString(s)
	n := len(u)
	if n > len(dst) {
		n = len(dst)
	}
	copy(dst[:n], u[:n])
	if len(dst) > 0 && n == len(dst) {
		dst[len(dst)-1] = 0
	}
}

func getWindowTextString(hwnd windows.Handle) string {
	ln, _, _ := procGetWindowTextLenW.Call(uintptr(hwnd))
	if ln == 0 {
		return ""
	}
	buf := make([]uint16, ln+2)
	_, _, _ = procGetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), ln+1)
	return windows.UTF16ToString(buf)
}

func promptPairingTokenDialog() (string, bool) {
	hInst, _, _ := procGetModuleHandle.Call(0)
	clsName, _ := windows.UTF16PtrFromString("PrintMindPairingDialogClass")
	titlePtr, _ := windows.UTF16PtrFromString("PrintMind Agente v" + AgentVersion + " — Pareamento")

	st := &tokenDialogState{}
	wndProc := syscall.NewCallback(func(hwnd windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
		switch msg {
		case wmCommand:
			cmdID := uint16(wParam & 0xFFFF)
			switch cmdID {
			case idBtnInstall:
				st.value = strings.TrimSpace(getWindowTextString(st.hwndEdit))
				st.submitted = true
				_, _, _ = procDestroyWindow.Call(uintptr(hwnd))
				return 0
			case idBtnCancel:
				st.submitted = false
				_, _, _ = procDestroyWindow.Call(uintptr(hwnd))
				return 0
			}
		case wmClose:
			st.submitted = false
			_, _, _ = procDestroyWindow.Call(uintptr(hwnd))
			return 0
		case wmDestroy:
			_, _, _ = procPostQuitMessage.Call(0)
			return 0
		}
		ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
		return ret
	})

	cursor, _, _ := procLoadCursorW.Call(0, 32512) // IDC_ARROW
	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   wndProc,
		hInstance:     windows.Handle(hInst),
		hCursor:       windows.Handle(cursor),
		hbrBackground: windows.Handle(16), // COLOR_BTNFACE + 1
		lpszClassName: clsName,
	}
	_, _, _ = procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	scrW, _, _ := procGetSystemMetrics.Call(0)
	scrH, _, _ := procGetSystemMetrics.Call(1)
	dlgW, dlgH := uintptr(470), uintptr(185)
	posX := (scrW - dlgW) / 2
	posY := (scrH - dlgH) / 2

	hwnd, _, err := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(clsName)),
		uintptr(unsafe.Pointer(titlePtr)),
		wsOverlapped|wsCaption|wsSysMenu|wsVisible,
		posX, posY, dlgW, dlgH,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		slog.Error("failed creating pairing dialog window", "error", err)
		return "", false
	}
	st.hwnd = windows.Handle(hwnd)

	fontFace, _ := windows.UTF16PtrFromString("Segoe UI")
	lfHeight := int32(-14)
	hFont, _, _ := procCreateFontW.Call(
		uintptr(lfHeight), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(fontFace)),
	)
	st.hFont = windows.Handle(hFont)
	if st.hFont != 0 {
		defer procDeleteObj.Call(uintptr(st.hFont))
	}

	staticCls, _ := windows.UTF16PtrFromString("STATIC")
	lblText, _ := windows.UTF16PtrFromString("Cole seu Token de Pareamento (pm_live_...) ou digite o e-mail da conta:")
	hLbl, _, _ := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(staticCls)), uintptr(unsafe.Pointer(lblText)),
		wsChild|wsVisible|ssLeft,
		16, 16, 424, 24,
		hwnd, 0, hInst, 0,
	)

	editCls, _ := windows.UTF16PtrFromString("EDIT")
	emptyPtr, _ := windows.UTF16PtrFromString("")
	hEdit, _, _ := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(editCls)), uintptr(unsafe.Pointer(emptyPtr)),
		wsChild|wsVisible|wsTabStop|wsBorder|esAutoHScroll,
		16, 46, 424, 28,
		hwnd, idEditToken, hInst, 0,
	)
	st.hwndEdit = windows.Handle(hEdit)

	btnCls, _ := windows.UTF16PtrFromString("BUTTON")
	okText, _ := windows.UTF16PtrFromString("Instalar e Conectar")
	hOK, _, _ := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(btnCls)), uintptr(unsafe.Pointer(okText)),
		wsChild|wsVisible|wsTabStop|bsDefPushBtn,
		196, 94, 140, 32,
		hwnd, idBtnInstall, hInst, 0,
	)

	cancelText, _ := windows.UTF16PtrFromString("Cancelar")
	hCancel, _, _ := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(btnCls)), uintptr(unsafe.Pointer(cancelText)),
		wsChild|wsVisible|wsTabStop|bsPushButton,
		346, 94, 94, 32,
		hwnd, idBtnCancel, hInst, 0,
	)

	if st.hFont != 0 {
		for _, ctrl := range []uintptr{hLbl, hEdit, hOK, hCancel} {
			_, _, _ = procSendMessageW.Call(ctrl, wmSetFont, uintptr(st.hFont), 1)
		}
	}

	_, _, _ = procShowWindow.Call(hwnd, swShowNormal)
	_, _, _ = procUpdateWindow.Call(hwnd)
	_, _, _ = procSetFocus.Call(hEdit)

	var m winMsg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		handled, _, _ := procIsDialogMessageW.Call(hwnd, uintptr(unsafe.Pointer(&m)))
		if handled != 0 {
			continue
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	return st.value, st.submitted
}

// --- Windows System Tray (-tray) ---

func resolveTrayStatusPath(fallback OSPaths) string {
	prodDir := defaultProductionDataDir()
	prodStatus := filepath.Join(prodDir, "status.json")
	if _, err := os.Stat(prodStatus); err == nil {
		return prodStatus
	}
	if fallback.StatusPath != "" {
		return fallback.StatusPath
	}
	return prodStatus
}

func formatTrayStatusLine(snap AgentStatusSnapshot, snapErr error) string {
	if snapErr != nil {
		return "Status: Aguardando serviço PrintMind"
	}
	stateLabel := "Online"
	switch snap.State {
	case "scanning":
		stateLabel = "Varrendo rede..."
	case "retrying":
		stateLabel = "Offline (fila local ativa)"
	case "error":
		stateLabel = "Atenção no serviço"
	case "updating":
		stateLabel = "Atualizando..."
	case "starting":
		stateLabel = "Iniciando..."
	}
	if snap.PrintersCount > 0 {
		return fmt.Sprintf("PrintMind v%s • %s (%d imp.)", AgentVersion, stateLabel, snap.PrintersCount)
	}
	return fmt.Sprintf("PrintMind v%s • %s", AgentVersion, stateLabel)
}

func loadTrayIconHandle(exePath string) windows.Handle {
	hInst, _, _ := procGetModuleHandle.Call(0)
	if exePath != "" {
		exePtr, _ := windows.UTF16PtrFromString(exePath)
		h, _, _ := procExtractIconW.Call(hInst, uintptr(unsafe.Pointer(exePtr)), 0)
		if h > 1 {
			return windows.Handle(h)
		}
	}
	hDef, _, _ := procLoadIconW.Call(0, 32512) // IDI_APPLICATION
	return windows.Handle(hDef)
}

func updateTrayTooltip(hwnd windows.Handle, tip string, balloonTitle string, balloonBody string, infoFlags uint32) {
	var nid notifyIconDataW
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = hwnd
	nid.UID = 1
	nid.UFlags = nifTip
	copyUTF16ToSlice(nid.SzTip[:], tip)
	if balloonBody != "" {
		nid.UFlags |= nifInfo
		nid.DwInfoFlags = infoFlags
		nid.UVersionOrTimeout = 4000
		copyUTF16ToSlice(nid.SzInfoTitle[:], balloonTitle)
		copyUTF16ToSlice(nid.SzInfo[:], balloonBody)
	}
	_, _, _ = procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

func openShellURLOrPath(target string) {
	verbPtr, _ := windows.UTF16PtrFromString("open")
	tgtPtr, _ := windows.UTF16PtrFromString(target)
	_, _, _ = procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verbPtr)), uintptr(unsafe.Pointer(tgtPtr)), 0, 0, swShowNormal)
}

func runTrayMode(_ Config, paths OSPaths) int {
	// Single-instance tray mutex per user session.
	mPtr, _ := windows.UTF16PtrFromString(trayMutexName)
	hMutex, err := windows.CreateMutex(nil, false, mPtr)
	if err == windows.ERROR_ALREADY_EXISTS {
		if hMutex != 0 {
			_ = windows.CloseHandle(hMutex)
		}
		return 0
	}
	if hMutex != 0 {
		defer windows.CloseHandle(hMutex)
	}

	statusPath := resolveTrayStatusPath(paths)
	exePath, _ := os.Executable()
	hInst, _, _ := procGetModuleHandle.Call(0)
	shutdownMsgPtr, _ := windows.UTF16PtrFromString(trayShutdownMsgName)
	shutdownMsgID, _, _ := procRegisterWindowMsgW.Call(uintptr(unsafe.Pointer(shutdownMsgPtr)))

	clsName, _ := windows.UTF16PtrFromString("PrintMindTrayHiddenWindowClass")
	var trayHwnd windows.Handle
	lastTip := ""
	timerTicks := 0

	showTrayMenu := func(hwnd windows.Handle) {
		snap, sErr := ReadStatusSnapshot(statusPath)
		statusLine := formatTrayStatusLine(snap, sErr)

		hMenu, _, _ := procCreatePopupMenu.Call()
		if hMenu == 0 {
			return
		}
		defer procDestroyMenu.Call(hMenu)

		stPtr, _ := windows.UTF16PtrFromString(statusLine)
		scanPtr, _ := windows.UTF16PtrFromString("Forçar Leitura Agora")
		panelPtr, _ := windows.UTF16PtrFromString("Abrir Painel PrintMind")
		logsPtr, _ := windows.UTF16PtrFromString("Abrir Pasta de Logs")
		exitPtr, _ := windows.UTF16PtrFromString("Sair do Ícone")

		_, _, _ = procAppendMenuW.Call(hMenu, mfString|mfGrayed, menuStatusInfo, uintptr(unsafe.Pointer(stPtr)))
		_, _, _ = procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
		_, _, _ = procAppendMenuW.Call(hMenu, mfString, menuScanNow, uintptr(unsafe.Pointer(scanPtr)))
		_, _, _ = procAppendMenuW.Call(hMenu, mfString, menuOpenPanel, uintptr(unsafe.Pointer(panelPtr)))
		_, _, _ = procAppendMenuW.Call(hMenu, mfString, menuOpenLogs, uintptr(unsafe.Pointer(logsPtr)))
		_, _, _ = procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
		_, _, _ = procAppendMenuW.Call(hMenu, mfString, menuExitTray, uintptr(unsafe.Pointer(exitPtr)))

		var pt winPoint
		_, _, _ = procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
		_, _, _ = procSetForegroundWnd.Call(uintptr(hwnd))
		_, _, _ = procTrackPopupMenu.Call(hMenu, tpmRightButton|tpmBottomAlign, uintptr(pt.X), uintptr(pt.Y), 0, uintptr(hwnd), 0)
		_, _, _ = procPostMessageW.Call(uintptr(hwnd), 0, 0, 0)
	}

	triggerScanNowAsync := func(hwnd windows.Handle) {
		go func() {
			alreadyPending, scanErr := SendIPCScanNow(statusPath)
			if scanErr != nil {
				updateTrayTooltip(hwnd, "PrintMind Agente v"+AgentVersion, "PrintMind Agente", "Serviço offline ou iniciando. Verifique o serviço PrintMindAgent.", niifWarning)
				return
			}
			if alreadyPending {
				updateTrayTooltip(hwnd, "PrintMind Agente v"+AgentVersion+" • Varrendo", "PrintMind Agente", "Uma varredura de impressoras já está em andamento.", niifInfo)
			} else {
				updateTrayTooltip(hwnd, "PrintMind Agente v"+AgentVersion+" • Varrendo", "PrintMind Agente", "Leitura imediata solicitada ao serviço PrintMind!", niifInfo)
			}
		}()
	}

	wndProc := syscall.NewCallback(func(hwnd windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
		if shutdownMsgID != 0 && msg == uint32(shutdownMsgID) {
			_, _, _ = procDestroyWindow.Call(uintptr(hwnd))
			return 0
		}
		switch msg {
		case wmAppTray:
			switch uint32(lParam) {
			case wmRButtonUp, wmContextMenu:
				showTrayMenu(hwnd)
			case wmLButtonDbl:
				snap, _ := ReadStatusSnapshot(statusPath)
				url := strings.TrimSpace(snap.DashboardURL)
				if url == "" {
					url = defaultDashboardURL
				}
				openShellURLOrPath(url)
			}
			return 0
		case wmTimer:
			timerTicks++
			snap, sErr := ReadStatusSnapshot(statusPath)
			if sErr == nil && snap.State == "updating" {
				_, _, _ = procDestroyWindow.Call(uintptr(hwnd))
				return 0
			}
			nextTip := formatTrayStatusLine(snap, sErr)
			if nextTip != lastTip || timerTicks%6 == 0 {
				lastTip = nextTip
				updateTrayTooltip(hwnd, nextTip, "", "", 0)
			}
			return 0
		case wmCommand:
			switch uint16(wParam & 0xFFFF) {
			case menuScanNow:
				triggerScanNowAsync(hwnd)
			case menuOpenPanel:
				snap, _ := ReadStatusSnapshot(statusPath)
				url := strings.TrimSpace(snap.DashboardURL)
				if url == "" {
					url = defaultDashboardURL
				}
				openShellURLOrPath(url)
			case menuOpenLogs:
				logDir := filepath.Dir(paths.LogPath)
				if _, err := os.Stat(filepath.Join(defaultProductionDataDir(), "logs")); err == nil {
					logDir = filepath.Join(defaultProductionDataDir(), "logs")
				}
				openShellURLOrPath(logDir)
			case menuExitTray:
				_, _, _ = procDestroyWindow.Call(uintptr(hwnd))
			}
			return 0
		case wmClose, wmDestroy:
			var nid notifyIconDataW
			nid.CbSize = uint32(unsafe.Sizeof(nid))
			nid.HWnd = hwnd
			nid.UID = 1
			_, _, _ = procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
			_, _, _ = procPostQuitMessage.Call(0)
			return 0
		}
		ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
		return ret
	})

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   wndProc,
		hInstance:     windows.Handle(hInst),
		lpszClassName: clsName,
	}
	_, _, _ = procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	titlePtr, _ := windows.UTF16PtrFromString("PrintMindTray")
	hwndRaw, _, _ := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(clsName)), uintptr(unsafe.Pointer(titlePtr)),
		0, 0, 0, 0, 0, 0, 0, hInst, 0,
	)
	if hwndRaw == 0 {
		return 1
	}
	trayHwnd = windows.Handle(hwndRaw)
	_, _, _ = procShowWindow.Call(hwndRaw, swHide)

	var nid notifyIconDataW
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = trayHwnd
	nid.UID = 1
	nid.UFlags = nifMessage | nifIcon | nifTip
	nid.UCallbackMessage = wmAppTray
	nid.HIcon = loadTrayIconHandle(exePath)
	snap, sErr := ReadStatusSnapshot(statusPath)
	lastTip = formatTrayStatusLine(snap, sErr)
	copyUTF16ToSlice(nid.SzTip[:], lastTip)
	_, _, _ = procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))

	_, _, _ = procSetTimer.Call(hwndRaw, 1, 500, 0)

	// Listen for named shutdown events triggered by installer/updater in local and global namespaces.
	for _, evName := range []string{trayShutdownLocalEvent, trayShutdownEventName} {
		go func(name string, target windows.Handle) {
			evPtr, _ := windows.UTF16PtrFromString(name)
			ev, cErr := windows.CreateEvent(nil, 1, 0, evPtr)
			if cErr != nil || ev == 0 {
				return
			}
			defer windows.CloseHandle(ev)
			_, _ = windows.WaitForSingleObject(ev, windows.INFINITE)
			_, _, _ = procPostMessageW.Call(uintptr(target), wmClose, 0, 0)
		}(evName, trayHwnd)
	}

	var m winMsg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	return 0
}
