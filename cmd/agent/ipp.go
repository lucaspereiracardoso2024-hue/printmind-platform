package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	ippOpGetJobs              uint16 = 0x000A
	ippOpGetPrinterAttributes uint16 = 0x000B

	ippTagOpAttrs      byte = 0x01
	ippTagJobAttrs     byte = 0x02
	ippTagEndOfAttrs   byte = 0x03
	ippTagPrinterAttrs byte = 0x04

	ippTagInteger     byte = 0x21
	ippTagBoolean     byte = 0x22
	ippTagEnum        byte = 0x23
	ippTagText        byte = 0x41
	ippTagName        byte = 0x42
	ippTagKeyword     byte = 0x44
	ippTagURI         byte = 0x45
	ippTagCharset     byte = 0x47
	ippTagNatLanguage byte = 0x48
)

// IPPAttributeGroup represents decoded IPP attributes grouped by tag.
type IPPAttributeGroup struct {
	Tag   byte
	Attrs map[string][]any
}

// IPPResponse represents a decoded binary IPP response.
type IPPResponse struct {
	VersionMajor byte
	VersionMinor byte
	StatusCode   uint16
	RequestID    uint32
	Groups       []IPPAttributeGroup
}

func writeIPPAttr(buf *bytes.Buffer, tag byte, name string, val []byte) {
	buf.WriteByte(tag)
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(name)))
	buf.Write(lenBuf[:])
	buf.WriteString(name)
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(val)))
	buf.Write(lenBuf[:])
	buf.Write(val)
}

// BuildIPPGetPrinterAttributes encodes a binary IPP/2.0 Get-Printer-Attributes request.
func BuildIPPGetPrinterAttributes(printerURI string, reqID uint32, requestedAttrs []string) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0x02) // IPP 2.0
	buf.WriteByte(0x00)
	_ = binary.Write(&buf, binary.BigEndian, ippOpGetPrinterAttributes)
	_ = binary.Write(&buf, binary.BigEndian, reqID)

	buf.WriteByte(ippTagOpAttrs)
	writeIPPAttr(&buf, ippTagCharset, "attributes-charset", []byte("utf-8"))
	writeIPPAttr(&buf, ippTagNatLanguage, "attributes-natural-language", []byte("en-us"))
	writeIPPAttr(&buf, ippTagURI, "printer-uri", []byte(printerURI))

	for i, attr := range requestedAttrs {
		name := ""
		if i == 0 {
			name = "requested-attributes"
		}
		writeIPPAttr(&buf, ippTagKeyword, name, []byte(attr))
	}
	buf.WriteByte(ippTagEndOfAttrs)
	return buf.Bytes()
}

// BuildIPPGetJobs encodes a binary IPP/1.1 Get-Jobs request.
func BuildIPPGetJobs(printerURI string, reqID uint32, whichJobs string) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0x01) // IPP 1.1 (broadly supported by CUPS)
	buf.WriteByte(0x01)
	_ = binary.Write(&buf, binary.BigEndian, ippOpGetJobs)
	_ = binary.Write(&buf, binary.BigEndian, reqID)

	buf.WriteByte(ippTagOpAttrs)
	writeIPPAttr(&buf, ippTagCharset, "attributes-charset", []byte("utf-8"))
	writeIPPAttr(&buf, ippTagNatLanguage, "attributes-natural-language", []byte("en-us"))
	writeIPPAttr(&buf, ippTagURI, "printer-uri", []byte(printerURI))
	if whichJobs != "" {
		writeIPPAttr(&buf, ippTagKeyword, "which-jobs", []byte(whichJobs))
	}
	for i, attr := range []string{"job-id", "job-name", "job-originating-user-name", "job-impressions-completed", "job-media-sheets-completed", "copies", "print-color-mode", "sides", "time-at-completed"} {
		name := ""
		if i == 0 {
			name = "requested-attributes"
		}
		writeIPPAttr(&buf, ippTagKeyword, name, []byte(attr))
	}
	buf.WriteByte(ippTagEndOfAttrs)
	return buf.Bytes()
}

// ParseIPPResponse decodes a binary IPP response packet into structured attribute groups.
func ParseIPPResponse(data []byte) (*IPPResponse, error) {
	if len(data) < 9 {
		return nil, fmt.Errorf("ipp response too short (%d bytes)", len(data))
	}
	resp := &IPPResponse{
		VersionMajor: data[0],
		VersionMinor: data[1],
		StatusCode:   binary.BigEndian.Uint16(data[2:4]),
		RequestID:    binary.BigEndian.Uint32(data[4:8]),
	}

	pos := 8
	var currentGroup *IPPAttributeGroup
	lastAttrName := ""

	for pos < len(data) {
		tag := data[pos]
		pos++
		if tag == ippTagEndOfAttrs {
			break
		}
		if tag < 0x10 {
			resp.Groups = append(resp.Groups, IPPAttributeGroup{
				Tag:   tag,
				Attrs: make(map[string][]any),
			})
			currentGroup = &resp.Groups[len(resp.Groups)-1]
			lastAttrName = ""
			continue
		}
		if pos+2 > len(data) {
			break
		}
		nameLen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2
		if pos+nameLen+2 > len(data) {
			break
		}
		name := string(data[pos : pos+nameLen])
		pos += nameLen
		if name != "" {
			lastAttrName = name
		} else {
			name = lastAttrName
		}

		valLen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2
		if pos+valLen > len(data) {
			break
		}
		valRaw := data[pos : pos+valLen]
		pos += valLen

		if currentGroup == nil || name == "" {
			continue
		}

		var val any
		switch tag {
		case ippTagInteger, ippTagEnum:
			if len(valRaw) == 4 {
				val = int(int32(binary.BigEndian.Uint32(valRaw)))
			}
		case ippTagBoolean:
			if len(valRaw) >= 1 {
				val = valRaw[0] != 0
			}
		case ippTagText, ippTagName, ippTagKeyword, ippTagURI, ippTagCharset, ippTagNatLanguage, 0x49:
			val = string(valRaw)
		default:
			if tag >= 0x35 && tag <= 0x4A {
				val = string(valRaw)
			} else {
				val = valRaw
			}
		}
		if val != nil {
			currentGroup.Attrs[name] = append(currentGroup.Attrs[name], val)
		}
	}
	return resp, nil
}

func ippGetString(attrs map[string][]any, key string) string {
	vals := attrs[key]
	if len(vals) == 0 {
		return ""
	}
	s, _ := vals[0].(string)
	return strings.TrimSpace(s)
}

func ippGetStrings(attrs map[string][]any, key string) []string {
	vals := attrs[key]
	var out []string
	for _, v := range vals {
		if s, ok := v.(string); ok {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func ippGetInts(attrs map[string][]any, key string) []int {
	vals := attrs[key]
	var out []int
	for _, v := range vals {
		if n, ok := v.(int); ok {
			out = append(out, n)
		}
	}
	return out
}

func ippGetBool(attrs map[string][]any, key string) (bool, bool) {
	vals := attrs[key]
	if len(vals) == 0 {
		return false, false
	}
	b, ok := vals[0].(bool)
	return b, ok
}

func newLocalHTTPClient(timeout time.Duration) *http.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many local HTTP redirects")
			}
			if len(via) > 0 {
				orig := via[0]
				// Preserve POST + IPP binary body across 301/302 HTTP->HTTPS redirects emitted by HP/Canon/Epson firmwares.
				if orig.Method == http.MethodPost && req.Method != http.MethodPost {
					req.Method = http.MethodPost
					if ct := orig.Header.Get("Content-Type"); ct != "" && req.Header.Get("Content-Type") == "" {
						req.Header.Set("Content-Type", ct)
					}
					if orig.GetBody != nil {
						bodyCopy, err := orig.GetBody()
						if err != nil {
							return err
						}
						req.Body = bodyCopy
						req.GetBody = orig.GetBody
						req.ContentLength = orig.ContentLength
					}
				}
			}
			return nil
		},
	}
}

func sendIPPRequest(ctx context.Context, client *http.Client, endpoint string, body []byte) (*IPPResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/ipp")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ipp http status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return nil, err
	}
	return ParseIPPResponse(raw)
}

var defaultIPPRequestedAttrs = []string{
	"printer-make-and-model",
	"printer-uuid",
	"device-uuid",
	"printer-dns-sd-name",
	"color-supported",
	"marker-names",
	"marker-levels",
	"marker-types",
	"marker-colors",
	"marker-high-levels",
	"marker-low-levels",
	"printer-impressions-completed",
	"printer-state",
	"printer-state-reasons",
}

// ParseIPPPrinterAttributes converts a decoded IPP Get-Printer-Attributes response into a DiscoveredDevice.
func ParseIPPPrinterAttributes(ip string, source string, resp *IPPResponse) (*DiscoveredDevice, bool) {
	if resp == nil || resp.StatusCode >= 0x0400 {
		return nil, false
	}
	merged := make(map[string][]any)
	for _, g := range resp.Groups {
		if g.Tag == ippTagPrinterAttrs || g.Tag == ippTagJobAttrs {
			for k, v := range g.Attrs {
				merged[k] = append(merged[k], v...)
			}
		}
	}
	if len(merged) == 0 {
		return nil, false
	}

	model := ippGetString(merged, "printer-make-and-model")
	if model == "" {
		model = ippGetString(merged, "printer-dns-sd-name")
	}
	if model == "" {
		return nil, false
	}
	vendor, pen := detectVendorFromModel(model)

	uuid := ippGetString(merged, "device-uuid")
	if uuid == "" {
		uuid = ippGetString(merged, "printer-uuid")
	}

	status := "online"
	if stInts := ippGetInts(merged, "printer-state"); len(stInts) > 0 {
		switch stInts[0] {
		case 4:
			status = "printing"
		case 5:
			status = "stopped"
		}
	}
	reasons := ippGetStrings(merged, "printer-state-reasons")
	var cleanReasons []string
	for _, r := range reasons {
		if r != "" && r != "none" {
			cleanReasons = append(cleanReasons, r)
		}
	}

	colorCapable, _ := ippGetBool(merged, "color-supported")

	mNames := ippGetStrings(merged, "marker-names")
	mLevels := ippGetInts(merged, "marker-levels")
	mTypes := ippGetStrings(merged, "marker-types")
	mColors := ippGetStrings(merged, "marker-colors")
	mHighs := ippGetInts(merged, "marker-high-levels")

	isTankModel := isTankPrinterWithoutContinuousLevel(model)
	allValidLevelsAtMax := len(mLevels) > 0
	validCount := 0
	for i, lvl := range mLevels {
		maxCap := 100
		if i < len(mHighs) && mHighs[i] > 0 {
			maxCap = mHighs[i]
		}
		if lvl >= 0 {
			validCount++
			if lvl != maxCap && lvl != 100 {
				allValidLevelsAtMax = false
				break
			}
		}
	}
	if validCount == 0 {
		allValidLevelsAtMax = false
	}

	var supplies []SupplyItem
	for i := 0; i < len(mNames) || i < len(mLevels); i++ {
		name := fmt.Sprintf("Suprimento %d", i+1)
		if i < len(mNames) && mNames[i] != "" {
			name = mNames[i]
		}
		lvl := -2
		if i < len(mLevels) {
			lvl = mLevels[i]
		}
		maxCap := 100
		if i < len(mHighs) && mHighs[i] > 0 {
			maxCap = mHighs[i]
		}
		mType := "toner"
		classCode := 3
		if i < len(mTypes) && mTypes[i] != "" {
			mType = mTypes[i]
			if strings.Contains(strings.ToLower(mType), "waste") {
				classCode = 4
			}
		}
		col := ""
		if i < len(mColors) && mColors[i] != "" {
			col = normalizeIPPColor(mColors[i], name)
		}
		if col == "" {
			col = inferSupplyColor(name)
		}
		if col == "cyan" || col == "magenta" || col == "yellow" || col == "color" {
			colorCapable = true
		}

		item := buildSupplyItem(name, col, classCode, 0, lvl, maxCap)
		item.SupplyType = mType

		if isTankModel && allValidLevelsAtMax && lvl >= 0 {
			// Tank models (HP Smart Tank, Epson EcoTank, Canon MegaTank) report static 100 across all tanks without an optical percentage sensor.
			item.MeasurementMode = "state_only"
			item.LevelState = "state_only"
			item.Percent = nil
			item.Level = -3
			item.SupplyState = deriveIPPSupplyState(cleanReasons)
		} else if lvl >= 0 && maxCap > 0 {
			pct := int(math.Round(float64(lvl) / float64(maxCap) * 100.0))
			if pct > 100 {
				pct = 100
			}
			item.Percent = &pct
			item.LevelState = "exact"
			item.MeasurementMode = "percentage"
			switch {
			case pct <= 0:
				item.SupplyState = "empty"
			case pct <= 15:
				item.SupplyState = "low"
			default:
				item.SupplyState = "ok"
			}
		} else {
			item.MeasurementMode = "state_only"
			if lvl == -3 {
				item.SupplyState = deriveIPPSupplyState(cleanReasons)
			} else {
				item.SupplyState = "unknown"
			}
		}
		supplies = append(supplies, item)
	}

	var impressions uint32
	if imp := ippGetInts(merged, "printer-impressions-completed"); len(imp) > 0 && imp[0] > 0 {
		impressions = uint32(imp[0])
	}
	odometerSource := ""
	if impressions > 0 {
		odometerSource = "ipp"
	}

	return &DiscoveredDevice{
		DiscoverySource: source,
		IPAddress:       ip,
		DeviceUUID:      uuid,
		VendorPEN:       pen,
		Vendor:          vendor,
		Model:           model,
		Status:          status,
		StatusReasons:   cleanReasons,
		IsColorCapable:  colorCapable,
		OdometerSource:  odometerSource,
		Counters: DeviceCounters{
			LifetimeEngineTotal: impressions,
		},
		Supplies: supplies,
	}, true
}

func normalizeIPPColor(hexOrName, fallbackName string) string {
	lower := strings.ToLower(strings.TrimSpace(hexOrName))
	switch lower {
	case "#000000", "black":
		return "black"
	case "#00ffff", "cyan":
		return "cyan"
	case "#ff00ff", "magenta":
		return "magenta"
	case "#ffff00", "yellow":
		return "yellow"
	}
	if strings.Contains(lower, "00ffff") {
		return "cyan"
	}
	if strings.Contains(lower, "ff00ff") {
		return "magenta"
	}
	if strings.Contains(lower, "ffff00") {
		return "yellow"
	}
	if strings.Contains(lower, "000000") {
		return "black"
	}
	return inferSupplyColor(fallbackName)
}

func isTankPrinterWithoutContinuousLevel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "smart tank") ||
		strings.Contains(m, "ecotank") ||
		strings.Contains(m, "megatank") ||
		strings.Contains(m, "ink tank") ||
		strings.Contains(m, "dcp-t") ||
		strings.Contains(m, "mfc-t")
}

func deriveIPPSupplyState(reasons []string) string {
	for _, r := range reasons {
		lr := strings.ToLower(r)
		if strings.Contains(lr, "marker-supply-empty") || strings.Contains(lr, "toner-empty") || strings.Contains(lr, "ink-empty") {
			return "empty"
		}
	}
	for _, r := range reasons {
		lr := strings.ToLower(r)
		if strings.Contains(lr, "marker-supply-low") || strings.Contains(lr, "toner-low") || strings.Contains(lr, "ink-low") {
			return "low"
		}
	}
	return "ok"
}

func probeDeviceIPP(ctx context.Context, client *http.Client, ip string, hint *LANDiscoveryHint) (*DiscoveredDevice, bool) {
	port := 631
	path := "ipp/print"
	scheme := "http"
	if hint != nil {
		if hint.IPPPort > 0 {
			port = hint.IPPPort
		}
		if hint.IPPPath != "" {
			path = strings.TrimPrefix(hint.IPPPath, "/")
		}
		if hint.HasIPPS && port == 443 {
			scheme = "https"
		}
	}

	candidates := []string{
		fmt.Sprintf("%s://%s:%d/%s", scheme, ip, port, path),
		fmt.Sprintf("https://%s:631/ipp/print", ip),
		fmt.Sprintf("http://%s:631/ipp/print", ip),
		fmt.Sprintf("http://%s:631/ipp", ip),
		fmt.Sprintf("https://%s:443/ipp/print", ip),
	}
	seen := make(map[string]bool)
	for _, endpoint := range candidates {
		if seen[endpoint] {
			continue
		}
		seen[endpoint] = true
		ippURI := strings.Replace(endpoint, "https://", "ipps://", 1)
		ippURI = strings.Replace(ippURI, "http://", "ipp://", 1)
		reqBytes := BuildIPPGetPrinterAttributes(ippURI, 1001, defaultIPPRequestedAttrs)
		resp, err := sendIPPRequest(ctx, client, endpoint, reqBytes)
		if err != nil {
			continue
		}
		if dev, ok := ParseIPPPrinterAttributes(ip, "ipp", resp); ok {
			if hint != nil && dev.MACAddress == "" && hint.MACAddress != "" {
				dev.MACAddress = hint.MACAddress
			}
			return dev, true
		}
	}
	return nil, false
}

// ParseProcNetTCPListeningPorts extracts listening localhost ports in [minPort, maxPort] from /proc/net/tcp.
func ParseProcNetTCPListeningPorts(content string, minPort, maxPort int) []int {
	var ports []int
	seen := make(map[int]bool)
	lines := strings.Split(content, "\n")
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		// st == 0A means TCP_LISTEN
		if fields[3] != "0A" {
			continue
		}
		parts := strings.Split(fields[1], ":")
		if len(parts) != 2 {
			continue
		}
		// 0100007F is 127.0.0.1 in little-endian hex; 00000000 is 0.0.0.0
		if parts[0] != "0100007F" && parts[0] != "00000000" {
			continue
		}
		p, err := strconv.ParseInt(parts[1], 16, 32)
		if err != nil {
			continue
		}
		port := int(p)
		if port >= minPort && port <= maxPort && !seen[port] {
			seen[port] = true
			ports = append(ports, port)
		}
	}
	return ports
}

func discoverLinuxIPPUSB(ctx context.Context, client *http.Client) []DiscoveredDevice {
	raw, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return nil
	}
	ports := ParseProcNetTCPListeningPorts(string(raw), 60000, 65535)
	var devices []DiscoveredDevice
	for _, port := range ports {
		endpoint := fmt.Sprintf("http://127.0.0.1:%d/ipp/print", port)
		ippURI := fmt.Sprintf("ipp://localhost:%d/ipp/print", port)
		reqBytes := BuildIPPGetPrinterAttributes(ippURI, uint32(2000+port), defaultIPPRequestedAttrs)
		resp, err := sendIPPRequest(ctx, client, endpoint, reqBytes)
		if err != nil {
			continue
		}
		if dev, ok := ParseIPPPrinterAttributes("127.0.0.1", "ipp_usb", resp); ok {
			dev.USBPort = fmt.Sprintf("ipp-usb:%d", port)
			devices = append(devices, *dev)
		}
	}
	return devices
}
