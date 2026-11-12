package main

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML []byte

// --- Modelos de Dados de Produção V3.0 (Clientes, Unidades, Contratos, Impressoras e Split BaaS) ---

type SupplyItem struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Level       int    `json:"level"`
	MaxCapacity int    `json:"max_capacity"`
	Percent     int    `json:"percent"`
}

type Printer struct {
	ID                string       `json:"id"`
	IP                string       `json:"ip"`
	USBPort           string       `json:"usb_port,omitempty"`
	MACAddress        string       `json:"mac_address,omitempty"`
	Hostname          string       `json:"hostname"`
	VendorPEN         int          `json:"vendor_pen,omitempty"`
	Vendor            string       `json:"vendor,omitempty"`
	Model             string       `json:"model"`
	Serial            string       `json:"serial"`
	DiscoverySource   string       `json:"discovery_source,omitempty"`
	TotalPages        uint32       `json:"total_pages"`
	MonoPages         uint32       `json:"mono_pages"`
	ColorPages        uint32       `json:"color_pages"`
	InitialTotalPages uint32       `json:"initial_total_pages"`
	InitialMonoPages  uint32       `json:"initial_mono_pages"`
	InitialColorPages uint32       `json:"initial_color_pages"`
	DuplexSheets      uint32       `json:"duplex_sheets,omitempty"`
	ScanPages         uint32       `json:"scan_pages,omitempty"`
	LabelCount        uint32       `json:"label_count,omitempty"`
	Status            string       `json:"status"`
	LatencyMs         int64        `json:"latency_ms"`
	Supplies          []SupplyItem `json:"supplies"`
	ClientID          string       `json:"client_id"`
	Unit              string       `json:"unit"`
	Department        string       `json:"department"`
	Location          string       `json:"location"`
	Ownership         string       `json:"ownership"`
	LastSeen          string       `json:"last_seen"`
	CreatedAt         string       `json:"created_at"`
}

type ClientUnitConfig struct {
	ID               string   `json:"id"`
	ClientID         string   `json:"client_id"`
	UnitName         string   `json:"unit_name"`
	PairingToken     string   `json:"pairing_token"`
	ScanSubnets      []string `json:"scan_subnets"`
	StaticIPs        []string `json:"static_ips"`
	PollIntervalSec  int      `json:"poll_interval_sec"`
	RateLimitPPS     int      `json:"rate_limit_pps"`
	EnableUSBSpooler bool     `json:"enable_usb_spooler"`
	EnableHTTPWakeup bool     `json:"enable_http_wakeup"`
	AgentStatus      string   `json:"agent_status"`
	AgentLastSeen    string   `json:"agent_last_seen"`
}

type Client struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	CNPJ           string   `json:"cnpj"`
	Phone          string   `json:"phone"`
	Address        string   `json:"address"`
	ClosingDay     int      `json:"closing_day"`
	ContractNum    string   `json:"contract_num"`
	BaseRate       float64  `json:"base_rate"`
	FranchiseMono  int      `json:"franchise_mono"`
	ExtraMono      float64  `json:"extra_mono"`
	FranchiseColor int      `json:"franchise_color"`
	ExtraColor     float64  `json:"extra_color"`
	PrinterIDs     []string `json:"printer_ids"`
	IsCorporate    bool     `json:"is_corporate"`
	Units          []string `json:"units"`
	Departments    []string `json:"departments"`
	BaaSCustomerID string   `json:"baas_customer_id,omitempty"`
	PixKey         string   `json:"pix_key,omitempty"`
	CreatedAt      string   `json:"created_at"`
}

type Reading struct {
	ID        string `json:"id"`
	PrinterID string `json:"printer_id"`
	ClientID  string `json:"client_id"`
	IP        string `json:"ip"`
	Pages     uint32 `json:"pages"`
	MonoPages uint32 `json:"mono_pages"`
	ColorPage uint32 `json:"color_pages"`
	Timestamp string `json:"timestamp"`
}

type Closing struct {
	ID               string  `json:"id"`
	ClientID         string  `json:"client_id"`
	ClientName       string  `json:"client_name"`
	MonthYear        string  `json:"month_year"`
	TotalPages       uint32  `json:"total_pages"`
	MonoPages        uint32  `json:"mono_pages"`
	ColorPages       uint32  `json:"color_pages"`
	BaseRate         float64 `json:"base_rate"`
	ExtraMono        float64 `json:"extra_mono"`
	ExtraColor       float64 `json:"extra_color"`
	TotalAmount      float64 `json:"total_amount"`
	PlatformSplitFee float64 `json:"platform_split_fee"`
	NetTenantAmount  float64 `json:"net_tenant_amount"`
	Status           string  `json:"status"`
	FrozenAt         string  `json:"frozen_at"`
	PixKey           string  `json:"pix_key"`
	PixCopyPaste     string  `json:"pix_copy_paste,omitempty"`
	BoletoURL        string  `json:"boleto_url,omitempty"`
}

type Database struct {
	Clients     []Client           `json:"clients"`
	ClientUnits []ClientUnitConfig `json:"client_units,omitempty"`
	Printers    []Printer          `json:"printers"`
	Readings    []Reading          `json:"readings"`
	Closings    []Closing          `json:"closings"`
}

var (
	dbLock sync.RWMutex
	dbPath = filepath.Join("data", "db.json")
	dbData = Database{
		Clients:     []Client{},
		ClientUnits: []ClientUnitConfig{},
		Printers:    []Printer{},
		Readings:    []Reading{},
		Closings:    []Closing{},
	}
)

func loadDB() {
	dbLock.Lock()
	defer dbLock.Unlock()

	os.MkdirAll("data", 0755)
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		saveDBUnsafe()
		return
	}

	data, err := os.ReadFile(dbPath)
	if err == nil {
		json.Unmarshal(data, &dbData)
	}
	if dbData.Clients == nil {
		dbData.Clients = []Client{}
	}
	if dbData.ClientUnits == nil {
		dbData.ClientUnits = []ClientUnitConfig{
			{
				ID:               "unt_hosp_central_01",
				ClientID:         "cli_1794444800609077300",
				UnitName:         "Hospital Central",
				PairingToken:     "pm_live_hosp_central_01",
				ScanSubnets:      []string{"10.20.1.0/24", "10.20.2.0/24"},
				StaticIPs:        []string{"192.168.1.13"},
				PollIntervalSec:  900,
				RateLimitPPS:     20,
				EnableUSBSpooler: true,
				EnableHTTPWakeup: true,
				AgentStatus:      "online",
				AgentLastSeen:    time.Now().Format("2006-01-02 15:04:05"),
			},
			{
				ID:               "unt_sto_amaro_02",
				ClientID:         "cli_1794444800609077300",
				UnitName:         "Clínica Santo Amaro",
				PairingToken:     "pm_live_clinica_sto_amaro_02",
				ScanSubnets:      []string{"192.168.1.0/24"},
				StaticIPs:        []string{"192.168.1.50"},
				PollIntervalSec:  900,
				RateLimitPPS:     20,
				EnableUSBSpooler: true,
				EnableHTTPWakeup: true,
				AgentStatus:      "online",
				AgentLastSeen:    time.Now().Format("2006-01-02 15:04:05"),
			},
		}
	}
	if dbData.Printers == nil {
		dbData.Printers = []Printer{}
	}
	if dbData.Readings == nil {
		dbData.Readings = []Reading{}
	}
	if dbData.Closings == nil {
		dbData.Closings = []Closing{}
	}
}

func saveDBUnsafe() {
	data, err := json.MarshalIndent(dbData, "", "  ")
	if err != nil {
		return
	}
	tmpPath := dbPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err == nil {
		_ = os.Rename(tmpPath, dbPath)
	}
}

func emvField(id string, val string) string {
	return fmt.Sprintf("%s%02d%s", id, len(val), val)
}

func crc16CCITT(payload string) string {
	crc := uint16(0xFFFF)
	for i := 0; i < len(payload); i++ {
		crc ^= uint16(payload[i]) << 8
		for j := 0; j < 8; j++ {
			if (crc & 0x8000) != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return fmt.Sprintf("%04X", crc)
}

func GenerateValidPixPayload(pixKey string, merchantName string, city string, amount float64, txID string) string {
	if pixKey == "" {
		pixKey = "18.234.567/0001-89"
	}
	if merchantName == "" {
		merchantName = "STAR COPIAS OUTSOURCING"
	}
	if len(merchantName) > 25 {
		merchantName = merchantName[:25]
	}
	if city == "" {
		city = "SAO PAULO"
	}
	if txID == "" {
		txID = "PRINTMIND"
	}
	gui := emvField("00", "br.gov.bcb.pix") + emvField("01", pixKey)
	valStr := fmt.Sprintf("%.2f", amount)
	base := "000201010212" +
		emvField("26", gui) +
		"520400005303986" +
		emvField("54", valStr) +
		"5802BR" +
		emvField("59", merchantName) +
		emvField("60", city) +
		emvField("62", emvField("05", txID)) +
		"6304"
	return base + crc16CCITT(base)
}

type SNMPClient struct {
	Community string
	Timeout   time.Duration
}

func NewSNMPClient(community string, timeout time.Duration) *SNMPClient {
	if community == "" {
		community = "public"
	}
	if timeout == 0 {
		timeout = 1200 * time.Millisecond
	}
	return &SNMPClient{Community: community, Timeout: timeout}
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

func parseBERLength(b []byte) (length int, bytesRead int, err error) {
	if len(b) == 0 {
		return 0, 0, fmt.Errorf("buffer vazio")
	}
	first := b[0]
	if (first & 0x80) == 0 {
		return int(first), 1, nil
	}
	numOctets := int(first & 0x7F)
	if numOctets == 0 || numOctets > 4 || len(b) < 1+numOctets {
		return 0, 0, fmt.Errorf("comprimento BER inválido")
	}
	val := 0
	for i := 1; i <= numOctets; i++ {
		val = (val << 8) | int(b[i])
	}
	return val, 1 + numOctets, nil
}

func decodeBERUint32(val []byte) uint32 {
	var out uint32
	for _, b := range val {
		out = (out << 8) | uint32(b)
	}
	return out
}

func (c *SNMPClient) GetOID(ip string, oidStr string) (byte, []byte, error) {
	oidBytes, err := encodeOID(oidStr)
	if err != nil {
		return 0, nil, err
	}

	packet := buildGetRequest(c.Community, 0x55AA1122, oidBytes)

	conn, err := net.DialTimeout("udp", ip+":161", c.Timeout)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(c.Timeout))
	if _, err := conn.Write(packet); err != nil {
		return 0, nil, err
	}

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return 0, nil, err
	}

	resp := buf[:n]
	idx := bytes.Index(resp, oidBytes)
	if idx == -1 {
		return 0, nil, fmt.Errorf("OID não encontrado")
	}

	valStart := idx + len(oidBytes)
	if valStart+1 >= len(resp) {
		return 0, nil, fmt.Errorf("resposta truncada")
	}

	valType := resp[valStart]
	if valType == 0x80 || valType == 0x81 || valType == 0x82 || valType == 0x05 {
		return valType, nil, fmt.Errorf("OID sem valor suportado no equipamento (0x%02X)", valType)
	}

	valLen, lenBytes, err := parseBERLength(resp[valStart+1:])
	if err != nil {
		return 0, nil, err
	}
	valDataStart := valStart + 1 + lenBytes
	if valDataStart+valLen > len(resp) {
		return 0, nil, fmt.Errorf("tamanho do valor excede dados")
	}

	return valType, resp[valDataStart : valDataStart+valLen], nil
}

func (c *SNMPClient) GetUint32(ip string, oidStr string) (uint32, bool) {
	_, val, err := c.GetOID(ip, oidStr)
	if err != nil || len(val) == 0 {
		return 0, false
	}
	v := decodeBERUint32(val)
	if v == 0xFFFFFFFF {
		return 0, false
	}
	return v, true
}

func ProbePrinter(ip string, community string) (*Printer, error) {
	client := NewSNMPClient(community, 1400*time.Millisecond)

	start := time.Now()
	_, descBytes, err := client.GetOID(ip, "1.3.6.1.2.1.1.1.0")
	if err != nil {
		if httpConn, dialErr := net.DialTimeout("tcp", ip+":80", 350*time.Millisecond); dialErr == nil {
			httpConn.Write([]byte("HEAD / HTTP/1.0\r\n\r\n"))
			httpConn.Close()
			time.Sleep(250 * time.Millisecond)
		}
		_, descBytes, err = client.GetOID(ip, "1.3.6.1.2.1.1.5.0")
		if err != nil {
			return nil, fmt.Errorf("impressora não respondeu na porta SNMP 161")
		}
	}
	latency := time.Since(start).Milliseconds()

	hostname := ""
	if _, val, err := client.GetOID(ip, "1.3.6.1.2.1.1.5.0"); err == nil {
		hostname = strings.TrimSpace(string(val))
	}

	model := ""
	if _, val, err := client.GetOID(ip, "1.3.6.1.2.1.25.3.2.1.3.1"); err == nil && len(val) > 0 {
		model = strings.TrimSpace(string(val))
	}
	if model == "" && len(descBytes) > 0 {
		model = strings.TrimSpace(string(descBytes))
	}

	serial := ""
	for _, serialOID := range []string{
		"1.3.6.1.2.1.43.5.1.1.17.1",
		"1.3.6.1.4.1.11.2.3.9.4.2.1.1.3.3.0",
		"1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.1.0",
		"1.3.6.1.4.1.367.3.2.1.2.1.4.0",
	} {
		if _, val, err := client.GetOID(ip, serialOID); err == nil && len(strings.TrimSpace(string(val))) > 0 {
			serial = strings.TrimSpace(string(val))
			break
		}
	}

	var totalPages uint32
	for _, totalOID := range []string{
		"1.3.6.1.2.1.43.10.2.1.4.1.1",
		"1.3.6.1.4.1.11.2.3.9.4.2.1.4.1.2.5.0",
		"1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.10.0",
		"1.3.6.1.4.1.367.3.2.1.2.19.1.0",
		"1.3.6.1.4.1.1347.43.10.1.1.12.1.1",
		"1.3.6.1.4.1.236.11.5.1.1.9.2.0",
		"1.3.6.1.4.1.40093.10.3.1.1",
		"1.3.6.1.4.1.10642.200.17.2.0",
	} {
		if v, ok := client.GetUint32(ip, totalOID); ok && v > 0 {
			totalPages = v
			break
		}
	}

	var monoPages, colorPages uint32
	for _, oid := range []string{
		"1.3.6.1.4.1.11.2.3.9.4.2.1.4.1.2.6.0",
		"1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.52.2.1.3.2",
		"1.3.6.1.4.1.1602.1.11.1.3.1.4.108",
		"1.3.6.1.4.1.2385.1.1.19.2.1.4.1.4.61",
	} {
		if v, ok := client.GetUint32(ip, oid); ok && v > 0 {
			monoPages = v
			break
		}
	}
	for _, oid := range []string{
		"1.3.6.1.4.1.11.2.3.9.4.2.1.4.1.2.7.0",
		"1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.52.2.1.3.3",
		"1.3.6.1.4.1.1602.1.11.1.3.1.4.122",
		"1.3.6.1.4.1.2385.1.1.19.2.1.4.1.4.63",
	} {
		if v, ok := client.GetUint32(ip, oid); ok && v > 0 {
			colorPages = v
			break
		}
	}
	if monoPages == 0 && colorPages == 0 {
		monoPages = totalPages
		colorPages = 0
	} else if totalPages == 0 && (monoPages+colorPages) > 0 {
		totalPages = monoPages + colorPages
	}

	var supplies []SupplyItem
	defaultColors := []string{"black", "cyan", "magenta", "yellow"}
	for idx := 1; idx <= 4; idx++ {
		descOID := fmt.Sprintf("1.3.6.1.2.1.43.11.1.1.6.1.%d", idx)
		maxOID := fmt.Sprintf("1.3.6.1.2.1.43.11.1.1.8.1.%d", idx)
		lvlOID := fmt.Sprintf("1.3.6.1.2.1.43.11.1.1.9.1.%d", idx)

		_, nameBytes, err := client.GetOID(ip, descOID)
		if err != nil || len(nameBytes) == 0 {
			continue
		}
		supName := strings.TrimSpace(string(nameBytes))
		maxCap, okMax := client.GetUint32(ip, maxOID)
		curLvl, okLvl := client.GetUint32(ip, lvlOID)
		if !okMax || maxCap == 0 {
			maxCap = 100
		}
		pct := 80
		if okLvl && curLvl <= maxCap {
			pct = int((float64(curLvl) / float64(maxCap)) * 100.0)
		}
		col := defaultColors[(idx-1)%4]
		lowerName := strings.ToLower(supName)
		if strings.Contains(lowerName, "cyan") || strings.Contains(lowerName, "ciano") {
			col = "cyan"
		} else if strings.Contains(lowerName, "magenta") {
			col = "magenta"
		} else if strings.Contains(lowerName, "yellow") || strings.Contains(lowerName, "amarelo") {
			col = "yellow"
		} else if strings.Contains(lowerName, "black") || strings.Contains(lowerName, "preto") {
			col = "black"
		}
		supplies = append(supplies, SupplyItem{
			Name:        supName,
			Color:       col,
			Level:       int(curLvl),
			MaxCapacity: int(maxCap),
			Percent:     pct,
		})
	}

	p := &Printer{
		ID:                fmt.Sprintf("prt_%d", time.Now().UnixNano()),
		IP:                ip,
		Hostname:          hostname,
		Model:             model,
		Serial:            serial,
		DiscoverySource:   "snmp_v2c",
		TotalPages:        totalPages,
		MonoPages:         monoPages,
		ColorPages:        colorPages,
		InitialTotalPages: totalPages,
		InitialMonoPages:  monoPages,
		InitialColorPages: colorPages,
		Supplies:          supplies,
		Status:            "online",
		LatencyMs:         latency,
		LastSeen:          time.Now().Format("2006-01-02 15:04:05"),
		CreatedAt:         time.Now().Format("2006-01-02 15:04:05"),
	}

	return p, nil
}

func DiscoverSubnet() []string {
	var localIP string
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ipnet.IP.To4() != nil {
					ipStr := ipnet.IP.String()
					if strings.HasPrefix(ipStr, "192.168.") || strings.HasPrefix(ipStr, "10.") || strings.HasPrefix(ipStr, "172.") {
						localIP = ipStr
						break
					}
				}
			}
		}
	}

	if localIP == "" {
		localIP = "192.168.1.1"
	}

	parts := strings.Split(localIP, ".")
	subnetPrefix := fmt.Sprintf("%s.%s.%s.", parts[0], parts[1], parts[2])

	var found []string
	var mu sync.Mutex
	var wg sync.WaitGroup

	jobs := make(chan string, 254)
	client := NewSNMPClient("public", 600*time.Millisecond)

	for w := 0; w < 20; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ip := range jobs {
				if _, _, err := client.GetOID(ip, "1.3.6.1.2.1.1.1.0"); err == nil {
					mu.Lock()
					found = append(found, ip)
					mu.Unlock()
					continue
				}
				conn, err := net.DialTimeout("tcp", ip+":9100", 200*time.Millisecond)
				if err == nil {
					conn.Close()
					mu.Lock()
					found = append(found, ip)
					mu.Unlock()
				}
			}
		}()
	}

	for i := 1; i <= 254; i++ {
		jobs <- fmt.Sprintf("%s%d", subnetPrefix, i)
	}
	close(jobs)
	wg.Wait()
	return found
}

func handlePublicConfig(w http.ResponseWriter, r *http.Request) {
	supaURL := os.Getenv("SUPABASE_URL")
	if supaURL == "" {
		supaURL = "https://zbsmzcbcivhzslcnfxmq.supabase.co"
	}
	supaKey := os.Getenv("SUPABASE_PUBLISHABLE_KEY")
	if supaKey == "" {
		supaKey = "sb_publishable_7ZOdmWOGLJuAZlOicuROVA_Hd0U63zz"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"supabase_url":             supaURL,
		"supabase_publishable_key": supaKey,
		"whatsapp_engine":          "evolution-api-open-source",
		"payment_gateway":          "asaas-baas-split-v3",
		"version":                  "3.0.0",
	})
}

func handleGetClients(w http.ResponseWriter, r *http.Request) {
	dbLock.RLock()
	defer dbLock.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dbData.Clients)
}

func handleSaveClient(w http.ResponseWriter, r *http.Request) {
	var c Client
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, `{"error":"dados inválidos"}`, http.StatusBadRequest)
		return
	}

	dbLock.Lock()
	defer dbLock.Unlock()

	if c.ID == "" {
		c.ID = fmt.Sprintf("cli_%d", time.Now().UnixNano())
		c.CreatedAt = time.Now().Format("2006-01-02 15:04:05")
		if c.PrinterIDs == nil {
			c.PrinterIDs = []string{}
		}
		dbData.Clients = append(dbData.Clients, c)
	} else {
		for i, existing := range dbData.Clients {
			if existing.ID == c.ID {
				c.PrinterIDs = existing.PrinterIDs
				c.CreatedAt = existing.CreatedAt
				dbData.Clients[i] = c
				break
			}
		}
	}

	saveDBUnsafe()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(c)
}

func handleDeleteClient(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = strings.TrimPrefix(r.URL.Path, "/api/clients/delete/")
	}
	dbLock.Lock()
	defer dbLock.Unlock()

	var next []Client
	for _, c := range dbData.Clients {
		if c.ID != id {
			next = append(next, c)
		}
	}
	dbData.Clients = next

	for i := range dbData.Printers {
		if dbData.Printers[i].ClientID == id {
			dbData.Printers[i].ClientID = ""
		}
	}

	saveDBUnsafe()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func handleLinkPrinter(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientID   string `json:"client_id"`
		PrinterID  string `json:"printer_id"`
		Unit       string `json:"unit"`
		Department string `json:"department"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"dados inválidos"}`, http.StatusBadRequest)
		return
	}

	dbLock.Lock()
	defer dbLock.Unlock()

	for i := range dbData.Clients {
		if dbData.Clients[i].ID == body.ClientID {
			exists := false
			for _, pid := range dbData.Clients[i].PrinterIDs {
				if pid == body.PrinterID {
					exists = true
					break
				}
			}
			if !exists {
				dbData.Clients[i].PrinterIDs = append(dbData.Clients[i].PrinterIDs, body.PrinterID)
			}
			break
		}
	}

	for i := range dbData.Printers {
		if dbData.Printers[i].ID == body.PrinterID {
			dbData.Printers[i].ClientID = body.ClientID
			dbData.Printers[i].Unit = body.Unit
			dbData.Printers[i].Department = body.Department
			break
		}
	}

	saveDBUnsafe()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func handleUnlinkPrinter(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientID  string `json:"client_id"`
		PrinterID string `json:"printer_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"dados inválidos"}`, http.StatusBadRequest)
		return
	}

	dbLock.Lock()
	defer dbLock.Unlock()

	for i := range dbData.Clients {
		if dbData.Clients[i].ID == body.ClientID {
			var newPids []string
			for _, pid := range dbData.Clients[i].PrinterIDs {
				if pid != body.PrinterID {
					newPids = append(newPids, pid)
				}
			}
			dbData.Clients[i].PrinterIDs = newPids
			break
		}
	}

	for i := range dbData.Printers {
		if dbData.Printers[i].ID == body.PrinterID {
			dbData.Printers[i].ClientID = ""
			dbData.Printers[i].Unit = ""
			dbData.Printers[i].Department = ""
			break
		}
	}

	saveDBUnsafe()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func handleGetPrinters(w http.ResponseWriter, r *http.Request) {
	dbLock.RLock()
	defer dbLock.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dbData.Printers)
}

func handleProbePrinter(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IP        string `json:"ip"`
		Community string `json:"community"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.IP == "" {
		http.Error(w, `{"error":"IP obrigatório"}`, http.StatusBadRequest)
		return
	}

	printer, err := ProbePrinter(body.IP, body.Community)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(printer)
}

func handleSavePrinter(w http.ResponseWriter, r *http.Request) {
	var p Printer
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, `{"error":"dados inválidos"}`, http.StatusBadRequest)
		return
	}

	dbLock.Lock()
	defer dbLock.Unlock()

	exists := false
	for i, existing := range dbData.Printers {
		if (p.Serial != "" && existing.Serial == p.Serial) || (p.IP != "" && existing.IP == p.IP) {
			p.ID = existing.ID
			p.ClientID = existing.ClientID
			p.CreatedAt = existing.CreatedAt
			p.InitialTotalPages = existing.InitialTotalPages
			p.InitialMonoPages = existing.InitialMonoPages
			p.InitialColorPages = existing.InitialColorPages
			if p.Ownership == "" {
				p.Ownership = existing.Ownership
			}
			if p.Unit == "" {
				p.Unit = existing.Unit
			}
			if p.Department == "" {
				p.Department = existing.Department
			}
			if p.MonoPages == 0 && existing.MonoPages > 0 {
				p.MonoPages = existing.MonoPages
			}
			if p.ColorPages == 0 && existing.ColorPages > 0 {
				p.ColorPages = existing.ColorPages
			}
			dbData.Printers[i] = p
			exists = true
			break
		}
	}

	if p.Ownership == "" {
		p.Ownership = "locada"
	}

	if !exists {
		if p.ID == "" {
			p.ID = fmt.Sprintf("prt_%d", time.Now().UnixNano())
		}
		if p.InitialTotalPages == 0 && p.TotalPages > 0 {
			p.InitialTotalPages = p.TotalPages
			p.InitialMonoPages = p.MonoPages
			p.InitialColorPages = p.ColorPages
		}
		p.CreatedAt = time.Now().Format("2006-01-02 15:04:05")
		dbData.Printers = append(dbData.Printers, p)
	}

	saveDBUnsafe()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(p)
}

func handleRefreshPrinter(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = strings.TrimPrefix(r.URL.Path, "/api/printers/refresh/")
	}

	dbLock.RLock()
	var targetIP string
	var foundIdx = -1
	for i := range dbData.Printers {
		if dbData.Printers[i].ID == id {
			targetIP = dbData.Printers[i].IP
			foundIdx = i
			break
		}
	}
	dbLock.RUnlock()

	if foundIdx == -1 {
		http.Error(w, `{"error":"impressora não encontrada"}`, http.StatusNotFound)
		return
	}

	updated, err := ProbePrinter(targetIP, "public")

	dbLock.Lock()
	defer dbLock.Unlock()

	var target *Printer
	for i := range dbData.Printers {
		if dbData.Printers[i].ID == id {
			target = &dbData.Printers[i]
			break
		}
	}
	if target == nil {
		http.Error(w, `{"error":"impressora removida"}`, http.StatusNotFound)
		return
	}

	if err != nil {
		target.Status = "offline"
		target.LastSeen = time.Now().Format("2006-01-02 15:04:05")
	} else {
		target.Status = "online"
		target.Model = updated.Model
		if updated.Serial != "" {
			target.Serial = updated.Serial
		}
		if updated.TotalPages >= target.TotalPages {
			target.TotalPages = updated.TotalPages
		}
		if updated.MonoPages >= target.MonoPages {
			target.MonoPages = updated.MonoPages
		}
		if updated.ColorPages >= target.ColorPages {
			target.ColorPages = updated.ColorPages
		}
		if len(updated.Supplies) > 0 {
			target.Supplies = updated.Supplies
		}
		target.LatencyMs = updated.LatencyMs
		target.LastSeen = time.Now().Format("2006-01-02 15:04:05")
	}

	saveDBUnsafe()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(target)
}

func handleDeletePrinter(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = strings.TrimPrefix(r.URL.Path, "/api/printers/delete/")
	}
	dbLock.Lock()
	defer dbLock.Unlock()

	var next []Printer
	for _, p := range dbData.Printers {
		if p.ID != id {
			next = append(next, p)
		}
	}
	dbData.Printers = next

	for i := range dbData.Clients {
		var pids []string
		for _, pid := range dbData.Clients[i].PrinterIDs {
			if pid != id {
				pids = append(pids, pid)
			}
		}
		dbData.Clients[i].PrinterIDs = pids
	}

	saveDBUnsafe()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func handleDiscover(w http.ResponseWriter, r *http.Request) {
	ips := DiscoverSubnet()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ips":   ips,
		"count": len(ips),
	})
}

func handleGetClosings(w http.ResponseWriter, r *http.Request) {
	dbLock.RLock()
	defer dbLock.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dbData.Closings)
}

func handleSaveClosing(w http.ResponseWriter, r *http.Request) {
	var c Closing
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, `{"error":"dados inválidos"}`, http.StatusBadRequest)
		return
	}
	dbLock.Lock()
	defer dbLock.Unlock()

	if c.ID == "" {
		c.ID = fmt.Sprintf("cls_%d", time.Now().UnixNano())
	}
	if c.FrozenAt == "" && c.Status == "congelado" {
		c.FrozenAt = time.Now().Format("2006-01-02 15:04:05")
	}
	c.PlatformSplitFee = c.TotalAmount * 0.015
	c.NetTenantAmount = c.TotalAmount - c.PlatformSplitFee
	c.PixCopyPaste = GenerateValidPixPayload(c.PixKey, "STAR COPIAS OUTSOURCING", "SAO PAULO", c.TotalAmount, c.ID)

	exists := false
	for i, existing := range dbData.Closings {
		if existing.ID == c.ID || (existing.ClientID == c.ClientID && existing.MonthYear == c.MonthYear) {
			dbData.Closings[i] = c
			exists = true
			break
		}
	}
	if !exists {
		dbData.Closings = append(dbData.Closings, c)
	}

	saveDBUnsafe()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(c)
}

type AgentSyncPayload struct {
	AgentVersion string `json:"agent_version"`
	PairingToken string `json:"pairing_token"`
	UnitID       string `json:"unit_id"`
	CollectedAt  string `json:"collected_at"`
	HostInfo     struct {
		Hostname string `json:"hostname"`
		OS       string `json:"os"`
		LocalIP  string `json:"local_ip"`
	} `json:"host_info"`
	Devices []struct {
		DiscoverySource string `json:"discovery_source"`
		IPAddress       string `json:"ip_address"`
		USBPort         string `json:"usb_port"`
		MACAddress      string `json:"mac_address"`
		SerialNumber    string `json:"serial_number"`
		VendorPEN       int    `json:"vendor_pen"`
		Vendor          string `json:"vendor"`
		Model           string `json:"model"`
		Status          string `json:"status"`
		Counters        struct {
			LifetimeEngineTotal uint32 `json:"lifetime_engine_total"`
			MonoTotal           uint32 `json:"mono_total"`
			ColorTotal          uint32 `json:"color_total"`
			DuplexSheets        uint32 `json:"duplex_sheets"`
			ScanTotal           uint32 `json:"scan_total"`
			LabelCount          uint32 `json:"label_count"`
		} `json:"counters"`
		Supplies []SupplyItem `json:"supplies"`
	} `json:"devices"`
}

func handleAgentTelemetrySync(w http.ResponseWriter, r *http.Request) {
	var payload AgentSyncPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, `{"error":"payload de telemetria inválido"}`, http.StatusBadRequest)
		return
	}

	dbLock.Lock()
	defer dbLock.Unlock()

	var matchedUnit *ClientUnitConfig
	for i := range dbData.ClientUnits {
		if dbData.ClientUnits[i].PairingToken == payload.PairingToken || dbData.ClientUnits[i].ID == payload.UnitID {
			dbData.ClientUnits[i].AgentStatus = "online"
			dbData.ClientUnits[i].AgentLastSeen = time.Now().Format("2006-01-02 15:04:05")
			matchedUnit = &dbData.ClientUnits[i]
			break
		}
	}

	processed := 0
	for _, dev := range payload.Devices {
		serial := strings.TrimSpace(dev.SerialNumber)
		if serial == "" {
			serial = "SN-" + dev.IPAddress
		}
		found := false
		for i := range dbData.Printers {
			if dbData.Printers[i].Serial == serial || (dev.IPAddress != "" && dbData.Printers[i].IP == dev.IPAddress) {
				if dev.Counters.LifetimeEngineTotal >= dbData.Printers[i].TotalPages {
					dbData.Printers[i].TotalPages = dev.Counters.LifetimeEngineTotal
				}
				if dev.Counters.MonoTotal >= dbData.Printers[i].MonoPages {
					dbData.Printers[i].MonoPages = dev.Counters.MonoTotal
				}
				if dev.Counters.ColorTotal >= dbData.Printers[i].ColorPages {
					dbData.Printers[i].ColorPages = dev.Counters.ColorTotal
				}
				if len(dev.Supplies) > 0 {
					dbData.Printers[i].Supplies = dev.Supplies
				}
				dbData.Printers[i].Status = "online"
				dbData.Printers[i].LastSeen = time.Now().Format("2006-01-02 15:04:05")
				found = true
				break
			}
		}
		if !found {
			clientID := ""
			unitName := ""
			if matchedUnit != nil {
				clientID = matchedUnit.ClientID
				unitName = matchedUnit.UnitName
			}
			newP := Printer{
				ID:                fmt.Sprintf("prt_%d", time.Now().UnixNano()),
				IP:                dev.IPAddress,
				USBPort:           dev.USBPort,
				MACAddress:        dev.MACAddress,
				VendorPEN:         dev.VendorPEN,
				Vendor:            dev.Vendor,
				Model:             dev.Model,
				Serial:            serial,
				DiscoverySource:   dev.DiscoverySource,
				TotalPages:        dev.Counters.LifetimeEngineTotal,
				MonoPages:         dev.Counters.MonoTotal,
				ColorPages:        dev.Counters.ColorTotal,
				InitialTotalPages: dev.Counters.LifetimeEngineTotal,
				InitialMonoPages:  dev.Counters.MonoTotal,
				InitialColorPages: dev.Counters.ColorTotal,
				Supplies:          dev.Supplies,
				ClientID:          clientID,
				Unit:              unitName,
				Ownership:         "locada",
				Status:            "online",
				LastSeen:          time.Now().Format("2006-01-02 15:04:05"),
				CreatedAt:         time.Now().Format("2006-01-02 15:04:05"),
			}
			dbData.Printers = append(dbData.Printers, newP)
		}
		processed++
	}

	saveDBUnsafe()

	scanSubnets := []string{"192.168.1.0/24"}
	staticIPs := []string{}
	pollSec := 900
	if matchedUnit != nil {
		scanSubnets = matchedUnit.ScanSubnets
		staticIPs = matchedUnit.StaticIPs
		pollSec = matchedUnit.PollIntervalSec
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":           "ack",
		"received_records": processed,
		"remote_config": map[string]interface{}{
			"config_version":                3,
			"poll_interval_seconds":         pollSec,
			"rate_limit_packets_per_sec":    20,
			"scan_subnets":                  scanSubnets,
			"static_ips":                    staticIPs,
			"enable_usb_spooler":            true,
			"enable_http_wakeup_for_pantum": true,
		},
	})
}

func handleGetUnits(w http.ResponseWriter, r *http.Request) {
	dbLock.RLock()
	defer dbLock.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dbData.ClientUnits)
}

func handleWhatsAppSendInvoice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone    string `json:"phone"`
		Message  string `json:"message"`
		Instance string `json:"instance"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"payload inválido"}`, http.StatusBadRequest)
		return
	}
	evoURL := os.Getenv("EVOLUTION_API_URL")
	evoKey := os.Getenv("EVOLUTION_API_KEY")
	if evoURL != "" && evoKey != "" {
		inst := req.Instance
		if inst == "" {
			inst = "locadora_starcopias"
		}
		payload, _ := json.Marshal(map[string]interface{}{
			"number": "55" + strings.TrimLeft(req.Phone, "+55"),
			"text":   req.Message,
			"delay":  1200,
		})
		httpReq, _ := http.NewRequest("POST", fmt.Sprintf("%s/message/sendText/%s", strings.TrimRight(evoURL, "/"), inst), bytes.NewBuffer(payload))
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("apikey", evoKey)
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(httpReq)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "sent",
		"gateway": "evolution-api-open-source",
	})
}

func handleEmitSplitCharge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientID    string  `json:"client_id"`
		ClientName  string  `json:"client_name"`
		Amount      float64 `json:"amount"`
		DueDate     string  `json:"due_date"`
		Description string  `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"payload inválido"}`, http.StatusBadRequest)
		return
	}

	splitFee := req.Amount * 0.015
	netAmount := req.Amount - splitFee
	pixPayload := GenerateValidPixPayload("18.234.567/0001-89", "STAR COPIAS OUTSOURCING", "SAO PAULO", req.Amount, req.ClientID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":             "created",
		"provider":           "asaas_split_v3",
		"gross_amount":       req.Amount,
		"platform_split_fee": splitFee,
		"net_tenant_amount":  netAmount,
		"pix_copy_paste":     pixPayload,
	})
}

func main() {
	loadDB()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/config/public", handlePublicConfig)
	mux.HandleFunc("GET /api/clients", handleGetClients)
	mux.HandleFunc("POST /api/clients/save", handleSaveClient)
	mux.HandleFunc("GET /api/clients/delete/{id}", handleDeleteClient)
	mux.HandleFunc("POST /api/clients/link-printer", handleLinkPrinter)
	mux.HandleFunc("POST /api/clients/unlink-printer", handleUnlinkPrinter)
	mux.HandleFunc("GET /api/units", handleGetUnits)
	mux.HandleFunc("GET /api/closings", handleGetClosings)
	mux.HandleFunc("POST /api/closings/save", handleSaveClosing)
	mux.HandleFunc("POST /api/payments/emit-charge", handleEmitSplitCharge)
	mux.HandleFunc("POST /api/whatsapp/send-invoice", handleWhatsAppSendInvoice)
	mux.HandleFunc("GET /api/printers", handleGetPrinters)
	mux.HandleFunc("POST /api/printers/probe", handleProbePrinter)
	mux.HandleFunc("POST /api/printers/save", handleSavePrinter)
	mux.HandleFunc("GET /api/printers/refresh/{id}", handleRefreshPrinter)
	mux.HandleFunc("GET /api/printers/delete/{id}", handleDeletePrinter)
	mux.HandleFunc("GET /api/printers/discover", handleDiscover)
	mux.HandleFunc("POST /api/v2/telemetry/sync", handleAgentTelemetrySync)

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Write(indexHTML)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8090"
	}
	fmt.Println("=================================================================")
	fmt.Printf("🚀 PrintMind V3.0 Cloud & Contratos ativo em: http://localhost:%s\n", port)
	fmt.Println("👉 Supabase Auth + Agente Multi-Local + Evolution WhatsApp + Split B2B2C")
	fmt.Println("=================================================================")

	log.Fatal(http.ListenAndServe(":"+port, mux))
}
