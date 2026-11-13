package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	pduGetRequest     byte = 0xA0
	pduGetNextRequest byte = 0xA1
	pduGetResponse    byte = 0xA2

	asn1Integer    byte = 0x02
	asn1OctetStr   byte = 0x04
	asn1Null       byte = 0x05
	asn1OID        byte = 0x06
	asn1Sequence   byte = 0x30
	asn1IPAddress  byte = 0x40
	asn1Counter32  byte = 0x41
	asn1Gauge32    byte = 0x42
	asn1TimeTicks  byte = 0x43
	asn1Counter64  byte = 0x46
	snmpNoSuchObj  byte = 0x80
	snmpNoSuchInst byte = 0x81
	snmpEndOfMib   byte = 0x82
)

// SNMPVarBind represents a single decoded OID/value pair in an SNMP PDU.
type SNMPVarBind struct {
	OID   string
	Tag   byte
	Value []byte
}

// encodeBERLength encodes an ASN.1/BER definite length supporting multi-byte long form (>127 bytes).
func encodeBERLength(length int) []byte {
	if length < 0x80 {
		return []byte{byte(length)}
	}
	if length <= 0xFF {
		return []byte{0x81, byte(length)}
	}
	if length <= 0xFFFF {
		return []byte{0x82, byte(length >> 8), byte(length)}
	}
	if length <= 0xFFFFFF {
		return []byte{0x83, byte(length >> 16), byte(length >> 8), byte(length)}
	}
	return []byte{0x84, byte(length >> 24), byte(length >> 16), byte(length >> 8), byte(length)}
}

func wrapBERTLV(tag byte, payload []byte) []byte {
	lenBytes := encodeBERLength(len(payload))
	out := make([]byte, 0, 1+len(lenBytes)+len(payload))
	out = append(out, tag)
	out = append(out, lenBytes...)
	out = append(out, payload...)
	return out
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

func readBERTLV(b []byte) (tag byte, val []byte, consumed int, err error) {
	if len(b) < 2 {
		return 0, nil, 0, fmt.Errorf("truncated TLV")
	}
	tag = b[0]
	length, lenOctets, err := parseBERLength(b[1:])
	if err != nil {
		return 0, nil, 0, err
	}
	start := 1 + lenOctets
	if start+length > len(b) {
		return 0, nil, 0, fmt.Errorf("TLV length %d exceeds buffer %d", length, len(b)-start)
	}
	return tag, b[start : start+length], start + length, nil
}

func encodeOID(oidStr string) ([]byte, error) {
	parts := strings.Split(strings.Trim(oidStr, "."), ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid OID")
	}
	nums := make([]uint32, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid OID segment %q: %w", p, err)
		}
		nums = append(nums, uint32(n))
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

func decodeOID(b []byte) (string, error) {
	if len(b) == 0 {
		return "", fmt.Errorf("empty OID bytes")
	}
	first := uint32(b[0]) / 40
	second := uint32(b[0]) % 40
	if first > 2 {
		first = 2
		second = uint32(b[0]) - 80
	}
	parts := []string{strconv.FormatUint(uint64(first), 10), strconv.FormatUint(uint64(second), 10)}
	var cur uint32
	inMulti := false
	for _, octet := range b[1:] {
		cur = (cur << 7) | uint32(octet&0x7F)
		inMulti = (octet & 0x80) != 0
		if !inMulti {
			parts = append(parts, strconv.FormatUint(uint64(cur), 10))
			cur = 0
		}
	}
	if inMulti {
		return "", fmt.Errorf("unterminated OID subidentifier")
	}
	return strings.Join(parts, "."), nil
}

// buildSNMPRequest builds an SNMPv2c GetRequest or GetNextRequest packet for one or more OIDs.
func buildSNMPRequest(pduTag byte, community string, reqID uint32, oids []string) ([]byte, error) {
	var vbListPayload []byte
	for _, oidStr := range oids {
		oidBytes, err := encodeOID(oidStr)
		if err != nil {
			return nil, err
		}
		vbInner := append(wrapBERTLV(asn1OID, oidBytes), asn1Null, 0x00)
		vbListPayload = append(vbListPayload, wrapBERTLV(asn1Sequence, vbInner)...)
	}
	vbList := wrapBERTLV(asn1Sequence, vbListPayload)

	reqIDBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(reqIDBytes, reqID)

	var pduPayload []byte
	pduPayload = append(pduPayload, wrapBERTLV(asn1Integer, reqIDBytes)...)
	pduPayload = append(pduPayload, asn1Integer, 0x01, 0x00) // error-status = 0
	pduPayload = append(pduPayload, asn1Integer, 0x01, 0x00) // error-index = 0
	pduPayload = append(pduPayload, vbList...)
	pdu := wrapBERTLV(pduTag, pduPayload)

	var msgPayload []byte
	msgPayload = append(msgPayload, asn1Integer, 0x01, 0x01) // SNMP v2c (1)
	msgPayload = append(msgPayload, wrapBERTLV(asn1OctetStr, []byte(community))...)
	msgPayload = append(msgPayload, pdu...)
	return wrapBERTLV(asn1Sequence, msgPayload), nil
}

func buildGetRequest(community string, reqID uint32, oidBytes []byte) []byte {
	oidStr, err := decodeOID(oidBytes)
	if err != nil {
		return nil
	}
	pkt, _ := buildSNMPRequest(pduGetRequest, community, reqID, []string{oidStr})
	return pkt
}

// parseSNMPResponse decodes an SNMP GetResponse packet into VarBinds.
func parseSNMPResponse(pkt []byte) (reqID uint32, errStatus int, varBinds []SNMPVarBind, err error) {
	tag, msgBody, _, err := readBERTLV(pkt)
	if err != nil || tag != asn1Sequence {
		return 0, 0, nil, fmt.Errorf("invalid outer SNMP sequence")
	}

	// version
	_, _, n, err := readBERTLV(msgBody)
	if err != nil {
		return 0, 0, nil, err
	}
	rest := msgBody[n:]

	// community
	_, _, n, err = readBERTLV(rest)
	if err != nil {
		return 0, 0, nil, err
	}
	rest = rest[n:]

	// PDU
	pduTag, pduBody, _, err := readBERTLV(rest)
	if err != nil || pduTag != pduGetResponse {
		return 0, 0, nil, fmt.Errorf("expected GetResponse PDU, got 0x%02X", pduTag)
	}

	// request-id
	_, reqIDBytes, n, err := readBERTLV(pduBody)
	if err != nil {
		return 0, 0, nil, err
	}
	for _, b := range reqIDBytes {
		reqID = (reqID << 8) | uint32(b)
	}
	pduRest := pduBody[n:]

	// error-status
	_, errBytes, n, err := readBERTLV(pduRest)
	if err != nil {
		return 0, 0, nil, err
	}
	errStatus = decodeASN1Int(errBytes)
	pduRest = pduRest[n:]

	// error-index
	_, _, n, err = readBERTLV(pduRest)
	if err != nil {
		return 0, 0, nil, err
	}
	pduRest = pduRest[n:]

	// varbind list
	vbListTag, vbListBody, _, err := readBERTLV(pduRest)
	if err != nil || vbListTag != asn1Sequence {
		return 0, 0, nil, fmt.Errorf("invalid varbind list")
	}

	for len(vbListBody) > 0 {
		vbTag, vbBody, consumed, err := readBERTLV(vbListBody)
		if err != nil || vbTag != asn1Sequence {
			break
		}
		vbListBody = vbListBody[consumed:]

		oidTag, oidBytes, n1, err := readBERTLV(vbBody)
		if err != nil || oidTag != asn1OID {
			continue
		}
		oidStr, err := decodeOID(oidBytes)
		if err != nil {
			continue
		}
		valTag, valBytes, _, err := readBERTLV(vbBody[n1:])
		if err != nil {
			continue
		}
		varBinds = append(varBinds, SNMPVarBind{
			OID:   oidStr,
			Tag:   valTag,
			Value: valBytes,
		})
	}
	return reqID, errStatus, varBinds, nil
}

func decodeASN1Int(b []byte) int {
	if len(b) == 0 || len(b) > 4 {
		return 0
	}
	var out int32
	if b[0]&0x80 != 0 {
		out = -1
	}
	for _, oct := range b {
		out = (out << 8) | int32(oct)
	}
	return int(out)
}

func decodeASN1Uint32(b []byte) (uint32, bool) {
	if len(b) == 0 || len(b) > 5 {
		return 0, false
	}
	var out uint32
	for _, oct := range b {
		out = (out << 8) | uint32(oct)
	}
	if out == 0xFFFFFFFF {
		return 0, false
	}
	return out, true
}

func exchangeSNMP(ctx context.Context, ip string, pkt []byte, timeout time.Duration) ([]SNMPVarBind, int, error) {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "udp", net.JoinHostPort(ip, "161"))
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write(pkt); err != nil {
		return nil, 0, err
	}
	buf := make([]byte, 8192)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, 0, err
	}
	_, errStatus, vbs, err := parseSNMPResponse(buf[:n])
	return vbs, errStatus, err
}

func getSNMPMulti(ctx context.Context, ip, community string, oids []string, timeout time.Duration) (map[string]SNMPVarBind, error) {
	pkt, err := buildSNMPRequest(pduGetRequest, community, 0x33445566, oids)
	if err != nil {
		return nil, err
	}
	vbs, errStatus, err := exchangeSNMP(ctx, ip, pkt, timeout)
	if err != nil {
		return nil, err
	}
	if errStatus != 0 && len(vbs) == 0 {
		return nil, fmt.Errorf("snmp error-status %d", errStatus)
	}
	res := make(map[string]SNMPVarBind, len(vbs))
	for _, vb := range vbs {
		if vb.Tag == snmpNoSuchObj || vb.Tag == snmpNoSuchInst || vb.Tag == snmpEndOfMib || vb.Tag == asn1Null {
			continue
		}
		res[vb.OID] = vb
	}
	return res, nil
}

func getSNMPOID(ip, community, oidStr string, timeout time.Duration) ([]byte, error) {
	res, err := getSNMPMulti(context.Background(), ip, community, []string{oidStr}, timeout)
	if err != nil {
		return nil, err
	}
	vb, ok := res[strings.Trim(oidStr, ".")]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return vb.Value, nil
}

func getSNMPUint32(ip, community, oidStr string, timeout time.Duration) (uint32, bool) {
	val, err := getSNMPOID(ip, community, oidStr, timeout)
	if err != nil || len(val) == 0 {
		return 0, false
	}
	return decodeASN1Uint32(val)
}

func getSNMPInt(ip, community, oidStr string, timeout time.Duration) (int, bool) {
	val, err := getSNMPOID(ip, community, oidStr, timeout)
	if err != nil || len(val) == 0 || len(val) > 4 {
		return 0, false
	}
	return decodeASN1Int(val), true
}

func compareOIDs(a, b string) int {
	pa := strings.Split(strings.Trim(a, "."), ".")
	pb := strings.Split(strings.Trim(b, "."), ".")
	minLen := len(pa)
	if len(pb) < minLen {
		minLen = len(pb)
	}
	for i := 0; i < minLen; i++ {
		na, _ := strconv.ParseUint(pa[i], 10, 32)
		nb, _ := strconv.ParseUint(pb[i], 10, 32)
		if na < nb {
			return -1
		}
		if na > nb {
			return 1
		}
	}
	if len(pa) < len(pb) {
		return -1
	}
	if len(pa) > len(pb) {
		return 1
	}
	return 0
}

// walkSNMP performs an SNMP GetNext walk under rootOID with a strict maxRows safety guard.
func walkSNMP(ctx context.Context, ip, community, rootOID string, maxRows int, timeout time.Duration) ([]SNMPVarBind, error) {
	if maxRows <= 0 {
		maxRows = 64
	}
	root := strings.Trim(rootOID, ".")
	cur := root
	var results []SNMPVarBind
	reqID := uint32(0x44000001)

	for i := 0; i < maxRows; i++ {
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
		pkt, err := buildSNMPRequest(pduGetNextRequest, community, reqID, []string{cur})
		reqID++
		if err != nil {
			return results, err
		}
		vbs, errStatus, err := exchangeSNMP(ctx, ip, pkt, timeout)
		if err != nil {
			if len(results) > 0 {
				return results, nil
			}
			return nil, err
		}
		if errStatus != 0 || len(vbs) == 0 {
			break
		}
		vb := vbs[0]
		if vb.Tag == snmpEndOfMib || vb.Tag == snmpNoSuchObj || vb.Tag == snmpNoSuchInst {
			break
		}
		if vb.OID != root && !strings.HasPrefix(vb.OID, root+".") {
			break
		}
		// Prevent infinite loop on broken SNMP agents returning non-increasing OIDs
		if compareOIDs(vb.OID, cur) <= 0 {
			break
		}
		results = append(results, vb)
		cur = vb.OID
	}
	return results, nil
}

// decodeErrorStateBitmask translates RFC 2790 hrPrinterDetectedErrorState OCTET STRING bits.
func decodeErrorStateBitmask(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var reasons []string
	b0 := raw[0]
	flags0 := []struct {
		mask byte
		name string
	}{
		{0x80, "low_paper"},
		{0x40, "no_paper"},
		{0x20, "low_toner"},
		{0x10, "no_toner"},
		{0x08, "door_open"},
		{0x04, "jammed"},
		{0x02, "offline"},
		{0x01, "service_requested"},
	}
	for _, f := range flags0 {
		if b0&f.mask != 0 {
			reasons = append(reasons, f.name)
		}
	}
	if len(raw) > 1 {
		b1 := raw[1]
		flags1 := []struct {
			mask byte
			name string
		}{
			{0x80, "input_tray_missing"},
			{0x40, "output_tray_missing"},
			{0x20, "marker_supply_missing"},
			{0x10, "output_near_full"},
			{0x08, "output_full"},
			{0x04, "input_tray_empty"},
			{0x02, "overdue_prevent_maint"},
		}
		for _, f := range flags1 {
			if b1&f.mask != 0 {
				reasons = append(reasons, f.name)
			}
		}
	}
	return reasons
}

func inferSupplyColor(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "cyan") || strings.Contains(lower, "ciano"):
		return "cyan"
	case strings.Contains(lower, "magenta"):
		return "magenta"
	case strings.Contains(lower, "yellow") || strings.Contains(lower, "amarelo"):
		return "yellow"
	case strings.Contains(lower, "photo black") || strings.Contains(lower, "pk"):
		return "photo_black"
	case strings.Contains(lower, "tri-color") || strings.Contains(lower, "cmy") || strings.Contains(lower, "color"):
		return "color"
	case strings.Contains(lower, "black") || strings.Contains(lower, "preto") || strings.Contains(lower, "bk"):
		return "black"
	case strings.Contains(lower, "waste") || strings.Contains(lower, "residuo") || strings.Contains(lower, "manutencao") || strings.Contains(lower, "maintenance"):
		return "waste"
	default:
		return "black"
	}
}

func mapSupplyTypeName(supplyType int) string {
	switch supplyType {
	case 3:
		return "toner"
	case 4:
		return "waste_toner"
	case 5:
		return "ink"
	case 6:
		return "ink_cartridge"
	case 9:
		return "opc_drum"
	case 10:
		return "developer"
	case 15:
		return "fuser"
	case 20:
		return "transfer_unit"
	case 21:
		return "toner_cartridge"
	case 23:
		return "waste_ink"
	default:
		return "other"
	}
}

// buildSupplyItem constructs a SupplyItem strictly following RFC 3805 semantics without inventing fake percentages.
func buildSupplyItem(name, color string, supplyClassCode, supplyTypeCode, level, maxCap int) SupplyItem {
	if color == "" {
		color = inferSupplyColor(name)
	}
	supplyClass := "consumed"
	if supplyClassCode == 4 {
		supplyClass = "waste"
	}
	item := SupplyItem{
		Name:        strings.TrimSpace(name),
		Color:       color,
		SupplyClass: supplyClass,
		SupplyType:  mapSupplyTypeName(supplyTypeCode),
		Level:       level,
		MaxCapacity: maxCap,
	}

	switch {
	case level == -1:
		item.LevelState = "other"
		item.Percent = nil
	case level == -2:
		item.LevelState = "unknown"
		item.Percent = nil
	case level == -3:
		item.LevelState = "some_remaining"
		item.Percent = nil
	case level >= 0 && maxCap > 0:
		pct := int(math.Round(float64(level) / float64(maxCap) * 100.0))
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		item.LevelState = "exact"
		item.Percent = &pct
	default:
		item.LevelState = "unknown"
		item.Percent = nil
	}
	return item
}

func formatMAC(raw []byte) string {
	if len(raw) != 6 {
		return ""
	}
	allZero := true
	for _, b := range raw {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return ""
	}
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", raw[0], raw[1], raw[2], raw[3], raw[4], raw[5])
}

func lastOIDIndex(oid, prefix string) string {
	if !strings.HasPrefix(oid, prefix+".") {
		return ""
	}
	return strings.TrimPrefix(oid, prefix+".")
}

func probeDeviceSNMP(ctx context.Context, ip, community string, enableWakeup bool) (*DiscoveredDevice, bool) {
	timeout := 900 * time.Millisecond
	baseVars, err := getSNMPMulti(ctx, ip, community, []string{
		"1.3.6.1.2.1.1.1.0",        // sysDescr
		"1.3.6.1.2.1.1.2.0",        // sysObjectID
		"1.3.6.1.2.1.1.3.0",        // sysUpTime
		"1.3.6.1.2.1.25.3.2.1.3.1", // hrDeviceDescr
	}, timeout)
	if err != nil && enableWakeup {
		if c, dErr := net.DialTimeout("tcp", net.JoinHostPort(ip, "80"), 250*time.Millisecond); dErr == nil {
			_, _ = c.Write([]byte("HEAD / HTTP/1.0\r\n\r\n"))
			_ = c.Close()
			time.Sleep(150 * time.Millisecond)
			baseVars, err = getSNMPMulti(ctx, ip, community, []string{
				"1.3.6.1.2.1.1.1.0",
				"1.3.6.1.2.1.1.2.0",
				"1.3.6.1.2.1.1.3.0",
				"1.3.6.1.2.1.25.3.2.1.3.1",
			}, timeout)
		}
	}
	if err != nil || len(baseVars) == 0 {
		return nil, false
	}

	model := strings.TrimSpace(string(baseVars["1.3.6.1.2.1.1.1.0"].Value))
	if vb, ok := baseVars["1.3.6.1.2.1.25.3.2.1.3.1"]; ok && len(strings.TrimSpace(string(vb.Value))) > 0 {
		model = strings.TrimSpace(string(vb.Value))
	}
	var sysObjID string
	if vb, ok := baseVars["1.3.6.1.2.1.1.2.0"]; ok {
		if decoded, dErr := decodeOID(vb.Value); dErr == nil {
			sysObjID = decoded
		}
	}
	var uptimeSec uint32
	if vb, ok := baseVars["1.3.6.1.2.1.1.3.0"]; ok {
		if ticks, okU := decodeASN1Uint32(vb.Value); okU {
			uptimeSec = ticks / 100
		}
	}

	vendor, pen := detectVendorFromModel(model + " " + sysObjID)

	// Real MAC address from ifPhysAddress
	mac := ""
	if macRows, wErr := walkSNMP(ctx, ip, community, "1.3.6.1.2.1.2.2.1.6", 16, timeout); wErr == nil {
		for _, r := range macRows {
			if m := formatMAC(r.Value); m != "" {
				mac = m
				break
			}
		}
	}

	// Serial number
	serial := ""
	for _, sOID := range []string{
		"1.3.6.1.2.1.43.5.1.1.17.1",
		"1.3.6.1.4.1.11.2.3.9.4.2.1.1.3.3.0",
		"1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.1.0",
		"1.3.6.1.4.1.367.3.2.1.2.1.4.0",
	} {
		if b, sErr := getSNMPOID(ip, community, sOID, timeout); sErr == nil {
			cleaned := sanitizeSerial(string(b))
			if cleaned != "" {
				serial = cleaned
				break
			}
		}
	}

	// Status + Error state bitmask
	status := "online"
	var reasons []string
	if hrVars, hErr := getSNMPMulti(ctx, ip, community, []string{
		"1.3.6.1.2.1.25.3.5.1.1.1", // hrPrinterStatus
		"1.3.6.1.2.1.25.3.5.1.2.1", // hrPrinterDetectedErrorState
	}, timeout); hErr == nil {
		if st, ok := hrVars["1.3.6.1.2.1.25.3.5.1.1.1"]; ok {
			switch decodeASN1Int(st.Value) {
			case 4:
				status = "printing"
			case 5:
				status = "warmup"
			case 1:
				status = "warning"
			}
		}
		if es, ok := hrVars["1.3.6.1.2.1.25.3.5.1.2.1"]; ok {
			reasons = decodeErrorStateBitmask(es.Value)
			for _, r := range reasons {
				if r == "offline" {
					status = "offline"
				} else if r == "jammed" || r == "no_toner" || r == "no_paper" || r == "door_open" {
					status = "alert"
				}
			}
		}
	}

	// prtAlertTable walk
	var alerts []PrinterAlert
	if alertDescs, aErr := walkSNMP(ctx, ip, community, "1.3.6.1.2.1.43.18.1.1.8", 24, timeout); aErr == nil && len(alertDescs) > 0 {
		codeMap := make(map[string]int)
		if alertCodes, cErr := walkSNMP(ctx, ip, community, "1.3.6.1.2.1.43.18.1.1.7", 24, timeout); cErr == nil {
			for _, c := range alertCodes {
				idx := lastOIDIndex(c.OID, "1.3.6.1.2.1.43.18.1.1.7")
				codeMap[idx] = decodeASN1Int(c.Value)
			}
		}
		sevMap := make(map[string]int)
		if alertSevs, sErr := walkSNMP(ctx, ip, community, "1.3.6.1.2.1.43.18.1.1.2", 24, timeout); sErr == nil {
			for _, s := range alertSevs {
				idx := lastOIDIndex(s.OID, "1.3.6.1.2.1.43.18.1.1.2")
				sevMap[idx] = decodeASN1Int(s.Value)
			}
		}
		for _, d := range alertDescs {
			idx := lastOIDIndex(d.OID, "1.3.6.1.2.1.43.18.1.1.8")
			sev := "warning"
			if sevMap[idx] == 3 {
				sev = "critical"
			}
			desc := strings.TrimSpace(string(d.Value))
			if desc != "" {
				alerts = append(alerts, PrinterAlert{
					Code:        codeMap[idx],
					Severity:    sev,
					Description: desc,
				})
			}
		}
	}

	// Counters
	var counters DeviceCounters
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
			counters.LifetimeEngineTotal = v
			break
		}
	}
	for _, mOID := range []string{
		"1.3.6.1.4.1.11.2.3.9.4.2.1.4.1.2.6.0",
		"1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.52.2.1.3.2",
		"1.3.6.1.4.1.1602.1.11.1.3.1.4.108",
	} {
		if v, ok := getSNMPUint32(ip, community, mOID, timeout); ok && v > 0 {
			counters.MonoTotal = v
			break
		}
	}
	for _, cOID := range []string{
		"1.3.6.1.4.1.11.2.3.9.4.2.1.4.1.2.7.0",
		"1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.52.2.1.3.3",
		"1.3.6.1.4.1.1602.1.11.1.3.1.4.122",
	} {
		if v, ok := getSNMPUint32(ip, community, cOID, timeout); ok && v > 0 {
			counters.ColorTotal = v
			break
		}
	}

	// Zebra ZEBRA-QL-MIB (1.3.6.1.4.1.10642.200.17.*)
	if vendor == "Zebra" || strings.Contains(sysObjID, "1.3.6.1.4.1.10642") {
		if zVars, zErr := getSNMPMulti(ctx, ip, community, []string{
			"1.3.6.1.4.1.10642.200.17.1.0", // zql-odometer-user-label-count
			"1.3.6.1.4.1.10642.200.17.2.0", // zql-odometer-total-label-count
			"1.3.6.1.4.1.10642.200.17.3.0", // zql-odometer-total-print-length
		}, timeout); zErr == nil {
			if vb, ok := zVars["1.3.6.1.4.1.10642.200.17.2.0"]; ok {
				if lbl, okU := decodeASN1Uint32(vb.Value); okU {
					counters.LabelCount = lbl
					if counters.LifetimeEngineTotal == 0 {
						counters.LifetimeEngineTotal = lbl
					}
				}
			}
			if vb, ok := zVars["1.3.6.1.4.1.10642.200.17.3.0"]; ok {
				if ln, okU := decodeASN1Uint32(vb.Value); okU {
					counters.PrintLengthMM = ln
				}
			}
		}
	}

	// RFC 3805 Supplies Walk (no hardcoded 1..4 limit)
	supplies, colorCapable := walkRFC3805Supplies(ctx, ip, community, timeout)
	if counters.ColorTotal > 0 {
		colorCapable = true
	}

	if counters.MonoTotal == 0 && counters.ColorTotal == 0 {
		counters.MonoTotal = counters.LifetimeEngineTotal
	} else if counters.LifetimeEngineTotal == 0 {
		counters.LifetimeEngineTotal = counters.MonoTotal + counters.ColorTotal
	}

	return &DiscoveredDevice{
		DiscoverySource: "snmp_v2c",
		IPAddress:       ip,
		MACAddress:      mac,
		SerialNumber:    serial,
		SysObjectID:     sysObjID,
		UptimeSeconds:   uptimeSec,
		VendorPEN:       pen,
		Vendor:          vendor,
		Model:           model,
		Status:          status,
		StatusReasons:   reasons,
		Alerts:          alerts,
		IsColorCapable:  colorCapable,
		Counters:        counters,
		Supplies:        supplies,
	}, true
}

func walkRFC3805Supplies(ctx context.Context, ip, community string, timeout time.Duration) ([]SupplyItem, bool) {
	const (
		baseClass = "1.3.6.1.2.1.43.11.1.1.4"
		baseType  = "1.3.6.1.2.1.43.11.1.1.5"
		baseDesc  = "1.3.6.1.2.1.43.11.1.1.6"
		baseMax   = "1.3.6.1.2.1.43.11.1.1.8"
		baseLevel = "1.3.6.1.2.1.43.11.1.1.9"
	)

	descRows, err := walkSNMP(ctx, ip, community, baseDesc, 32, timeout)
	if err != nil || len(descRows) == 0 {
		return nil, false
	}

	classMap := make(map[string]int)
	if rows, rErr := walkSNMP(ctx, ip, community, baseClass, 32, timeout); rErr == nil {
		for _, r := range rows {
			classMap[lastOIDIndex(r.OID, baseClass)] = decodeASN1Int(r.Value)
		}
	}
	typeMap := make(map[string]int)
	if rows, rErr := walkSNMP(ctx, ip, community, baseType, 32, timeout); rErr == nil {
		for _, r := range rows {
			typeMap[lastOIDIndex(r.OID, baseType)] = decodeASN1Int(r.Value)
		}
	}
	maxMap := make(map[string]int)
	if rows, rErr := walkSNMP(ctx, ip, community, baseMax, 32, timeout); rErr == nil {
		for _, r := range rows {
			maxMap[lastOIDIndex(r.OID, baseMax)] = decodeASN1Int(r.Value)
		}
	}
	lvlMap := make(map[string]int)
	if rows, rErr := walkSNMP(ctx, ip, community, baseLevel, 32, timeout); rErr == nil {
		for _, r := range rows {
			lvlMap[lastOIDIndex(r.OID, baseLevel)] = decodeASN1Int(r.Value)
		}
	}

	colorCapable := false
	if colorants, cErr := walkSNMP(ctx, ip, community, "1.3.6.1.2.1.43.12.1.1.4", 16, timeout); cErr == nil {
		for _, c := range colorants {
			cv := strings.ToLower(string(c.Value))
			if strings.Contains(cv, "cyan") || strings.Contains(cv, "magenta") || strings.Contains(cv, "yellow") {
				colorCapable = true
				break
			}
		}
	}

	var supplies []SupplyItem
	for _, d := range descRows {
		idx := lastOIDIndex(d.OID, baseDesc)
		name := strings.TrimSpace(string(d.Value))
		if name == "" {
			continue
		}
		lvl, hasLvl := lvlMap[idx]
		if !hasLvl {
			lvl = -2
		}
		maxCap := maxMap[idx]
		item := buildSupplyItem(name, "", classMap[idx], typeMap[idx], lvl, maxCap)
		if item.Color == "cyan" || item.Color == "magenta" || item.Color == "yellow" || item.Color == "color" {
			colorCapable = true
		}
		supplies = append(supplies, item)
	}
	return supplies, colorCapable
}
