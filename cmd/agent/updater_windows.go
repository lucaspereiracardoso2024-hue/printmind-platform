//go:build windows

package main

import (
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
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	wintrustDLL        = windows.NewLazySystemDLL("wintrust.dll")
	procWinVerifyTrust = wintrustDLL.NewProc("WinVerifyTrust")
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

// verifyAuthenticodeSignature validates the MSI Authenticode signature via Windows WinVerifyTrust.
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
	// Always close WinVerifyTrust state handle
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

func checkAndApplyUpdate(ctx context.Context, client *http.Client, cfg Config, paths OSPaths, rc RemoteConfig) {
	checkAndApplyMSIUpdate(ctx, client, cfg, paths, rc)
}

func checkAndApplyMSIUpdate(ctx context.Context, client *http.Client, cfg Config, paths OSPaths, rc RemoteConfig) {
	msiURL := strings.TrimSpace(rc.UpdateMSIURL)
	expectedHash := strings.ToLower(strings.TrimSpace(rc.UpdateSHA256))
	if msiURL == "" || expectedHash == "" {
		return
	}
	if rc.UpdateVersion != "" && rc.UpdateVersion == AgentVersion {
		return
	}
	if isAutoUpdateDisabled(cfg) {
		slog.Info("skipping MSI auto-update because AUTOUPDATE=0 is configured")
		return
	}

	_ = os.MkdirAll(paths.StagingDir, 0755)
	msiPath := filepath.Join(paths.StagingDir, "PrintMindAgent-update.msi")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, msiURL, nil)
	if err != nil {
		slog.Warn("invalid MSI update URL", "error", err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("failed to download MSI update", "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("MSI update HTTP error", "status", resp.StatusCode)
		return
	}

	f, err := os.OpenFile(msiPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		slog.Warn("failed to open MSI staging file", "error", err)
		return
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 150*1024*1024))
	_ = f.Close()
	if copyErr != nil {
		_ = os.Remove(msiPath)
		slog.Warn("failed writing MSI update", "error", copyErr)
		return
	}

	actualHash := hex.EncodeToString(h.Sum(nil))
	if actualHash != expectedHash {
		_ = os.Remove(msiPath)
		slog.Error("MSI SHA-256 mismatch; aborting update", "expected", expectedHash, "actual", actualHash)
		return
	}

	if err := verifyAuthenticodeSignature(msiPath); err != nil {
		_ = os.Remove(msiPath)
		slog.Error("MSI Authenticode signature verification failed; aborting update", "error", err)
		return
	}

	slog.Info("launching verified MSI upgrade", "version", rc.UpdateVersion, "path", msiPath)
	cmd := exec.Command("msiexec.exe", "/i", msiPath, "/qn", "/norestart")
	if err := cmd.Start(); err != nil {
		slog.Error("failed to start msiexec.exe", "error", err)
	}
}
