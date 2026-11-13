package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// stageAndVerifyBinaryUpdate downloads a binary update to stagingDir, verifies SHA-256
// (and optional ELF magic), then atomically replaces targetExePath via a sibling temp file.
func stageAndVerifyBinaryUpdate(ctx context.Context, client *http.Client, downloadURL, expectedSHA256, stagingDir, targetExePath string, requireELF bool) error {
	downloadURL = strings.TrimSpace(downloadURL)
	expectedHash := strings.ToLower(strings.TrimSpace(expectedSHA256))
	if downloadURL == "" || expectedHash == "" {
		return fmt.Errorf("missing update URL or SHA-256")
	}
	if len(expectedHash) != 64 {
		return fmt.Errorf("invalid SHA-256 length %d", len(expectedHash))
	}
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return fmt.Errorf("invalid update URL: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("update HTTP status %d", resp.StatusCode)
	}

	stagedPath := filepath.Join(stagingDir, "printmind-agent.staged")
	sf, err := os.OpenFile(stagedPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return fmt.Errorf("open staging file: %w", err)
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(sf, h), io.LimitReader(resp.Body, 150*1024*1024))
	_ = sf.Close()
	if copyErr != nil {
		_ = os.Remove(stagedPath)
		return fmt.Errorf("write staged binary: %w", copyErr)
	}
	defer func() { _ = os.Remove(stagedPath) }()

	actualHash := hex.EncodeToString(h.Sum(nil))
	if actualHash != expectedHash {
		return fmt.Errorf("SHA-256 mismatch: expected %s, got %s", expectedHash, actualHash)
	}

	if requireELF {
		rf, rErr := os.Open(stagedPath)
		if rErr != nil {
			return rErr
		}
		var magic [4]byte
		_, mErr := io.ReadFull(rf, magic[:])
		_ = rf.Close()
		if mErr != nil || !bytes.Equal(magic[:], []byte{0x7F, 'E', 'L', 'F'}) {
			return fmt.Errorf("staged binary is not a valid ELF executable")
		}
	}

	targetDir := filepath.Dir(targetExePath)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}
	siblingTmp := filepath.Join(targetDir, "."+filepath.Base(targetExePath)+".new")
	src, err := os.Open(stagedPath)
	if err != nil {
		return err
	}
	dst, err := os.OpenFile(siblingTmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		_ = src.Close()
		return fmt.Errorf("create sibling temp binary: %w", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = src.Close()
		_ = dst.Close()
		_ = os.Remove(siblingTmp)
		return fmt.Errorf("copy staged binary: %w", err)
	}
	_ = src.Close()
	_ = dst.Sync()
	if err := dst.Close(); err != nil {
		_ = os.Remove(siblingTmp)
		return err
	}
	_ = os.Chmod(siblingTmp, 0755)

	if err := os.Rename(siblingTmp, targetExePath); err != nil {
		_ = os.Remove(siblingTmp)
		return fmt.Errorf("atomic binary swap failed: %w", err)
	}
	return nil
}
