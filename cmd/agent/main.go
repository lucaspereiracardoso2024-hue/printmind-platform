package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	AgentVersion      = "3.2.1"
	DefaultConfigPath = "printmind-config.json"
	DefaultSupabaseRP = "https://zbsmzcbcivhzslcnfxmq.supabase.co/rest/v1/rpc/sync_agent_telemetry"
	DefaultAnonAPIKey = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJzdXBhYmFzZSIsInJlZiI6Inpic216Y2JjaXZoenNsY25meG1xIiwicm9sZSI6ImFub24iLCJpYXQiOjE3OTQ0NDUxNTcsImV4cCI6MjExMDAyMTE1N30.U9F-EL3ddBBqEK0MqkNuZBC6ZvwtuEElN6Z_VpEGT7Q"
)

var exeTokenRegex = regexp.MustCompile(`pm_live_[A-Za-z0-9]+`)

type Config struct {
	ServerURL       string   `json:"server_url"`
	APIKey          string   `json:"api_key"`
	PairingToken    string   `json:"pairing_token"`
	PollIntervalSec int      `json:"poll_interval_sec"`
	EnableUSB       bool     `json:"enable_usb"`
	Subnets         []string `json:"subnets,omitempty"`
}

type SupplyItem struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Level       int    `json:"level"`
	MaxCapacity int    `json:"max_capacity"`
	Percent     int    `json:"percent"`
}

type DeviceCounters struct {
	LifetimeEngineTotal uint32 `json:"lifetime_engine_total"`
	MonoTotal           uint32 `json:"mono_total"`
	ColorTotal          uint32 `json:"color_total"`
	DuplexSheets        uint32 `json:"duplex_sheets"`
	ScanTotal           uint32 `json:"scan_total"`
	LabelCount          uint32 `json:"label_count"`
}

type DiscoveredDevice struct {
	DiscoverySource string         `json:"discovery_source"`
	IPAddress       string         `json:"ip_address"`
	USBPort         string         `json:"usb_port,omitempty"`
	MACAddress      string         `json:"mac_address,omitempty"`
	SerialNumber    string         `json:"serial_number"`
	VendorPEN       int            `json:"vendor_pen"`
	Vendor          string         `json:"vendor"`
	Model           string         `json:"model"`
	Status          string         `json:"status"`
	Counters        DeviceCounters `json:"counters"`
	Supplies        []SupplyItem   `json:"supplies"`
}

type SyncRequest struct {
	AgentVersion string             `json:"agent_version"`
	PairingToken string             `json:"pairing_token"`
	UnitID       string             `json:"unit_id"`
	CollectedAt  string             `json:"collected_at"`
	HostInfo     map[string]string  `json:"host_info"`
	Devices      []DiscoveredDevice `json:"devices"`
}

type SupabaseRPCPayload struct {
	PairingToken string      `json:"p_pairing_token"`
	Payload      SyncRequest `json:"p_payload"`
}

type SyncResponse struct {
	Status       string `json:"status"`
	Message      string `json:"message,omitempty"`
	SyncedCount  int    `json:"synced_devices,omitempty"`
	RemoteConfig struct {
		PollIntervalSeconds       int      `json:"poll_interval_seconds"`
		RateLimitPacketsPerSec    int      `json:"rate_limit_packets_per_sec"`
		ScanSubnets               []string `json:"scan_subnets"`
		StaticIPs                 []string `json:"static_ips"`
		EnableUSBSpooler          bool     `json:"enable_usb_spooler"`
		EnableHTTPWakeupForPantum bool     `json:"enable_http_wakeup_for_pantum"`
	} `json:"remote_config"`
}

type localSpoolerPrinter struct {
	Name          string `json:"Name"`
	PortName      string `json:"PortName"`
	DriverName    string `json:"DriverName"`
	PrinterStatus int    `json:"PrinterStatus"`
	WorkOffline   bool   `json:"WorkOffline"`
}

func extractTokenFromExeName(exePath string) string {
	base := filepath.Base(exePath)
	return exeTokenRegex.FindString(base)
}

func saveConfig(path string, cfg Config) {
	if out, err := json.MarshalIndent(cfg, "", "  "); err == nil {
		_ = os.WriteFile(path, out, 0644)
	}
}

func loadOrInitConfig(path, flagServer, flagToken, flagSubnets string) Config {
	cfg := Config{
		ServerURL:       DefaultSupabaseRP,
		APIKey:          DefaultAnonAPIKey,
		PairingToken:    "",
		PollIntervalSec: 300,
		EnableUSB:       true,
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
			if len(loaded.Subnets) > 0 {
				cfg.Subnets = loaded.Subnets
			}
		}
	}

	// Token embutido no nome do arquivo baixado (ex: printmind-agent__pm_live_xxxx.exe)
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
			result.Message = "conta não encontrada"
		}
		return "", fmt.Errorf("%s", result.Message)
	}
	return result.PairingToken, nil
}

func isPrivateIPv4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
		return false
	}
	switch {
	case ip4[0] == 10:
		return true
	case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
		return true
	case ip4[0] == 192 && ip4[1] == 168:
		return true
	default:
		return false
	}
}

func detectLocalSubnets() []string {
	seen := make(map[string]bool)
	var subnets []string

	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if (iface.Flags&net.FlagUp) == 0 || (iface.Flags&net.FlagLoopback) != 0 {
				continue
			}
			addrs, aErr := iface.Addrs()
			if aErr != nil {
				continue
			}
			for _, addr := range addrs {
				ipNet, ok := addr.(*net.IPNet)
				if !ok || !isPrivateIPv4(ipNet.IP) {
					continue
				}
				ip4 := ipNet.IP.To4()
				cidr := fmt.Sprintf("%d.%d.%d.0/24", ip4[0], ip4[1], ip4[2])
				if !seen[cidr] {
					seen[cidr] = true
					subnets = append(subnets, cidr)
				}
			}
		}
	}

	if len(subnets) == 0 {
		subnets = append(subnets, "192.168.1.0/24")
	}
	return subnets
}

func detectVendorFromModel(model string) (string, int) {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "hp") || strings.Contains(m, "hewlett") || strings.Contains(m, "laserjet") || strings.Contains(m, "deskjet") || strings.Contains(m, "officejet"):
		return "HP", 11
	case strings.Contains(m, "brother"):
		return "Brother", 2435
	case strings.Contains(m, "ricoh") || strings.Contains(m, "aficio"):
		return "Ricoh", 367
	case strings.Contains(m, "kyocera") || strings.Contains(m, "ecosys") || strings.Contains(m, "taskalfa"):
		return "Kyocera", 1347
	case strings.Contains(m, "samsung"):
		return "Samsung", 236
	case strings.Contains(m, "pantum"):
		return "Pantum", 40093
	case strings.Contains(m, "epson"):
		return "Epson", 1248
	case strings.Contains(m, "canon"):
		return "Canon", 1602
	case strings.Contains(m, "konica") || strings.Contains(m, "minolta") || strings.Contains(m, "bizhub"):
		return "Konica Minolta", 18334
	case strings.Contains(m, "xerox"):
		return "Xerox", 253
	case strings.Contains(m, "lexmark"):
		return "Lexmark", 641
	case strings.Contains(m, "zebra"):
		return "Zebra", 10642
	default:
		return "Genérico", 0
	}
}

func sanitizeSerial(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func encodeOID(oidStr string) ([]byte, error) {
	parts := strings.Split(strings.Trim(oidStr, "."), ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid OID")
	}
	var nums []uint32
	for _, p := range parts {
		var n uint32
		if _, err := fmt.Sscanf(p, "%d", &n); err != nil {
			return nil, fmt.Errorf("invalid OID segment %q: %w", p, err)
		}
		nums = append(nums, n)
	}
	buf := []byte{byte(nums[0]*40 + nums[1])}
	for _, val := range nums[2:] {
		if val < 128 {
			buf = append(buf, byte(val))
		} else {
			var sub []byte
			sub = append(sub, byte(val&0x7F))
			val >>= 7
			for val > 0 {
				sub = append(sub, byte((val&0x7F)|0x80))
				val >>= 7
			}
			for i, j := 0, len(sub)-1; i < j; i, j = i+1, j-1 {
				sub[i], sub[j] = sub[j], sub[i]
			}
			buf = append(buf, sub...)
		}
	}
	return buf, nil
}

func buildGetRequest(community string, reqID uint32, oidBytes []byte) []byte {
	vbVal := []byte{0x05, 0x00}
	vbHead := []byte{0x06, byte(len(oidBytes))}
	varbind := append(append(vbHead, oidBytes...), vbVal...)
	vbSeq := append([]byte{0x30, byte(len(varbind))}, varbind...)
	vbList := append([]byte{0x30, byte(len(vbSeq))}, vbSeq...)

	var pduPayload []byte
	reqIDBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(reqIDBytes, reqID)
	pduPayload = append(pduPayload, 0x02, 0x04)
	pduPayload = append(pduPayload, reqIDBytes...)
	pduPayload = append(pduPayload, 0x02, 0x01, 0x00)
	pduPayload = append(pduPayload, 0x02, 0x01, 0x00)
	pduPayload = append(pduPayload, vbList...)
	pdu := append([]byte{0xA0, byte(len(pduPayload))}, pduPayload...)

	commBytes := []byte(community)
	var msgPayload []byte
	msgPayload = append(msgPayload, 0x02, 0x01, 0x01)
	msgPayload = append(msgPayload, 0x04, byte(len(commBytes)))
	msgPayload = append(msgPayload, commBytes...)
	msgPayload = append(msgPayload, pdu...)
	return append([]byte{0x30, byte(len(msgPayload))}, msgPayload...)
}

func parseBERLength(b []byte) (int, int, error) {
	if len(b) == 0 {
		return 0, 0, fmt.Errorf("empty")
	}
	first := b[0]
	if (first & 0x80) == 0 {
		return int(first), 1, nil
	}
	numOctets := int(first & 0x7F)
	if numOctets == 0 || numOctets > 4 || len(b) < 1+numOctets {
		return 0, 0, fmt.Errorf("invalid BER length")
	}
	val := 0
	for i := 1; i <= numOctets; i++ {
		val = (val << 8) | int(b[i])
	}
	return val, 1 + numOctets, nil
}

func getSNMPOID(ip, community, oidStr string, timeout time.Duration) ([]byte, error) {
	oidBytes, err := encodeOID(oidStr)
	if err != nil {
		return nil, err
	}
	pkt := buildGetRequest(community, 0x33445566, oidBytes)
	conn, err := net.DialTimeout("udp", ip+":161", timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(pkt); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	resp := buf[:n]
	idx := bytes.Index(resp, oidBytes)
	if idx == -1 {
		return nil, fmt.Errorf("not found")
	}
	valStart := idx + len(oidBytes)
	if valStart+1 >= len(resp) {
		return nil, fmt.Errorf("truncated")
	}
	valType := resp[valStart]
	if valType == 0x80 || valType == 0x81 || valType == 0x82 || valType == 0x05 {
		return nil, fmt.Errorf("snmp exception")
	}
	valLen, lenBytes, err := parseBERLength(resp[valStart+1:])
	if err != nil {
		return nil, err
	}
	dataStart := valStart + 1 + lenBytes
	if dataStart+valLen > len(resp) {
		return nil, fmt.Errorf("overflow")
	}
	return resp[dataStart : dataStart+valLen], nil
}

func getSNMPUint32(ip, community, oidStr string, timeout time.Duration) (uint32, bool) {
	val, err := getSNMPOID(ip, community, oidStr, timeout)
	if err != nil || len(val) == 0 {
		return 0, false
	}
	var out uint32
	for _, b := range val {
		out = (out << 8) | uint32(b)
	}
	if out == 0xFFFFFFFF {
		return 0, false
	}
	return out, true
}

func getSNMPInt(ip, community, oidStr string, timeout time.Duration) (int, bool) {
	val, err := getSNMPOID(ip, community, oidStr, timeout)
	if err != nil || len(val) == 0 || len(val) > 4 {
		return 0, false
	}
	var out int32
	if val[0]&0x80 != 0 {
		out = -1
	}
	for _, b := range val {
		out = (out << 8) | int32(b)
	}
	return int(out), true
}

func expandCIDRToIPs(cidr string) []string {
	if !strings.Contains(cidr, "/") {
		return []string{cidr}
	}
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	var all []string
	for cur := ip.Mask(ipnet.Mask); ipnet.Contains(cur); {
		all = append(all, cur.String())
		for i := len(cur) - 1; i >= 0; i-- {
			cur[i]++
			if cur[i] > 0 {
				break
			}
		}
	}
	ones, bits := ipnet.Mask.Size()
	if bits == 32 && ones < 31 && len(all) > 2 {
		return all[1 : len(all)-1]
	}
	return all
}

func probeDevice(ip, community string, enableWakeup bool) (*DiscoveredDevice, bool) {
	timeout := 850 * time.Millisecond
	desc, err := getSNMPOID(ip, community, "1.3.6.1.2.1.1.1.0", timeout)
	if err != nil && enableWakeup {
		if c, dErr := net.DialTimeout("tcp", ip+":80", 250*time.Millisecond); dErr == nil {
			_, _ = c.Write([]byte("HEAD / HTTP/1.0\r\n\r\n"))
			_ = c.Close()
			time.Sleep(150 * time.Millisecond)
			desc, err = getSNMPOID(ip, community, "1.3.6.1.2.1.1.1.0", timeout)
		}
	}
	if err != nil {
		return nil, false
	}

	model := strings.TrimSpace(string(desc))
	if hrDesc, err := getSNMPOID(ip, community, "1.3.6.1.2.1.25.3.2.1.3.1", timeout); err == nil && len(strings.TrimSpace(string(hrDesc))) > 0 {
		model = strings.TrimSpace(string(hrDesc))
	}
	vendor, pen := detectVendorFromModel(model)

	serial := ""
	for _, sOID := range []string{
		"1.3.6.1.2.1.43.5.1.1.17.1",
		"1.3.6.1.4.1.11.2.3.9.4.2.1.1.3.3.0",
		"1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.1.0",
		"1.3.6.1.4.1.367.3.2.1.2.1.4.0",
	} {
		if b, err := getSNMPOID(ip, community, sOID, timeout); err == nil {
			cleaned := sanitizeSerial(string(b))
			if cleaned != "" {
				serial = cleaned
				break
			}
		}
	}

	var total, mono, color uint32
	for _, tOID := range []string{
		"1.3.6.1.2.1.43.10.2.1.4.1.1",
		"1.3.6.1.4.1.11.2.3.9.4.2.1.4.1.2.5.0",
		"1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.10.0",
		"1.3.6.1.4.1.367.3.2.1.2.19.1.0",
		"1.3.6.1.4.1.1347.43.10.1.1.12.1.1",
		"1.3.6.1.4.1.236.11.5.1.1.9.2.0",
		"1.3.6.1.4.1.40093.10.3.1.1",
	} {
		if v, ok := getSNMPUint32(ip, community, tOID, timeout); ok && v > 0 {
			total = v
			break
		}
	}
	for _, mOID := range []string{
		"1.3.6.1.4.1.11.2.3.9.4.2.1.4.1.2.6.0",
		"1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.52.2.1.3.2",
		"1.3.6.1.4.1.1602.1.11.1.3.1.4.108",
	} {
		if v, ok := getSNMPUint32(ip, community, mOID, timeout); ok && v > 0 {
			mono = v
			break
		}
	}
	for _, cOID := range []string{
		"1.3.6.1.4.1.11.2.3.9.4.2.1.4.1.2.7.0",
		"1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.52.2.1.3.3",
		"1.3.6.1.4.1.1602.1.11.1.3.1.4.122",
	} {
		if v, ok := getSNMPUint32(ip, community, cOID, timeout); ok && v > 0 {
			color = v
			break
		}
	}
	if mono == 0 && color == 0 {
		mono = total
	} else if total == 0 {
		total = mono + color
	}

	var supplies []SupplyItem
	defaultColors := []string{"black", "cyan", "magenta", "yellow"}
	for idx := 1; idx <= 4; idx++ {
		nameOID := fmt.Sprintf("1.3.6.1.2.1.43.11.1.1.6.1.%d", idx)
		maxOID := fmt.Sprintf("1.3.6.1.2.1.43.11.1.1.8.1.%d", idx)
		lvlOID := fmt.Sprintf("1.3.6.1.2.1.43.11.1.1.9.1.%d", idx)

		nameBytes, nErr := getSNMPOID(ip, community, nameOID, timeout)
		if nErr != nil || len(strings.TrimSpace(string(nameBytes))) == 0 {
			continue
		}
		supName := strings.TrimSpace(string(nameBytes))
		if supName == "" {
			supName = "Toner Principal"
		}
		maxCap, okMax := getSNMPInt(ip, community, maxOID, timeout)
		curLvl, okLvl := getSNMPInt(ip, community, lvlOID, timeout)
		if !okMax || maxCap <= 0 {
			maxCap = 100
		}
		pct := 0
		if okLvl {
			switch {
			case curLvl == -3:
				pct = 100
			case curLvl < 0:
				pct = 50
			case curLvl <= maxCap:
				pct = int((float64(curLvl) / float64(maxCap)) * 100.0)
			default:
				pct = 100
			}
		}

		col := defaultColors[(idx-1)%4]
		lower := strings.ToLower(supName)
		switch {
		case strings.Contains(lower, "cyan") || strings.Contains(lower, "ciano"):
			col = "cyan"
		case strings.Contains(lower, "magenta"):
			col = "magenta"
		case strings.Contains(lower, "yellow") || strings.Contains(lower, "amarelo"):
			col = "yellow"
		case strings.Contains(lower, "black") || strings.Contains(lower, "preto"):
			col = "black"
		}

		supplies = append(supplies, SupplyItem{
			Name:        supName,
			Color:       col,
			Level:       curLvl,
			MaxCapacity: maxCap,
			Percent:     pct,
		})
	}

	return &DiscoveredDevice{
		DiscoverySource: "snmp_v2c",
		IPAddress:       ip,
		SerialNumber:    serial,
		VendorPEN:       pen,
		Vendor:          vendor,
		Model:           model,
		Status:          "online",
		Counters: DeviceCounters{
			LifetimeEngineTotal: total,
			MonoTotal:           mono,
			ColorTotal:          color,
		},
		Supplies: supplies,
	}, true
}

func scanNetworkSubnets(subnets []string, staticIPs []string, rateLimitPPS int, enableWakeup bool) []DiscoveredDevice {
	ipSet := make(map[string]bool)
	for _, cidr := range subnets {
		for _, ip := range expandCIDRToIPs(cidr) {
			ipSet[ip] = true
		}
	}
	for _, ip := range staticIPs {
		if strings.TrimSpace(ip) != "" {
			ipSet[strings.TrimSpace(ip)] = true
		}
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
		<-ticker.C
		wg.Add(1)
		go func(targetIP string) {
			defer wg.Done()
			if dev, ok := probeDevice(targetIP, "public", enableWakeup); ok {
				mu.Lock()
				results = append(results, *dev)
				mu.Unlock()
			}
		}(ip)
	}
	wg.Wait()
	return results
}

func isVirtualPrinter(name, port, driver string) bool {
	combined := strings.ToLower(name + " " + port + " " + driver)
	virtualKeywords := []string{"microsoft print to pdf", "onenote", "xps document", "fax", "send to onenote", "pdfcreator", "foxit", "adobe pdf", "cutepdf", "nul:"}
	for _, kw := range virtualKeywords {
		if strings.Contains(combined, kw) {
			return true
		}
	}
	return false
}

func discoverLocalSpoolerPrinters() []DiscoveredDevice {
	if runtime.GOOS != "windows" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	psCmd := "Get-CimInstance Win32_Printer | Select-Object Name, PortName, DriverName, PrinterStatus, WorkOffline | ConvertTo-Json -Compress"
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", psCmd)
	out, err := cmd.Output()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return nil
	}

	trimmed := bytes.TrimSpace(out)
	var rawList []localSpoolerPrinter
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &rawList); err != nil {
			return nil
		}
	} else if trimmed[0] == '{' {
		var single localSpoolerPrinter
		if err := json.Unmarshal(trimmed, &single); err != nil {
			return nil
		}
		rawList = append(rawList, single)
	}

	var devices []DiscoveredDevice
	for _, p := range rawList {
		if isVirtualPrinter(p.Name, p.PortName, p.DriverName) {
			continue
		}
		portUpper := strings.ToUpper(p.PortName)
		isUSB := strings.HasPrefix(portUpper, "USB") || strings.HasPrefix(portUpper, "DOT4") || strings.Contains(portUpper, "WSD")
		if !isUSB {
			continue
		}

		vendor, pen := detectVendorFromModel(p.Name + " " + p.DriverName)
		status := "online"
		if p.WorkOffline {
			status = "offline"
		}
		serial := sanitizeSerial("USB-" + p.Name + "-" + p.PortName)

		devices = append(devices, DiscoveredDevice{
			DiscoverySource: "usb_spooler",
			IPAddress:       "0.0.0.0",
			USBPort:         p.PortName,
			SerialNumber:    serial,
			VendorPEN:       pen,
			Vendor:          vendor,
			Model:           strings.TrimSpace(p.Name),
			Status:          status,
			Supplies: []SupplyItem{
				{Name: "Toner / Tinta Geral", Color: "black", Level: 80, MaxCapacity: 100, Percent: 80},
			},
		})
	}
	return devices
}

func sendTelemetry(client *http.Client, cfg Config, reqPayload SyncRequest) (*SyncResponse, error) {
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

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBuffer(bodyBytes))
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
		_ = os.WriteFile("agent_buffer.json.tmp", bodyBytes, 0644)
		_ = os.Rename("agent_buffer.json.tmp", "agent_buffer.json")
		return nil, err
	}
	defer resp.Body.Close()

	respData, _ := io.ReadAll(resp.Body)
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

func main() {
	serverURL := flag.String("server", "", "URL do Endpoint PrintMind / Supabase RPC")
	token := flag.String("token", "", "Token de Pareamento da Unidade/Filial")
	emailFlag := flag.String("email", "", "E-mail cadastrado no PrintMind (pareamento automático)")
	subnetsFlag := flag.String("subnets", "", "Sub-redes locais ou VLANs remotas separadas por vírgula")
	configPath := flag.String("config", DefaultConfigPath, "Caminho do arquivo de configuração persistente")
	once := flag.Bool("once", false, "Executa apenas 1 ciclo de varredura e encerra")
	flag.Parse()

	httpClient := &http.Client{Timeout: 20 * time.Second}
	cfg := loadOrInitConfig(*configPath, *serverURL, *token, *subnetsFlag)

	if cfg.PairingToken == "" && strings.TrimSpace(*emailFlag) != "" {
		if resolved, err := resolveTokenByEmail(httpClient, cfg.ServerURL, cfg.APIKey, *emailFlag); err == nil {
			cfg.PairingToken = resolved
			saveConfig(*configPath, cfg)
		} else {
			log.Printf("[Pareamento] Falha ao localizar conta para %s: %v", *emailFlag, err)
		}
	}

	if cfg.PairingToken == "" {
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
				log.Fatalf("Nao foi possivel vincular a conta (%v). Baixe o scanner novamente pelo seu painel.", err)
			}
			cfg.PairingToken = resolved
			saveConfig(*configPath, cfg)
			log.Printf("[Pareamento] Conta vinculada com sucesso! Token salvo em %s.", *configPath)
		} else if input != "" {
			cfg.PairingToken = input
			saveConfig(*configPath, cfg)
		} else {
			log.Fatalf("Token ou e-mail nao informado. Baixe o scanner pelo seu painel PrintMind.")
		}
	}

	hostname, _ := os.Hostname()
	subnets := cfg.Subnets
	if len(subnets) == 0 {
		subnets = detectLocalSubnets()
	}

	pollSec := cfg.PollIntervalSec
	if pollSec < 30 {
		pollSec = 300
	}
	rateLimit := 25
	enableWakeup := true

	log.Printf("PrintMind Satellite Agent v%s iniciado em %s (%s/%s)", AgentVersion, hostname, runtime.GOOS, runtime.GOARCH)
	log.Printf("[Varredura de Rede] Iniciando varredura SNMP em %v...", subnets)

	for {
		snmpDevices := scanNetworkSubnets(subnets, nil, rateLimit, enableWakeup)
		if len(snmpDevices) > 0 {
			log.Printf("[SNMP Rede] %d impressora(s) respondendo via SNMP RFC 3805:", len(snmpDevices))
		} else {
			log.Printf("[SNMP Rede] Nenhuma impressora SNMP respondeu neste ciclo.")
		}

		var usbDevices []DiscoveredDevice
		if cfg.EnableUSB {
			usbDevices = discoverLocalSpoolerPrinters()
			if len(usbDevices) > 0 {
				log.Printf("[Spooler/USB] %d impressora(s) local(is) detectada(s):", len(usbDevices))
			}
		}

		devices := append(snmpDevices, usbDevices...)
		for _, dev := range devices {
			for _, sup := range dev.Supplies {
				if sup.Percent <= 15 {
					log.Printf("[Alerta Heurístico] %s (%s): %s em nível crítico (%d%%)", dev.Model, dev.IPAddress, sup.Name, sup.Percent)
				}
			}
		}

		reqPayload := SyncRequest{
			AgentVersion: AgentVersion,
			PairingToken: cfg.PairingToken,
			CollectedAt:  time.Now().Format(time.RFC3339),
			HostInfo: map[string]string{
				"hostname": hostname,
				"os":       runtime.GOOS + "/" + runtime.GOARCH,
			},
			Devices: devices,
		}

		syncResp, err := sendTelemetry(httpClient, cfg, reqPayload)
		if err != nil {
			log.Printf("[Nuvem PrintMind] Falha na sincronização (%v). Buffer local salvo em agent_buffer.json.", err)
		} else {
			if len(syncResp.RemoteConfig.ScanSubnets) > 0 && len(cfg.Subnets) == 0 {
				subnets = syncResp.RemoteConfig.ScanSubnets
			}
			if syncResp.RemoteConfig.PollIntervalSeconds >= 60 {
				pollSec = syncResp.RemoteConfig.PollIntervalSeconds
			}
			if syncResp.RemoteConfig.RateLimitPacketsPerSec > 0 {
				rateLimit = syncResp.RemoteConfig.RateLimitPacketsPerSec
			}
			enableWakeup = syncResp.RemoteConfig.EnableHTTPWakeupForPantum
			log.Printf("[Nuvem PrintMind] Sync concluído (%d impressoras reportadas). Próxima varredura em %ds.", len(devices), pollSec)
		}

		if *once {
			break
		}
		time.Sleep(time.Duration(pollSec) * time.Second)
	}
}
