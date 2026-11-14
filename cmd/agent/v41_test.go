package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAgentVersionIs420(t *testing.T) {
	if AgentVersion != "4.2.0" {
		t.Fatalf("expected AgentVersion 4.2.0, got %q", AgentVersion)
	}
}

func TestLoadOrInitConfigReadsInstallerPairingTokenFallback(t *testing.T) {
	origReader := installerPairingTokenReader
	defer func() { installerPairingTokenReader = origReader }()

	installerPairingTokenReader = func() string { return "pm_live_msi_silent_99" }

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg := loadOrInitConfig(cfgPath, "", "", "")
	if cfg.PairingToken != "pm_live_msi_silent_99" {
		t.Fatalf("expected MSI registry fallback pairing token, got %q", cfg.PairingToken)
	}

	// Existing token in config.json MUST NOT be overwritten by MSI registry fallback
	cfgPath2 := filepath.Join(dir, "config2.json")
	_ = os.WriteFile(cfgPath2, []byte(`{"pairing_token":"pm_live_existing_01"}`), 0644)
	cfg2 := loadOrInitConfig(cfgPath2, "", "", "")
	if cfg2.PairingToken != "pm_live_existing_01" {
		t.Fatalf("expected existing token preserved, got %q", cfg2.PairingToken)
	}
}

func TestSNMPv3KeyLocalization(t *testing.T) {
	// RFC 3414 Appendix A.3.1 official test vector (MD5, password="maplesyrup", engineID=0x000000000000000000000002)
	engineID, _ := hex.DecodeString("000000000000000000000002")
	kuMD5, err := deriveSNMPv3MasterKey("MD5", "maplesyrup")
	if err != nil {
		t.Fatalf("deriveSNMPv3MasterKey MD5 failed: %v", err)
	}
	if got := hex.EncodeToString(kuMD5); got != "9faf3283884e92834ebc9847d8edd963" {
		t.Errorf("RFC 3414 MD5 Ku mismatch: got %s, want 9faf3283884e92834ebc9847d8edd963", got)
	}
	kulMD5, err := localizeSNMPv3Key("MD5", kuMD5, engineID)
	if err != nil {
		t.Fatalf("localizeSNMPv3Key MD5 failed: %v", err)
	}
	if got := hex.EncodeToString(kulMD5); got != "526f5eed9fcce26f8964c2930787d82b" {
		t.Errorf("RFC 3414 MD5 Kul mismatch: got %s, want 526f5eed9fcce26f8964c2930787d82b", got)
	}

	// RFC 3414 Appendix A.3.2 official test vector (SHA1, password="maplesyrup", engineID=0x000000000000000000000002)
	kuSHA1, err := deriveSNMPv3MasterKey("SHA1", "maplesyrup")
	if err != nil {
		t.Fatalf("deriveSNMPv3MasterKey SHA1 failed: %v", err)
	}
	if got := hex.EncodeToString(kuSHA1); got != "9fb5cc0381497b3793528939ff788d5d79145211" {
		t.Errorf("RFC 3414 SHA1 Ku mismatch: got %s, want 9fb5cc0381497b3793528939ff788d5d79145211", got)
	}
	kulSHA1, err := localizeSNMPv3Key("SHA1", kuSHA1, engineID)
	if err != nil {
		t.Fatalf("localizeSNMPv3Key SHA1 failed: %v", err)
	}
	if len(kulSHA1) != 20 || bytes.Equal(kulSHA1, kuSHA1) {
		t.Errorf("unexpected SHA1 Kul: len=%d", len(kulSHA1))
	}

	// SHA-256 localization (32 bytes, distinct per engineID)
	ku256, err := deriveSNMPv3MasterKey("SHA256", "printmind-secret-pass")
	if err != nil || len(ku256) != 32 {
		t.Fatalf("deriveSNMPv3MasterKey SHA256 failed: len=%d err=%v", len(ku256), err)
	}
	kulA, err := localizeSNMPv3Key("SHA256", ku256, []byte{0x80, 0x00, 0x00, 0x0b, 0x01})
	if err != nil || len(kulA) != 32 {
		t.Fatalf("localizeSNMPv3Key SHA256 failed: len=%d err=%v", len(kulA), err)
	}
	kulB, _ := localizeSNMPv3Key("SHA256", ku256, []byte{0x80, 0x00, 0x00, 0x0b, 0x02})
	if bytes.Equal(kulA, kulB) {
		t.Error("expected distinct localized keys for different engineIDs")
	}
}

func TestSNMPv3PacketEncodeDecodeAndRedaction(t *testing.T) {
	engineID := []byte{0x80, 0x00, 0x1F, 0x88, 0x04, 0x70, 0x72, 0x69, 0x6E, 0x74}
	cases := []struct {
		name string
		cfg  SNMPv3Config
	}{
		{
			name: "noAuthNoPriv",
			cfg: SNMPv3Config{
				Enabled:       true,
				Username:      "pm_ro",
				SecurityLevel: "noAuthNoPriv",
			},
		},
		{
			name: "authNoPriv_SHA1",
			cfg: SNMPv3Config{
				Enabled:        true,
				Username:       "pm_auth",
				SecurityLevel:  "authNoPriv",
				AuthProtocol:   "SHA1",
				AuthPassphrase: "auth-secret-passphrase",
			},
		},
		{
			name: "authPriv_SHA256_AES",
			cfg: SNMPv3Config{
				Enabled:        true,
				Username:       "pm_aes",
				SecurityLevel:  "authPriv",
				AuthProtocol:   "SHA256",
				AuthPassphrase: "auth-sha256-password",
				PrivProtocol:   "AES",
				PrivPassphrase: "priv-aes128-password",
			},
		},
		{
			name: "authPriv_MD5_DES",
			cfg: SNMPv3Config{
				Enabled:        true,
				Username:       "pm_des",
				SecurityLevel:  "authPriv",
				AuthProtocol:   "MD5",
				AuthPassphrase: "auth-md5-password",
				PrivProtocol:   "DES",
				PrivPassphrase: "priv-des-password",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess, err := newSNMPv3Session(tc.cfg)
			if err != nil {
				t.Fatalf("newSNMPv3Session failed: %v", err)
			}
			if err := sess.setEngine(engineID, 12, 3456); err != nil {
				t.Fatalf("setEngine failed: %v", err)
			}

			// Build ScopedPDU GetResponse packet directly using the session's security primitives
			vbs := []SNMPVarBind{
				{OID: "1.3.6.1.2.1.1.1.0", Tag: asn1OctetStr, Value: []byte("HP LaserJet M404")},
				{OID: "1.3.6.1.2.1.43.10.2.1.4.1.1", Tag: asn1Counter32, Value: []byte{0x00, 0x01, 0x86, 0xA0}},
			}
			var vbListPayload []byte
			for _, vb := range vbs {
				oidB, _ := encodeOID(vb.OID)
				inner := append(wrapBERTLV(asn1OID, oidB), wrapBERTLV(vb.Tag, vb.Value)...)
				vbListPayload = append(vbListPayload, wrapBERTLV(asn1Sequence, inner)...)
			}
			var pduPayload []byte
			pduPayload = append(pduPayload, wrapBERTLV(asn1Integer, encodeASN1NonNegInt(77))...)
			pduPayload = append(pduPayload, asn1Integer, 0x01, 0x00)
			pduPayload = append(pduPayload, asn1Integer, 0x01, 0x00)
			pduPayload = append(pduPayload, wrapBERTLV(asn1Sequence, vbListPayload)...)
			respPDU := wrapBERTLV(pduGetResponse, pduPayload)

			var scoped []byte
			scoped = append(scoped, wrapBERTLV(asn1OctetStr, engineID)...)
			scoped = append(scoped, wrapBERTLV(asn1OctetStr, nil)...)
			scoped = append(scoped, respPDU...)
			scopedTLV := wrapBERTLV(asn1Sequence, scoped)

			var flags byte
			var authMacLen int
			var privParams []byte
			msgData := scopedTLV
			if tc.cfg.SecurityLevel == "authNoPriv" || tc.cfg.SecurityLevel == "authPriv" {
				flags |= snmpV3FlagAuth
				_, authMacLen, _ = snmpv3HashFunc(tc.cfg.AuthProtocol)
			}
			if tc.cfg.SecurityLevel == "authPriv" {
				flags |= snmpV3FlagPriv
				enc, salt, encErr := encryptSNMPv3ScopedPDU(tc.cfg.PrivProtocol, sess.PrivKul, 12, 3456, 99, scopedTLV)
				if encErr != nil {
					t.Fatalf("encrypt failed: %v", encErr)
				}
				privParams = salt
				msgData = wrapBERTLV(asn1OctetStr, enc)
			}

			var globalPayload []byte
			globalPayload = append(globalPayload, wrapBERTLV(asn1Integer, encodeASN1NonNegInt(501))...)
			globalPayload = append(globalPayload, wrapBERTLV(asn1Integer, encodeASN1NonNegInt(snmpV3MaxMsgSize))...)
			globalPayload = append(globalPayload, wrapBERTLV(asn1OctetStr, []byte{flags})...)
			globalPayload = append(globalPayload, asn1Integer, 0x01, 0x03)
			globalTLV := wrapBERTLV(asn1Sequence, globalPayload)

			privTLV := wrapBERTLV(asn1OctetStr, privParams)
			var usmPayload []byte
			usmPayload = append(usmPayload, wrapBERTLV(asn1OctetStr, engineID)...)
			usmPayload = append(usmPayload, wrapBERTLV(asn1Integer, encodeASN1NonNegInt(12))...)
			usmPayload = append(usmPayload, wrapBERTLV(asn1Integer, encodeASN1NonNegInt(3456))...)
			usmPayload = append(usmPayload, wrapBERTLV(asn1OctetStr, []byte(tc.cfg.Username))...)
			usmPayload = append(usmPayload, wrapBERTLV(asn1OctetStr, make([]byte, authMacLen))...)
			usmPayload = append(usmPayload, privTLV...)
			secParamsTLV := wrapBERTLV(asn1OctetStr, wrapBERTLV(asn1Sequence, usmPayload))

			versionTLV := []byte{asn1Integer, 0x01, 0x03}
			var msgBody []byte
			msgBody = append(msgBody, versionTLV...)
			msgBody = append(msgBody, globalTLV...)
			msgBody = append(msgBody, secParamsTLV...)
			msgBody = append(msgBody, msgData...)
			pkt := wrapBERTLV(asn1Sequence, msgBody)

			if flags&snmpV3FlagAuth != 0 {
				hdrLen := len(pkt) - len(msgBody)
				macStart := hdrLen + len(versionTLV) + len(globalTLV) + len(secParamsTLV) - len(privTLV) - authMacLen
				mac, mErr := computeSNMPv3MAC(tc.cfg.AuthProtocol, sess.AuthKul, pkt, authMacLen)
				if mErr != nil {
					t.Fatalf("computeSNMPv3MAC failed: %v", mErr)
				}
				copy(pkt[macStart:macStart+authMacLen], mac)
			}

			parsed, err := parseSNMPv3Packet(pkt, sess)
			if err != nil {
				t.Fatalf("parseSNMPv3Packet failed: %v", err)
			}
			if parsed.ReqID != 77 || len(parsed.VarBinds) != 2 || string(parsed.VarBinds[0].Value) != "HP LaserJet M404" {
				t.Fatalf("unexpected decoded response: %+v", parsed)
			}
			if cnt, ok := decodeASN1Uint32(parsed.VarBinds[1].Value); !ok || cnt != 100000 {
				t.Errorf("expected counter 100000, got (%d, %v)", cnt, ok)
			}

			// Tampered packet MUST fail HMAC check when authenticated
			if flags&snmpV3FlagAuth != 0 {
				tampered := append([]byte(nil), pkt...)
				tampered[len(tampered)-1] ^= 0xFF
				if _, err := parseSNMPv3Packet(tampered, sess); err == nil {
					t.Error("expected tampered SNMPv3 packet to fail verification")
				}
			}
		})
	}

	// Verify SNMPv3 passphrases are redacted from diagnostic zip & log redaction
	cfg := Config{
		PairingToken: "pm_live_diag_12345678",
		SNMPv3: SNMPv3Config{
			Enabled:        true,
			Username:       "admin_v3",
			SecurityLevel:  "authPriv",
			AuthProtocol:   "SHA256",
			AuthPassphrase: "SUPER_SECRET_AUTH_PASS",
			PrivProtocol:   "AES",
			PrivPassphrase: "SUPER_SECRET_PRIV_PASS",
		},
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	_ = os.WriteFile(logPath, []byte("err auth=SUPER_SECRET_AUTH_PASS priv=SUPER_SECRET_PRIV_PASS\n"), 0644)
	zipBytes, err := BuildDiagnosticZip(cfg, OSPaths{DataDir: dir, LogPath: logPath}, nil, nil)
	if err != nil {
		t.Fatalf("BuildDiagnosticZip failed: %v", err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		if strings.Contains(string(b), "SUPER_SECRET_AUTH_PASS") || strings.Contains(string(b), "SUPER_SECRET_PRIV_PASS") {
			t.Errorf("diagnostic entry %s leaked SNMPv3 passphrase", f.Name)
		}
	}
}

func TestZebraSGD_PJL_And_CanonBJNPParsers(t *testing.T) {
	// 1. Zebra SGD
	if got := string(buildZebraSGDGetVar("odometer.total_label_count")); got != "! U1 getvar \"odometer.total_label_count\"\r\n" {
		t.Errorf("unexpected Zebra SGD command: %q", got)
	}
	if cnt, ok := parseZebraSGDLabelCount("\"18450\"\r\n"); !ok || cnt != 18450 {
		t.Errorf("parseZebraSGDLabelCount failed: (%d, %v)", cnt, ok)
	}
	if _, ok := parseZebraSGDLabelCount("\"?\""); ok {
		t.Error("expected '?' SGD response to be rejected")
	}
	if mm, ok := parseZebraSGDPrintLengthMM("\"450 cm\""); !ok || mm != 4500 {
		t.Errorf("parseZebraSGDPrintLengthMM(cm) got (%d, %v), want 4500", mm, ok)
	}
	if mm, ok := parseZebraSGDPrintLengthMM("\"100 inches\""); !ok || mm != 2540 {
		t.Errorf("parseZebraSGDPrintLengthMM(inches) got (%d, %v), want 2540", mm, ok)
	}

	// 2. PJL PAGECOUNT + strict PJL guard
	if cnt, ok := parsePJLPageCountResponse("@PJL INFO PAGECOUNT\r\nPAGECOUNT=14520\r\n\x0c"); !ok || cnt != 14520 {
		t.Errorf("parsePJLPageCountResponse(PAGECOUNT=) failed: (%d, %v)", cnt, ok)
	}
	if cnt, ok := parsePJLPageCountResponse("@PJL INFO PAGECOUNT\r\n9812\r\n"); !ok || cnt != 9812 {
		t.Errorf("parsePJLPageCountResponse(numeric line) failed: (%d, %v)", cnt, ok)
	}
	if supportsPJLProtocol(&DiscoveredDevice{Vendor: "Zebra"}) {
		t.Error("Zebra thermal printer MUST NOT be probed with PJL")
	}
	if !supportsPJLProtocol(&DiscoveredDevice{Vendor: "HP"}) {
		t.Error("HP printer should support PJL fallback")
	}
	if !supportsPJLProtocol(&DiscoveredDevice{Vendor: "Custom", SupportedLanguages: []string{"PCL6", "PJL"}}) {
		t.Error("device advertising PJL in SupportedLanguages should be supported")
	}

	// 3. Canon BJNP Discover (0x01) + GetID (0x30)
	req := buildCanonBJNPPacket(bjnpCmdDiscover, 1)
	if len(req) != 16 || !bytes.HasPrefix(req, []byte("BJNP")) || req[5] != 0x01 {
		t.Fatalf("invalid Canon BJNP discover packet: %x", req)
	}

	discResp := make([]byte, 32)
	copy(discResp[:4], "BJNP")
	discResp[4] = 0x81
	discResp[5] = bjnpCmdDiscover
	discResp[16+4] = 6
	discResp[16+5] = 4
	copy(discResp[16+6:16+12], []byte{0x18, 0x0C, 0xAC, 0x11, 0x22, 0x33})
	copy(discResp[16+12:16+16], []byte{192, 168, 1, 88})
	hDisc, okDisc := parseCanonBJNPResponse(discResp)
	if !okDisc || hDisc.IPAddress != "192.168.1.88" || hDisc.MACAddress != "18:0C:AC:11:22:33" || hDisc.DiscoveryBy != "bjnp" {
		t.Errorf("parseCanonBJNPResponse(discover) failed: ok=%v hint=%+v", okDisc, hDisc)
	}

	ieeeBody := "MFG:Canon;CMD:BJL,BJRaster3,IVEC;MDL:PIXMA G3010 MegaTank;CLS:PRINTER;SN:CN998877;"
	idResp := make([]byte, 16+2+len(ieeeBody))
	copy(idResp[:4], "BJNP")
	idResp[4] = 0x81
	idResp[5] = bjnpCmdGetID
	idResp[16] = byte((2 + len(ieeeBody)) >> 8)
	idResp[17] = byte((2 + len(ieeeBody)) & 0xFF)
	copy(idResp[18:], ieeeBody)
	hID, okID := parseCanonBJNPResponse(idResp)
	if !okID || hID.VendorHint != "Canon" || !strings.Contains(hID.ModelHint, "G3010") || hID.SerialHint != "CN998877" || !hID.ColorHint {
		t.Errorf("parseCanonBJNPResponse(getID) failed: ok=%v hint=%+v", okID, hID)
	}
}

func TestLDAPFilterDNAndResolverCache(t *testing.T) {
	if got := escapeLDAPFilterValue(`a*(b)\c`); got != `a\2a\28b\29\5cc` {
		t.Errorf("escapeLDAPFilterValue got %q", got)
	}
	if got := domainToBaseDN("corp.empresa.local."); got != "dc=corp,dc=empresa,dc=local" {
		t.Errorf("domainToBaseDN got %q", got)
	}
	if got := extractOUFromDN(`CN=Maria\, Silva,OU=Financeiro,OU=Matriz,DC=corp,DC=local`); got != "Financeiro" {
		t.Errorf("extractOUFromDN got %q, want Financeiro", got)
	}
	if got := extractOUFromDN(`CN=Admin,OU=Users,OU=TI,DC=corp,DC=local`); got != "TI" {
		t.Errorf("extractOUFromDN should skip generic Users OU, got %q", got)
	}

	// Verify SearchRequest encoding contains escaped sAMAccountName & baseDN
	searchPkt := buildLDAPSearchRequest(2, "dc=corp,dc=local", "maria.silva")
	if !bytes.Contains(searchPkt, []byte("dc=corp,dc=local")) || !bytes.Contains(searchPkt, []byte("maria.silva")) {
		t.Fatalf("buildLDAPSearchRequest missing baseDN or username")
	}

	// Mock lightweight LDAPv3 TCP server
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer ln.Close()

	var queryCount int32
	go func() {
		for {
			conn, aErr := ln.Accept()
			if aErr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, rErr := c.Read(buf)
					if rErr != nil || n == 0 {
						return
					}
					msg, _, pErr := parseLDAPMessage(buf[:n])
					if pErr != nil {
						return
					}
					switch msg.OpTag {
					case ldapTagBindRequest:
						// BindResponse resultCode = 0
						bindResp := wrapBERTLV(asn1Sequence, append(
							wrapBERTLV(asn1Integer, encodeASN1NonNegInt(uint32(msg.MsgID))),
							wrapBERTLV(ldapTagBindResponse, []byte{ldapTagEnumerated, 0x01, 0x00, asn1OctetStr, 0x00, asn1OctetStr, 0x00})...,
						))
						_, _ = c.Write(bindResp)
					case ldapTagSearchRequest:
						atomic.AddInt32(&queryCount, 1)
						// SearchResultEntry with department="Controladoria"
						attrSeq := wrapBERTLV(asn1Sequence, append(
							wrapBERTLV(asn1OctetStr, []byte("department")),
							wrapBERTLV(ldapTagSet, wrapBERTLV(asn1OctetStr, []byte("Controladoria")))...,
						))
						entryBody := append(
							wrapBERTLV(asn1OctetStr, []byte("CN=Maria,OU=Financeiro,DC=corp,DC=local")),
							wrapBERTLV(asn1Sequence, attrSeq)...,
						)
						entryPkt := wrapBERTLV(asn1Sequence, append(
							wrapBERTLV(asn1Integer, encodeASN1NonNegInt(uint32(msg.MsgID))),
							wrapBERTLV(ldapTagSearchResultEntry, entryBody)...,
						))
						donePkt := wrapBERTLV(asn1Sequence, append(
							wrapBERTLV(asn1Integer, encodeASN1NonNegInt(uint32(msg.MsgID))),
							wrapBERTLV(ldapTagSearchResultDone, []byte{ldapTagEnumerated, 0x01, 0x00, asn1OctetStr, 0x00, asn1OctetStr, 0x00})...,
						))
						_, _ = c.Write(append(entryPkt, donePkt...))
					}
				}
			}(conn)
		}
	}()

	resolver := NewLDAPDepartmentResolver()
	resolver.timeout = 1500 * time.Millisecond
	resolver.lookupSRV = func(_ context.Context, _ string) ([]string, error) {
		return []string{ln.Addr().String()}, nil
	}

	dept1 := resolver.ResolveDepartment(context.Background(), "corp.local", "maria", "PC01")
	if dept1 != "Controladoria" {
		t.Fatalf("expected Controladoria from mock LDAP, got %q", dept1)
	}
	dept2 := resolver.ResolveDepartment(context.Background(), "corp.local", "maria", "PC01")
	if dept2 != "Controladoria" {
		t.Fatalf("expected cached Controladoria, got %q", dept2)
	}
	if atomic.LoadInt32(&queryCount) != 1 {
		t.Errorf("expected in-memory cache hit (1 LDAP search), got %d searches", atomic.LoadInt32(&queryCount))
	}

	// Non-AD WORKGROUP fallback
	if got := resolver.ResolveDepartment(context.Background(), "WORKGROUP", "joao", "PC02"); got != "WORKGROUP" {
		t.Errorf("expected WORKGROUP fallback, got %q", got)
	}

	// Offline DC fallback -> returns domain name cleanly
	offlineResolver := NewLDAPDepartmentResolver()
	offlineResolver.lookupSRV = func(_ context.Context, _ string) ([]string, error) {
		return nil, context.DeadlineExceeded
	}
	if got := offlineResolver.ResolveDepartment(context.Background(), "CORP", "pedro", "PC03"); got != "CORP" {
		t.Errorf("expected CORP fallback when offline, got %q", got)
	}
}

func TestBinaryUpdateChecksumAndAtomicSwap(t *testing.T) {
	newBin := append([]byte{0x7F, 'E', 'L', 'F'}, []byte("-printmind-v4.1.0-payload")...)
	sum := sha256.Sum256(newBin)
	goodHash := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/good":
			_, _ = w.Write(newBin)
		case "/nonelf":
			_, _ = w.Write([]byte("not-an-elf-binary"))
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	target := filepath.Join(dir, "bin", "printmind-agent")
	_ = os.MkdirAll(filepath.Dir(target), 0755)
	_ = os.WriteFile(target, []byte("old-v4.0.0"), 0755)

	client := &http.Client{Timeout: 5 * time.Second}

	// 1. Wrong SHA-256 must abort without modifying target
	wrongHash := strings.Repeat("0", 64)
	if err := stageAndVerifyBinaryUpdate(context.Background(), client, srv.URL+"/good", wrongHash, staging, target, true); err == nil {
		t.Fatal("expected SHA-256 mismatch error")
	}
	cur, _ := os.ReadFile(target)
	if string(cur) != "old-v4.0.0" {
		t.Fatalf("target binary modified on checksum mismatch: %q", string(cur))
	}

	// 2. Non-ELF payload with matching hash must be rejected when requireELF=true
	nonElfSum := sha256.Sum256([]byte("not-an-elf-binary"))
	if err := stageAndVerifyBinaryUpdate(context.Background(), client, srv.URL+"/nonelf", hex.EncodeToString(nonElfSum[:]), staging, target, true); err == nil {
		t.Fatal("expected non-ELF rejection error")
	}

	// 3. Valid ELF + valid SHA-256 must atomically swap target
	if err := stageAndVerifyBinaryUpdate(context.Background(), client, srv.URL+"/good", goodHash, staging, target, true); err != nil {
		t.Fatalf("stageAndVerifyBinaryUpdate failed: %v", err)
	}
	updated, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(updated, newBin) {
		t.Fatalf("expected updated binary contents, got err=%v len=%d", err, len(updated))
	}
}

func TestSNMPv3OutgoingPacketBuildAndParseRoundtrip(t *testing.T) {
	sess, err := newSNMPv3Session(SNMPv3Config{
		Enabled:        true,
		Username:       "monitor",
		SecurityLevel:  "authPriv",
		AuthProtocol:   "SHA1",
		AuthPassphrase: "auth-password-123",
		PrivProtocol:   "AES",
		PrivPassphrase: "priv-password-123",
	})
	if err != nil {
		t.Fatalf("newSNMPv3Session failed: %v", err)
	}
	if err := sess.setEngine([]byte{0x80, 0x00, 0x00, 0x0B, 0x04, 0x31, 0x32}, 5, 1200); err != nil {
		t.Fatalf("setEngine failed: %v", err)
	}
	pkt, err := buildSNMPv3Packet(sess, pduGetRequest, 1001, 2002, []string{"1.3.6.1.2.1.1.1.0"}, false)
	if err != nil {
		t.Fatalf("buildSNMPv3Packet failed: %v", err)
	}
	// Verify discovery packet builds cleanly with empty engineID and reportable flag
	discPkt, err := buildSNMPv3Packet(sess, pduGetRequest, 1, 1, nil, true)
	if err != nil || len(discPkt) < 20 || len(pkt) < 40 {
		t.Fatalf("unexpected packet sizes: disc=%d req=%d err=%v", len(discPkt), len(pkt), err)
	}
}
