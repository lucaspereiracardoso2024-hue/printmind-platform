//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	wintrustDLL        = windows.NewLazySystemDLL("wintrust.dll")
	procWinVerifyTrust = wintrustDLL.NewProc("WinVerifyTrust")

	wtsapi32DLL                   = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSGetActiveConsoleSessID = kernel32DLL.NewProc("WTSGetActiveConsoleSessionId")
	procProcessIdToSessID         = kernel32DLL.NewProc("ProcessIdToSessionId")
	procWTSQueryUserToken         = wtsapi32DLL.NewProc("WTSQueryUserToken")

	userenvDLL                 = windows.NewLazySystemDLL("userenv.dll")
	procCreateEnvironmentBlock = userenvDLL.NewProc("CreateEnvironmentBlock")
	procDestroyEnvironmentBlk  = userenvDLL.NewProc("DestroyEnvironmentBlock")

	advapi32DLL              = windows.NewLazySystemDLL("advapi32.dll")
	procCreateProcessAsUserW = advapi32DLL.NewProc("CreateProcessAsUserW")
)

var winTrustActionGenericVerifyV2 = windows.GUID{
	Data1: 0x00AAC56B,
	Data2: 0xCD44,
	Data3: 0x11d0,
	Data4: [8]byte{0x8C, 0xC2, 0x00, 0xC0, 0x4F, 0xC2, 0x95, 0xEE},
}

type winTrustFileInfo struct {
	cbStruct       uint32
	pcwszFilePath  *uint16
	hFile          windows.Handle
	pgKnownSubject *windows.GUID
}

type winTrustData struct {
	cbStruct            uint32
	pPolicyCallbackData uintptr
	pSIPClientData      uintptr
	dwUIChoice          uint32
	fdwRevocationChecks uint32
	dwUnionChoice       uint32
	pFile               *winTrustFileInfo
	dwStateAction       uint32
	hWVTStateData       windows.Handle
	pwszURLReference    *uint16
	dwProvFlags         uint32
	dwUIContext         uint32
	pSignatureSettings  uintptr
}

// isAutoUpdateDisabled checks config and HKLM\SOFTWARE\PrintMind\Agent AUTOUPDATE=0 (Intune/GPO managed).
func isAutoUpdateDisabled(cfg Config) bool {
	if cfg.DisableAutoUpdate {
		return true
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\PrintMind\Agent`, registry.READ)
	if err != nil {
		return false
	}
	defer k.Close()
	if v, _, err := k.GetIntegerValue("AUTOUPDATE"); err == nil && v == 0 {
		return true
	}
	if s, _, err := k.GetStringValue("AUTOUPDATE"); err == nil && strings.TrimSpace(s) == "0" {
		return true
	}
	return false
}

// cleanupOrphanUpdateFiles removes leftover .old binaries and staged artifacts after a completed update.
func cleanupOrphanUpdateFiles(paths OSPaths) {
	if exePath, err := os.Executable(); err == nil && exePath != "" {
		oldPath := exePath + ".old"
		if _, stErr := os.Stat(oldPath); stErr == nil {
			if rmErr := os.Remove(oldPath); rmErr != nil {
				slog.Debug("deferred cleanup of previous .old binary", "path", oldPath, "error", rmErr)
			}
		}
	}
	for _, dir := range []string{paths.UpdatesDir, paths.StagingDir} {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		cutoff := time.Now().Add(-24 * time.Hour)
		for _, ent := range entries {
			if ent.IsDir() {
				continue
			}
			if info, err := ent.Info(); err == nil && info.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(dir, ent.Name()))
			}
		}
	}
}

// verifyAuthenticodeSignature validates an installer/binary Authenticode signature via Windows WinVerifyTrust.
func verifyAuthenticodeSignature(filePath string) error {
	pathPtr, err := windows.UTF16PtrFromString(filePath)
	if err != nil {
		return err
	}
	fileInfo := winTrustFileInfo{
		cbStruct:      uint32(unsafe.Sizeof(winTrustFileInfo{})),
		pcwszFilePath: pathPtr,
	}
	data := winTrustData{
		cbStruct:      uint32(unsafe.Sizeof(winTrustData{})),
		dwUIChoice:    2, // WTD_UI_NONE
		dwUnionChoice: 1, // WTD_CHOICE_FILE
		pFile:         &fileInfo,
		dwStateAction: 1, // WTD_STATEACTION_VERIFY
	}
	r1, _, _ := procWinVerifyTrust.Call(
		0,
		uintptr(unsafe.Pointer(&winTrustActionGenericVerifyV2)),
		uintptr(unsafe.Pointer(&data)),
	)
	data.dwStateAction = 2 // WTD_STATEACTION_CLOSE
	_, _, _ = procWinVerifyTrust.Call(
		0,
		uintptr(unsafe.Pointer(&winTrustActionGenericVerifyV2)),
		uintptr(unsafe.Pointer(&data)),
	)
	if r1 != 0 {
		return fmt.Errorf("WinVerifyTrust failed with status 0x%08X", uint32(r1))
	}
	return nil
}

func processSessionID(pid uint32) (uint32, bool) {
	if pid == 0 {
		return 0, false
	}
	var sessID uint32
	r1, _, _ := procProcessIdToSessID.Call(uintptr(pid), uintptr(unsafe.Pointer(&sessID)))
	return sessID, r1 != 0
}

func currentProcessSessionID() uint32 {
	if sessID, ok := processSessionID(uint32(os.Getpid())); ok {
		return sessID
	}
	return 0
}

func shouldUseCrossSessionTrayLaunch(runningAsService bool, currentSessionID uint32) bool {
	return runningAsService || currentSessionID == 0
}

func findMatchingTrayPIDs(targetBase string, skipPIDs ...uint32) []uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snap, &pe); err != nil {
		return nil
	}

	var matched []uint32
	for {
		exeName := strings.ToLower(windows.UTF16ToString(pe.ExeFile[:]))
		pid := pe.ProcessID
		skip := false
		for _, s := range skipPIDs {
			if s != 0 && pid == s {
				skip = true
				break
			}
		}
		if !skip && exeName == targetBase {
			matched = append(matched, pid)
		}
		if err := windows.Process32Next(snap, &pe); err != nil {
			break
		}
	}
	return matched
}

// stopAllTrayProcesses signals any running -tray instances to exit and ensures file locks
// on targetExePath are released prior to binary swap or MSI upgrade.
func stopAllTrayProcesses(targetExePath string) {
	// 1. Signal named shutdown events (for graceful icon removal) and broadcast msg.
	for _, evName := range []string{trayShutdownLocalEvent, trayShutdownEventName} {
		evPtr, _ := windows.UTF16PtrFromString(evName)
		if hEv, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, evPtr); err == nil && hEv != 0 {
			_ = windows.SetEvent(hEv)
			_ = windows.CloseHandle(hEv)
		}
	}
	msgPtr, _ := windows.UTF16PtrFromString(trayShutdownMsgName)
	if msgID, _, _ := procRegisterWindowMsgW.Call(uintptr(unsafe.Pointer(msgPtr))); msgID != 0 {
		_, _, _ = procPostMessageW.Call(0xFFFF, msgID, 0, 0) // HWND_BROADCAST
	}

	selfPID := uint32(os.Getpid())
	parentPID := uint32(os.Getppid())
	svcPID := queryPrintMindServicePID()
	targetBase := strings.ToLower(filepath.Base(targetExePath))
	if targetBase == "" {
		targetBase = "printmind-agent.exe"
	}

	// Give -tray (which polls status.json every 500ms) up to 2s to exit gracefully.
	deadline := time.Now().Add(2 * time.Second)
	var remaining []uint32
	for {
		remaining = findMatchingTrayPIDs(targetBase, selfPID, parentPID, svcPID)
		if len(remaining) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	// 2. Terminate only stubborn sibling tray processes left after grace period.
	for _, pid := range remaining {
		hProc, oErr := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if oErr == nil {
			var exitCode uint32 = 1
			_ = windows.TerminateProcess(hProc, exitCode)
			_, _ = windows.WaitForSingleObject(hProc, 3000)
			_ = windows.CloseHandle(hProc)
		}
	}
}

// relaunchTrayInActiveSession starts `"<exePath>" -tray` inside the currently active Windows
// desktop user session when called from Session 0 (LocalSystem service or helper subprocess).
func relaunchTrayInActiveSession(exePath string) {
	if strings.TrimSpace(exePath) == "" {
		return
	}
	if !shouldUseCrossSessionTrayLaunch(isRunningAsService(), currentProcessSessionID()) {
		cmd := exec.Command(exePath, "-tray")
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
		if err := cmd.Start(); err != nil {
			slog.Warn("failed launching tray in interactive session", "error", err)
		}
		return
	}

	sessRaw, _, _ := procWTSGetActiveConsoleSessID.Call()
	sessID := uint32(sessRaw)
	if sessID == 0xFFFFFFFF {
		slog.Debug("no active console user session; skipping tray relaunch")
		return
	}

	var userTok windows.Token
	r1, _, callErr := procWTSQueryUserToken.Call(uintptr(sessID), uintptr(unsafe.Pointer(&userTok)))
	if r1 == 0 || userTok == 0 {
		slog.Debug("WTSQueryUserToken returned no active desktop user token", "session", sessID, "error", callErr)
		return
	}
	defer userTok.Close()

	var primaryTok windows.Token
	if err := windows.DuplicateTokenEx(
		userTok,
		windows.MAXIMUM_ALLOWED,
		nil,
		windows.SecurityIdentification,
		windows.TokenPrimary,
		&primaryTok,
	); err != nil {
		slog.Warn("DuplicateTokenEx failed for tray relaunch", "error", err)
		return
	}
	defer primaryTok.Close()

	var envBlock uintptr
	rEnv, _, _ := procCreateEnvironmentBlock.Call(uintptr(unsafe.Pointer(&envBlock)), uintptr(primaryTok), 0)
	if rEnv != 0 && envBlock != 0 {
		defer procDestroyEnvironmentBlk.Call(envBlock)
	}

	cmdLine, _ := windows.UTF16PtrFromString(`"` + exePath + `" -tray`)
	desktopPtr, _ := windows.UTF16PtrFromString(`winsta0\default`)
	workDirPtr, _ := windows.UTF16PtrFromString(filepath.Dir(exePath))

	var si windows.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Desktop = desktopPtr

	var pi windows.ProcessInformation
	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.DETACHED_PROCESS)
	if envBlock == 0 {
		flags = uint32(windows.DETACHED_PROCESS)
	}
	rProc, _, pErr := procCreateProcessAsUserW.Call(
		uintptr(primaryTok),
		0,
		uintptr(unsafe.Pointer(cmdLine)),
		0,
		0,
		0,
		uintptr(flags),
		envBlock,
		uintptr(unsafe.Pointer(workDirPtr)),
		uintptr(unsafe.Pointer(&si)),
		uintptr(unsafe.Pointer(&pi)),
	)
	if rProc == 0 {
		slog.Warn("CreateProcessAsUserW failed launching tray", "error", pErr)
		return
	}
	_ = windows.CloseHandle(pi.Thread)
	_ = windows.CloseHandle(pi.Process)
}

func downloadAndVerifyHash(ctx context.Context, client *http.Client, url, expectedHash, dstPath string) error {
	expectedHash = strings.ToLower(strings.TrimSpace(expectedHash))
	if len(expectedHash) != 64 {
		return fmt.Errorf("invalid SHA-256 digest length %d", len(expectedHash))
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 150*1024*1024))
	_ = f.Close()
	if copyErr != nil {
		_ = os.Remove(dstPath)
		return copyErr
	}

	actualHash := hex.EncodeToString(h.Sum(nil))
	if actualHash != expectedHash {
		_ = os.Remove(dstPath)
		return fmt.Errorf("SHA-256 mismatch: expected %s, got %s", expectedHash, actualHash)
	}
	return nil
}

func checkAndApplyUpdate(ctx context.Context, client *http.Client, cfg Config, paths OSPaths, rc RemoteConfig) {
	if rc.UpdateVersion != "" && rc.UpdateVersion == AgentVersion {
		return
	}
	if isAutoUpdateDisabled(cfg) {
		slog.Info("skipping Windows auto-update because AUTOUPDATE=0 is configured")
		return
	}

	if strings.TrimSpace(rc.UpdateExeURL) != "" && strings.TrimSpace(rc.UpdateExeSHA256) != "" {
		if applied := checkAndApplyExeUpdate(ctx, client, paths, rc); applied {
			return
		}
	}
	checkAndApplyMSIUpdate(ctx, client, paths, rc)
}

func restorePreUpdateStatus(statusPath string, prevSnap AgentStatusSnapshot, prevErr error) {
	if prevErr == nil && prevSnap.State != "" && prevSnap.State != "updating" {
		_ = WriteStatusSnapshot(statusPath, prevSnap)
		return
	}
	_ = WriteStatusSnapshot(statusPath, AgentStatusSnapshot{
		AgentVersion: AgentVersion,
		State:        "online",
	})
}

func checkAndApplyExeUpdate(ctx context.Context, client *http.Client, paths OSPaths, rc RemoteConfig) bool {
	updateDir := paths.UpdatesDir
	if updateDir == "" {
		updateDir = paths.StagingDir
	}
	stagedExe := filepath.Join(updateDir, "printmind-agent.update.exe")
	if err := downloadAndVerifyHash(ctx, client, strings.TrimSpace(rc.UpdateExeURL), rc.UpdateExeSHA256, stagedExe); err != nil {
		slog.Warn("EXE update verification failed; aborting", "error", err)
		return false
	}

	// Verify PE MZ magic bytes before replacing binary on disk.
	rf, err := os.Open(stagedExe)
	if err != nil {
		_ = os.Remove(stagedExe)
		return false
	}
	var magic [2]byte
	_, rErr := io.ReadFull(rf, magic[:])
	_ = rf.Close()
	if rErr != nil || !bytes.Equal(magic[:], []byte{'M', 'Z'}) {
		_ = os.Remove(stagedExe)
		slog.Error("downloaded EXE update is not a valid Windows PE binary")
		return false
	}

	exePath, err := os.Executable()
	if err != nil || exePath == "" {
		_ = os.Remove(stagedExe)
		return false
	}

	prevSnap, prevErr := ReadStatusSnapshot(paths.StatusPath)
	_ = WriteStatusSnapshot(paths.StatusPath, AgentStatusSnapshot{
		AgentVersion: AgentVersion,
		State:        "updating",
	})
	stopAllTrayProcesses(exePath)

	oldExe := exePath + ".old"
	_ = os.Remove(oldExe)
	if err := os.Rename(exePath, oldExe); err != nil {
		slog.Error("failed renaming active EXE to .old", "error", err)
		_ = os.Remove(stagedExe)
		restorePreUpdateStatus(paths.StatusPath, prevSnap, prevErr)
		relaunchTrayInActiveSession(exePath)
		return false
	}

	// Copy from updates dir onto target directory first so rename stays same-volume atomic.
	newSibling := exePath + ".new"
	if err := copyFileAtomicWindows(stagedExe, newSibling); err != nil {
		_ = os.Rename(oldExe, exePath)
		_ = os.Remove(stagedExe)
		slog.Error("failed staging new EXE in install dir; rolled back", "error", err)
		restorePreUpdateStatus(paths.StatusPath, prevSnap, prevErr)
		relaunchTrayInActiveSession(exePath)
		return false
	}
	_ = os.Remove(stagedExe)

	if err := os.Rename(newSibling, exePath); err != nil {
		_ = os.Remove(newSibling)
		_ = os.Rename(oldExe, exePath)
		slog.Error("failed promoting new EXE; rolled back to .old", "error", err)
		restorePreUpdateStatus(paths.StatusPath, prevSnap, prevErr)
		relaunchTrayInActiveSession(exePath)
		return false
	}

	slog.Info("Windows EXE replaced via .old atomic swap; triggering clean service restart", "version", rc.UpdateVersion)
	if isRunningAsService() {
		cmd := exec.Command(exePath, "restart-service")
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP}
		if err := cmd.Start(); err != nil {
			slog.Warn("could not launch restart-service helper; exiting for SCM recovery", "error", err)
			os.Exit(1)
		}
		return true
	}

	relaunchTrayInActiveSession(exePath)
	return true
}

func checkAndApplyMSIUpdate(ctx context.Context, client *http.Client, paths OSPaths, rc RemoteConfig) {
	msiURL := strings.TrimSpace(rc.UpdateMSIURL)
	expectedHash := strings.ToLower(strings.TrimSpace(rc.UpdateSHA256))
	if msiURL == "" || expectedHash == "" {
		return
	}

	updateDir := paths.UpdatesDir
	if updateDir == "" {
		updateDir = paths.StagingDir
	}
	msiPath := filepath.Join(updateDir, "PrintMindAgent-update.msi")
	if err := downloadAndVerifyHash(ctx, client, msiURL, expectedHash, msiPath); err != nil {
		slog.Error("MSI update download/SHA-256 verification failed; aborting", "error", err)
		return
	}

	if err := verifyAuthenticodeSignature(msiPath); err != nil {
		_ = os.Remove(msiPath)
		slog.Error("MSI Authenticode signature verification failed; aborting update", "error", err)
		return
	}

	prevSnap, prevErr := ReadStatusSnapshot(paths.StatusPath)
	_ = WriteStatusSnapshot(paths.StatusPath, AgentStatusSnapshot{
		AgentVersion: AgentVersion,
		State:        "updating",
	})
	exePath, exeErr := os.Executable()
	if exeErr == nil {
		stopAllTrayProcesses(exePath)
	}

	slog.Info("launching verified MSI upgrade", "version", rc.UpdateVersion, "path", msiPath)
	cmd := exec.Command("msiexec.exe", "/i", msiPath, "/qn", "/norestart")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Start(); err != nil {
		slog.Error("failed to start msiexec.exe", "error", err)
		restorePreUpdateStatus(paths.StatusPath, prevSnap, prevErr)
		if exeErr == nil {
			relaunchTrayInActiveSession(exePath)
		}
	}
}
