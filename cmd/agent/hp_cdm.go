package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HPCDMData stores parsed telemetry retrieved from HP CDM (/cdm/...) REST JSON APIs.
type HPCDMData struct {
	Model        string
	SerialNumber string
	Firmware     string
	Hostname     string
	Status       string
	Counters     DeviceCounters
	Supplies     []SupplyItem
	CollectedAt  time.Time
}

type hpCDMCacheEntry struct {
	Data      *HPCDMData
	ExpiresAt time.Time
}

var (
	hpCDMGuardsMu sync.Mutex
	hpCDMGuards   = map[string]*sync.Mutex{}
	hpCDMCacheMu  sync.RWMutex
	hpCDMCache    = map[string]hpCDMCacheEntry{}
)

func getHPCDMIPGuard(ip string) *sync.Mutex {
	hpCDMGuardsMu.Lock()
	defer hpCDMGuardsMu.Unlock()
	m, ok := hpCDMGuards[ip]
	if !ok {
		m = &sync.Mutex{}
		hpCDMGuards[ip] = m
	}
	return m
}

func getCachedHPCDM(ip string) (*HPCDMData, bool) {
	hpCDMCacheMu.RLock()
	defer hpCDMCacheMu.RUnlock()
	entry, ok := hpCDMCache[ip]
	if !ok || entry.Data == nil || time.Now().After(entry.ExpiresAt) {
		return nil, false
	}
	return entry.Data, true
}

func setCachedHPCDM(ip string, data *HPCDMData, ttl time.Duration) {
	if data == nil {
		return
	}
	hpCDMCacheMu.Lock()
	defer hpCDMCacheMu.Unlock()
	hpCDMCache[ip] = hpCDMCacheEntry{
		Data:      data,
		ExpiresAt: time.Now().Add(ttl),
	}
}

func parseFlexibleBool(raw json.RawMessage) *bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return &b
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes", "genuine":
			v := true
			return &v
		case "false", "0", "no", "non-genuine", "counterfeit":
			v := false
			return &v
		}
	}
	return nil
}

func normalizeHPCDMSupplyState(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "", "unknown":
		return "unknown"
	case "ok", "ready", "normal", "informational", "okfull", "full":
		return "ok"
	case "low", "verylow", "very_low", "warning", "lowwarning", "replace_soon":
		return "low"
	case "empty", "depleted", "out", "error", "missing", "fault", "nonhp", "incompatible":
		return "empty"
	default:
		if strings.Contains(s, "empty") || strings.Contains(s, "depleted") {
			return "empty"
		}
		if strings.Contains(s, "low") {
			return "low"
		}
		if strings.Contains(s, "ok") || strings.Contains(s, "ready") {
			return "ok"
		}
		return "unknown"
	}
}

func normalizeHPColorAndLabel(code string) (normColor string, ptLabel string, shortCode string) {
	c := strings.ToUpper(strings.TrimSpace(code))
	switch c {
	case "K", "BLK", "BLACK", "PRETO":
		return "black", "Preta (K)", "K"
	case "C", "CYA", "CYAN", "CIANO":
		return "cyan", "Ciano (C)", "C"
	case "M", "MAG", "MAGENTA":
		return "magenta", "Magenta (M)", "M"
	case "Y", "YEL", "YELLOW", "AMARELO":
		return "yellow", "Amarela (Y)", "Y"
	case "CMY", "TRI", "TRICOLOR", "TRI-COLOR":
		return "color", "Colorida (CMY)", "CMY"
	case "CMYK", "COLOR":
		return "color", "Colorida (CMYK)", "CMYK"
	default:
		if strings.Contains(c, "K") && !strings.Contains(c, "C") {
			return "black", "Preta (K)", "K"
		}
		return "unknown", c, c
	}
}

func isLikelyHPPrinthead(supplyType, productNumber, description string) bool {
	st := strings.ToLower(strings.TrimSpace(supplyType))
	if st == "printhead" || strings.Contains(st, "print_head") {
		return true
	}
	pn := strings.ToUpper(strings.TrimSpace(productNumber))
	printheadPrefixes := []string{"M0H50", "M0H51", "X4E75", "6ZA17", "6ZA18", "C9381", "C9382", "F6V", "LOS"}
	for _, pref := range printheadPrefixes {
		if strings.HasPrefix(pn, pref) {
			return true
		}
	}
	desc := strings.ToLower(description)
	return strings.Contains(desc, "printhead") || strings.Contains(desc, "print head") || strings.Contains(desc, "cabeçote")
}

// ParseHPCDMSuppliesPublic parses HP CDM /cdm/supply/v1/suppliesPublic JSON payloads.
func ParseHPCDMSuppliesPublic(raw []byte) ([]SupplyItem, error) {
	var doc struct {
		SuppliesList []struct {
			SupplyType           string          `json:"supplyType"`
			SupplyColorCode      string          `json:"supplyColorCode"`
			Colors               []string        `json:"colors"`
			SupplyState          string          `json:"supplyState"`
			StateReasons         []string        `json:"stateReasons"`
			PercentLifeDisplay   *int            `json:"percentLifeDisplay"`
			ApproximateRemaining *bool           `json:"isApproximate"`
			ProductNumber        string          `json:"productNumber"`
			OrderPartNumber      string          `json:"orderPartNumber"`
			SelectabilityNumber  string          `json:"selectabilityNumber"`
			SerialNumber         string          `json:"serialNumber"`
			ManufactureDate      string          `json:"manufactureDate"`
			SupplyDescription    string          `json:"supplyDescription"`
			IsGenuineHP          json.RawMessage `json:"isGenuineHP"`
			WarrantyStatus       string          `json:"warrantyStatus"`
		} `json:"suppliesList"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("invalid HP CDM suppliesPublic JSON: %w", err)
	}

	items := make([]SupplyItem, 0, len(doc.SuppliesList))
	for _, entry := range doc.SuppliesList {
		stLower := strings.ToLower(strings.TrimSpace(entry.SupplyType))
		colorCode := strings.TrimSpace(entry.SupplyColorCode)
		if colorCode == "" && len(entry.Colors) > 0 {
			colorCode = strings.Join(entry.Colors, "")
		}
		normColor, ptLabel, shortCode := normalizeHPColorAndLabel(colorCode)
		state := normalizeHPCDMSupplyState(entry.SupplyState)
		if state == "unknown" && len(entry.StateReasons) > 0 {
			state = normalizeHPCDMSupplyState(strings.Join(entry.StateReasons, " "))
		}

		partNo := strings.TrimSpace(entry.ProductNumber)
		if partNo == "" {
			partNo = strings.TrimSpace(entry.OrderPartNumber)
		}
		if partNo == "" {
			partNo = strings.TrimSpace(entry.SelectabilityNumber)
		}

		genuine := parseFlexibleBool(entry.IsGenuineHP)

		item := SupplyItem{
			Color:           normColor,
			SupplyClass:     "consumed",
			SupplyType:      entry.SupplyType,
			SerialNumber:    strings.TrimSpace(entry.SerialNumber),
			PartNumber:      partNo,
			ManufactureDate: strings.TrimSpace(entry.ManufactureDate),
			SupplyState:     state,
			IsGenuine:       genuine,
		}

		if stLower == "inktank" || stLower == "tonertank" || stLower == "ink_tank" {
			item.Name = "Tanque de Tinta " + ptLabel
			if ptLabel == "" {
				item.Name = "Tanque de Tinta"
			}
			item.SupplyType = "inkTank"
			item.MeasurementMode = "state_only"
			item.LevelState = "state_only"
			item.Level = -3
			item.MaxCapacity = 100
			item.Percent = nil
			items = append(items, item)
			continue
		}

		if isLikelyHPPrinthead(entry.SupplyType, partNo, entry.SupplyDescription) {
			if shortCode != "" {
				item.Name = fmt.Sprintf("Cabeçote de Impressão (%s)", shortCode)
			} else {
				item.Name = "Cabeçote de Impressão"
			}
			item.SupplyType = "printhead"
			item.MeasurementMode = "state_only"
			item.LevelState = "state_only"
			item.Level = -3
			item.MaxCapacity = 100
			item.Percent = nil
			items = append(items, item)
			continue
		}

		if stLower == "wastetoner" || stLower == "maintenancekit" || strings.Contains(stLower, "waste") {
			item.SupplyClass = "waste"
		}

		// Continuous percentage only when firmware genuinely reports 0..100 for a metered cartridge.
		if entry.PercentLifeDisplay != nil && *entry.PercentLifeDisplay >= 0 && *entry.PercentLifeDisplay <= 100 {
			pct := *entry.PercentLifeDisplay
			item.MeasurementMode = "percentage"
			item.LevelState = "exact"
			item.Level = pct
			item.MaxCapacity = 100
			item.Percent = IntPtr(pct)
		} else {
			item.MeasurementMode = "state_only"
			item.LevelState = "state_only"
			item.Level = -3
			item.MaxCapacity = 100
			item.Percent = nil
		}

		if strings.TrimSpace(entry.SupplyDescription) != "" {
			item.Name = strings.TrimSpace(entry.SupplyDescription)
		} else if ptLabel != "" {
			item.Name = "Suprimento " + ptLabel
		} else {
			item.Name = "Suprimento"
		}
		items = append(items, item)
	}
	return items, nil
}

// ParseHPCDMLifetimeCounters parses HP CDM /cdm/deviceUsage/v1/lifetimeCounters JSON payloads.
func ParseHPCDMLifetimeCounters(raw []byte) (DeviceCounters, error) {
	var doc struct {
		PrintUsage struct {
			Impressions struct {
				Monochrome uint32 `json:"monochrome"`
				Color      uint32 `json:"color"`
				Total      uint32 `json:"total"`
			} `json:"impressions"`
			Sheets struct {
				Simplex uint32 `json:"simplex"`
				Duplex  uint32 `json:"duplex"`
				Total   uint32 `json:"total"`
			} `json:"sheets"`
		} `json:"printUsage"`
		ScanUsage struct {
			TotalImages   uint32 `json:"totalImages"`
			AdfImages     uint32 `json:"adfImages"`
			FlatbedImages uint32 `json:"flatbedImages"`
		} `json:"scanUsage"`
		JobUsage struct {
			PrintJobCount uint32 `json:"printJobCount"`
		} `json:"jobUsage"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return DeviceCounters{}, fmt.Errorf("invalid HP CDM lifetimeCounters JSON: %w", err)
	}

	mono := doc.PrintUsage.Impressions.Monochrome
	color := doc.PrintUsage.Impressions.Color
	total := doc.PrintUsage.Impressions.Total
	if total == 0 && (mono > 0 || color > 0) {
		total = mono + color
	}
	if mono == 0 && total > color && color > 0 {
		mono = total - color
	}
	scans := doc.ScanUsage.TotalImages
	if scans == 0 {
		scans = doc.ScanUsage.AdfImages + doc.ScanUsage.FlatbedImages
	}

	return DeviceCounters{
		LifetimeEngineTotal: total,
		MonoTotal:           mono,
		ColorTotal:          color,
		DuplexSheets:        doc.PrintUsage.Sheets.Duplex,
		ScanTotal:           scans,
	}, nil
}

func parseHPCDMIdentity(raw []byte) (model, serial, firmware, hostname string, err error) {
	var doc struct {
		MakeAndModel    json.RawMessage `json:"makeAndModel"`
		SerialNumber    string          `json:"serialNumber"`
		FirmwareVersion string          `json:"firmwareVersion"`
		HostName        string          `json:"hostName"`
	}
	if uErr := json.Unmarshal(raw, &doc); uErr != nil {
		return "", "", "", "", uErr
	}
	var mmObj struct {
		Name string `json:"name"`
		Base string `json:"base"`
	}
	if len(doc.MakeAndModel) > 0 {
		if json.Unmarshal(doc.MakeAndModel, &mmObj) == nil && (mmObj.Name != "" || mmObj.Base != "") {
			model = strings.TrimSpace(mmObj.Name)
			if model == "" {
				model = strings.TrimSpace(mmObj.Base)
			}
		} else {
			var mmStr string
			if json.Unmarshal(doc.MakeAndModel, &mmStr) == nil {
				model = strings.TrimSpace(mmStr)
			}
		}
	}
	return model, strings.TrimSpace(doc.SerialNumber), strings.TrimSpace(doc.FirmwareVersion), strings.TrimSpace(doc.HostName), nil
}

func fetchHPCDMJSON(ctx context.Context, client *http.Client, baseURL, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("HTTP %d from %s%s", resp.StatusCode, baseURL, path)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return nil, err
	}
	return body, nil
}

func collectHPCDMSequential(ctx context.Context, ip string, needUsage bool) (*HPCDMData, bool) {
	if cached, ok := getCachedHPCDM(ip); ok {
		if !needUsage || cached.Counters.LifetimeEngineTotal > 0 {
			return cached, true
		}
	}

	guard := getHPCDMIPGuard(ip)
	guard.Lock()
	defer guard.Unlock()

	if cached, ok := getCachedHPCDM(ip); ok {
		if !needUsage || cached.Counters.LifetimeEngineTotal > 0 {
			return cached, true
		}
	}

	client := newLocalHTTPClient(4500 * time.Millisecond)
	bases := []string{"https://" + ip, "http://" + ip}

	var activeBase string
	var idRaw []byte
	for _, b := range bases {
		raw, err := fetchHPCDMJSON(ctx, client, b, "/cdm/system/v1/identity")
		if err == nil {
			activeBase = b
			idRaw = raw
			break
		}
	}

	data := &HPCDMData{
		Status:      "online",
		CollectedAt: time.Now(),
	}
	gotAny := false

	if len(idRaw) > 0 {
		if model, serial, fw, host, err := parseHPCDMIdentity(idRaw); err == nil {
			data.Model = model
			data.SerialNumber = serial
			data.Firmware = fw
			data.Hostname = host
			gotAny = model != "" || serial != ""
		}
	}

	if activeBase == "" {
		// Some firmwares disable /identity unauthenticated while still serving suppliesPublic or lifetimeCounters.
		for _, b := range bases {
			if raw, err := fetchHPCDMJSON(ctx, client, b, "/cdm/supply/v1/suppliesPublic"); err == nil {
				activeBase = b
				if supplies, pErr := ParseHPCDMSuppliesPublic(raw); pErr == nil && len(supplies) > 0 {
					data.Supplies = supplies
					gotAny = true
				}
				break
			}
		}
	} else {
		if raw, err := fetchHPCDMJSON(ctx, client, activeBase, "/cdm/supply/v1/suppliesPublic"); err == nil {
			if supplies, pErr := ParseHPCDMSuppliesPublic(raw); pErr == nil && len(supplies) > 0 {
				data.Supplies = supplies
				gotAny = true
			}
		}
	}

	if activeBase == "" && needUsage {
		for _, b := range bases {
			if raw, err := fetchHPCDMJSON(ctx, client, b, "/cdm/deviceUsage/v1/lifetimeCounters"); err == nil {
				activeBase = b
				if counters, cErr := ParseHPCDMLifetimeCounters(raw); cErr == nil && (counters.LifetimeEngineTotal > 0 || counters.ColorTotal > 0 || counters.MonoTotal > 0) {
					data.Counters = counters
					gotAny = true
				}
				break
			}
		}
	} else if activeBase != "" && needUsage {
		if raw, err := fetchHPCDMJSON(ctx, client, activeBase, "/cdm/deviceUsage/v1/lifetimeCounters"); err == nil {
			if counters, cErr := ParseHPCDMLifetimeCounters(raw); cErr == nil && (counters.LifetimeEngineTotal > 0 || counters.ColorTotal > 0 || counters.MonoTotal > 0) {
				data.Counters = counters
				gotAny = true
			}
		}
	}

	if !gotAny {
		return nil, false
	}

	setCachedHPCDM(ip, data, 15*time.Second)
	return data, true
}

// MergeIPPAndHPCDM enriches a DiscoveredDevice with authoritative HP CDM identity,
// color/mono lifetime counters, and accurate state_only/printhead supply telemetry.
func MergeIPPAndHPCDM(dev *DiscoveredDevice, cdm *HPCDMData) {
	if dev == nil || cdm == nil {
		return
	}
	if strings.TrimSpace(cdm.Model) != "" && (dev.Model == "" || strings.EqualFold(dev.Model, "Desconhecido")) {
		dev.Model = strings.TrimSpace(cdm.Model)
	}
	if strings.TrimSpace(cdm.SerialNumber) != "" && (dev.SerialNumber == "" || strings.HasPrefix(dev.SerialNumber, "NO-SERIAL-")) {
		dev.SerialNumber = strings.TrimSpace(cdm.SerialNumber)
	}

	if cdm.Counters.LifetimeEngineTotal > 0 || cdm.Counters.ColorTotal > 0 || cdm.Counters.MonoTotal > 0 {
		// CDM lifetimeCounters is the ground truth for color vs mono separation on HP devices
		// where SNMP often omits OID color and sums total into mono_total.
		if cdm.Counters.LifetimeEngineTotal >= dev.Counters.LifetimeEngineTotal || dev.Counters.ColorTotal == 0 {
			if cdm.Counters.LifetimeEngineTotal > dev.Counters.LifetimeEngineTotal {
				dev.Counters.LifetimeEngineTotal = cdm.Counters.LifetimeEngineTotal
			}
			if cdm.Counters.ColorTotal > 0 {
				dev.Counters.ColorTotal = cdm.Counters.ColorTotal
				dev.Counters.MonoTotal = cdm.Counters.MonoTotal
				dev.IsColorCapable = true
			} else if cdm.Counters.MonoTotal > dev.Counters.MonoTotal {
				dev.Counters.MonoTotal = cdm.Counters.MonoTotal
			}
		}
		if cdm.Counters.DuplexSheets > dev.Counters.DuplexSheets {
			dev.Counters.DuplexSheets = cdm.Counters.DuplexSheets
		}
		if cdm.Counters.ScanTotal > dev.Counters.ScanTotal {
			dev.Counters.ScanTotal = cdm.Counters.ScanTotal
		}
		if dev.OdometerSource == "" || dev.OdometerSource == "snmp" {
			dev.OdometerSource = "hp_cdm"
		} else if !strings.Contains(dev.OdometerSource, "hp_cdm") {
			dev.OdometerSource += "+hp_cdm"
		}
	}

	if len(cdm.Supplies) > 0 {
		// Drop generic/synthetic IPP or placeholder supplies when CDM supplies are authoritative.
		kept := make([]SupplyItem, 0, len(cdm.Supplies)+len(dev.Supplies))
		for _, s := range dev.Supplies {
			lower := strings.ToLower(s.Name)
			if strings.Contains(lower, "tri-color") || strings.Contains(lower, "unknown") || s.LevelState == "unknown" || s.MeasurementMode == "state_only" {
				continue
			}
			// Also avoid color/type duplication against CDM items.
			dup := false
			for _, cs := range cdm.Supplies {
				if strings.EqualFold(cs.Color, s.Color) && strings.EqualFold(cs.SupplyType, s.SupplyType) {
					dup = true
					break
				}
			}
			if !dup {
				kept = append(kept, s)
			}
		}
		kept = append(kept, cdm.Supplies...)
		dev.Supplies = kept
	}
}
