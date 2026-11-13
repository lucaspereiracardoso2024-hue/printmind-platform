package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// PrintServiceEvent represents a parsed Windows PrintService Event 307 or 805.
type PrintServiceEvent struct {
	EventID       int
	RecordID      int64
	Provider      string
	Channel       string
	TimeCreated   string
	Computer      string
	JobID         int
	DocumentName  string
	UserName      string
	MachineName   string
	PrinterName   string
	PortName      string
	SizeBytes     int64
	PagesPrinted  int
	Copies        int
	Color         int  // 2 = Color, 1 = Monochrome
	Duplex        bool
}

// ParsePrintServiceEventXML parses an Event 307 or 805 XML string rendered by wevtapi.dll.
// Rejects events not emitted by Microsoft-Windows-PrintService on the Operational channel.
func ParsePrintServiceEventXML(xmlStr string) (*PrintServiceEvent, error) {
	dec := xml.NewDecoder(bytes.NewBufferString(xmlStr))
	ev := &PrintServiceEvent{Copies: 1, Color: 1}

	var curElem string
	var inSystem, inUserData bool
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			local := t.Name.Local
			curElem = local
			switch local {
			case "System":
				inSystem = true
			case "UserData":
				inUserData = true
			case "Provider":
				if inSystem {
					for _, a := range t.Attr {
						if strings.EqualFold(a.Name.Local, "Name") {
							ev.Provider = a.Value
						}
					}
				}
			case "TimeCreated":
				if inSystem {
					for _, a := range t.Attr {
						if strings.EqualFold(a.Name.Local, "SystemTime") {
							ev.TimeCreated = a.Value
						}
					}
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "System":
				inSystem = false
			case "UserData":
				inUserData = false
			}
			curElem = ""
		case xml.CharData:
			val := strings.TrimSpace(string(t))
			if val == "" || curElem == "" {
				continue
			}
			if inSystem {
				switch curElem {
				case "EventID":
					ev.EventID, _ = strconv.Atoi(val)
				case "EventRecordID":
					ev.RecordID, _ = strconv.ParseInt(val, 10, 64)
				case "Channel":
					ev.Channel = val
				case "Computer":
					ev.Computer = val
				}
			} else if inUserData {
				switch curElem {
				case "Param1", "JobId":
					ev.JobID, _ = strconv.Atoi(val)
				case "Param2":
					ev.DocumentName = val
				case "Param3":
					ev.UserName = val
				case "Param4":
					ev.MachineName = strings.TrimPrefix(val, `\\`)
				case "Param5":
					ev.PrinterName = val
				case "Param6":
					ev.PortName = val
				case "Param7":
					ev.SizeBytes, _ = strconv.ParseInt(val, 10, 64)
				case "Param8":
					ev.PagesPrinted, _ = strconv.Atoi(val)
				case "Copies":
					if c, cErr := strconv.Atoi(val); cErr == nil && c > 0 {
						ev.Copies = c
					}
				case "Color":
					if c, cErr := strconv.Atoi(val); cErr == nil {
						ev.Color = c
					}
				case "Duplex":
					ev.Duplex = val == "1" || strings.EqualFold(val, "true") || val == "2"
				}
			}
		}
	}

	if ev.Provider != "Microsoft-Windows-PrintService" {
		return nil, fmt.Errorf("unexpected provider %q", ev.Provider)
	}
	if ev.Channel != "" && !strings.EqualFold(ev.Channel, "Microsoft-Windows-PrintService/Operational") {
		return nil, fmt.Errorf("unexpected channel %q", ev.Channel)
	}
	if ev.EventID != 307 && ev.EventID != 805 {
		return nil, fmt.Errorf("unsupported event id %d", ev.EventID)
	}
	return ev, nil
}

// DeviceColorLookup resolves whether a printer name or IP is physically color-capable.
type DeviceColorLookup struct {
	ByPrinterName map[string]bool
	ByIP          map[string]bool
	PrinterIPMap  map[string]string
}

// BuildDeviceColorLookup indexes discovered devices and spooler queues for Physical Color Gate checks.
func BuildDeviceColorLookup(devices []DiscoveredDevice, printerIPMap map[string]string) DeviceColorLookup {
	lk := DeviceColorLookup{
		ByPrinterName: make(map[string]bool),
		ByIP:          make(map[string]bool),
		PrinterIPMap:  make(map[string]string),
	}
	for k, v := range printerIPMap {
		lk.PrinterIPMap[strings.ToLower(strings.TrimSpace(k))] = v
	}
	for _, d := range devices {
		if d.IPAddress != "" && d.IPAddress != "0.0.0.0" {
			if d.IsColorCapable {
				lk.ByIP[d.IPAddress] = true
			} else if _, exists := lk.ByIP[d.IPAddress]; !exists {
				lk.ByIP[d.IPAddress] = false
			}
		}
		if d.Model != "" {
			lk.ByPrinterName[strings.ToLower(strings.TrimSpace(d.Model))] = d.IsColorCapable
		}
	}
	return lk
}

func (lk DeviceColorLookup) resolve(printerName, portName string) (printerIP string, isColorCapable bool) {
	pLower := strings.ToLower(strings.TrimSpace(printerName))
	if ip, ok := lk.PrinterIPMap[pLower]; ok {
		printerIP = ip
		if col, okCol := lk.ByIP[ip]; okCol {
			return printerIP, col
		}
	}
	if ip := firstValidIPv4(portName); ip != "" {
		printerIP = ip
		if col, okCol := lk.ByIP[ip]; okCol {
			return printerIP, col
		}
	}
	if col, ok := lk.ByPrinterName[pLower]; ok {
		return printerIP, col
	}
	for nameKey, col := range lk.ByPrinterName {
		if nameKey != "" && (strings.Contains(pLower, nameKey) || strings.Contains(nameKey, pLower)) {
			if col {
				return printerIP, true
			}
		}
	}
	return printerIP, false
}

// CorrelatePrintJobs joins Event 307 and Event 805 by JobID and enforces the Physical Color Gate:
// color_gated / color_pages ONLY count color if (Event805.Color == 2) AND device.IsColorCapable == true.
func CorrelatePrintJobs(events []PrintServiceEvent, lookup DeviceColorLookup, defaultDomain string, fallbackHost string) []PrintJobRecord {
	ev805ByJob := make(map[int]PrintServiceEvent)
	for _, ev := range events {
		if ev.EventID == 805 && ev.JobID > 0 {
			ev805ByJob[ev.JobID] = ev
		}
	}

	var jobs []PrintJobRecord
	for _, ev := range events {
		if ev.EventID != 307 {
			continue
		}
		copies := 1
		reqColor := false
		duplex := false
		if ev805, ok := ev805ByJob[ev.JobID]; ok {
			if ev805.Copies > 0 {
				copies = ev805.Copies
			}
			reqColor = ev805.Color == 2
			duplex = ev805.Duplex
		}

		pages := ev.PagesPrinted
		effectivePages := pages * copies
		printerIP, isColorCapable := lookup.resolve(ev.PrinterName, ev.PortName)

		// Physical Color Gate
		colorGated := reqColor && isColorCapable
		colorPages := 0
		monoPages := effectivePages
		if colorGated {
			colorPages = effectivePages
			monoPages = 0
		}

		machine := ev.MachineName
		if machine == "" {
			machine = ev.Computer
		}
		if machine == "" {
			machine = fallbackHost
		}

		user := ev.UserName
		domain := defaultDomain
		if idx := strings.IndexByte(user, '\\'); idx > 0 {
			domain = user[:idx]
			user = user[idx+1:]
		}

		jobs = append(jobs, PrintJobRecord{
			JobID:          ev.JobID,
			EventRecordID:  ev.RecordID,
			Timestamp:      ev.TimeCreated,
			PrinterName:    ev.PrinterName,
			PortName:       ev.PortName,
			PrinterIP:      printerIP,
			UserName:       user,
			MachineName:    machine,
			Domain:         domain,
			Department:     domain,
			DocumentName:   ev.DocumentName,
			PagesPrinted:   pages,
			Copies:         copies,
			EffectivePages: effectivePages,
			MonoPages:      monoPages,
			ColorPages:     colorPages,
			RequestedColor: reqColor,
			ColorGated:     colorGated,
			Duplex:         duplex,
			SizeBytes:      ev.SizeBytes,
			Source:         "windows_printservice",
		})
	}
	return jobs
}

// EnrichPrintJobsDepartment resolves AD/LDAP departments for correlated print jobs with safe domain fallback.
func EnrichPrintJobsDepartment(ctx context.Context, jobs []PrintJobRecord, resolver *LDAPDepartmentResolver) {
	if resolver == nil || len(jobs) == 0 {
		return
	}
	for i := range jobs {
		dept := resolver.ResolveDepartment(ctx, jobs[i].Domain, jobs[i].UserName, jobs[i].MachineName)
		if dept != "" {
			jobs[i].Department = dept
		} else if jobs[i].Department == "" {
			jobs[i].Department = jobs[i].Domain
		}
	}
}

// AggregateJobSummaries rolls up print job records by printer, user, machine, domain, and department.
func AggregateJobSummaries(jobs []PrintJobRecord) []JobSummary {
	type key struct {
		printer    string
		ip         string
		user       string
		machine    string
		domain     string
		department string
	}
	agg := make(map[key]*JobSummary)
	var order []key

	for _, j := range jobs {
		k := key{
			printer:    j.PrinterName,
			ip:         j.PrinterIP,
			user:       j.UserName,
			machine:    j.MachineName,
			domain:     j.Domain,
			department: j.Department,
		}
		s, ok := agg[k]
		if !ok {
			s = &JobSummary{
				PrinterName: j.PrinterName,
				PrinterIP:   j.PrinterIP,
				UserName:    j.UserName,
				MachineName: j.MachineName,
				Domain:      j.Domain,
				Department:  j.Department,
			}
			agg[k] = s
			order = append(order, k)
		}
		s.JobCount++
		s.TotalPages += j.EffectivePages
		s.MonoPages += j.MonoPages
		s.ColorPages += j.ColorPages
		if j.Duplex {
			s.DuplexPages += j.EffectivePages
		} else {
			s.SimplexPages += j.EffectivePages
		}
		if j.RequestedColor && !j.ColorGated {
			s.ColorGatedOut += j.EffectivePages
		}
	}

	out := make([]JobSummary, 0, len(order))
	for _, k := range order {
		out = append(out, *agg[k])
	}
	return out
}

// ParseCUPSPageLog parses /var/log/cups/page_log lines starting after checkpointLine.
// Standard CUPS page_log line format:
// printer user job-id [date-time] page-number num-copies billing host [job-name media sides]
func ParseCUPSPageLog(content string, checkpointLine int64, lookup DeviceColorLookup, hostname string) ([]PrintJobRecord, int64) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	type jobAccum struct {
		jobID     int
		printer   string
		user      string
		timestamp string
		host      string
		jobName   string
		duplex    bool
		pages     int
		maxCopies int
	}
	accum := make(map[string]*jobAccum)
	var order []string
	var maxLine int64 = checkpointLine

	for idx, rawLine := range lines {
		lineNum := int64(idx + 1)
		if lineNum <= checkpointLine {
			continue
		}
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			maxLine = lineNum
			continue
		}
		// Extract bracketed timestamp if present
		ts := ""
		restBefore := line
		restAfter := ""
		if lb := strings.IndexByte(line, '['); lb != -1 {
			if rb := strings.IndexByte(line[lb:], ']'); rb != -1 {
				ts = line[lb+1 : lb+rb]
				restBefore = strings.TrimSpace(line[:lb])
				restAfter = strings.TrimSpace(line[lb+rb+1:])
			}
		}
		leftFields := strings.Fields(restBefore)
		rightFields := strings.Fields(restAfter)
		if len(leftFields) < 3 || len(rightFields) < 2 {
			maxLine = lineNum
			continue
		}

		printer := leftFields[0]
		user := leftFields[1]
		jobID, _ := strconv.Atoi(leftFields[2])

		var copies int
		var host, jobName string
		duplex := false

		// Check if rightFields[0] is "total" (CUPS PageLogFormat total line) or page-number
		if strings.EqualFold(rightFields[0], "total") {
			pages, _ := strconv.Atoi(rightFields[1])
			if len(rightFields) >= 4 {
				host = rightFields[3]
			}
			if len(rightFields) >= 5 {
				jobName = rightFields[4]
			}
			keyStr := fmt.Sprintf("%s#%d#%d", printer, jobID, lineNum)
			accum[keyStr] = &jobAccum{
				jobID:     jobID,
				printer:   printer,
				user:      user,
				timestamp: ts,
				host:      host,
				jobName:   jobName,
				pages:     pages,
				maxCopies: 1,
			}
			order = append(order, keyStr)
		} else {
			copies, _ = strconv.Atoi(rightFields[1])
			if copies <= 0 {
				copies = 1
			}
			if len(rightFields) >= 4 && rightFields[3] != "-" {
				host = rightFields[3]
			}
			if len(rightFields) >= 5 && rightFields[4] != "-" {
				jobName = rightFields[4]
			}
			for _, rf := range rightFields {
				if strings.Contains(strings.ToLower(rf), "two-sided") || strings.Contains(strings.ToLower(rf), "duplex") {
					duplex = true
				}
			}
			keyStr := fmt.Sprintf("%s#%d", printer, jobID)
			a, ok := accum[keyStr]
			if !ok {
				a = &jobAccum{
					jobID:     jobID,
					printer:   printer,
					user:      user,
					timestamp: ts,
					host:      host,
					jobName:   jobName,
					duplex:    duplex,
				}
				accum[keyStr] = a
				order = append(order, keyStr)
			}
			a.pages++
			if copies > a.maxCopies {
				a.maxCopies = copies
			}
			if duplex {
				a.duplex = true
			}
		}
		maxLine = lineNum
	}

	var out []PrintJobRecord
	for _, k := range order {
		a := accum[k]
		copies := a.maxCopies
		if copies <= 0 {
			copies = 1
		}
		effective := a.pages * copies
		ip, _ := lookup.resolve(a.printer, "")
		machine := a.host
		if machine == "" || machine == "localhost" {
			machine = hostname
		}
		out = append(out, PrintJobRecord{
			JobID:          a.jobID,
			Timestamp:      a.timestamp,
			PrinterName:    a.printer,
			PrinterIP:      ip,
			UserName:       a.user,
			MachineName:    machine,
			DocumentName:   a.jobName,
			PagesPrinted:   a.pages,
			Copies:         copies,
			EffectivePages: effective,
			MonoPages:      effective,
			ColorPages:     0,
			Duplex:         a.duplex,
			Source:         "cups_page_log",
		})
	}
	return out, maxLine
}
