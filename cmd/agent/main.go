//go:debug x509negativeserial=1

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

func sendTelemetry(ctx context.Context, client *http.Client, cfg Config, reqPayload SyncRequest) (*SyncResponse, error) {
	isSupabaseRPC := strings.Contains(cfg.ServerURL, "/rest/v1/rpc/")
	var bodyBytes []byte
	var err error
	endpoint := strings.TrimRight(cfg.ServerURL, "/")

	if isSupabaseRPC {
		bodyBytes, err = json.Marshal(SupabaseRPCPayload{
			PairingToken: cfg.PairingToken,
			Payload:      reqPayload,
		})
	} else {
		endpoint = endpoint + "/api/v2/telemetry/sync"
		bodyBytes, err = json.Marshal(reqPayload)
	}
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("PrintMind-Agent/%s (%s; %s)", AgentVersion, runtime.GOOS, runtime.GOARCH))
	if isSupabaseRPC && cfg.APIKey != "" {
		req.Header.Set("apikey", cfg.APIKey)
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respData, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respData)))
	}

	var syncResp SyncResponse
	_ = json.Unmarshal(respData, &syncResp)
	if syncResp.Status == "error" {
		return &syncResp, fmt.Errorf("servidor recusou sincronizacao: %s", syncResp.Message)
	}
	return &syncResp, nil
}

// probeNetworkDevice executes the multi-protocol collection cascade for a single IP.
func probeNetworkDevice(ctx context.Context, ip string, communities []string, snmpV3 SNMPv3Config, hint *LANDiscoveryHint, enableWakeup bool) (*DiscoveredDevice, bool) {
	var snmpDev *DiscoveredDevice
	var activeSNMP *snmpClient

	if snmpV3.Enabled && strings.TrimSpace(snmpV3.Username) != "" {
		if sess, err := newSNMPv3Session(snmpV3); err == nil {
			v3Cli := &snmpClient{ip: ip, v3: sess}
			if dev, ok := probeDeviceWithSNMPClient(ctx, v3Cli, enableWakeup); ok {
				snmpDev = dev
				activeSNMP = v3Cli
			}
		}
	}

	if snmpDev == nil {
		for _, comm := range communities {
			if ctx.Err() != nil {
				break
			}
			v2Cli := &snmpClient{ip: ip, community: comm}
			if dev, ok := probeDeviceWithSNMPClient(ctx, v2Cli, enableWakeup); ok {
				snmpDev = dev
				activeSNMP = v2Cli
				break
			}
		}
	}

	ippClient := newLocalHTTPClient(2500 * time.Millisecond)
	ippDev, hasIPP := probeDeviceIPP(ctx, ippClient, ip, hint)

	var bjnpDev *DiscoveredDevice
	if snmpDev == nil && !hasIPP && hint != nil && (hint.DiscoveryBy == "bjnp" || strings.EqualFold(hint.VendorHint, "Canon")) {
		if bDev, ok := probeCanonBJNPUnicast(ctx, ip, hint); ok {
			bjnpDev = bDev
		}
	}

	if snmpDev == nil && !hasIPP && bjnpDev == nil {
		return nil, false
	}

	var dev *DiscoveredDevice
	if snmpDev != nil {
		dev = snmpDev
		if hasIPP {
			if dev.DeviceUUID == "" {
				dev.DeviceUUID = ippDev.DeviceUUID
			}
			if ippDev.IsColorCapable {
				dev.IsColorCapable = true
			}
			if dev.Counters.LifetimeEngineTotal == 0 && ippDev.Counters.LifetimeEngineTotal > 0 {
				dev.Counters.LifetimeEngineTotal = ippDev.Counters.LifetimeEngineTotal
				if dev.OdometerSource == "" {
					dev.OdometerSource = "ipp"
				}
			}
			if len(dev.Supplies) == 0 && len(ippDev.Supplies) > 0 {
				dev.Supplies = ippDev.Supplies
			} else if len(ippDev.Supplies) > 0 {
				// Fill unknown (-2) SNMP supplies from IPP if IPP has exact values
				for i := range dev.Supplies {
					if dev.Supplies[i].Percent == nil {
						for _, is := range ippDev.Supplies {
							if is.Percent != nil && (is.Color == dev.Supplies[i].Color || strings.EqualFold(is.Name, dev.Supplies[i].Name)) {
								dev.Supplies[i].Percent = is.Percent
								dev.Supplies[i].Level = is.Level
								dev.Supplies[i].MaxCapacity = is.MaxCapacity
								dev.Supplies[i].LevelState = is.LevelState
								break
							}
						}
					}
				}
			}
		}
	} else if hasIPP {
		dev = ippDev
	} else {
		dev = bjnpDev
	}

	if hint != nil {
		if dev.MACAddress == "" && hint.MACAddress != "" {
			dev.MACAddress = hint.MACAddress
		}
		if dev.DeviceUUID == "" && hint.UUIDHint != "" {
			dev.DeviceUUID = hint.UUIDHint
		}
		if hint.ColorHint {
			dev.IsColorCapable = true
		}
	}

	// Brand-specific firmware-safe enrichment
	switch dev.Vendor {
	case "HP":
		if cdm, ok := collectHPCDMSequential(ctx, ip, true); ok {
			MergeIPPAndHPCDM(dev, cdm)
		}
		needsLEDM := len(dev.Supplies) == 0 || dev.Counters.LifetimeEngineTotal == 0 || dev.SerialNumber == ""
		if !needsLEDM {
			for _, s := range dev.Supplies {
				if s.Percent == nil && s.MeasurementMode != "state_only" || s.LevelState == "unknown" {
					needsLEDM = true
					break
				}
			}
		}
		if needsLEDM {
			if ledm, ok := collectHPLEDMSequential(ctx, ip, true); ok {
				MergeIPPAndHPLEDM(dev, ledm)
			}
		}
	case "Brother":
		enrichBrotherFromSNMP(ctx, dev, activeSNMP)
	case "Epson":
		enrichEpsonProtected(ctx, dev, activeSNMP)
	case "Canon":
		enrichCanonEWS(ctx, dev)
	case "Zebra":
		enrichZebraSGD9100(ctx, dev)
	}

	// Safe read-only PJL PAGECOUNT fallback only when vendor/language supports PJL and counters are still zero
	if dev.Counters.LifetimeEngineTotal == 0 {
		enrichPJL9100(ctx, dev)
	}

	return dev, true
}

func scanNetworkDevices(ctx context.Context, subnets []string, staticIPs []string, communities []string, snmpV3 SNMPv3Config, hints map[string]LANDiscoveryHint, rateLimitPPS int, enableWakeup bool) []DiscoveredDevice {
	ipSet := make(map[string]bool)
	for _, cidr := range subnets {
		for _, ip := range expandCIDRToIPs(cidr) {
			ipSet[ip] = true
		}
	}
	for _, ip := range staticIPs {
		if trimmed := strings.TrimSpace(ip); trimmed != "" {
			ipSet[trimmed] = true
		}
	}
	for ip := range hints {
		if strings.TrimSpace(ip) != "" {
			ipSet[ip] = true
		}
	}

	if len(communities) == 0 {
		communities = []string{"public"}
	}
	if rateLimitPPS <= 0 {
		rateLimitPPS = 25
	}
	ticker := time.NewTicker(time.Second / time.Duration(rateLimitPPS))
	defer ticker.Stop()

	var results []DiscoveredDevice
	var mu sync.Mutex
	var wg sync.WaitGroup

	for ip := range ipSet {
		select {
		case <-ctx.Done():
			wg.Wait()
			return results
		case <-ticker.C:
		}
		var hintPtr *LANDiscoveryHint
		if h, ok := hints[ip]; ok {
			hCopy := h
			hintPtr = &hCopy
		}
		wg.Add(1)
		go func(targetIP string, h *LANDiscoveryHint) {
			defer wg.Done()
			if dev, ok := probeNetworkDevice(ctx, targetIP, communities, snmpV3, h, enableWakeup); ok {
				mu.Lock()
				results = append(results, *dev)
				mu.Unlock()
			}
		}(ip, hintPtr)
	}
	wg.Wait()
	return results
}

func extractSubcommandAndFlags(args []string) (subcmd string, flagArgs []string) {
	known := map[string]bool{
		"install":         true,
		"uninstall":       true,
		"start":           true,
		"stop":            true,
		"status":          true,
		"run-console":     true,
		"restart-service": true,
	}
	for _, a := range args {
		if subcmd == "" && !strings.HasPrefix(a, "-") && known[strings.ToLower(a)] {
			subcmd = strings.ToLower(a)
			continue
		}
		flagArgs = append(flagArgs, a)
	}
	return subcmd, flagArgs
}

func mergeUniqueStrings(slices ...[]string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, sl := range slices {
		for _, item := range sl {
			item = strings.TrimSpace(item)
			if item != "" && !seen[item] {
				seen[item] = true
				out = append(out, item)
			}
		}
	}
	return out
}

func main() {
	subcmd, flagArgs := extractSubcommandAndFlags(os.Args[1:])

	fs := flag.NewFlagSet("printmind-agent", flag.ExitOnError)
	serverURL := fs.String("server", "", "URL do Endpoint PrintMind / Supabase RPC")
	token := fs.String("token", "", "Token de Pareamento da Unidade/Filial")
	emailFlag := fs.String("email", "", "E-mail cadastrado no PrintMind (pareamento automatico)")
	subnetsFlag := fs.String("subnets", "", "Sub-redes locais ou VLANs remotas separadas por virgula")
	configPathFlag := fs.String("config", "", "Caminho do arquivo de configuracao persistente")
	once := fs.Bool("once", false, "Executa apenas 1 ciclo de varredura e encerra")
	usbFlag := fs.Bool("usb", true, "Habilita coleta local USB/Spooler")
	trayFlag := fs.Bool("tray", false, "Inicia icone na bandeja do Windows (sessao do usuario)")
	installGUIFlag := fs.Bool("install-gui", false, "Executa instalacao grafica sem console")
	_ = fs.Parse(flagArgs)

	isSvc := isRunningAsService() && subcmd != "run-console" && !*once && !*trayFlag && !*installGUIFlag
	if *trayFlag {
		trayPaths := resolveTrayReadOnlyPaths(*configPathFlag)
		os.Exit(runTrayMode(Config{}, trayPaths))
	}

	if !isSvc && !*once && shouldRunWindowsBootstrapGUI(subcmd, *installGUIFlag, Config{}) {
		bootCfg := Config{
			ServerURL:    strings.TrimSpace(*serverURL),
			PairingToken: strings.TrimSpace(*token),
		}
		os.Exit(runWindowsBootstrapGUI(bootCfg, OSPaths{}))
	}

	paths := ResolvePaths(*configPathFlag, isSvc)

	sink := openPlatformEventSink(isSvc)
	if sink != nil {
		defer sink.Close()
	}
	alsoConsole := !isSvc && (runtime.GOOS != "windows" || subcmd == "run-console" || subcmd == "status" || *once)
	_, logCloser := SetupLogger(paths.LogPath, alsoConsole, sink)
	if logCloser != nil {
		defer logCloser.Close()
	}

	httpTransport := &http.Transport{
		MaxIdleConns:        32,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	httpClient := &http.Client{
		Timeout:   25 * time.Second,
		Transport: httpTransport,
	}

	cfg := loadOrInitConfig(paths.ConfigPath, *serverURL, *token, *subnetsFlag)
	if !*usbFlag {
		cfg.EnableUSB = false
		saveConfig(paths.ConfigPath, cfg)
	}

	if cfg.PairingToken == "" && strings.TrimSpace(*emailFlag) != "" {
		if resolved, err := resolveTokenByEmail(httpClient, cfg.ServerURL, cfg.APIKey, *emailFlag); err == nil {
			cfg.PairingToken = resolved
			saveConfig(paths.ConfigPath, cfg)
		} else {
			slog.Warn("failed to resolve pairing token by email", "email", *emailFlag, "error", err)
		}
	}

	if subcmd != "" {
		handled, err := handleServiceSubcommand(subcmd, cfg, paths)
		if handled {
			if err != nil {
				slog.Error("service command failed", "command", subcmd, "error", err)
				os.Exit(1)
			}
			return
		}
	}

	if cfg.PairingToken == "" {
		if isSvc || (runtime.GOOS == "windows" && subcmd != "run-console" && !*once) {
			slog.Error("pairing token missing", "config", paths.ConfigPath)
			notifyGUIErrorIfInteractive("Token de pareamento não encontrado. Execute o instalador com o token pm_live_... no nome do arquivo ou informe na tela inicial.")
			os.Exit(1)
		}
		fmt.Println("================================================================")
		fmt.Println("  PrintMind Scanner v" + AgentVersion + " - Vinculacao Inicial da Conta")
		fmt.Println("================================================================")
		fmt.Print("Digite o E-MAIL da sua conta PrintMind (ou seu Token pm_live_...): ")
		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)
		if strings.Contains(input, "@") {
			resolved, err := resolveTokenByEmail(httpClient, cfg.ServerURL, cfg.APIKey, input)
			if err != nil {
				slog.Error("account pairing failed", "error", err)
				os.Exit(1)
			}
			cfg.PairingToken = resolved
			saveConfig(paths.ConfigPath, cfg)
			slog.Info("account paired successfully", "config", paths.ConfigPath)
		} else if input != "" {
			cfg.PairingToken = input
			saveConfig(paths.ConfigPath, cfg)
		} else {
			slog.Error("pairing token or email required")
			os.Exit(1)
		}
	}

	runLoop := func(rootCtx context.Context) {
		onPlatformServiceStartup(paths)

		store, dbErr := OpenSQLiteStore(paths.DBPath)
		if dbErr != nil {
			slog.Warn("failed to open sqlite telemetry store; running without offline buffer", "path", paths.DBPath, "error", dbErr)
		} else {
			defer store.Close()
			if cwd, cErr := os.Getwd(); cErr == nil {
				_ = store.MigrateLegacyBuffer(filepath.Join(cwd, "agent_buffer.json"))
			}
			_ = store.MigrateLegacyBuffer(filepath.Join(paths.DataDir, "agent_buffer.json"))
		}

		hostname, _ := os.Hostname()
		pollSec := cfg.PollIntervalSec
		if pollSec < 30 {
			pollSec = 300
		}
		rateLimit := 25
		enableWakeup := true
		var remoteSubnets []string
		var remoteStaticIPs []string
		var remoteCommunities []string
		activeSNMPv3 := cfg.SNMPv3

		var statusMu sync.Mutex
		statusSnap := AgentStatusSnapshot{
			AgentVersion: AgentVersion,
			State:        "starting",
			DashboardURL: "https://lucaspereiracardoso2024-hue.github.io/printmind-platform/",
			UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
		}
		forceScanCh := make(chan struct{}, 1)

		persistStatus := func(mutator func(*AgentStatusSnapshot)) {
			statusMu.Lock()
			mutator(&statusSnap)
			statusSnap.AgentVersion = AgentVersion
			statusSnap.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			copySnap := statusSnap
			statusMu.Unlock()
			if wErr := WriteStatusSnapshot(paths.StatusPath, copySnap); wErr != nil {
				slog.Debug("failed writing status.json", "error", wErr)
			}
		}

		if !*once {
			ipcAddr, ipcTok, _, ipcErr := StartIPCServer(
				rootCtx,
				func() bool {
					select {
					case forceScanCh <- struct{}{}:
						return false
					default:
						return true
					}
				},
				func() AgentStatusSnapshot {
					statusMu.Lock()
					defer statusMu.Unlock()
					return statusSnap
				},
			)
			if ipcErr != nil {
				slog.Warn("failed starting loopback IPC server", "error", ipcErr)
			} else {
				persistStatus(func(s *AgentStatusSnapshot) {
					s.IPCAddr = ipcAddr
					s.IPCToken = ipcTok
				})
				slog.Info("loopback IPC server listening", "addr", ipcAddr)
			}
		}

		slog.Info("PrintMind Next-Gen Agent started",
			"version", AgentVersion,
			"hostname", hostname,
			"os", runtime.GOOS+"/"+runtime.GOARCH,
			"service_mode", isSvc,
			"data_dir", paths.DataDir,
		)

		for {
			if rootCtx.Err() != nil {
				return
			}

			persistStatus(func(s *AgentStatusSnapshot) {
				s.State = "scanning"
			})

			cycleTimeout := time.Duration(pollSec) * 2 * time.Second
			if cycleTimeout < 60*time.Second {
				cycleTimeout = 60 * time.Second
			}
			cycleCtx, cycleCancel := context.WithTimeout(rootCtx, cycleTimeout)

			localSubnets := detectLocalSubnets()
			scanSubnets := mergeUniqueStrings(cfg.Subnets, remoteSubnets, localSubnets)
			communities := mergeUniqueStrings(cfg.SNMPCommunities, remoteCommunities)
			if len(communities) == 0 {
				communities = []string{"public"}
			}

			// 1. Local spooler + USB IEEE 1284 + WSD Registry IP resolution
			spooler := discoverLocalSpooler(cfg.EnableUSB)
			var wsdIPs []string
			for portName, ip := range spooler.WSDResolvedIPs {
				wsdIPs = append(wsdIPs, ip)
				slog.Info("resolved WSD printer port to IPv4", "port", portName, "ip", ip)
			}

			// 2. Passive LAN Discovery (mDNS, WS-Discovery, Epson ENPC query-only)
			lanHints := discoverLANHints(cycleCtx)
			allStaticIPs := mergeUniqueStrings(cfg.StaticIPs, remoteStaticIPs, wsdIPs)

			// 3. Multi-protocol network scan
			netDevices := scanNetworkDevices(cycleCtx, scanSubnets, allStaticIPs, communities, activeSNMPv3, lanHints, rateLimit, enableWakeup)
			devices := append(netDevices, spooler.USBDevices...)

			if store != nil {
				store.RecordSupplyHistory(devices)
			}

			for _, dev := range devices {
				for _, sup := range dev.Supplies {
					if sup.Percent != nil {
						if sup.SupplyClass == "waste" && *sup.Percent >= 85 {
							slog.Warn("waste receptacle nearing capacity", "model", dev.Model, "ip", dev.IPAddress, "supply", sup.Name, "percent", *sup.Percent)
						} else if sup.SupplyClass != "waste" && *sup.Percent <= 15 {
							slog.Warn("supply level critical", "model", dev.Model, "ip", dev.IPAddress, "supply", sup.Name, "percent", *sup.Percent)
						}
					}
				}
			}

			// 4. Audited Print Jobs + Physical Color Gate + WPP Readiness Sensor
			jobs, summaries, wpp := collectPlatformPrintJobs(store, devices, spooler, hostname)
			qDepth := 0
			if store != nil {
				qDepth, _ = store.QueueCount()
			}
			hostInfo := map[string]any{
				"hostname":         hostname,
				"os":               runtime.GOOS + "/" + runtime.GOARCH,
				"domain":           ResolveHostDomain(),
				"service_mode":     isSvc,
				"data_dir":         paths.DataDir,
				"queue_depth":      qDepth,
				"wpp_enabled":      wpp != nil && wpp.WPPEnabled,
				"wpp_policy_state": uint32(0),
			}
			if wpp != nil {
				hostInfo["wpp_policy_state"] = wpp.WPPPolicyState
			}

			reqPayload := SyncRequest{
				AgentVersion: AgentVersion,
				PairingToken: cfg.PairingToken,
				CollectedAt:  time.Now().UTC().Format(time.RFC3339),
				HostInfo:     hostInfo,
				Devices:      devices,
				JobsSummary:  summaries,
				PrintJobs:    jobs,
				WPPReadiness: wpp,
			}

			syncResp, err := sendTelemetry(cycleCtx, httpClient, cfg, reqPayload)
			if err != nil {
				httpTransport.CloseIdleConnections()
				if store != nil {
					if qErr := store.EnqueueTelemetry(reqPayload); qErr != nil {
						slog.Error("failed to persist telemetry to sqlite queue", "error", qErr)
					}
					qDepth, _ = store.QueueCount()
				}
				persistStatus(func(s *AgentStatusSnapshot) {
					s.State = "retrying"
					s.LastScanAt = time.Now().UTC().Format(time.RFC3339)
					s.PrintersCount = len(devices)
					s.PendingQueue = qDepth
					s.LastError = err.Error()
				})
				slog.Warn("cloud sync failed; telemetry queued offline", "error", err, "devices", len(devices))
			} else {
				if store != nil {
					drained, _ := store.DrainQueue(cycleCtx, time.Now().Unix(), func(queued SyncRequest) error {
						queued.PairingToken = cfg.PairingToken
						_, dErr := sendTelemetry(cycleCtx, httpClient, cfg, queued)
						return dErr
					})
					if drained > 0 {
						slog.Info("drained offline telemetry queue", "snapshots", drained)
					}
					qDepth, _ = store.QueueCount()
				}
				nowIso := time.Now().UTC().Format(time.RFC3339)
				persistStatus(func(s *AgentStatusSnapshot) {
					s.State = "online"
					s.LastScanAt = nowIso
					s.LastSyncAt = nowIso
					s.PrintersCount = len(devices)
					s.PendingQueue = qDepth
					s.LastError = ""
				})

				if len(syncResp.RemoteConfig.ScanSubnets) > 0 {
					remoteSubnets = syncResp.RemoteConfig.ScanSubnets
				}
				if len(syncResp.RemoteConfig.StaticIPs) > 0 {
					remoteStaticIPs = syncResp.RemoteConfig.StaticIPs
				}
				if len(syncResp.RemoteConfig.SNMPCommunities) > 0 {
					remoteCommunities = syncResp.RemoteConfig.SNMPCommunities
				}
				if syncResp.RemoteConfig.SNMPv3.Enabled {
					activeSNMPv3 = syncResp.RemoteConfig.SNMPv3
				}
				if syncResp.RemoteConfig.PollIntervalSeconds >= 30 {
					pollSec = syncResp.RemoteConfig.PollIntervalSeconds
				}
				if syncResp.RemoteConfig.RateLimitPacketsPerSec > 0 {
					rateLimit = syncResp.RemoteConfig.RateLimitPacketsPerSec
				}
				enableWakeup = syncResp.RemoteConfig.EnableHTTPWakeupForPantum

				if syncResp.RemoteConfig.DiagnosticRequested {
					handleDiagnosticRequest(cycleCtx, httpClient, cfg, paths, store, wpp, syncResp.RemoteConfig.DiagnosticUploadURL)
				}
				if syncResp.RemoteConfig.UpdateMSIURL != "" || syncResp.RemoteConfig.UpdateExeURL != "" || syncResp.RemoteConfig.UpdateLinuxURL != "" {
					checkAndApplyUpdate(cycleCtx, httpClient, cfg, paths, syncResp.RemoteConfig)
				}

				slog.Info("cloud sync completed", "devices", len(devices), "jobs", len(jobs), "next_poll_sec", pollSec)
			}

			if cycleCtx.Err() == context.DeadlineExceeded {
				httpTransport.CloseIdleConnections()
				slog.Warn("scan cycle exceeded timeout; idle HTTP connections closed")
			}
			cycleCancel()

			if *once {
				return
			}

			timer := time.NewTimer(time.Duration(pollSec) * time.Second)
			select {
			case <-rootCtx.Done():
				timer.Stop()
				return
			case <-forceScanCh:
				timer.Stop()
				slog.Info("immediate scan triggered via tray IPC")
			case <-timer.C:
			}
		}
	}

	if isSvc && runtime.GOOS == "windows" {
		if err := runWindowsService(runLoop); err != nil {
			slog.Error("windows service runner exited with error", "error", err)
			os.Exit(1)
		}
		return
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitForOSSignals(cancel)
	runLoop(rootCtx)
}
