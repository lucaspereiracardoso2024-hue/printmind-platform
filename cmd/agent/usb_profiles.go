package main

import (
	"encoding/binary"
	"net"
	"regexp"
	"strings"
)

// IEEE1284Info holds parsed fields from a USB/Parallel IEEE 1284 Device ID string.
type IEEE1284Info struct {
	Raw          string
	Manufacturer string
	Model        string
	SerialNumber string
	CommandSet   []string
	Class        string
	Description  string
}

// WSDRegistryEntry represents a DAFWSDProvider registry record for unit-testable IP matching.
type WSDRegistryEntry struct {
	KeyName      string
	FriendlyName string
	IPAddress    string
	Location     string
}

var ipv4ExtractRegex = regexp.MustCompile(`\b((?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b`)

func detectVendorFromModel(model string) (string, int) {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "hp") || strings.Contains(m, "hewlett") || strings.Contains(m, "laserjet") || strings.Contains(m, "deskjet") || strings.Contains(m, "officejet") || strings.Contains(m, "smart tank") || strings.Contains(m, "11.2.3.9"):
		return "HP", 11
	case strings.Contains(m, "brother") || strings.Contains(m, "2435"):
		return "Brother", 2435
	case strings.Contains(m, "ricoh") || strings.Contains(m, "aficio") || strings.Contains(m, "367"):
		return "Ricoh", 367
	case strings.Contains(m, "kyocera") || strings.Contains(m, "ecosys") || strings.Contains(m, "taskalfa") || strings.Contains(m, "1347"):
		return "Kyocera", 1347
	case strings.Contains(m, "samsung") || strings.Contains(m, "236"):
		return "Samsung", 236
	case strings.Contains(m, "pantum") || strings.Contains(m, "40093"):
		return "Pantum", 40093
	case strings.Contains(m, "epson") || strings.Contains(m, "1248"):
		return "Epson", 1248
	case strings.Contains(m, "canon") || strings.Contains(m, "1602"):
		return "Canon", 1602
	case strings.Contains(m, "konica") || strings.Contains(m, "minolta") || strings.Contains(m, "bizhub"):
		return "Konica Minolta", 18334
	case strings.Contains(m, "xerox"):
		return "Xerox", 253
	case strings.Contains(m, "lexmark"):
		return "Lexmark", 641
	case strings.Contains(m, "zebra") || strings.Contains(m, "10642"):
		return "Zebra", 10642
	case strings.Contains(m, "elgin") || strings.Contains(m, "l42"):
		return "Elgin", 0
	case strings.Contains(m, "honeywell") || strings.Contains(m, "intermec"):
		return "Honeywell", 0
	case strings.Contains(m, "tsc"):
		return "TSC", 0
	case strings.Contains(m, "bematech"):
		return "Bematech", 0
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

func isVirtualPrinter(name, port, driver string) bool {
	combined := strings.ToLower(name + " " + port + " " + driver)
	virtualKeywords := []string{
		"microsoft print to pdf",
		"onenote",
		"xps document",
		"fax",
		"send to onenote",
		"pdfcreator",
		"foxit",
		"adobe pdf",
		"cutepdf",
		"nul:",
	}
	for _, kw := range virtualKeywords {
		if strings.Contains(combined, kw) {
			return true
		}
	}
	return false
}

// ParseIEEE1284DeviceID parses an IEEE 1284 Device ID buffer (with optional 2-byte BE length prefix).
func ParseIEEE1284DeviceID(raw []byte) IEEE1284Info {
	if len(raw) == 0 {
		return IEEE1284Info{}
	}
	data := raw
	if len(data) >= 2 {
		prefLen := int(binary.BigEndian.Uint16(data[:2]))
		if prefLen > 2 && prefLen <= len(data) && (data[2] >= 0x20 && data[2] <= 0x7E) {
			data = data[2:prefLen]
		}
	}
	str := strings.TrimRight(string(data), "\x00\r\n ")
	info := IEEE1284Info{Raw: str}

	for _, part := range strings.Split(str, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		colon := strings.IndexByte(part, ':')
		if colon <= 0 {
			continue
		}
		key := strings.ToUpper(strings.TrimSpace(part[:colon]))
		val := strings.TrimSpace(part[colon+1:])
		switch key {
		case "MFG", "MANUFACTURER":
			info.Manufacturer = val
		case "MDL", "MODEL":
			info.Model = val
		case "SN", "SER", "SERIALNUMBER":
			info.SerialNumber = sanitizeSerial(val)
		case "CMD", "COMMAND SET":
			for _, c := range strings.Split(val, ",") {
				c = strings.TrimSpace(c)
				if c != "" {
					info.CommandSet = append(info.CommandSet, strings.ToUpper(c))
				}
			}
		case "CLS", "CLASS":
			info.Class = val
		case "DES", "DESCRIPTION":
			info.Description = val
		}
	}
	return info
}

// ClassifyPrinterCapabilities infers supported PDL languages and color capability from vendor/model/CMD.
// Strictly differentiates Elgin L42 (BPLA/BPLB ONLY - never ZPL) from Elgin L42 Pro (auto ZPL2/BPLB/EPL/PPLA/PPLB).
func ClassifyPrinterCapabilities(vendor, model string, cmdSet []string) (langs []string, isColor bool, isThermal bool) {
	combined := strings.ToLower(vendor + " " + model)

	switch {
	case strings.Contains(combined, "l42 pro") || strings.Contains(combined, "l42pro"):
		return []string{"PPLA", "PPLB", "BPLB", "EPL", "EPL2", "ZPL", "ZPL2"}, false, true
	case strings.Contains(combined, "elgin") && strings.Contains(combined, "l42"):
		// Classic Elgin L42 (BTP) only supports BPLA/BPLB; NEVER send ZPL!
		return []string{"BPLA", "BPLB"}, false, true
	case strings.Contains(combined, "zebra"):
		return []string{"ZPL", "ZPL2", "EPL"}, false, true
	case strings.Contains(combined, "honeywell") || strings.Contains(combined, "tsc") || strings.Contains(combined, "sato") || strings.Contains(combined, "dymo") || strings.Contains(combined, "bematech"):
		return cmdSet, false, true
	}

	if len(cmdSet) > 0 {
		langs = append(langs, cmdSet...)
	}
	if strings.Contains(combined, "color") || strings.Contains(combined, "ecotank") || strings.Contains(combined, "megatank") || strings.Contains(combined, "smart tank") || strings.Contains(combined, "inkbenefit") || strings.Contains(combined, "officejet") || strings.Contains(combined, "deskjet") {
		isColor = true
	}
	return langs, isColor, false
}

// CalculateBPLBLabelLengthMM calculates linear label length in mm for BPLB (8 dots = 1 mm at 203 dpi).
func CalculateBPLBLabelLengthMM(heightDots, gapDots, dotsPerMM int) float64 {
	if dotsPerMM <= 0 {
		dotsPerMM = 8
	}
	if heightDots < 0 {
		heightDots = 0
	}
	if gapDots < 0 {
		gapDots = 0
	}
	return float64(heightDots+gapDots) / float64(dotsPerMM)
}

// MatchWSDPortIP resolves a WSD-* port name to an IPv4 address using DAFWSDProvider entries.
func MatchWSDPortIP(portName string, printerName string, entries []WSDRegistryEntry) string {
	pLower := strings.ToLower(strings.TrimSpace(portName))
	if !strings.HasPrefix(pLower, "wsd") {
		return ""
	}
	uuidPart := strings.TrimPrefix(pLower, "wsd-")
	if idx := strings.IndexByte(uuidPart, '.'); idx > 0 {
		uuidPart = uuidPart[:idx]
	}

	// 1. Direct UUID/key match
	for _, e := range entries {
		keyL := strings.ToLower(e.KeyName)
		if uuidPart != "" && strings.Contains(keyL, uuidPart) {
			if ip := firstValidIPv4(e.IPAddress); ip != "" {
				return ip
			}
			if ip := firstValidIPv4(e.Location); ip != "" {
				return ip
			}
		}
	}

	// 2. Fallback by FriendlyName matching printerName
	prLower := strings.ToLower(strings.TrimSpace(printerName))
	if prLower != "" {
		for _, e := range entries {
			if strings.EqualFold(strings.TrimSpace(e.FriendlyName), prLower) || strings.Contains(strings.ToLower(e.FriendlyName), prLower) {
				if ip := firstValidIPv4(e.IPAddress); ip != "" {
					return ip
				}
			}
		}
	}
	return ""
}

func firstValidIPv4(raw string) string {
	for _, m := range ipv4ExtractRegex.FindAllString(raw, -1) {
		ip := net.ParseIP(m)
		if ip != nil && ip.To4() != nil && !ip.IsLoopback() {
			return m
		}
	}
	return ""
}
