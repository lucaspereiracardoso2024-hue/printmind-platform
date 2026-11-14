package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- HP LEDM Sequential Collector + 5-min Cache + Attribute Merge ---

type HPLEDMData struct {
	Model          string
	SerialNumber   string
	Firmware       string
	Supplies       []SupplyItem
	Counters       DeviceCounters
	IsColorCapable bool
	Status         string
}

type hpCacheEntry struct {
	Data          HPLEDMData
	DynamicAt     time.Time
	ConfigAt      time.Time
	BackoffUntil  time.Time
	ConsecTimeout int
}

var (
	hpGuardsMu sync.Mutex
	hpIPGuards = make(map[string]*sync.Mutex)
	hpCacheMu  sync.Mutex
	hpCache    = make(map[string]*hpCacheEntry)
)

func getHPIPGuard(ip string) *sync.Mutex {
	hpGuardsMu.Lock()
	defer hpGuardsMu.Unlock()
	m, ok := hpIPGuards[ip]
	if !ok {
		m = &sync.Mutex{}
		hpIPGuards[ip] = m
	}
	return m
}

func fetchURLText(ctx context.Context, client *http.Client, rawURL string) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, rErr := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if rErr != nil {
		return "", resp.StatusCode, rErr
	}
	return string(body), resp.StatusCode, nil
}

// ParseHPProductConfigXML parses /DevMgmt/ProductConfigDyn.xml safely.
func ParseHPProductConfigXML(xmlData []byte) (model, serial, firmware string) {
	dec := xml.NewDecoder(bytes.NewReader(xmlData))
	var curTag string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			curTag = strings.ToLower(t.Name.Local)
		case xml.EndElement:
			curTag = ""
		case xml.CharData:
			val := strings.TrimSpace(string(t))
			if val == "" {
				continue
			}
			switch curTag {
			case "makeandmodel", "productname", "modelname":
				if model == "" {
					model = val
				}
			case "serialnumber":
				if serial == "" {
					serial = sanitizeSerial(val)
				}
			case "version", "revision":
				if firmware == "" {
					firmware = val
				}
			}
		}
	}
	return model, serial, firmware
}

// ParseHPConsumableConfigXML parses /DevMgmt/ConsumableConfigDyn.xml tolerating Print head, OPC, pK, and CMY.
func ParseHPConsumableConfigXML(xmlData []byte) ([]SupplyItem, bool) {
	dec := xml.NewDecoder(bytes.NewReader(xmlData))
	var supplies []SupplyItem
	colorCapable := false

	type rawCons struct {
		labelCode string
		typeEnum  string
		desc      string
		state     string
		serial    string
		partNum   string
		warranty  string
		pct       *int
		rawPct    *int
		isHead    bool
		isOPC     bool
	}

	var cur *rawCons
	var curTag string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			local := strings.ToLower(t.Name.Local)
			if local == "consumableinfo" || local == "consumable" {
				cur = &rawCons{}
			} else if cur != nil {
				curTag = local
			}
		case xml.EndElement:
			local := strings.ToLower(t.Name.Local)
			if (local == "consumableinfo" || local == "consumable") && cur != nil {
				name := strings.TrimSpace(cur.desc)
				if name == "" {
					name = strings.TrimSpace(cur.labelCode)
				}
				if name == "" {
					name = strings.TrimSpace(cur.typeEnum)
				}
				if name != "" {
					col := inferHPColor(cur.labelCode, name)
					if col == "cyan" || col == "magenta" || col == "yellow" || col == "color" {
						colorCapable = true
					}
					pctPtr := cur.pct
					if pctPtr == nil {
						pctPtr = cur.rawPct
					}
					lvl := -2
					lvlState := "unknown"
					if pctPtr != nil {
						v := *pctPtr
						if v >= 0 && v <= 100 {
							lvl = v
							lvlState = "exact"
						} else if v == -3 {
							pctPtr = nil
							lvl = -3
							lvlState = "some_remaining"
						} else {
							pctPtr = nil
						}
					}
					supType := "ink"
					lowerType := strings.ToLower(cur.typeEnum + " " + name)
					switch {
					case cur.isHead || strings.Contains(lowerType, "printhead") || strings.Contains(lowerType, "print head"):
						supType = "printhead"
					case cur.isOPC || strings.Contains(lowerType, "opc") || strings.Contains(lowerType, "drum"):
						supType = "opc_drum"
					case strings.Contains(lowerType, "toner"):
						supType = "toner"
					}
					supplies = append(supplies, SupplyItem{
						Name:         name,
						Color:        col,
						SupplyClass:  "consumed",
						SupplyType:   supType,
						Level:        lvl,
						MaxCapacity:  100,
						Percent:      pctPtr,
						LevelState:   lvlState,
						SerialNumber: sanitizeSerial(cur.serial),
						PartNumber:   strings.TrimSpace(cur.partNum),
						WarrantyEnd:  strings.TrimSpace(cur.warranty),
					})
				}
				cur = nil
			}
			curTag = ""
		case xml.CharData:
			if cur == nil || curTag == "" {
				continue
			}
			val := strings.TrimSpace(string(t))
			if val == "" {
				continue
			}
			switch curTag {
			case "consumablelabelcode":
				cur.labelCode = val
			case "consumabletypeenum":
				cur.typeEnum = val
				if strings.Contains(strings.ToLower(val), "printhead") {
					cur.isHead = true
				}
				if strings.Contains(strings.ToLower(val), "opc") {
					cur.isOPC = true
				}
			case "consumablestation", "consumablereplaceabilitytype", "description":
				if cur.desc == "" {
					cur.desc = val
				}
			case "consumablestate", "state":
				cur.state = val
			case "serialnumber":
				cur.serial = val
			case "productnumber", "consumablepartnumber", "consumableselectabilitynumber":
				if cur.partNum == "" {
					cur.partNum = val
				}
			case "warrantyexpirationdate", "expirationdate":
				if cur.warranty == "" {
					cur.warranty = val
				}
			case "consumablepercentagelevelremaining":
				if n, nErr := strconv.Atoi(val); nErr == nil {
					cur.pct = &n
				}
			case "consumablerawpercentagelevelremaining":
				if n, nErr := strconv.Atoi(val); nErr == nil {
					cur.rawPct = &n
				}
			}
		}
	}
	return supplies, colorCapable
}

func inferHPColor(labelCode, name string) string {
	combined := strings.ToLower(strings.TrimSpace(labelCode) + " " + strings.TrimSpace(name))
	switch {
	case strings.EqualFold(strings.TrimSpace(labelCode), "pk") || strings.Contains(combined, "photo black"):
		return "photo_black"
	case strings.EqualFold(strings.TrimSpace(labelCode), "cmy") || strings.Contains(combined, "tri-color") || strings.Contains(combined, "cmy"):
		return "color"
	case strings.EqualFold(strings.TrimSpace(labelCode), "c") || strings.Contains(combined, "cyan"):
		return "cyan"
	case strings.EqualFold(strings.TrimSpace(labelCode), "m") || strings.Contains(combined, "magenta"):
		return "magenta"
	case strings.EqualFold(strings.TrimSpace(labelCode), "y") || strings.Contains(combined, "yellow"):
		return "yellow"
	case strings.EqualFold(strings.TrimSpace(labelCode), "k") || strings.Contains(combined, "black") || strings.Contains(combined, "opc"):
		return "black"
	default:
		return inferSupplyColor(name)
	}
}

// ParseHPProductUsageXML parses /DevMgmt/ProductUsageDyn.xml counters and jam events.
func ParseHPProductUsageXML(xmlData []byte) DeviceCounters {
	dec := xml.NewDecoder(bytes.NewReader(xmlData))
	var c DeviceCounters
	var curTag string
	var inPrinterSubunit bool
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			curTag = strings.ToLower(t.Name.Local)
			if curTag == "printersubunit" {
				inPrinterSubunit = true
			}
		case xml.EndElement:
			if strings.ToLower(t.Name.Local) == "printersubunit" {
				inPrinterSubunit = false
			}
			curTag = ""
		case xml.CharData:
			val := strings.TrimSpace(string(t))
			if val == "" {
				continue
			}
			n, err := strconv.ParseUint(val, 10, 32)
			if err != nil {
				continue
			}
			switch curTag {
			case "totalimpressions":
				if inPrinterSubunit || c.LifetimeEngineTotal == 0 {
					c.LifetimeEngineTotal = uint32(n)
				}
			case "monochromeimpressions":
				if inPrinterSubunit || c.MonoTotal == 0 {
					c.MonoTotal = uint32(n)
				}
			case "colorimpressions":
				if inPrinterSubunit || c.ColorTotal == 0 {
					c.ColorTotal = uint32(n)
				}
			case "duplexsheets":
				if inPrinterSubunit || c.DuplexSheets == 0 {
					c.DuplexSheets = uint32(n)
				}
			case "jamevents":
				if inPrinterSubunit || c.JamsTotal == 0 {
					c.JamsTotal = uint32(n)
				}
			case "scanimages":
				if c.ScanTotal == 0 {
					c.ScanTotal = uint32(n)
				}
			}
		}
	}
	if c.LifetimeEngineTotal == 0 && (c.MonoTotal > 0 || c.ColorTotal > 0) {
		c.LifetimeEngineTotal = c.MonoTotal + c.ColorTotal
	}
	return c
}

// collectHPLEDMSequential queries HP LEDM endpoints strictly sequentially per IP with a 5-min dynamic cache.
func collectHPLEDMSequential(ctx context.Context, ip string, needUsage bool) (*HPLEDMData, bool) {
	guard := getHPIPGuard(ip)
	guard.Lock()
	defer guard.Unlock()

	now := time.Now()
	hpCacheMu.Lock()
	entry := hpCache[ip]
	if entry != nil && now.Before(entry.BackoffUntil) {
		cached := entry.Data
		hpCacheMu.Unlock()
		return &cached, cached.Model != "" || len(cached.Supplies) > 0
	}
	if entry != nil && now.Sub(entry.DynamicAt) < 5*time.Minute && len(entry.Data.Supplies) > 0 {
		cached := entry.Data
		hpCacheMu.Unlock()
		return &cached, true
	}
	hpCacheMu.Unlock()

	client := newLocalHTTPClient(4500 * time.Millisecond)
	baseURL := "https://" + ip

	var data HPLEDMData
	if entry != nil {
		data = entry.Data
	}

	// 1. ProductConfigDyn.xml (1 hour cache; try HTTPS first, fallback to HTTP)
	if entry == nil || now.Sub(entry.ConfigAt) >= time.Hour {
		body, code, err := fetchURLText(ctx, client, baseURL+"/DevMgmt/ProductConfigDyn.xml")
		if err != nil {
			baseURL = "http://" + ip
			body, code, err = fetchURLText(ctx, client, baseURL+"/DevMgmt/ProductConfigDyn.xml")
		}
		if err == nil && code == 200 {
			m, s, fw := ParseHPProductConfigXML([]byte(body))
			if m != "" {
				data.Model = m
			}
			if s != "" {
				data.SerialNumber = s
			}
			data.Firmware = fw
		} else if err != nil {
			recordHPBackoff(ip, entry, data)
			return nil, false
		}
	}

	// 2. ConsumableConfigDyn.xml (sequential after ProductConfigDyn)
	body2, code2, err2 := fetchURLText(ctx, client, baseURL+"/DevMgmt/ConsumableConfigDyn.xml")
	if err2 != nil && strings.HasPrefix(baseURL, "https://") {
		baseURL = "http://" + ip
		body2, code2, err2 = fetchURLText(ctx, client, baseURL+"/DevMgmt/ConsumableConfigDyn.xml")
	}
	if err2 == nil && code2 == 200 {
		sups, colCap := ParseHPConsumableConfigXML([]byte(body2))
		if len(sups) > 0 {
			data.Supplies = sups
		}
		if colCap {
			data.IsColorCapable = true
		}
	} else if err2 != nil {
		recordHPBackoff(ip, entry, data)
		return nil, false
	}

	// 3. ProductUsageDyn.xml (sequential; HTTP 404 on older Photosmart models is ignored cleanly)
	if needUsage {
		if body, code, err := fetchURLText(ctx, client, baseURL+"/DevMgmt/ProductUsageDyn.xml"); err == nil && code == 200 {
			c := ParseHPProductUsageXML([]byte(body))
			if c.LifetimeEngineTotal > 0 || c.MonoTotal > 0 || c.ColorTotal > 0 || c.JamsTotal > 0 {
				data.Counters = c
			}
			if c.ColorTotal > 0 {
				data.IsColorCapable = true
			}
		}
	}

	hpCacheMu.Lock()
	hpCache[ip] = &hpCacheEntry{
		Data:      data,
		DynamicAt: now,
		ConfigAt:  now,
	}
	hpCacheMu.Unlock()
	return &data, data.Model != "" || len(data.Supplies) > 0 || data.Counters.LifetimeEngineTotal > 0
}

func recordHPBackoff(ip string, prev *hpCacheEntry, data HPLEDMData) {
	hpCacheMu.Lock()
	defer hpCacheMu.Unlock()
	timeouts := 1
	if prev != nil {
		timeouts = prev.ConsecTimeout + 1
	}
	if timeouts > 4 {
		timeouts = 4
	}
	hpCache[ip] = &hpCacheEntry{
		Data:          data,
		BackoffUntil:  time.Now().Add(time.Duration(timeouts*2) * time.Minute),
		ConsecTimeout: timeouts,
	}
}

// MergeIPPAndHPLEDM performs attribute-level merge between IPP results and HP LEDM XML.
func MergeIPPAndHPLEDM(dev *DiscoveredDevice, ledm *HPLEDMData) {
	if dev == nil || ledm == nil {
		return
	}
	if dev.SerialNumber == "" && ledm.SerialNumber != "" {
		dev.SerialNumber = ledm.SerialNumber
	}
	if ledm.IsColorCapable {
		dev.IsColorCapable = true
	}
	if dev.Counters.LifetimeEngineTotal == 0 && ledm.Counters.LifetimeEngineTotal > 0 {
		dev.Counters = ledm.Counters
		dev.OdometerSource = "ews"
	} else {
		if dev.Counters.MonoTotal == 0 {
			dev.Counters.MonoTotal = ledm.Counters.MonoTotal
		}
		if dev.Counters.ColorTotal == 0 {
			dev.Counters.ColorTotal = ledm.Counters.ColorTotal
		}
		if dev.Counters.JamsTotal == 0 {
			dev.Counters.JamsTotal = ledm.Counters.JamsTotal
		}
		if dev.Counters.DuplexSheets == 0 {
			dev.Counters.DuplexSheets = ledm.Counters.DuplexSheets
		}
		if dev.Counters.ScanTotal == 0 {
			dev.Counters.ScanTotal = ledm.Counters.ScanTotal
		}
	}

	if len(dev.Supplies) == 0 && len(ledm.Supplies) > 0 {
		dev.Supplies = ledm.Supplies
		return
	}

	for i := range dev.Supplies {
		cur := &dev.Supplies[i]
		for _, ls := range ledm.Supplies {
			if cur.Color == ls.Color || strings.EqualFold(cur.Name, ls.Name) {
				if (cur.Percent == nil || cur.LevelState == "unknown") && ls.Percent != nil {
					cur.Percent = ls.Percent
					cur.Level = ls.Level
					cur.MaxCapacity = ls.MaxCapacity
					cur.LevelState = ls.LevelState
				}
				if cur.SerialNumber == "" {
					cur.SerialNumber = ls.SerialNumber
				}
				if cur.PartNumber == "" {
					cur.PartNumber = ls.PartNumber
				}
				if cur.WarrantyEnd == "" {
					cur.WarrantyEnd = ls.WarrantyEnd
				}
				break
			}
		}
	}
}

// --- Brother 3-OID TLV Decoder + EWS ---

// ParseBrotherTLV decodes Brother's binary TLV OctetStrings ([Tag:1][Type:0x01][Len:0x04][4B BE]) until 0xFF.
func ParseBrotherTLV(raw []byte) map[byte]uint32 {
	out := make(map[byte]uint32)
	pos := 0
	for pos < len(raw) {
		tag := raw[pos]
		if tag == 0xFF {
			break
		}
		if pos+3 > len(raw) {
			break
		}
		// typ := raw[pos+1]
		length := int(raw[pos+2])
		if pos+3+length > len(raw) {
			break
		}
		valSlice := raw[pos+3 : pos+3+length]
		var val uint32
		for _, b := range valSlice {
			val = (val << 8) | uint32(b)
		}
		out[tag] = val
		pos += 3 + length
	}
	return out
}

// ParseBrotherEWSHTML extracts counters/toner from Brother /general/information.html.
func ParseBrotherEWSHTML(htmlStr string) (total uint32, supplies []SupplyItem) {
	rePage := regexp.MustCompile(`(?i)Page\s+Counter[^0-9]*([0-9]{1,9})`)
	if m := rePage.FindStringSubmatch(htmlStr); len(m) == 2 {
		if n, err := strconv.ParseUint(m[1], 10, 32); err == nil {
			total = uint32(n)
		}
	}
	reToner := regexp.MustCompile(`(?i)(Toner|Drum|Black|Cyan|Magenta|Yellow)[^%]{0,60}?([0-9]{1,3})\s*%`)
	for _, m := range reToner.FindAllStringSubmatch(htmlStr, -1) {
		pct, err := strconv.Atoi(m[2])
		if err != nil || pct < 0 || pct > 100 {
			continue
		}
		supplies = append(supplies, buildSupplyItem(m[1], inferSupplyColor(m[1]), 3, 3, pct, 100))
	}
	return total, supplies
}

func enrichBrotherFromSNMP(ctx context.Context, dev *DiscoveredDevice, snmpCli *snmpClient) {
	const (
		oidMaint    = "1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.8.0"
		oidCounter  = "1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.10.0"
		oidNextCare = "1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.11.0"
	)
	if snmpCli != nil {
		vbs, err := snmpCli.getMulti(ctx, []string{oidMaint, oidCounter, oidNextCare}, 1200*time.Millisecond)
		if err == nil {
			if vb, ok := vbs[oidCounter]; ok {
				tags := ParseBrotherTLV(vb.Value)
				if v, exists := tags[0x00]; exists && v > 0 {
					dev.Counters.LifetimeEngineTotal = v
					if dev.OdometerSource == "" {
						dev.OdometerSource = "snmp"
					}
				}
				if v, exists := tags[0x01]; exists && v > 0 {
					dev.Counters.MonoTotal = v
				}
				if v, exists := tags[0x06]; exists && v > 0 && dev.Counters.MonoTotal == 0 {
					dev.Counters.MonoTotal = v
				}
				if v, exists := tags[0x02]; exists && v > 0 {
					dev.Counters.ColorTotal = v
					dev.IsColorCapable = true
				}
			}
			if vb, ok := vbs[oidMaint]; ok {
				tags := ParseBrotherTLV(vb.Value)
				if len(dev.Supplies) == 0 {
					tagNames := map[byte]string{
						0x81: "Black Toner",
						0x82: "Cyan Toner",
						0x83: "Magenta Toner",
						0x84: "Yellow Toner",
						0x6F: "Drum Unit",
						0x79: "Fuser Unit",
					}
					for tag, name := range tagNames {
						if pct, exists := tags[tag]; exists && pct <= 100 {
							item := buildSupplyItem(name, inferSupplyColor(name), 3, 3, int(pct), 100)
							if item.Color == "cyan" || item.Color == "magenta" || item.Color == "yellow" {
								dev.IsColorCapable = true
							}
							dev.Supplies = append(dev.Supplies, item)
						}
					}
				}
			}
		}
	}

	if dev.Counters.LifetimeEngineTotal == 0 || len(dev.Supplies) == 0 {
		client := newLocalHTTPClient(3 * time.Second)
		if body, code, hErr := fetchURLText(ctx, client, "http://"+dev.IPAddress+"/general/information.html"); hErr == nil && code == 200 {
			tot, sups := ParseBrotherEWSHTML(body)
			if dev.Counters.LifetimeEngineTotal == 0 && tot > 0 {
				dev.Counters.LifetimeEngineTotal = tot
				dev.OdometerSource = "ews"
			}
			if len(dev.Supplies) == 0 && len(sups) > 0 {
				dev.Supplies = sups
			}
		}
	}
}

// --- Epson EcoTank Protected Collector (@BDC ST2 + EWS; NO .44.1.1.2.1 EEPROM over SNMP!) ---

// EpsonBDCStatus holds parsed fields from Epson's @BDC ST2 SNMP status block.
type EpsonBDCStatus struct {
	StatusCode string
	Supplies   []SupplyItem
	IsColor    bool
}

// ParseEpsonBDCST2 decodes Epson @BDC ST2 status bytes returned by OID 1.3.6.1.4.1.1248.1.2.2.1.1.1.1.1.
func ParseEpsonBDCST2(raw []byte) (EpsonBDCStatus, bool) {
	idx := bytes.Index(raw, []byte("@BDC ST2"))
	if idx == -1 {
		return EpsonBDCStatus{}, false
	}
	payload := raw[idx:]
	// Format after "@BDC ST2\r\n" + 2-byte length or ASCII key:value; fields
	res := EpsonBDCStatus{}
	text := string(payload)

	// Extract ST:xx status code
	reST := regexp.MustCompile(`ST:([0-9A-Fa-f]{2})`)
	if m := reST.FindStringSubmatch(text); len(m) == 2 {
		res.StatusCode = m[1]
	}

	// Binary IQ: field -> "IQ:" followed by length byte and ink pairs or ASCII hex
	if iqIdx := bytes.Index(payload, []byte("IQ:")); iqIdx != -1 {
		rem := payload[iqIdx+3:]
		// Check ASCII semicolon-delimited hex values first or binary pairs
		if semi := bytes.IndexByte(rem, ';'); semi > 0 && semi <= 32 {
			chunk := rem[:semi]
			// If all bytes are <= 100 or ASCII hex
			isPrintableHex := true
			for _, b := range chunk {
				if !((b >= '0' && b <= '9') || (b >= 'A' && b <= 'F') || (b >= 'a' && b <= 'f') || b == ',') {
					isPrintableHex = false
					break
				}
			}
			colorOrder := []string{"black", "cyan", "magenta", "yellow"}
			if isPrintableHex {
				parts := strings.Split(string(chunk), ",")
				for i, p := range parts {
					if v, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && v >= 0 && v <= 100 {
						col := colorOrder[i%len(colorOrder)]
						if col != "black" {
							res.IsColor = true
						}
						res.Supplies = append(res.Supplies, buildSupplyItem("Tinta "+strings.Title(col), col, 3, 5, v, 100))
					}
				}
			} else if len(chunk) >= 3 && chunk[0] > 0 {
				// Binary ST2 tag-length-value: [len][colorCode][subCode][percent]...
				for pos := 1; pos+2 < len(chunk) && len(res.Supplies) < 8; pos += 3 {
					pct := int(chunk[pos+2])
					if pct <= 100 {
						col := colorOrder[len(res.Supplies)%len(colorOrder)]
						if col != "black" {
							res.IsColor = true
						}
						res.Supplies = append(res.Supplies, buildSupplyItem("Tinta "+strings.Title(col), col, 3, 5, pct, 100))
					}
				}
			}
		}
	}
	return res, true
}

// ParseEpsonEWSHTML extracts ink tank levels, serial number, and page counts from Epson EWS HTML.
func ParseEpsonEWSHTML(htmlStr string) (serial string, total uint32, supplies []SupplyItem, isColor bool) {
	reSerial := regexp.MustCompile(`(?is)Serial\s*(?:Number|No\.?)[^<]{0,40}<[^>]*>\s*([A-Z0-9]{6,20})`)
	if m := reSerial.FindStringSubmatch(htmlStr); len(m) == 2 {
		serial = sanitizeSerial(m[1])
	}
	rePages := regexp.MustCompile(`(?is)Total\s*(?:Number\s*of\s*)?(?:Pages|Sheets)[^0-9]{0,40}([0-9]{1,9})`)
	if m := rePages.FindStringSubmatch(htmlStr); len(m) == 2 {
		if n, err := strconv.ParseUint(m[1], 10, 32); err == nil {
			total = uint32(n)
		}
	}
	reInk := regexp.MustCompile(`(?is)(Black|Cyan|Magenta|Yellow|Maintenance\s*Box)[^%]{0,80}?([0-9]{1,3})\s*%`)
	for _, m := range reInk.FindAllStringSubmatch(htmlStr, -1) {
		pct, err := strconv.Atoi(m[2])
		if err != nil || pct < 0 || pct > 100 {
			continue
		}
		name := strings.TrimSpace(m[1])
		classCode := 3
		if strings.Contains(strings.ToLower(name), "maintenance") {
			classCode = 4
		}
		col := inferSupplyColor(name)
		if col == "cyan" || col == "magenta" || col == "yellow" {
			isColor = true
		}
		supplies = append(supplies, buildSupplyItem(name, col, classCode, 5, pct, 100))
	}
	return serial, total, supplies, isColor
}

func enrichEpsonProtected(ctx context.Context, dev *DiscoveredDevice, snmpCli *snmpClient) {
	// 1. Network SNMP @BDC ST2 ONLY (NEVER .44.1.1.2.1 EEPROM!)
	if snmpCli != nil {
		if raw, err := snmpCli.getOID(ctx, "1.3.6.1.4.1.1248.1.2.2.1.1.1.1.1", 1200*time.Millisecond); err == nil {
			if st, ok := ParseEpsonBDCST2(raw); ok {
				if len(dev.Supplies) == 0 && len(st.Supplies) > 0 {
					dev.Supplies = st.Supplies
				}
				if st.IsColor {
					dev.IsColorCapable = true
				}
			}
		}
	}

	// 2. EWS /PRESENTATION/ADVANCED/INFO_PRTINFO/TOP -> fallback /PRESENTATION/HTML/TOP/PRTINFO.HTML
	client := newLocalHTTPClient(3500 * time.Millisecond)
	for _, path := range []string{
		"/PRESENTATION/ADVANCED/INFO_PRTINFO/TOP",
		"/PRESENTATION/HTML/TOP/PRTINFO.HTML",
	} {
		for _, scheme := range []string{"https://", "http://"} {
			body, code, err := fetchURLText(ctx, client, scheme+dev.IPAddress+path)
			if err != nil || code != 200 {
				continue
			}
			ser, tot, sups, col := ParseEpsonEWSHTML(body)
			if dev.SerialNumber == "" && ser != "" {
				dev.SerialNumber = ser
			}
			if dev.Counters.LifetimeEngineTotal == 0 && tot > 0 {
				dev.Counters.LifetimeEngineTotal = tot
				dev.OdometerSource = "ews"
			}
			if len(dev.Supplies) == 0 && len(sups) > 0 {
				dev.Supplies = sups
			}
			if col {
				dev.IsColorCapable = true
			}
			return
		}
	}
}

// --- Canon MegaTank / RUI Parser ---

// ParseCanonEWSHTML parses Canon RUI inktank[x] entries and page totals.
func ParseCanonEWSHTML(htmlStr string) (supplies []SupplyItem, isColor bool) {
	reTank := regexp.MustCompile(`(?i)inktank\[(\d+)\]\s*=\s*['"]?([A-Za-z0-9_]+)[:=,\s]+(\d{1,3})`)
	for _, m := range reTank.FindAllStringSubmatch(htmlStr, -1) {
		pct, err := strconv.Atoi(m[3])
		if err != nil || pct < 0 || pct > 100 {
			continue
		}
		name := strings.TrimSpace(m[2])
		col := inferSupplyColor(name)
		if col == "cyan" || col == "magenta" || col == "yellow" {
			isColor = true
		}
		supplies = append(supplies, buildSupplyItem(name, col, 3, 5, pct, 100))
	}
	return supplies, isColor
}

func enrichCanonEWS(ctx context.Context, dev *DiscoveredDevice) {
	if len(dev.Supplies) > 0 {
		return
	}
	client := newLocalHTTPClient(3 * time.Second)
	for _, path := range []string{"/ink_information.html", "/general_status.html"} {
		body, code, err := fetchURLText(ctx, client, "http://"+dev.IPAddress+path)
		if err == nil && code == 200 {
			sups, col := ParseCanonEWSHTML(body)
			if len(sups) > 0 {
				dev.Supplies = sups
				if col {
					dev.IsColorCapable = true
				}
				return
			}
		}
	}
}
