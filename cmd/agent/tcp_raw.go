package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// --- Zebra SGD / ZPL Read-Only Odometer (TCP 9100) ---

func buildZebraSGDGetVar(varName string) []byte {
	return []byte("! U1 getvar \"" + varName + "\"\r\n")
}

func parseZebraSGDString(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "\"")
	s = strings.TrimSpace(s)
	if s == "" || s == "?" {
		return ""
	}
	return s
}

func parseZebraSGDLabelCount(raw string) (uint32, bool) {
	val := parseZebraSGDString(raw)
	if val == "" {
		return 0, false
	}
	fields := strings.Fields(val)
	if len(fields) == 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint32(n), true
}

func parseZebraSGDPrintLengthMM(raw string) (uint32, bool) {
	val := parseZebraSGDString(raw)
	if val == "" {
		return 0, false
	}
	fields := strings.Fields(strings.ToLower(val))
	if len(fields) == 0 {
		return 0, false
	}
	num, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || num <= 0 {
		return 0, false
	}
	unit := ""
	if len(fields) > 1 {
		unit = fields[1]
	}
	var mm float64
	switch {
	case strings.HasPrefix(unit, "in"):
		mm = num * 25.4
	case unit == "cm":
		mm = num * 10.0
	case unit == "m":
		mm = num * 1000.0
	default:
		mm = num
	}
	if mm > float64(math.MaxUint32) {
		return 0, false
	}
	return uint32(math.Round(mm)), true
}

func queryTCP9100Line(ctx context.Context, conn net.Conn, payload []byte, readWait time.Duration) (string, error) {
	deadline := time.Now().Add(readWait)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)
	if _, err := conn.Write(payload); err != nil {
		return "", err
	}
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		if n == 0 {
			return "", err
		}
	}
	return string(buf[:n]), nil
}

// enrichZebraSGD9100 performs strictly read-only Zebra SGD getvar queries on TCP 9100
// ONLY when Vendor == "Zebra" and hardware odometer counters are missing.
func enrichZebraSGD9100(ctx context.Context, dev *DiscoveredDevice) {
	if dev == nil || dev.Vendor != "Zebra" || dev.IPAddress == "" || dev.IPAddress == "0.0.0.0" {
		return
	}
	if dev.Counters.LifetimeEngineTotal > 0 || dev.Counters.LabelCount > 0 {
		return
	}

	dialer := net.Dialer{Timeout: 1200 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(dev.IPAddress, "9100"))
	if err != nil {
		return
	}
	defer conn.Close()

	updated := false
	if raw, qErr := queryTCP9100Line(ctx, conn, buildZebraSGDGetVar("odometer.total_label_count"), 800*time.Millisecond); qErr == nil {
		if cnt, ok := parseZebraSGDLabelCount(raw); ok {
			dev.Counters.LabelCount = cnt
			if dev.Counters.LifetimeEngineTotal == 0 {
				dev.Counters.LifetimeEngineTotal = cnt
			}
			if dev.Counters.MonoTotal == 0 {
				dev.Counters.MonoTotal = cnt
			}
			updated = true
		}
	}

	if raw, qErr := queryTCP9100Line(ctx, conn, buildZebraSGDGetVar("odometer.total_print_length"), 800*time.Millisecond); qErr == nil {
		if mm, ok := parseZebraSGDPrintLengthMM(raw); ok {
			dev.Counters.PrintLengthMM = mm
			updated = true
		}
	}

	if dev.SerialNumber == "" {
		if raw, qErr := queryTCP9100Line(ctx, conn, buildZebraSGDGetVar("device.unique_id"), 800*time.Millisecond); qErr == nil {
			if ser := sanitizeSerial(parseZebraSGDString(raw)); ser != "" {
				dev.SerialNumber = ser
			}
		}
	}

	if updated && dev.OdometerSource == "" {
		dev.OdometerSource = "zpl_sgd"
	}
}

// --- PJL Read-Only PageCount Probe (TCP 9100) ---

var pjlPageCountRegex = regexp.MustCompile(`(?im)(?:PAGECOUNT\s*=\s*|^\s*)([0-9]{1,10})\s*$`)

func supportsPJLProtocol(dev *DiscoveredDevice) bool {
	if dev == nil {
		return false
	}
	for _, lang := range dev.SupportedLanguages {
		if strings.EqualFold(strings.TrimSpace(lang), "PJL") {
			return true
		}
	}
	if len(dev.SupportedLanguages) > 0 {
		// Explicit PDL list provided without PJL -> skip PJL
		return false
	}
	switch dev.Vendor {
	case "HP", "Brother", "Kyocera", "Ricoh", "Lexmark", "Xerox", "Konica Minolta", "Samsung":
		return true
	default:
		return false
	}
}

func buildPJLPageCountQuery() []byte {
	return []byte("\x1b%-12345X@PJL INFO PAGECOUNT\r\n\x1b%-12345X")
}

func parsePJLPageCountResponse(raw string) (uint32, bool) {
	cleaned := strings.ReplaceAll(raw, "\x0c", "")
	cleaned = strings.ReplaceAll(cleaned, "\r\n", "\n")
	lines := strings.Split(cleaned, "\n")
	seenHeader := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		upper := strings.ToUpper(trimmed)
		if strings.HasPrefix(upper, "@PJL INFO PAGECOUNT") {
			seenHeader = true
			continue
		}
		if strings.Contains(upper, "PAGECOUNT") {
			if m := pjlPageCountRegex.FindStringSubmatch(trimmed); len(m) == 2 {
				if n, err := strconv.ParseUint(m[1], 10, 32); err == nil && n > 0 {
					return uint32(n), true
				}
			}
		} else if seenHeader {
			if n, err := strconv.ParseUint(trimmed, 10, 32); err == nil && n > 0 {
				return uint32(n), true
			}
		}
	}
	return 0, false
}

// enrichPJL9100 queries @PJL INFO PAGECOUNT ONLY when the device supports PJL and page counters are still zero.
func enrichPJL9100(ctx context.Context, dev *DiscoveredDevice) {
	if !supportsPJLProtocol(dev) || dev.IPAddress == "" || dev.IPAddress == "0.0.0.0" {
		return
	}
	if dev.Counters.LifetimeEngineTotal > 0 {
		return
	}

	dialer := net.Dialer{Timeout: 1200 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(dev.IPAddress, "9100"))
	if err != nil {
		return
	}
	defer conn.Close()

	raw, err := queryTCP9100Line(ctx, conn, buildPJLPageCountQuery(), 900*time.Millisecond)
	if err != nil {
		return
	}
	if count, ok := parsePJLPageCountResponse(raw); ok && count > 0 {
		dev.Counters.LifetimeEngineTotal = count
		if dev.Counters.MonoTotal == 0 && dev.Counters.ColorTotal == 0 {
			dev.Counters.MonoTotal = count
		}
		if dev.OdometerSource == "" {
			dev.OdometerSource = "pjl"
		}
	}
}

// --- Canon BJNP Read-Only Identity Probe (UDP 8611) ---

const (
	bjnpCmdDiscover byte = 0x01
	bjnpCmdGetID    byte = 0x30
)

func buildCanonBJNPPacket(cmdCode byte, seq uint16) []byte {
	pkt := make([]byte, 16)
	copy(pkt[:4], "BJNP")
	pkt[4] = 0x01 // Printer device type
	pkt[5] = cmdCode
	binary.BigEndian.PutUint16(pkt[8:10], seq)
	return pkt
}

// parseCanonBJNPResponse decodes Canon BJNP UDP 8611 Discover (0x01) or GetID (0x30) responses.
func parseCanonBJNPResponse(data []byte) (LANDiscoveryHint, bool) {
	if len(data) < 16 || !bytes.HasPrefix(data, []byte("BJNP")) {
		return LANDiscoveryHint{}, false
	}
	devType := data[4]
	if (devType & 0x80) == 0 {
		return LANDiscoveryHint{}, false
	}
	cmdCode := data[5]
	payload := data[16:]

	switch cmdCode {
	case bjnpCmdDiscover:
		// Discover response payload: [0..3 flags][4: macLen=6][5: ipLen=4][6..11 MAC][12..15 IPv4]
		if len(payload) < 16 {
			return LANDiscoveryHint{}, false
		}
		macLen := int(payload[4])
		ipLen := int(payload[5])
		if macLen != 6 || ipLen != 4 {
			return LANDiscoveryHint{}, false
		}
		ip := net.IPv4(payload[12], payload[13], payload[14], payload[15]).String()
		if ip == "0.0.0.0" {
			return LANDiscoveryHint{}, false
		}
		return LANDiscoveryHint{
			IPAddress:   ip,
			MACAddress:  formatMAC(payload[6:12]),
			VendorHint:  "Canon",
			DiscoveryBy: "bjnp",
		}, true

	case bjnpCmdGetID:
		if len(payload) < 4 {
			return LANDiscoveryHint{}, false
		}
		info := ParseIEEE1284DeviceID(payload)
		if info.Model == "" && info.Manufacturer == "" {
			return LANDiscoveryHint{}, false
		}
		model := strings.TrimSpace(info.Manufacturer + " " + info.Model)
		if info.Manufacturer != "" && strings.HasPrefix(strings.ToLower(info.Model), strings.ToLower(info.Manufacturer)) {
			model = info.Model
		}
		_, isCol, _ := ClassifyPrinterCapabilities("Canon", model, info.CommandSet)
		return LANDiscoveryHint{
			VendorHint:   "Canon",
			ModelHint:    model,
			SerialHint:   info.SerialNumber,
			ColorHint:    isCol,
			CommandHints: info.CommandSet,
			DiscoveryBy:  "bjnp",
		}, true
	}
	return LANDiscoveryHint{}, false
}

func discoverCanonBJNP(ctx context.Context, wait time.Duration) []LANDiscoveryHint {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil
	}
	defer conn.Close()

	req := buildCanonBJNPPacket(bjnpCmdDiscover, 1)
	bcast := &net.UDPAddr{IP: net.IPv4bcast, Port: 8611}
	_, _ = conn.WriteToUDP(req, bcast)

	deadline := time.Now().Add(wait)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetReadDeadline(deadline)

	byIP := make(map[string]LANDiscoveryHint)
	buf := make([]byte, 2048)
	for {
		if ctx.Err() != nil {
			break
		}
		n, src, rErr := conn.ReadFromUDP(buf)
		if rErr != nil {
			break
		}
		hint, ok := parseCanonBJNPResponse(buf[:n])
		if !ok {
			continue
		}
		if hint.IPAddress == "" && src != nil {
			hint.IPAddress = src.IP.String()
		}
		if hint.IPAddress == "" {
			continue
		}
		existing := byIP[hint.IPAddress]
		existing.IPAddress = hint.IPAddress
		existing.VendorHint = "Canon"
		existing.DiscoveryBy = "bjnp"
		if hint.MACAddress != "" {
			existing.MACAddress = hint.MACAddress
		}
		if hint.ModelHint != "" {
			existing.ModelHint = hint.ModelHint
		}
		if hint.SerialHint != "" {
			existing.SerialHint = hint.SerialHint
		}
		existing.ColorHint = existing.ColorHint || hint.ColorHint
		if len(hint.CommandHints) > 0 {
			existing.CommandHints = hint.CommandHints
		}
		byIP[hint.IPAddress] = existing
	}

	out := make([]LANDiscoveryHint, 0, len(byIP))
	for _, h := range byIP {
		out = append(out, h)
	}
	return out
}

func probeCanonBJNPUnicast(ctx context.Context, ip string, hint *LANDiscoveryHint) (*DiscoveredDevice, bool) {
	dialer := net.Dialer{Timeout: 800 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "udp4", net.JoinHostPort(ip, "8611"))
	if err != nil {
		return nil, false
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(800 * time.Millisecond))
	if _, err := conn.Write(buildCanonBJNPPacket(bjnpCmdGetID, 2)); err != nil {
		return nil, false
	}
	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, false
	}
	idHint, ok := parseCanonBJNPResponse(buf[:n])
	if !ok {
		return nil, false
	}

	model := idHint.ModelHint
	if model == "" && hint != nil {
		model = hint.ModelHint
	}
	if model == "" {
		model = "Canon Network Printer"
	}
	mac := ""
	if hint != nil {
		mac = hint.MACAddress
	}
	serial := idHint.SerialHint
	if serial == "" && hint != nil {
		serial = hint.SerialHint
	}
	langs, isColor, _ := ClassifyPrinterCapabilities("Canon", model, idHint.CommandHints)
	if idHint.ColorHint || (hint != nil && hint.ColorHint) {
		isColor = true
	}

	return &DiscoveredDevice{
		DiscoverySource:    "canon_bjnp",
		IPAddress:          ip,
		MACAddress:         mac,
		SerialNumber:       serial,
		VendorPEN:          1602,
		Vendor:             "Canon",
		Model:              model,
		Status:             "online",
		IsColorCapable:     isColor,
		SupportedLanguages: langs,
		Supplies:           []SupplyItem{},
	}, true
}
