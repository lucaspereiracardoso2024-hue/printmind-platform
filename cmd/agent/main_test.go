package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsPrivateIPv4(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"192.168.1.10", true},
		{"10.20.30.40", true},
		{"172.16.0.1", true},
		{"172.31.255.254", true},
		{"172.32.0.1", false},
		{"169.254.10.20", false},
		{"127.0.0.1", false},
		{"8.8.8.8", false},
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if got := isPrivateIPv4(ip); got != tc.want {
			t.Errorf("isPrivateIPv4(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

func TestExpandCIDRToIPs(t *testing.T) {
	ips := expandCIDRToIPs("192.168.10.0/29")
	if len(ips) != 6 {
		t.Fatalf("expected 6 host IPs in /29, got %d (%v)", len(ips), ips)
	}
	if ips[0] != "192.168.10.1" || ips[len(ips)-1] != "192.168.10.6" {
		t.Errorf("unexpected host bounds: %v", ips)
	}
	single := expandCIDRToIPs("10.0.0.55")
	if len(single) != 1 || single[0] != "10.0.0.55" {
		t.Errorf("unexpected single IP result: %v", single)
	}
}

func TestEncodeOIDAndBERLength(t *testing.T) {
	oid, err := encodeOID("1.3.6.1.2.1.43.5.1.1.17.1")
	if err != nil {
		t.Fatalf("encodeOID failed: %v", err)
	}
	if len(oid) == 0 || oid[0] != 0x2B {
		t.Errorf("unexpected OID prefix: %x", oid)
	}

	l, n, err := parseBERLength([]byte{0x82, 0x01, 0x05})
	if err != nil || l != 261 || n != 3 {
		t.Errorf("parseBERLength long form got (%d, %d, %v), want (261, 3, nil)", l, n, err)
	}

	l2, n2, err2 := parseBERLength([]byte{0x1A})
	if err2 != nil || l2 != 26 || n2 != 1 {
		t.Errorf("parseBERLength short form got (%d, %d, %v), want (26, 1, nil)", l2, n2, err2)
	}
}

func TestDetectVendorAndSanitizeSerial(t *testing.T) {
	if v, pen := detectVendorFromModel("HP LaserJet Pro MFP M428fdw"); v != "HP" || pen != 11 {
		t.Errorf("got (%s, %d), want (HP, 11)", v, pen)
	}
	if v, pen := detectVendorFromModel("Brother HL-L5102DW"); v != "Brother" || pen != 2435 {
		t.Errorf("got (%s, %d), want (Brother, 2435)", v, pen)
	}
	if s := sanitizeSerial(" SN: BRBS-1234/56 \r\n"); s != "SNBRBS-123456" {
		t.Errorf("sanitizeSerial got %q, want %q", s, "SNBRBS-123456")
	}
}

func TestIsVirtualPrinter(t *testing.T) {
	if !isVirtualPrinter("Microsoft Print to PDF", "PORTPROMPT:", "Microsoft Print To PDF") {
		t.Error("expected Microsoft Print to PDF to be detected as virtual")
	}
	if isVirtualPrinter("Brother DCP-L2540DW", "USB001", "Brother Universal Printer") {
		t.Error("expected Brother USB printer not to be filtered as virtual")
	}
}

func TestLoadOrInitConfigMigratesLocalhostAndPersists(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "printmind-config.json")

	legacy := `{"server_url":"http://localhost:8090","pairing_token":"pm_custom_123","poll_interval_sec":120,"enable_usb":true}`
	if err := os.WriteFile(cfgPath, []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadOrInitConfig(cfgPath, "", "", "")
	if cfg.ServerURL != DefaultSupabaseRP {
		t.Errorf("expected localhost:8090 to migrate to DefaultSupabaseRP, got %q", cfg.ServerURL)
	}
	if cfg.PairingToken != "pm_custom_123" {
		t.Errorf("expected pairing token preserved, got %q", cfg.PairingToken)
	}
	if cfg.PollIntervalSec != 120 {
		t.Errorf("expected poll_interval_sec 120, got %d", cfg.PollIntervalSec)
	}
}

func TestSendTelemetrySupabaseRPCFormat(t *testing.T) {
	var gotAuth, gotAPIKey, gotUA string
	var gotBody SupabaseRPCPayload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("apikey")
		gotUA = r.Header.Get("User-Agent")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","synced_devices":1,"remote_config":{"poll_interval_seconds":300,"rate_limit_packets_per_sec":25,"scan_subnets":["192.168.1.0/24"]}}`))
	}))
	defer srv.Close()

	cfg := Config{
		ServerURL:       srv.URL + "/rest/v1/rpc/sync_agent_telemetry",
		APIKey:          "test-anon-key",
		PairingToken:    "pm_test_unit",
		PollIntervalSec: 300,
		EnableUSB:       true,
	}
	req := SyncRequest{
		AgentVersion: AgentVersion,
		PairingToken: cfg.PairingToken,
		CollectedAt:  time.Now().Format(time.RFC3339),
		Devices: []DiscoveredDevice{
			{Model: "HP M404", SerialNumber: "VNB123", IPAddress: "192.168.1.50", Status: "online"},
		},
	}

	resp, err := sendTelemetry(&http.Client{Timeout: 5 * time.Second}, cfg, req)
	if err != nil {
		t.Fatalf("sendTelemetry failed: %v", err)
	}
	if gotAPIKey != "test-anon-key" || gotAuth != "Bearer test-anon-key" {
		t.Errorf("unexpected auth headers: apikey=%q auth=%q", gotAPIKey, gotAuth)
	}
	if gotUA == "" || gotBody.PairingToken != "pm_test_unit" || len(gotBody.Payload.Devices) != 1 {
		t.Errorf("unexpected RPC payload or UA: ua=%q body=%+v", gotUA, gotBody)
	}
	if resp.Status != "success" || resp.SyncedCount != 1 {
		t.Errorf("unexpected sync response: %+v", resp)
	}
}

func TestExtractTokenFromExeName(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{`C:\Users\Test\Downloads\printmind-agent__pm_live_560af0c12f274ffa9b131ae5b1b7d7ad.exe`, "pm_live_560af0c12f274ffa9b131ae5b1b7d7ad"},
		{`C:\Users\Test\Downloads\printmind-agent__pm_live_abc123def (1).exe`, "pm_live_abc123def"},
		{`/home/user/printmind-agent-linux-amd64__pm_live_xyz789`, "pm_live_xyz789"},
		{`C:\Users\Test\Downloads\printmind-agent.exe`, ""},
		{`printmind-agent-linux-amd64`, ""},
	}
	for _, tc := range cases {
		if got := extractTokenFromExeName(tc.path); got != tc.want {
			t.Errorf("extractTokenFromExeName(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestLoadOrInitConfigIgnoresLegacyPlaceholder(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "printmind-config.json")

	legacy := `{"server_url":"https://zbsmzcbcivhzslcnfxmq.supabase.co/rest/v1/rpc/sync_agent_telemetry","pairing_token":"pm_live_hosp_central_01","poll_interval_sec":300,"enable_usb":true}`
	if err := os.WriteFile(cfgPath, []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadOrInitConfig(cfgPath, "", "", "")
	if cfg.PairingToken != "" {
		t.Errorf("expected legacy placeholder token to be cleared, got %q", cfg.PairingToken)
	}
}

func TestResolveTokenByEmail(t *testing.T) {
	var gotPath, gotEmail string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotEmail = body["p_email"]
		w.Header().Set("Content-Type", "application/json")
		if gotEmail == "ok@example.com" {
			_, _ = w.Write([]byte(`{"status":"success","pairing_token":"pm_live_resolved_99","unit_name":"Matriz"}`))
		} else {
			_, _ = w.Write([]byte(`{"status":"error","message":"Conta não encontrada"}`))
		}
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	tok, err := resolveTokenByEmail(client, srv.URL+"/rest/v1/rpc/sync_agent_telemetry", "anon", " ok@example.com ")
	if err != nil {
		t.Fatalf("resolveTokenByEmail expected success, got %v", err)
	}
	if gotPath != "/rest/v1/rpc/resolve_unit_token_by_email" || tok != "pm_live_resolved_99" {
		t.Errorf("got path=%q token=%q", gotPath, tok)
	}

	if _, err := resolveTokenByEmail(client, srv.URL+"/rest/v1/rpc/sync_agent_telemetry", "anon", "missing@example.com"); err == nil {
		t.Error("expected error when account not found")
	}
}

func TestSendTelemetryServerErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"error","message":"Token inválido ou conta suspensa"}`))
	}))
	defer srv.Close()

	cfg := Config{
		ServerURL:    srv.URL + "/rest/v1/rpc/sync_agent_telemetry",
		APIKey:       "test-key",
		PairingToken: "pm_invalid",
	}
	req := SyncRequest{AgentVersion: AgentVersion, PairingToken: "pm_invalid"}
	if _, err := sendTelemetry(&http.Client{Timeout: 5 * time.Second}, cfg, req); err == nil {
		t.Error("expected error when server returns status=error")
	}
}
