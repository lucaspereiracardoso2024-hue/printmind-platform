package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

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

type SyncResponse struct {
	Status       string `json:"status"`
	RemoteConfig struct {
		PollIntervalSeconds       int      `json:"poll_interval_seconds"`
		RateLimitPacketsPerSec    int      `json:"rate_limit_packets_per_sec"`
		ScanSubnets               []string `json:"scan_subnets"`
		StaticIPs                 []string `json:"static_ips"`
		EnableUSBSpooler          bool     `json:"enable_usb_spooler"`
		EnableHTTPWakeupForPantum bool     `json:"enable_http_wakeup_for_pantum"`
	} `json:"remote_config"`
}

func encodeOID(oidStr string) ([]byte, error) {
	parts := strings.Split(strings.Trim(oidStr, "."), ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid OID")
	}
	var nums []uint32
	for _, p := range parts {
		var n uint32
		fmt.Sscanf(p, "%d", &n)
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
	conn.SetDeadline(time.Now().Add(timeout))
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

func expandCIDRToIPs(cidr string) []string {
	if !strings.Contains(cidr, "/") {
		return []string{cidr}
	}
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	var ips []string
	for cur := ip.Mask(ipnet.Mask); ipnet.Contains(cur); {
		lastByte := cur[len(cur)-1]
		if lastByte != 0 && lastByte != 255 {
			ips = append(ips, cur.String())
		}
		for i := len(cur) - 1; i >= 0; i-- {
			cur[i]++
			if cur[i] > 0 {
				break
			}
		}
	}
	return ips
}

func probeDevice(ip, community string, enableWakeup bool) (*DiscoveredDevice, bool) {
	timeout := 850 * time.Millisecond
	desc, err := getSNMPOID(ip, community, "1.3.6.1.2.1.1.1.0", timeout)
	if err != nil && enableWakeup {
		if c, dErr := net.DialTimeout("tcp", ip+":80", 250*time.Millisecond); dErr == nil {
			c.Write([]byte("HEAD / HTTP/1.0\r\n\r\n"))
			c.Close()
			time.Sleep(150 * time.Millisecond)
			desc, err = getSNMPOID(ip, community, "1.3.6.1.2.1.1.1.0", timeout)
		}
	}
	if err != nil {
		return nil, false
	}

	model := strings.TrimSpace(string(desc))
	if hrDesc, err := getSNMPOID(ip, community, "1.3.6.1.2.1.25.3.2.1.3.1", timeout); err == nil && len(hrDesc) > 0 {
		model = strings.TrimSpace(string(hrDesc))
	}

	serial := ""
	for _, sOID := range []string{
		"1.3.6.1.2.1.43.5.1.1.17.1",
		"1.3.6.1.4.1.11.2.3.9.4.2.1.1.3.3.0",
		"1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.1.0",
		"1.3.6.1.4.1.367.3.2.1.2.1.4.0",
	} {
		if b, err := getSNMPOID(ip, community, sOID, timeout); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			serial = strings.TrimSpace(string(b))
			break
		}
	}

	var total, mono, color uint32
	for _, tOID := range []string{
		"1.3.6.1.2.1.43.10.2.1.4.1.1",
		"1.3.6.1.4.1.11.2.3.9.4.2.1.4.1.2.5.0",
		"1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.10.0",
		"1.3.6.1.4.1.367.3.2.1.2.19.1.0",
		"1.3.6.1.4.1.1347.43.10.1.1.12.1.1",
	} {
		if v, ok := getSNMPUint32(ip, community, tOID, timeout); ok && v > 0 {
			total = v
			break
		}
	}
	if v, ok := getSNMPUint32(ip, community, "1.3.6.1.4.1.11.2.3.9.4.2.1.4.1.2.6.0", timeout); ok {
		mono = v
	}
	if v, ok := getSNMPUint32(ip, community, "1.3.6.1.4.1.11.2.3.9.4.2.1.4.1.2.7.0", timeout); ok {
		color = v
	}
	if mono == 0 && color == 0 {
		mono = total
	}

	return &DiscoveredDevice{
		DiscoverySource: "snmp_v2c",
		IPAddress:       ip,
		SerialNumber:    serial,
		Model:           model,
		Status:          "online",
		Counters: DeviceCounters{
			LifetimeEngineTotal: total,
			MonoTotal:           mono,
			ColorTotal:          color,
		},
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
		rateLimitPPS = 20
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

func main() {
	serverURL := flag.String("server", "http://localhost:8090", "URL do Servidor Central PrintMind")
	token := flag.String("token", "pm_live_hosp_central_01", "Token de Pareamento da Unidade/Filial")
	subnetsFlag := flag.String("subnets", "192.168.1.0/24", "Sub-redes locais ou VLANs remotas separadas por vírgula")
	once := flag.Bool("once", false, "Executa apenas 1 ciclo de varredura e encerra")
	flag.Parse()

	hostname, _ := os.Hostname()
	subnets := strings.Split(*subnetsFlag, ",")
	pollSec := 900
	rateLimit := 20
	enableWakeup := true

	log.Printf("🚀 PrintMind Satellite Agent V3.0 iniciado em %s (%s) • Sub-redes iniciais: %v", hostname, runtime.GOOS, subnets)

	for {
		devices := scanNetworkSubnets(subnets, nil, rateLimit, enableWakeup)
		reqPayload := SyncRequest{
			AgentVersion: "3.0.0",
			PairingToken: *token,
			CollectedAt:  time.Now().Format(time.RFC3339),
			HostInfo: map[string]string{
				"hostname": hostname,
				"os":       runtime.GOOS + "/" + runtime.GOARCH,
			},
			Devices: devices,
		}

		body, _ := json.Marshal(reqPayload)
		resp, err := http.Post(strings.TrimRight(*serverURL, "/")+"/api/v2/telemetry/sync", "application/json", bytes.NewBuffer(body))
		if err != nil {
			_ = os.WriteFile("agent_buffer.json.tmp", body, 0644)
			_ = os.Rename("agent_buffer.json.tmp", "agent_buffer.json")
			log.Printf("⚠️ Sem conexão com a nuvem (%v). Leituras salvas no buffer local.", err)
		} else {
			var syncResp SyncResponse
			if json.NewDecoder(resp.Body).Decode(&syncResp) == nil && len(syncResp.RemoteConfig.ScanSubnets) > 0 {
				subnets = syncResp.RemoteConfig.ScanSubnets
				if syncResp.RemoteConfig.PollIntervalSeconds >= 60 {
					pollSec = syncResp.RemoteConfig.PollIntervalSeconds
				}
				if syncResp.RemoteConfig.RateLimitPacketsPerSec > 0 {
					rateLimit = syncResp.RemoteConfig.RateLimitPacketsPerSec
				}
				enableWakeup = syncResp.RemoteConfig.EnableHTTPWakeupForPantum
			}
			resp.Body.Close()
			log.Printf("✅ Sync concluído (%d impressoras reportadas). Próxima varredura em %ds.", len(devices), pollSec)
		}

		if *once {
			break
		}
		time.Sleep(time.Duration(pollSec) * time.Second)
	}
}
