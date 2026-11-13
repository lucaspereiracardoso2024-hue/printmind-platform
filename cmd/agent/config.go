package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var exeTokenRegex = regexp.MustCompile(`pm_live_[A-Za-z0-9]+`)

// installerPairingTokenReader is a test-overridable hook for reading the MSI installer registry token.
var installerPairingTokenReader = readInstallerPairingToken

// extractTokenFromExeName extracts an embedded pm_live_* pairing token from the executable name.
func extractTokenFromExeName(exePath string) string {
	base := filepath.Base(exePath)
	return exeTokenRegex.FindString(base)
}

func isDirWritable(dir string) bool {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return false
	}
	testFile := filepath.Join(dir, ".pm_write_probe")
	f, err := os.OpenFile(testFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return false
	}
	_ = f.Close()
	_ = os.Remove(testFile)
	return true
}

func defaultProductionDataDir() string {
	if runtime.GOOS == "windows" {
		progData := strings.TrimSpace(os.Getenv("ProgramData"))
		if progData == "" {
			progData = `C:\ProgramData`
		}
		return filepath.Join(progData, "PrintMind")
	}
	return "/var/lib/printmind"
}

// ResolvePaths computes OS-appropriate paths for config, SQLite telemetry DB, logs, and staging.
// If explicitConfig is non-empty, its directory is used for sibling state files.
// If the production OS directory is not writable (e.g. unprivileged tests), it falls back to exeDir.
func ResolvePaths(explicitConfig string, isService bool) OSPaths {
	if explicitConfig != "" && explicitConfig != LegacyConfigName && explicitConfig != DefaultConfigName {
		absCfg, err := filepath.Abs(explicitConfig)
		if err != nil {
			absCfg = explicitConfig
		}
		dir := filepath.Dir(absCfg)
		return OSPaths{
			DataDir:    dir,
			ConfigPath: absCfg,
			DBPath:     filepath.Join(dir, DefaultDBName),
			LogPath:    filepath.Join(dir, "logs", "agent.log"),
			StagingDir: filepath.Join(dir, "staging"),
		}
	}

	prodDir := defaultProductionDataDir()
	dataDir := prodDir
	if !isService && !isDirWritable(prodDir) {
		exeDir := "."
		if exePath, err := os.Executable(); err == nil {
			exeDir = filepath.Dir(exePath)
		}
		if isDirWritable(exeDir) {
			dataDir = exeDir
		} else if cwd, err := os.Getwd(); err == nil {
			dataDir = cwd
		}
	} else {
		_ = os.MkdirAll(dataDir, 0755)
	}

	_ = os.MkdirAll(filepath.Join(dataDir, "logs"), 0755)
	_ = os.MkdirAll(filepath.Join(dataDir, "staging"), 0755)

	cfgName := DefaultConfigName
	if explicitConfig != "" {
		cfgName = filepath.Base(explicitConfig)
	}

	return OSPaths{
		DataDir:    dataDir,
		ConfigPath: filepath.Join(dataDir, cfgName),
		DBPath:     filepath.Join(dataDir, DefaultDBName),
		LogPath:    filepath.Join(dataDir, "logs", "agent.log"),
		StagingDir: filepath.Join(dataDir, "staging"),
	}
}

// migrateLegacyConfig copies legacy printmind-config.json from CWD or executable dir to targetPath if needed.
func migrateLegacyConfig(targetPath string) {
	if _, err := os.Stat(targetPath); err == nil {
		return
	}
	var candidates []string
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, LegacyConfigName))
	}
	if exePath, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exePath), LegacyConfigName))
	}
	for _, src := range candidates {
		if filepath.Clean(src) == filepath.Clean(targetPath) {
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil || len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		_ = os.MkdirAll(filepath.Dir(targetPath), 0755)
		if err := os.WriteFile(targetPath, data, 0644); err == nil {
			return
		}
	}
}

func saveConfig(path string, cfg Config) {
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	if out, err := json.MarshalIndent(cfg, "", "  "); err == nil {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, out, 0644); err == nil {
			_ = os.Rename(tmp, path)
		}
	}
}

func loadOrInitConfig(path, flagServer, flagToken, flagSubnets string) Config {
	migrateLegacyConfig(path)

	cfg := Config{
		ServerURL:       DefaultSupabaseRP,
		APIKey:          DefaultAnonAPIKey,
		PairingToken:    "",
		PollIntervalSec: 300,
		EnableUSB:       true,
		SNMPCommunities: []string{"public"},
	}

	if data, err := os.ReadFile(path); err == nil {
		var loaded Config
		if json.Unmarshal(data, &loaded) == nil {
			if strings.TrimSpace(loaded.ServerURL) != "" && !strings.Contains(loaded.ServerURL, "localhost:8090") {
				cfg.ServerURL = strings.TrimSpace(loaded.ServerURL)
			}
			if strings.TrimSpace(loaded.APIKey) != "" {
				cfg.APIKey = strings.TrimSpace(loaded.APIKey)
			}
			tok := strings.TrimSpace(loaded.PairingToken)
			if tok != "" && tok != "pm_live_hosp_central_01" {
				cfg.PairingToken = tok
			}
			if loaded.PollIntervalSec >= 30 {
				cfg.PollIntervalSec = loaded.PollIntervalSec
			}
			cfg.EnableUSB = loaded.EnableUSB
			cfg.DisableAutoUpdate = loaded.DisableAutoUpdate
			if len(loaded.Subnets) > 0 {
				cfg.Subnets = loaded.Subnets
			}
			if len(loaded.StaticIPs) > 0 {
				cfg.StaticIPs = loaded.StaticIPs
			}
			if len(loaded.SNMPCommunities) > 0 {
				cfg.SNMPCommunities = loaded.SNMPCommunities
			}
			if loaded.SNMPv3.Enabled || loaded.SNMPv3.Username != "" {
				cfg.SNMPv3 = loaded.SNMPv3
			}
		}
	}

	if exePath, err := os.Executable(); err == nil {
		if exeTok := extractTokenFromExeName(exePath); exeTok != "" {
			cfg.PairingToken = exeTok
		}
	}
	if cfg.PairingToken == "" && len(os.Args) > 0 {
		if argTok := extractTokenFromExeName(os.Args[0]); argTok != "" {
			cfg.PairingToken = argTok
		}
	}
	if cfg.PairingToken == "" {
		if regTok := strings.TrimSpace(installerPairingTokenReader()); regTok != "" {
			cfg.PairingToken = regTok
		}
	}

	if strings.TrimSpace(flagServer) != "" {
		cfg.ServerURL = strings.TrimSpace(flagServer)
	}
	if strings.TrimSpace(flagToken) != "" {
		cfg.PairingToken = strings.TrimSpace(flagToken)
	}
	if strings.TrimSpace(flagSubnets) != "" {
		var custom []string
		for _, s := range strings.Split(flagSubnets, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				custom = append(custom, s)
			}
		}
		if len(custom) > 0 {
			cfg.Subnets = custom
		}
	}

	saveConfig(path, cfg)
	return cfg
}

func resolveTokenByEmail(client *http.Client, serverURL, apiKey, email string) (string, error) {
	baseRPC := strings.TrimRight(serverURL, "/")
	if idx := strings.Index(baseRPC, "/rest/v1/rpc/"); idx != -1 {
		baseRPC = baseRPC[:idx] + "/rest/v1/rpc/resolve_unit_token_by_email"
	} else {
		baseRPC = "https://zbsmzcbcivhzslcnfxmq.supabase.co/rest/v1/rpc/resolve_unit_token_by_email"
	}

	payloadBytes, _ := json.Marshal(map[string]string{"p_email": strings.TrimSpace(email)})
	req, err := http.NewRequest(http.MethodPost, baseRPC, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("apikey", apiKey)
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Status       string `json:"status"`
		Message      string `json:"message"`
		PairingToken string `json:"pairing_token"`
		UnitName     string `json:"unit_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.Status != "success" || result.PairingToken == "" {
		if result.Message == "" {
			result.Message = "conta nao encontrada"
		}
		return "", fmt.Errorf("%s", result.Message)
	}
	return result.PairingToken, nil
}
