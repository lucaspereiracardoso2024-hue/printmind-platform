package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"
)

var secretMaskRegex = regexp.MustCompile(`(pm_live_[A-Za-z0-9]{4})[A-Za-z0-9]+`)

func redactSecrets(s string, cfg Config) string {
	out := secretMaskRegex.ReplaceAllString(s, "${1}****")
	if cfg.APIKey != "" && len(cfg.APIKey) > 8 {
		out = strings.ReplaceAll(out, cfg.APIKey, cfg.APIKey[:4]+"****"+cfg.APIKey[len(cfg.APIKey)-4:])
	}
	if cfg.PairingToken != "" && len(cfg.PairingToken) > 8 {
		out = strings.ReplaceAll(out, cfg.PairingToken, cfg.PairingToken[:8]+"****")
	}
	if cfg.SNMPv3.AuthPassphrase != "" {
		out = strings.ReplaceAll(out, cfg.SNMPv3.AuthPassphrase, "****")
	}
	if cfg.SNMPv3.PrivPassphrase != "" {
		out = strings.ReplaceAll(out, cfg.SNMPv3.PrivPassphrase, "****")
	}
	return out
}

// BuildDiagnosticZip constructs an in-memory redacted zip bundle of config, network interfaces, host info, and recent logs.
func BuildDiagnosticZip(cfg Config, paths OSPaths, store *SQLiteStore, wpp *WPPReadiness) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// 1. Sanitized config
	sanitizedCfg := cfg
	if len(sanitizedCfg.PairingToken) > 8 {
		sanitizedCfg.PairingToken = sanitizedCfg.PairingToken[:8] + "****"
	} else if sanitizedCfg.PairingToken != "" {
		sanitizedCfg.PairingToken = "****"
	}
	if len(sanitizedCfg.APIKey) > 8 {
		sanitizedCfg.APIKey = sanitizedCfg.APIKey[:4] + "****" + sanitizedCfg.APIKey[len(sanitizedCfg.APIKey)-4:]
	} else if sanitizedCfg.APIKey != "" {
		sanitizedCfg.APIKey = "****"
	}
	if sanitizedCfg.SNMPv3.AuthPassphrase != "" {
		sanitizedCfg.SNMPv3.AuthPassphrase = "****"
	}
	if sanitizedCfg.SNMPv3.PrivPassphrase != "" {
		sanitizedCfg.SNMPv3.PrivPassphrase = "****"
	}
	if cfgBytes, err := json.MarshalIndent(sanitizedCfg, "", "  "); err == nil {
		if w, err := zw.Create("sanitized_config.json"); err == nil {
			_, _ = w.Write(cfgBytes)
		}
	}

	// 2. Network interfaces
	type ifaceDump struct {
		Name  string   `json:"name"`
		Flags string   `json:"flags"`
		Addrs []string `json:"addrs"`
	}
	var ifaceList []ifaceDump
	if ifaces, err := net.Interfaces(); err == nil {
		for _, ifc := range ifaces {
			var addrStrs []string
			if addrs, aErr := ifc.Addrs(); aErr == nil {
				for _, a := range addrs {
					addrStrs = append(addrStrs, a.String())
				}
			}
			ifaceList = append(ifaceList, ifaceDump{
				Name:  ifc.Name,
				Flags: ifc.Flags.String(),
				Addrs: addrStrs,
			})
		}
	}
	if ifBytes, err := json.MarshalIndent(map[string]any{
		"detected_subnets": detectLocalSubnets(),
		"interfaces":       ifaceList,
	}, "", "  "); err == nil {
		if w, err := zw.Create("interfaces.json"); err == nil {
			_, _ = w.Write(ifBytes)
		}
	}

	// 3. Host & Queue info
	hostname, _ := os.Hostname()
	qCount := 0
	if store != nil {
		qCount, _ = store.QueueCount()
	}
	if hBytes, err := json.MarshalIndent(map[string]any{
		"agent_version": AgentVersion,
		"hostname":      hostname,
		"os":            runtime.GOOS + "/" + runtime.GOARCH,
		"data_dir":      paths.DataDir,
		"queue_depth":   qCount,
		"wpp_readiness": wpp,
		"generated_at":  time.Now().UTC().Format(time.RFC3339),
	}, "", "  "); err == nil {
		if w, err := zw.Create("host_info.json"); err == nil {
			_, _ = w.Write(hBytes)
		}
	}

	// 4. Tail of recent logs (up to 512 KB, redacted)
	if logData, err := os.ReadFile(paths.LogPath); err == nil && len(logData) > 0 {
		const maxTail = 512 * 1024
		if len(logData) > maxTail {
			logData = logData[len(logData)-maxTail:]
		}
		redacted := redactSecrets(string(logData), cfg)
		if w, err := zw.Create("recent_logs.jsonl"); err == nil {
			_, _ = w.Write([]byte(redacted))
		}
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func handleDiagnosticRequest(ctx context.Context, client *http.Client, cfg Config, paths OSPaths, store *SQLiteStore, wpp *WPPReadiness, uploadURL string) {
	zipBytes, err := BuildDiagnosticZip(cfg, paths, store, wpp)
	if err != nil {
		slog.Warn("failed to build diagnostic bundle", "error", err)
		return
	}
	if strings.TrimSpace(uploadURL) == "" {
		slog.Info("diagnostic bundle built in memory (no upload URL configured)", "bytes", len(zipBytes))
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(zipBytes))
	if err != nil {
		slog.Warn("failed to create diagnostic upload request", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/zip")
	req.Header.Set("X-PrintMind-Pairing-Token", cfg.PairingToken)
	req.Header.Set("User-Agent", fmt.Sprintf("PrintMind-Agent/%s", AgentVersion))

	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("diagnostic bundle upload failed", "error", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	slog.Info("diagnostic bundle uploaded", "status", resp.StatusCode, "bytes", len(zipBytes))
}
