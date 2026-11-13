package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestEncodeOIDAndBERMultiByteLength(t *testing.T) {
	oid, err := encodeOID("1.3.6.1.2.1.43.5.1.1.17.1")
	if err != nil {
		t.Fatalf("encodeOID failed: %v", err)
	}
	if len(oid) == 0 || oid[0] != 0x2B {
		t.Errorf("unexpected OID prefix: %x", oid)
	}
	decoded, err := decodeOID(oid)
	if err != nil || decoded != "1.3.6.1.2.1.43.5.1.1.17.1" {
		t.Errorf("decodeOID got (%q, %v)", decoded, err)
	}

	// Test multi-byte BER length encoding (>127 bytes)
	for _, n := range []int{26, 127, 128, 255, 261, 70000} {
		enc := encodeBERLength(n)
		got, used, err := parseBERLength(enc)
		if err != nil || got != n || used != len(enc) {
			t.Errorf("BER roundtrip(%d): got (%d, %d, %v), enc=%x", n, got, used, err, enc)
		}
	}

	// Multi-OID packet > 127 bytes must encode valid BER long-form outer length
	manyOIDs := []string{
		"1.3.6.1.2.1.1.1.0",
		"1.3.6.1.2.1.1.2.0",
		"1.3.6.1.2.1.1.3.0",
		"1.3.6.1.2.1.43.5.1.1.17.1",
		"1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.8.0",
		"1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.10.0",
		"1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.11.0",
	}
	pkt, err := buildSNMPRequest(pduGetRequest, "public", 1234, manyOIDs)
	if err != nil || len(pkt) <= 128 {
		t.Fatalf("expected >128 byte multi-OID packet, len=%d err=%v", len(pkt), err)
	}
	outerLen, lenBytes, err := parseBERLength(pkt[1:])
	if err != nil || 1+lenBytes+outerLen != len(pkt) {
		t.Fatalf("invalid outer BER length for multi-OID packet: outerLen=%d lenBytes=%d total=%d err=%v", outerLen, lenBytes, len(pkt), err)
	}
}

func buildTestSNMPResponse(reqID uint32, community string, vbs []SNMPVarBind) []byte {
	var vbListPayload []byte
	for _, vb := range vbs {
		oidBytes, _ := encodeOID(vb.OID)
		inner := append(wrapBERTLV(asn1OID, oidBytes), wrapBERTLV(vb.Tag, vb.Value)...)
		vbListPayload = append(vbListPayload, wrapBERTLV(asn1Sequence, inner)...)
	}
	reqIDBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(reqIDBytes, reqID)
	var pduPayload []byte
	pduPayload = append(pduPayload, wrapBERTLV(asn1Integer, reqIDBytes)...)
	pduPayload = append(pduPayload, asn1Integer, 0x01, 0x00)
	pduPayload = append(pduPayload, asn1Integer, 0x01, 0x00)
	pduPayload = append(pduPayload, wrapBERTLV(asn1Sequence, vbListPayload)...)
	pdu := wrapBERTLV(pduGetResponse, pduPayload)

	var msgPayload []byte
	msgPayload = append(msgPayload, asn1Integer, 0x01, 0x01)
	msgPayload = append(msgPayload, wrapBERTLV(asn1OctetStr, []byte(community))...)
	msgPayload = append(msgPayload, pdu...)
	return wrapBERTLV(asn1Sequence, msgPayload)
}

func TestSNMPWalkErrorBitmaskAndStrictRFC3805Supplies(t *testing.T) {
	// Test error state bitmask decoder
	reasons := decodeErrorStateBitmask([]byte{0x24, 0x08}) // low_toner (0x20) + jammed (0x04) + output_full (0x08 byte 1)
	joined := strings.Join(reasons, ",")
	if !strings.Contains(joined, "low_toner") || !strings.Contains(joined, "jammed") || !strings.Contains(joined, "output_full") {
		t.Errorf("unexpected decoded bitmask reasons: %v", reasons)
	}

	// Strict RFC 3805 Supply Semantics: -1 other, -2 unknown, -3 some_remaining -> Percent MUST be nil!
	sMinus1 := buildSupplyItem("Aux", "black", 3, 3, -1, 100)
	sMinus2 := buildSupplyItem("Black Cartridge", "black", 3, 3, -2, 100)
	sMinus3 := buildSupplyItem("Cyan Cartridge", "cyan", 3, 3, -3, 100)
	sWaste := buildSupplyItem("Waste Toner Box", "waste", 4, 4, 75, 100)

	if sMinus1.Percent != nil || sMinus1.LevelState != "other" {
		t.Errorf("-1 supply expected nil percent & other, got %+v", sMinus1)
	}
	if sMinus2.Percent != nil || sMinus2.LevelState != "unknown" {
		t.Errorf("-2 supply expected nil percent & unknown, got %+v", sMinus2)
	}
	if sMinus3.Percent != nil || sMinus3.LevelState != "some_remaining" {
		t.Errorf("-3 supply expected nil percent & some_remaining, got %+v", sMinus3)
	}
	if sWaste.SupplyClass != "waste" || sWaste.Percent == nil || *sWaste.Percent != 75 {
		t.Errorf("Class 4 waste receptacle expected waste & 75%%, got %+v", sWaste)
	}

	// Verify SNMP Response parser with multi-varbind packet
	respPkt := buildTestSNMPResponse(0x11223344, "public", []SNMPVarBind{
		{OID: "1.3.6.1.2.1.43.11.1.1.6.1.1", Tag: asn1OctetStr, Value: []byte("Black Toner")},
		{OID: "1.3.6.1.2.1.43.11.1.1.9.1.1", Tag: asn1Integer, Value: []byte{0xFE}}, // -2
	})
	id, errSt, vbs, err := parseSNMPResponse(respPkt)
	if err != nil || id != 0x11223344 || errSt != 0 || len(vbs) != 2 {
		t.Fatalf("parseSNMPResponse failed: id=%x errSt=%d vbs=%d err=%v", id, errSt, len(vbs), err)
	}
	if decodeASN1Int(vbs[1].Value) != -2 {
		t.Errorf("expected -2 int, got %d", decodeASN1Int(vbs[1].Value))
	}
}

func TestBrother3OIDTLVDecoder(t *testing.T) {
	// brInfoMaintenance: tag 0x81 = 90% (0x5A), tag 0x6F = 45% (0x2D), terminated by 0xFF
	maintRaw := []byte{
		0x81, 0x01, 0x04, 0x00, 0x00, 0x00, 0x5A,
		0x6F, 0x01, 0x04, 0x00, 0x00, 0x00, 0x2D,
		0xFF,
	}
	maint := ParseBrotherTLV(maintRaw)
	if maint[0x81] != 90 || maint[0x6F] != 45 {
		t.Errorf("unexpected brInfoMaintenance TLV decode: %+v", maint)
	}

	// brInfoCounter: tag 0x00 = 1401 (0x0579), tag 0x06 = 350 (0x015E)
	counterRaw := []byte{
		0x00, 0x01, 0x04, 0x00, 0x00, 0x05, 0x79,
		0x06, 0x01, 0x04, 0x00, 0x00, 0x01, 0x5E,
		0xFF,
	}
	cnt := ParseBrotherTLV(counterRaw)
	if cnt[0x00] != 1401 || cnt[0x06] != 350 {
		t.Errorf("unexpected brInfoCounter TLV decode: %+v", cnt)
	}

	// brInfoNextCare: tag 0x82 = 10599 (0x2967)
	nextRaw := []byte{0x82, 0x01, 0x04, 0x00, 0x00, 0x29, 0x67, 0xFF}
	nxt := ParseBrotherTLV(nextRaw)
	if nxt[0x82] != 10599 {
		t.Errorf("unexpected brInfoNextCare TLV decode: %+v", nxt)
	}
}

func TestHPLEDMXMLParserAndIPPMerge(t *testing.T) {
	prodXML := `<ProductConfigDyn><ProductInformation><MakeAndModel>HP OfficeJet Pro 8010</MakeAndModel><SerialNumber>TH98A12345</SerialNumber></ProductInformation></ProductConfigDyn>`
	model, serial, _ := ParseHPProductConfigXML([]byte(prodXML))
	if model != "HP OfficeJet Pro 8010" || serial != "TH98A12345" {
		t.Errorf("ParseHPProductConfigXML got (%q, %q)", model, serial)
	}

	consXML := `<ConsumableConfigDyn>
		<ConsumableInfo>
			<ConsumableLabelCode>K</ConsumableLabelCode>
			<ConsumableTypeEnum>ink</ConsumableTypeEnum>
			<ConsumablePercentageLevelRemaining>64</ConsumablePercentageLevelRemaining>
			<SerialNumber>HPK-9911</SerialNumber>
			<ProductNumber>3YL65A</ProductNumber>
			<WarrantyExpirationDate>2027-08-01</WarrantyExpirationDate>
		</ConsumableInfo>
		<ConsumableInfo>
			<ConsumableLabelCode>pK</ConsumableLabelCode>
			<ConsumableTypeEnum>printhead</ConsumableTypeEnum>
			<ConsumablePercentageLevelRemaining>88</ConsumablePercentageLevelRemaining>
		</ConsumableInfo>
		<ConsumableInfo>
			<ConsumableLabelCode>CMY</ConsumableLabelCode>
			<ConsumableTypeEnum>ink</ConsumableTypeEnum>
			<ConsumablePercentageLevelRemaining>50</ConsumablePercentageLevelRemaining>
		</ConsumableInfo>
		<ConsumableInfo>
			<ConsumableLabelCode>OPC</ConsumableLabelCode>
			<ConsumableTypeEnum>opc</ConsumableTypeEnum>
			<ConsumablePercentageLevelRemaining>72</ConsumablePercentageLevelRemaining>
		</ConsumableInfo>
	</ConsumableConfigDyn>`

	sups, colorCap := ParseHPConsumableConfigXML([]byte(consXML))
	if !colorCap || len(sups) != 4 {
		t.Fatalf("expected 4 LEDM supplies & color=true, got %d color=%v", len(sups), colorCap)
	}
	if sups[0].SerialNumber != "HPK-9911" || sups[0].PartNumber != "3YL65A" || sups[0].WarrantyEnd != "2027-08-01" {
		t.Errorf("missing cartridge metadata: %+v", sups[0])
	}
	if sups[1].Color != "photo_black" || sups[1].SupplyType != "printhead" {
		t.Errorf("expected photo_black printhead, got %+v", sups[1])
	}
	if sups[3].SupplyType != "opc_drum" {
		t.Errorf("expected opc_drum, got %+v", sups[3])
	}

	usageXML := `<ProductUsageDyn><PrinterSubunit><TotalImpressions>5420</TotalImpressions><MonochromeImpressions>3100</MonochromeImpressions><ColorImpressions>2320</ColorImpressions><JamEvents>4</JamEvents></PrinterSubunit></ProductUsageDyn>`
	counters := ParseHPProductUsageXML([]byte(usageXML))
	if counters.LifetimeEngineTotal != 5420 || counters.MonoTotal != 3100 || counters.ColorTotal != 2320 || counters.JamsTotal != 4 {
		t.Errorf("unexpected ProductUsageDyn counters: %+v", counters)
	}

	// Merge IPP unknown (-2) black supply with LEDM 64%
	dev := &DiscoveredDevice{
		Vendor: "HP",
		Model:  "HP OfficeJet Pro 8010",
		Supplies: []SupplyItem{
			buildSupplyItem("Black", "black", 3, 5, -2, 100),
		},
	}
	ledm := &HPLEDMData{
		SerialNumber:   serial,
		Supplies:       sups,
		Counters:       counters,
		IsColorCapable: true,
	}
	MergeIPPAndHPLEDM(dev, ledm)
	if dev.Supplies[0].Percent == nil || *dev.Supplies[0].Percent != 64 || dev.Supplies[0].SerialNumber != "HPK-9911" {
		t.Errorf("MergeIPPAndHPLEDM failed to fill unknown supply: %+v", dev.Supplies[0])
	}
	if dev.Counters.LifetimeEngineTotal != 5420 || !dev.IsColorCapable {
		t.Errorf("MergeIPPAndHPLEDM failed counters/color merge: %+v", dev)
	}
}

func TestEpsonBDCST2EWSAndENPCQueryOnly(t *testing.T) {
	bdc := []byte("@BDC ST2\r\nST:04;IQ:80,65,55,90;")
	st, ok := ParseEpsonBDCST2(bdc)
	if !ok || st.StatusCode != "04" || !st.IsColor || len(st.Supplies) != 4 {
		t.Fatalf("ParseEpsonBDCST2 failed: ok=%v st=%+v", ok, st)
	}
	if *st.Supplies[0].Percent != 80 || st.Supplies[1].Color != "cyan" || *st.Supplies[1].Percent != 65 {
		t.Errorf("unexpected BDC supplies: %+v", st.Supplies)
	}

	ewsHTML := `<div>Serial Number: <span>X89L123456</span></div><div>Total Pages: 1280</div><div>Black 70%</div><div>Cyan 40%</div><div>Maintenance Box 25%</div>`
	ser, tot, sups, col := ParseEpsonEWSHTML(ewsHTML)
	if ser != "X89L123456" || tot != 1280 || !col || len(sups) != 3 {
		t.Fatalf("ParseEpsonEWSHTML failed: ser=%q tot=%d col=%v sups=%+v", ser, tot, col, sups)
	}
	if sups[2].SupplyClass != "waste" || *sups[2].Percent != 25 {
		t.Errorf("expected waste maintenance box 25%%, got %+v", sups[2])
	}

	// ENPC strict query-only check (must start with EPSONQ, never EPSONC)
	q0, err0 := buildENPCQueryPacket(0x00)
	q3, err3 := buildENPCQueryPacket(0x03)
	if err0 != nil || err3 != nil || !bytes.HasPrefix(q0, []byte("EPSONQ")) || !bytes.HasPrefix(q3, []byte("EPSONQ")) {
		t.Fatalf("ENPC query builder failed: err0=%v err3=%v", err0, err3)
	}
	if _, err := buildENPCQueryPacket(0x99); err == nil {
		t.Error("expected error for unsupported ENPC query type")
	}

	// Parse ENPC 0x00 response
	resp0 := make([]byte, 40)
	copy(resp0[:6], "EPSONQ")
	resp0[6] = 0x00
	copy(resp0[16:22], []byte{0xAA, 0xBB, 0xCC, 0x11, 0x22, 0x33})
	copy(resp0[24:28], []byte{192, 168, 1, 77})
	h0, ok0 := parseENPCResponse(resp0)
	if !ok0 || h0.IPAddress != "192.168.1.77" || h0.MACAddress != "AA:BB:CC:11:22:33" {
		t.Errorf("parseENPCResponse(0x00) failed: ok=%v hint=%+v", ok0, h0)
	}
}

func TestCanonAndDiscoveryParsers(t *testing.T) {
	canonHTML := `<script>inktank[0]="Black:85"; inktank[1]="Cyan:42";</script>`
	cSups, cCol := ParseCanonEWSHTML(canonHTML)
	if !cCol || len(cSups) != 2 || *cSups[0].Percent != 85 || cSups[1].Color != "cyan" {
		t.Errorf("ParseCanonEWSHTML failed: col=%v sups=%+v", cCol, cSups)
	}

	wsdXML := `<soap:Envelope><soap:Body><wsd:ProbeMatches><wsd:ProbeMatch><wsd:Types>wsdp:Device print:PrintDeviceType</wsd:Types><wsd:XAddrs>http://192.168.50.25:3911/WebServices/Printer</wsd:XAddrs></wsd:ProbeMatch></wsd:ProbeMatches></soap:Body></soap:Envelope>`
	wsdHints := parseWSDProbeMatch([]byte(wsdXML))
	if len(wsdHints) != 1 || wsdHints[0].IPAddress != "192.168.50.25" {
		t.Errorf("parseWSDProbeMatch failed: %+v", wsdHints)
	}

	// Build minimal mDNS response with PTR, SRV, TXT, A
	var mdns bytes.Buffer
	hdr := make([]byte, 12)
	binary.BigEndian.PutUint16(hdr[6:8], 4) // 4 answer RRs
	mdns.Write(hdr)
	instName := encodeDNSName("Brother._ipp._tcp.local")
	targetHost := encodeDNSName("brother.local")

	// 1. PTR
	svcName := encodeDNSName("_ipp._tcp.local")
	mdns.Write(svcName)
	mdns.Write([]byte{0x00, 12, 0x00, 0x01, 0, 0, 0, 60})
	_ = binary.Write(&mdns, binary.BigEndian, uint16(len(instName)))
	mdns.Write(instName)

	// 2. SRV
	mdns.Write(instName)
	mdns.Write([]byte{0x00, 33, 0x00, 0x01, 0, 0, 0, 60})
	_ = binary.Write(&mdns, binary.BigEndian, uint16(6+len(targetHost)))
	mdns.Write([]byte{0, 0, 0, 0, 0x02, 0x77}) // port 631
	mdns.Write(targetHost)

	// 3. TXT
	txt1 := "ty=Brother MFC-L3750CDW"
	txt2 := "Color=T"
	mdns.Write(instName)
	mdns.Write([]byte{0x00, 16, 0x00, 0x01, 0, 0, 0, 60})
	_ = binary.Write(&mdns, binary.BigEndian, uint16(2+len(txt1)+len(txt2)))
	mdns.WriteByte(byte(len(txt1)))
	mdns.WriteString(txt1)
	mdns.WriteByte(byte(len(txt2)))
	mdns.WriteString(txt2)

	// 4. A
	mdns.Write(targetHost)
	mdns.Write([]byte{0x00, 1, 0x00, 0x01, 0, 0, 0, 60, 0, 4, 192, 168, 1, 99})

	mHints := parseMDNSResponse(mdns.Bytes())
	if len(mHints) != 1 || mHints[0].IPAddress != "192.168.1.99" || mHints[0].IPPPort != 631 || !mHints[0].ColorHint {
		t.Errorf("parseMDNSResponse failed: %+v", mHints)
	}
}

func TestIPPEncoderDecoderAndProcNetTCP(t *testing.T) {
	req := BuildIPPGetPrinterAttributes("ipp://192.168.1.40:631/ipp/print", 42, []string{"printer-make-and-model", "marker-levels"})
	if len(req) < 20 || req[0] != 0x02 || req[1] != 0x00 {
		t.Fatalf("unexpected IPP request header: %x", req[:8])
	}

	// Build mock IPP response
	var respBuf bytes.Buffer
	respBuf.Write([]byte{0x02, 0x00, 0x00, 0x00})
	_ = binary.Write(&respBuf, binary.BigEndian, uint32(42))
	respBuf.WriteByte(ippTagPrinterAttrs)
	writeIPPAttr(&respBuf, ippTagText, "printer-make-and-model", []byte("HP Smart Tank 580"))
	writeIPPAttr(&respBuf, ippTagBoolean, "color-supported", []byte{1})
	var impBytes [4]byte
	binary.BigEndian.PutUint32(impBytes[:], 812)
	writeIPPAttr(&respBuf, ippTagInteger, "printer-impressions-completed", impBytes[:])
	writeIPPAttr(&respBuf, ippTagName, "marker-names", []byte("Black"))
	writeIPPAttr(&respBuf, ippTagName, "", []byte("Cyan"))
	var lvB, lvC [4]byte
	binary.BigEndian.PutUint32(lvB[:], 0xFFFFFFFE) // -2 unknown
	binary.BigEndian.PutUint32(lvC[:], 55)
	writeIPPAttr(&respBuf, ippTagInteger, "marker-levels", lvB[:])
	writeIPPAttr(&respBuf, ippTagInteger, "", lvC[:])
	respBuf.WriteByte(ippTagEndOfAttrs)

	parsed, err := ParseIPPResponse(respBuf.Bytes())
	if err != nil {
		t.Fatalf("ParseIPPResponse failed: %v", err)
	}
	dev, ok := ParseIPPPrinterAttributes("192.168.1.40", "ipp", parsed)
	if !ok || dev.Model != "HP Smart Tank 580" || !dev.IsColorCapable || dev.Counters.LifetimeEngineTotal != 812 {
		t.Fatalf("ParseIPPPrinterAttributes unexpected result: ok=%v dev=%+v", ok, dev)
	}
	if len(dev.Supplies) != 2 || dev.Supplies[0].Percent != nil || dev.Supplies[0].LevelState != "unknown" {
		t.Errorf("expected unknown (-2) black supply with nil Percent, got %+v", dev.Supplies)
	}
	if *dev.Supplies[1].Percent != 55 {
		t.Errorf("expected cyan 55%%, got %+v", dev.Supplies[1])
	}

	procNet := "  sl  local_address rem_address   st\n   0: 0100007F:EA60 00000000:0000 0A\n   1: 0100007F:0277 00000000:0000 0A\n"
	ports := ParseProcNetTCPListeningPorts(procNet, 60000, 65535)
	if len(ports) != 1 || ports[0] != 60000 {
		t.Errorf("ParseProcNetTCPListeningPorts got %v, want [60000]", ports)
	}
}

func TestIEEE1284ElginL42vsL42ProAndWSDMatcher(t *testing.T) {
	// With 2-byte BE length prefix (per IOCTL_USBPRINT_GET_1284_ID)
	body := "MFG:Elgin;MDL:L42;CMD:BPLB;SN:ELG2026001;"
	raw := make([]byte, 2+len(body))
	binary.BigEndian.PutUint16(raw[:2], uint16(2+len(body)))
	copy(raw[2:], body)

	info := ParseIEEE1284DeviceID(raw)
	if info.Manufacturer != "Elgin" || info.Model != "L42" || info.SerialNumber != "ELG2026001" {
		t.Fatalf("ParseIEEE1284DeviceID failed: %+v", info)
	}

	langsL42, _, thermL42 := ClassifyPrinterCapabilities("Elgin", "L42", info.CommandSet)
	if !thermL42 || strings.Contains(strings.Join(langsL42, ","), "ZPL") {
		t.Errorf("classic Elgin L42 MUST NOT include ZPL, got %v", langsL42)
	}

	langsPro, _, thermPro := ClassifyPrinterCapabilities("Elgin", "L42 Pro", nil)
	if !thermPro || !strings.Contains(strings.Join(langsPro, ","), "ZPL2") || !strings.Contains(strings.Join(langsPro, ","), "BPLB") {
		t.Errorf("Elgin L42 Pro should support ZPL2 & BPLB, got %v", langsPro)
	}

	if mm := CalculateBPLBLabelLengthMM(320, 24, 8); mm != 43.0 {
		t.Errorf("CalculateBPLBLabelLengthMM got %.2f, want 43.0", mm)
	}

	entries := []WSDRegistryEntry{
		{KeyName: "urn:uuid:e189f362-50bd-44e0-9539-ba4820a552c0", FriendlyName: "HP LaserJet M404", IPAddress: "192.168.10.55"},
	}
	ip := MatchWSDPortIP("WSD-e189f362-50bd-44e0-9539-ba4820a552c0.0065", "HP LaserJet M404", entries)
	if ip != "192.168.10.55" {
		t.Errorf("MatchWSDPortIP got %q, want 192.168.10.55", ip)
	}
}

func TestEvent307And805PhysicalColorGateAndCUPSPageLog(t *testing.T) {
	ev307MonoXML := `<Event xmlns="http://schemas.microsoft.com/win/2004/08/events/event">
		<System><Provider Name="Microsoft-Windows-PrintService"/><EventID>307</EventID><EventRecordID>101</EventRecordID><TimeCreated SystemTime="2026-11-13T10:00:00Z"/><Channel>Microsoft-Windows-PrintService/Operational</Channel></System>
		<UserData><DocumentPrinted><Param1>42</Param1><Param2>Contrato.pdf</Param2><Param3>CORP\maria</Param3><Param4>\\PC-FIN01</Param4><Param5>HP LaserJet M404 Mono</Param5><Param6>192.168.1.20</Param6><Param7>102400</Param7><Param8>5</Param8></DocumentPrinted></UserData>
	</Event>`
	// Windows falsely marks Color=2 when printing a colored Word doc to a physical Mono printer!
	ev805FalseColorXML := `<Event xmlns="http://schemas.microsoft.com/win/2004/08/events/event">
		<System><Provider Name="Microsoft-Windows-PrintService"/><EventID>805</EventID><EventRecordID>100</EventRecordID><TimeCreated SystemTime="2026-11-13T09:59:59Z"/><Channel>Microsoft-Windows-PrintService/Operational</Channel></System>
		<UserData><RenderJobDiag><JobId>42</JobId><Copies>3</Copies><Color>2</Color><Duplex>1</Duplex></RenderJobDiag></UserData>
	</Event>`

	ev307ColorXML := `<Event xmlns="http://schemas.microsoft.com/win/2004/08/events/event">
		<System><Provider Name="Microsoft-Windows-PrintService"/><EventID>307</EventID><EventRecordID>103</EventRecordID><TimeCreated SystemTime="2026-11-13T10:05:00Z"/><Channel>Microsoft-Windows-PrintService/Operational</Channel></System>
		<UserData><DocumentPrinted><Param1>43</Param1><Param2>Banner.pdf</Param2><Param3>joao</Param3><Param4>PC-MKT01</Param4><Param5>Epson EcoTank L4260</Param5><Param6>192.168.1.30</Param6><Param7>204800</Param7><Param8>4</Param8></DocumentPrinted></UserData>
	</Event>`
	ev805ColorXML := `<Event xmlns="http://schemas.microsoft.com/win/2004/08/events/event">
		<System><Provider Name="Microsoft-Windows-PrintService"/><EventID>805</EventID><EventRecordID>102</EventRecordID><TimeCreated SystemTime="2026-11-13T10:04:59Z"/><Channel>Microsoft-Windows-PrintService/Operational</Channel></System>
		<UserData><RenderJobDiag><JobId>43</JobId><Copies>2</Copies><Color>2</Color><Duplex>0</Duplex></RenderJobDiag></UserData>
	</Event>`

	var events []PrintServiceEvent
	for _, raw := range []string{ev307MonoXML, ev805FalseColorXML, ev307ColorXML, ev805ColorXML} {
		parsed, err := ParsePrintServiceEventXML(raw)
		if err != nil {
			t.Fatalf("ParsePrintServiceEventXML failed: %v", err)
		}
		events = append(events, *parsed)
	}

	devices := []DiscoveredDevice{
		{IPAddress: "192.168.1.20", Model: "HP LaserJet M404 Mono", IsColorCapable: false},
		{IPAddress: "192.168.1.30", Model: "Epson EcoTank L4260", IsColorCapable: true},
	}
	lookup := BuildDeviceColorLookup(devices, nil)
	jobs := CorrelatePrintJobs(events, lookup, "DEFAULT", "HOST")
	if len(jobs) != 2 {
		t.Fatalf("expected 2 correlated jobs, got %d", len(jobs))
	}

	// Job 42: Mono printer MUST gate out Color=2 -> 15 mono pages, 0 color pages!
	jMono := jobs[0]
	if jMono.EffectivePages != 15 || jMono.ColorGated || jMono.ColorPages != 0 || jMono.MonoPages != 15 || !jMono.Duplex || jMono.Domain != "CORP" {
		t.Errorf("Physical Color Gate failed on mono printer: %+v", jMono)
	}

	// Job 43: Color printer with Color=2 -> 8 color pages, ColorGated=true
	jCol := jobs[1]
	if jCol.EffectivePages != 8 || !jCol.ColorGated || jCol.ColorPages != 8 || jCol.MonoPages != 0 {
		t.Errorf("Physical Color Gate failed on color printer: %+v", jCol)
	}

	summaries := AggregateJobSummaries(jobs)
	if len(summaries) != 2 || summaries[0].ColorGatedOut != 15 || !summaries[0].IsDelta || !summaries[1].IsDelta {
		t.Errorf("unexpected JobSummary: %+v", summaries)
	}
	rawSummaryJSON, _ := json.Marshal(summaries[0])
	if !strings.Contains(string(rawSummaryJSON), `"is_delta":true`) {
		t.Errorf("expected JobSummary JSON to serialize is_delta:true, got %s", string(rawSummaryJSON))
	}

	// CUPS page_log parser
	cupsLog := "HP_M404 alice 101 [13/Nov/2026:10:00:00 -0300] 1 2 - 192.168.1.50 Relatorio.pdf A4 two-sided-long-edge\n" +
		"HP_M404 alice 101 [13/Nov/2026:10:00:01 -0300] 2 2 - 192.168.1.50 Relatorio.pdf A4 two-sided-long-edge\n"
	cupsJobs, nextLine := ParseCUPSPageLog(cupsLog, 0, lookup, "linux-pc")
	if len(cupsJobs) != 1 || cupsJobs[0].EffectivePages != 4 || !cupsJobs[0].Duplex || nextLine != 3 {
		t.Errorf("ParseCUPSPageLog failed: jobs=%+v nextLine=%d", cupsJobs, nextLine)
	}
}

func TestSQLiteQueueReplayCapsAndDiagnosticZip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "telemetry.db")
	store, err := OpenSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Legacy agent_buffer.json migration
	legacyBuf := filepath.Join(dir, "agent_buffer.json")
	legacyPayload, _ := json.Marshal(SyncRequest{AgentVersion: "3.2.1", PairingToken: "pm_live_test1234", CollectedAt: "2026-11-13T00:00:00Z"})
	_ = os.WriteFile(legacyBuf, legacyPayload, 0644)
	if err := store.MigrateLegacyBuffer(legacyBuf); err != nil {
		t.Fatalf("MigrateLegacyBuffer failed: %v", err)
	}
	if _, err := os.Stat(legacyBuf); !os.IsNotExist(err) {
		t.Error("expected legacy agent_buffer.json to be removed after migration")
	}

	// Enqueue 2nd snapshot
	_ = store.EnqueueTelemetry(SyncRequest{AgentVersion: AgentVersion, PairingToken: "pm_live_test1234", CollectedAt: "2026-11-13T00:05:00Z"})
	cnt, _ := store.QueueCount()
	if cnt != 2 {
		t.Fatalf("expected 2 queued snapshots, got %d", cnt)
	}

	// Chronological drain
	var order []string
	drained, err := store.DrainQueue(context.Background(), time.Now().Unix()+10, func(r SyncRequest) error {
		order = append(order, r.CollectedAt)
		return nil
	})
	if err != nil || drained != 2 || len(order) != 2 || order[0] != "2026-11-13T00:00:00Z" {
		t.Fatalf("DrainQueue unexpected: drained=%d order=%v err=%v", drained, order, err)
	}

	// Checkpoint persistence
	_ = store.SaveCheckpoint("windows_printservice", 555, "2026-11-13T12:00:00Z")
	recID, _, _ := store.GetCheckpoint("windows_printservice")
	if recID != 555 {
		t.Errorf("expected checkpoint 555, got %d", recID)
	}

	// Rotating log & redacted diagnostic zip
	logPath := filepath.Join(dir, "logs", "agent.log")
	rfw, err := NewRotatingFileWriter(logPath, 64, 2)
	if err != nil {
		t.Fatalf("NewRotatingFileWriter failed: %v", err)
	}
	_, _ = rfw.Write([]byte("line1 pm_live_secret99999999999999999\n"))
	_, _ = rfw.Write([]byte("line2 trigger rotation more than sixty four bytes of log content\n"))
	_ = rfw.Close()
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Errorf("expected rotated backup .1 to exist: %v", err)
	}

	cfg := Config{PairingToken: "pm_live_secret99999999999999999", APIKey: "anon_secret_key_123456"}
	paths := OSPaths{DataDir: dir, LogPath: logPath}
	zipBytes, err := BuildDiagnosticZip(cfg, paths, store, &WPPReadiness{Supported: true, WPPEnabled: false})
	if err != nil {
		t.Fatalf("BuildDiagnosticZip failed: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("invalid diagnostic zip: %v", err)
	}
	for _, f := range zr.File {
		rc, _ := f.Open()
		content, _ := io.ReadAll(rc)
		_ = rc.Close()
		if strings.Contains(string(content), "pm_live_secret99999999999999999") || strings.Contains(string(content), "anon_secret_key_123456") {
			t.Errorf("diagnostic zip entry %s leaked unredacted secret!", f.Name)
		}
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

func TestLoadOrInitConfigMigratesLocalhostAndLegacy(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

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

	resp, err := sendTelemetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, cfg, req)
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
	cfgPath := filepath.Join(dir, "config.json")

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
	if _, err := sendTelemetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, cfg, req); err == nil {
		t.Error("expected error when server returns status=error")
	}
}
