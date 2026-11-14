package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseHPCDMSuppliesPublicStateOnlyAndPrintheads(t *testing.T) {
	payload := []byte(`{
		"suppliesList": [
			{
				"supplyType": "inkTank",
				"supplyColorCode": "K",
				"supplyState": "ok",
				"percentLifeDisplay": 100,
				"isGenuineHP": "true"
			},
			{
				"supplyType": "inkTank",
				"supplyColorCode": "C",
				"supplyState": "low",
				"percentLifeDisplay": 100
			},
			{
				"supplyType": "inkTank",
				"supplyColorCode": "M",
				"supplyState": "empty",
				"percentLifeDisplay": 100
			},
			{
				"supplyType": "inkTank",
				"supplyColorCode": "Y",
				"supplyState": "ok",
				"percentLifeDisplay": 100
			},
			{
				"supplyType": "inkCartridge",
				"supplyColorCode": "CMY",
				"supplyState": "inform",
				"productNumber": "M0H50A",
				"serialNumber": "PH-CMY-9911",
				"manufactureDate": "2025-02-14T00:00:00Z",
				"isGenuineHP": true
			},
			{
				"supplyType": "inkCartridge",
				"supplyColorCode": "K",
				"supplyState": "ok",
				"productNumber": "X4E75A",
				"serialNumber": "PH-BLK-7722",
				"manufactureDate": "2025-02-15T00:00:00Z",
				"isGenuineHP": "true"
			}
		]
	}`)

	items, err := ParseHPCDMSuppliesPublic(payload)
	if err != nil {
		t.Fatalf("ParseHPCDMSuppliesPublic failed: %v", err)
	}
	if len(items) != 6 {
		t.Fatalf("expected 6 items (4 ink tanks + 2 printheads), got %d", len(items))
	}

	// Verify HP Smart Tank tanks NEVER report fake 100% and preserve real supply_state
	expectedStates := []string{"ok", "low", "empty", "ok"}
	for i := 0; i < 4; i++ {
		if items[i].Percent != nil {
			t.Fatalf("inkTank[%d] must have nil Percent (no fake 100%%), got %d", i, *items[i].Percent)
		}
		if items[i].MeasurementMode != "state_only" || items[i].LevelState != "state_only" {
			t.Fatalf("inkTank[%d] expected state_only, got mode=%q state=%q", i, items[i].MeasurementMode, items[i].LevelState)
		}
		if items[i].SupplyState != expectedStates[i] {
			t.Fatalf("inkTank[%d] expected supply_state=%q, got %q", i, expectedStates[i], items[i].SupplyState)
		}
	}

	// Verify printheads capture part number, serial, manufacture date, normalized inform->ok state, and genuine boolean
	phCMY := items[4]
	if phCMY.PartNumber != "M0H50A" || phCMY.SerialNumber != "PH-CMY-9911" || phCMY.SupplyType != "printhead" || phCMY.SupplyState != "ok" {
		t.Fatalf("unexpected printhead CMY metadata: %+v", phCMY)
	}
	if phCMY.IsGenuine == nil || !*phCMY.IsGenuine {
		t.Fatalf("expected IsGenuine=true for printhead CMY")
	}
	if !strings.HasPrefix(phCMY.ManufactureDate, "2025-02-14") {
		t.Fatalf("expected manufacture date, got %q", phCMY.ManufactureDate)
	}
}

func TestParseHPCDMLifetimeCountersAndMerge(t *testing.T) {
	raw := []byte(`{
		"printUsage": {
			"impressions": { "monochrome": 140, "color": 360, "total": 500 },
			"sheets": { "simplex": 420, "duplex": 40, "total": 460 }
		},
		"scanUsage": { "totalImages": 85 }
	}`)
	counters, err := ParseHPCDMLifetimeCounters(raw)
	if err != nil {
		t.Fatalf("ParseHPCDMLifetimeCounters failed: %v", err)
	}
	if counters.LifetimeEngineTotal != 500 || counters.MonoTotal != 140 || counters.ColorTotal != 360 || counters.DuplexSheets != 40 || counters.ScanTotal != 85 {
		t.Fatalf("unexpected CDM counters: %+v", counters)
	}

	// Verify SNMP fallback that put all 500 pages into MonoTotal and 0 into ColorTotal gets corrected by CDM merge
	dev := &DiscoveredDevice{
		Vendor:         "HP",
		Model:          "HP Smart Tank 580-590 series",
		OdometerSource: "snmp",
		Counters: DeviceCounters{
			LifetimeEngineTotal: 500,
			MonoTotal:           500,
			ColorTotal:          0,
		},
		Supplies: []SupplyItem{
			{Name: "tri-color ink cartridge", Color: "color", Level: -2, LevelState: "unknown"},
		},
	}
	cdm := &HPCDMData{
		Model:        "HP Smart Tank 580-590 series",
		SerialNumber: "BR55Z12345",
		Counters:     counters,
		Supplies: []SupplyItem{
			{Name: "Tanque de Tinta Preta (K)", Color: "black", MeasurementMode: "state_only", LevelState: "state_only", SupplyState: "ok", Level: -3, MaxCapacity: 100},
		},
	}
	MergeIPPAndHPCDM(dev, cdm)

	if dev.Counters.ColorTotal != 360 || dev.Counters.MonoTotal != 140 || !dev.IsColorCapable {
		t.Fatalf("expected CDM color/mono split (140/360), got %+v (colorCapable=%v)", dev.Counters, dev.IsColorCapable)
	}
	if len(dev.Supplies) != 1 || dev.Supplies[0].Name != "Tanque de Tinta Preta (K)" {
		t.Fatalf("expected tri-color unknown supply to be replaced by CDM supply, got %+v", dev.Supplies)
	}
}

func TestLocalHTTPClientPreservesPOSTRedirectAndBody(t *testing.T) {
	var receivedMethod string
	var receivedCT string
	var receivedBody []byte

	targetTLS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedCT = r.Header.Get("Content-Type")
		receivedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	defer targetTLS.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetTLS.URL+"/ipp/print", http.StatusMovedPermanently)
	}))
	defer redirector.Close()

	client := newLocalHTTPClient(5 * 1e9)
	payload := []byte{0x02, 0x00, 0x00, 0x0B, 0xDE, 0xAD, 0xBE, 0xEF}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, redirector.URL+"/ipp/print", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/ipp")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do across 301->TLS failed: %v", err)
	}
	_ = resp.Body.Close()

	if receivedMethod != http.MethodPost {
		t.Fatalf("expected redirected method POST, got %q", receivedMethod)
	}
	if receivedCT != "application/ipp" {
		t.Fatalf("expected Content-Type application/ipp preserved, got %q", receivedCT)
	}
	if !bytes.Equal(receivedBody, payload) {
		t.Fatalf("expected binary IPP body preserved across 301 redirect, got %x", receivedBody)
	}
}

func buildMockIPPAttrBytes(tag byte, name string, values [][]byte) []byte {
	var buf bytes.Buffer
	for i, v := range values {
		buf.WriteByte(tag)
		if i == 0 {
			_ = binary.Write(&buf, binary.BigEndian, uint16(len(name)))
			buf.WriteString(name)
		} else {
			_ = binary.Write(&buf, binary.BigEndian, uint16(0))
		}
		_ = binary.Write(&buf, binary.BigEndian, uint16(len(v)))
		buf.Write(v)
	}
	return buf.Bytes()
}

func ippInt32Bytes(v int32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(v))
	return b[:]
}

func TestParseIPPPrinterAttributesTankStatic100BecomesStateOnly(t *testing.T) {
	var wire bytes.Buffer
	wire.Write([]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01})
	wire.WriteByte(ippTagPrinterAttrs)
	wire.Write(buildMockIPPAttrBytes(0x41, "printer-make-and-model", [][]byte{[]byte("HP Smart Tank 580-590 series")}))
	wire.Write(buildMockIPPAttrBytes(0x41, "marker-names", [][]byte{[]byte("black ink"), []byte("cyan ink")}))
	wire.Write(buildMockIPPAttrBytes(0x21, "marker-levels", [][]byte{ippInt32Bytes(100), ippInt32Bytes(100)}))
	wire.Write(buildMockIPPAttrBytes(0x44, "marker-types", [][]byte{[]byte("ink-cartridge"), []byte("ink-cartridge")}))
	wire.Write(buildMockIPPAttrBytes(0x44, "printer-state-reasons", [][]byte{[]byte("marker-supply-low-warning")}))
	wire.WriteByte(ippTagEndOfAttrs)

	resp, err := ParseIPPResponse(wire.Bytes())
	if err != nil {
		t.Fatalf("ParseIPPResponse failed: %v", err)
	}
	dev, ok := ParseIPPPrinterAttributes("192.168.1.13", "ipp", resp)
	if !ok {
		t.Fatalf("ParseIPPPrinterAttributes returned ok=false")
	}
	if len(dev.Supplies) != 2 {
		t.Fatalf("expected 2 supplies, got %d", len(dev.Supplies))
	}
	for _, s := range dev.Supplies {
		if s.Percent != nil {
			t.Fatalf("Smart Tank static-100 IPP supply must have nil Percent, got %d", *s.Percent)
		}
		if s.MeasurementMode != "state_only" || s.LevelState != "state_only" || s.SupplyState != "low" {
			t.Fatalf("unexpected tank state_only supply: %+v", s)
		}
	}
}

func TestExtractTokenFromExeNameCopySuffixes(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{`C:\Users\Admin\Downloads\printmind-agent__pm_live_0123456789abcdef.exe`, "pm_live_0123456789abcdef"},
		{`C:\Users\Admin\Downloads\printmind-agent__pm_live_0123456789abcdef (1).exe`, "pm_live_0123456789abcdef"},
		{`C:\Users\Admin\Downloads\printmind-agent__pm_live_0123456789abcdef(2).exe`, "pm_live_0123456789abcdef"},
		{`C:\Users\Admin\Downloads\printmind-agent__pm_live_0123456789abcdef - Copia.exe`, "pm_live_0123456789abcdef"},
		{`C:\Users\Admin\Downloads\printmind-agent__pm_live_0123456789abcdef - Copy (3).exe`, "pm_live_0123456789abcdef"},
		{`C:\Users\Admin\Downloads\printmind-agent.exe`, ""},
	}
	for _, tc := range cases {
		if got := extractTokenFromExeName(tc.path); got != tc.want {
			t.Errorf("extractTokenFromExeName(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestIPCServerLoopbackStatusAndScanNowAuth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	forceCh := make(chan struct{}, 1)
	addr, tok, _, err := StartIPCServer(
		ctx,
		func() bool {
			select {
			case forceCh <- struct{}{}:
				return false
			default:
				return true
			}
		},
		func() AgentStatusSnapshot {
			return AgentStatusSnapshot{
				AgentVersion:  AgentVersion,
				State:         "online",
				PrintersCount: 4,
				IPCAddr:       "127.0.0.1:0",
				IPCToken:      "should_never_leak",
			}
		},
	)
	if err != nil {
		t.Fatalf("StartIPCServer failed: %v", err)
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("IPC must bind strictly to 127.0.0.1, got %q", addr)
	}

	// 1. Unauthorized POST /scan-now without token -> 401
	unauthResp, err := http.Post("http://"+addr+"/scan-now", "application/json", nil)
	if err != nil {
		t.Fatalf("unauth request failed: %v", err)
	}
	_ = unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing IPC token, got %d", unauthResp.StatusCode)
	}

	// 2. Authorized GET /status -> 200 and stripped IPCToken
	reqStatus, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/status", nil)
	reqStatus.Header.Set(ipcHeaderToken, tok)
	stResp, err := http.DefaultClient.Do(reqStatus)
	if err != nil {
		t.Fatalf("GET /status failed: %v", err)
	}
	var snap AgentStatusSnapshot
	_ = json.NewDecoder(stResp.Body).Decode(&snap)
	_ = stResp.Body.Close()
	if stResp.StatusCode != http.StatusOK || snap.PrintersCount != 4 || snap.IPCToken != "" {
		t.Fatalf("unexpected /status response (%d): %+v", stResp.StatusCode, snap)
	}

	// 3. Atomic status.json + SendIPCScanNow flow
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "status.json")
	if err := WriteStatusSnapshot(statusPath, AgentStatusSnapshot{
		AgentVersion:  AgentVersion,
		State:         "online",
		PrintersCount: 4,
		IPCAddr:       addr,
		IPCToken:      tok,
	}); err != nil {
		t.Fatalf("WriteStatusSnapshot failed: %v", err)
	}

	alreadyPending, err := SendIPCScanNow(statusPath)
	if err != nil || alreadyPending {
		t.Fatalf("first SendIPCScanNow got pending=%v err=%v", alreadyPending, err)
	}
	alreadyPending2, err := SendIPCScanNow(statusPath)
	if err != nil || !alreadyPending2 {
		t.Fatalf("second SendIPCScanNow expected alreadyPending=true, got pending=%v err=%v", alreadyPending2, err)
	}

	select {
	case <-forceCh:
	default:
		t.Fatalf("expected forceScanCh to be triggered by IPC /scan-now")
	}
}

func TestResolveTrayReadOnlyPathsCreatesNoDirectoriesOnDisk(t *testing.T) {
	base := t.TempDir()
	ghostDir := filepath.Join(base, "nonexistent-tray-dir")
	explicitCfg := filepath.Join(ghostDir, "config.json")

	paths := resolveTrayReadOnlyPaths(explicitCfg)
	if paths.DataDir != ghostDir {
		t.Fatalf("expected DataDir %q, got %q", ghostDir, paths.DataDir)
	}
	if paths.StatusPath != filepath.Join(ghostDir, "status.json") {
		t.Fatalf("expected StatusPath inside %q, got %q", ghostDir, paths.StatusPath)
	}
	if _, err := os.Stat(ghostDir); !os.IsNotExist(err) {
		t.Fatalf("expected resolveTrayReadOnlyPaths NOT to create %q on disk, got stat err=%v", ghostDir, err)
	}
}
