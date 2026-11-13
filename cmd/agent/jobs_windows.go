//go:build windows

package main

import (
	"log/slog"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	wevtapiDLL                    = windows.NewLazySystemDLL("wevtapi.dll")
	procEvtOpenChannelConfig      = wevtapiDLL.NewProc("EvtOpenChannelConfig")
	procEvtSetChannelConfigProp   = wevtapiDLL.NewProc("EvtSetChannelConfigProperty")
	procEvtSaveChannelConfig      = wevtapiDLL.NewProc("EvtSaveChannelConfig")
	procEvtQuery                  = wevtapiDLL.NewProc("EvtQuery")
	procEvtNext                   = wevtapiDLL.NewProc("EvtNext")
	procEvtRender                 = wevtapiDLL.NewProc("EvtRender")
	procEvtClose                  = wevtapiDLL.NewProc("EvtClose")

	netapi32DLL           = windows.NewLazySystemDLL("netapi32.dll")
	procNetWkstaGetInfo   = netapi32DLL.NewProc("NetWkstaGetInfo")
	procNetApiBufferFree  = netapi32DLL.NewProc("NetApiBufferFree")
)

const (
	printServiceChannel = "Microsoft-Windows-PrintService/Operational"

	evtChannelConfigEnabled    = 0
	evtVarTypeBoolean          = 13
	evtQueryChannelPath        = 0x1
	evtQueryReverseDirection   = 0x200
	evtRenderEventXml          = 1
)

type evtVariantBoolean struct {
	Val   int32
	Pad   uint32
	Count uint32
	Type  uint32
}

type wkstaInfo100 struct {
	PlatformID  uint32
	ComputerName *uint16
	LanGroup    *uint16
	VerMajor    uint32
	VerMinor    uint32
}

// EnablePrintServiceOperationalLog enables Microsoft-Windows-PrintService/Operational via wevtapi.dll.
func EnablePrintServiceOperationalLog() error {
	chPtr, err := windows.UTF16PtrFromString(printServiceChannel)
	if err != nil {
		return err
	}
	hCfg, _, callErr := procEvtOpenChannelConfig.Call(0, uintptr(unsafe.Pointer(chPtr)), 0)
	if hCfg == 0 {
		return callErr
	}
	defer procEvtClose.Call(hCfg)

	v := evtVariantBoolean{
		Val:  1,
		Type: evtVarTypeBoolean,
	}
	r1, _, sErr := procEvtSetChannelConfigProp.Call(hCfg, evtChannelConfigEnabled, 0, uintptr(unsafe.Pointer(&v)))
	if r1 == 0 {
		return sErr
	}
	r2, _, saveErr := procEvtSaveChannelConfig.Call(hCfg, 0)
	if r2 == 0 {
		return saveErr
	}
	return nil
}

// QueryPrintServiceEvents queries recent Event ID 307 & 805 records after minRecordID using wevtapi.dll.
func QueryPrintServiceEvents(minRecordID int64, maxEvents int) ([]PrintServiceEvent, int64) {
	if maxEvents <= 0 {
		maxEvents = 250
	}
	chPtr, err := windows.UTF16PtrFromString(printServiceChannel)
	if err != nil {
		return nil, minRecordID
	}

	xpath := "*[System[(EventID=307 or EventID=805) and Provider[@Name='Microsoft-Windows-PrintService']]]"
	if minRecordID > 0 {
		xpath = "*[System[(EventID=307 or EventID=805) and Provider[@Name='Microsoft-Windows-PrintService'] and EventRecordID>" +
			int64ToStr(minRecordID) + "]]"
	}
	qPtr, err := windows.UTF16PtrFromString(xpath)
	if err != nil {
		return nil, minRecordID
	}

	hQuery, _, _ := procEvtQuery.Call(
		0,
		uintptr(unsafe.Pointer(chPtr)),
		uintptr(unsafe.Pointer(qPtr)),
		evtQueryChannelPath,
	)
	if hQuery == 0 {
		return nil, minRecordID
	}
	defer procEvtClose.Call(hQuery)

	var out []PrintServiceEvent
	maxSeen := minRecordID
	handles := make([]uintptr, 32)

	for len(out) < maxEvents {
		var returned uint32
		r1, _, _ := procEvtNext.Call(
			hQuery,
			uintptr(len(handles)),
			uintptr(unsafe.Pointer(&handles[0])),
			2000, // 2s timeout
			0,
			uintptr(unsafe.Pointer(&returned)),
		)
		if r1 == 0 || returned == 0 {
			break
		}
		for i := uint32(0); i < returned; i++ {
			hEv := handles[i]
			xmlStr := renderEvtXML(hEv)
			_, _, _ = procEvtClose.Call(hEv)
			if xmlStr == "" {
				continue
			}
			parsed, pErr := ParsePrintServiceEventXML(xmlStr)
			if pErr != nil {
				continue
			}
			if parsed.RecordID > maxSeen {
				maxSeen = parsed.RecordID
			}
			out = append(out, *parsed)
		}
	}
	return out, maxSeen
}

func renderEvtXML(hEvent uintptr) string {
	var bufUsed, propCount uint32
	_, _, _ = procEvtRender.Call(
		0,
		hEvent,
		evtRenderEventXml,
		0,
		0,
		uintptr(unsafe.Pointer(&bufUsed)),
		uintptr(unsafe.Pointer(&propCount)),
	)
	if bufUsed == 0 || bufUsed > 256*1024 {
		return ""
	}
	buf := make([]uint16, (bufUsed+1)/2)
	r1, _, _ := procEvtRender.Call(
		0,
		hEvent,
		evtRenderEventXml,
		uintptr(bufUsed),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&bufUsed)),
		uintptr(unsafe.Pointer(&propCount)),
	)
	if r1 == 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

func int64ToStr(v int64) string {
	if v == 0 {
		return "0"
	}
	var neg bool
	if v < 0 {
		neg = true
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ResolveHostDomain resolves the Windows workstation domain/workgroup via NetWkstaGetInfo or environment.
func ResolveHostDomain() string {
	var info *wkstaInfo100
	r1, _, _ := procNetWkstaGetInfo.Call(0, 100, uintptr(unsafe.Pointer(&info)))
	if r1 == 0 && info != nil {
		defer procNetApiBufferFree.Call(uintptr(unsafe.Pointer(info)))
		if info.LanGroup != nil {
			dom := strings.TrimSpace(windows.UTF16PtrToString(info.LanGroup))
			if dom != "" {
				return dom
			}
		}
	}
	if d := strings.TrimSpace(os.Getenv("USERDOMAIN")); d != "" {
		return d
	}
	return "WORKGROUP"
}

// ReadWPPReadinessSensor reads Windows 11 24H2 / Server 2025 Protected Print Mode registry keys.
func ReadWPPReadinessSensor(printers []winSpoolPrinter) *WPPReadiness {
	sensor := &WPPReadiness{Supported: true}
	const wppKeyPath = `SOFTWARE\Policies\Microsoft\Windows NT\Printers\WPP`
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, wppKeyPath, registry.READ)
	if err == nil {
		defer k.Close()
		if val, _, err := k.GetIntegerValue("WindowsProtectedPrintMode"); err == nil {
			sensor.WPPEnabled = val == 1
		}
		if val, _, err := k.GetIntegerValue("WindowsProtectedPrintGroupPolicyState"); err == nil {
			sensor.WPPPolicyState = uint32(val)
			if val == 1 {
				sensor.WPPEnabled = true
			}
		}
	}

	// Identify non-Mopria thermal/legacy printers at risk under WPP
	for _, p := range printers {
		if isVirtualPrinter(p.Name, p.PortName, p.DriverName) {
			continue
		}
		vendor, _ := detectVendorFromModel(p.Name + " " + p.DriverName)
		_, _, isThermal := ClassifyPrinterCapabilities(vendor, p.Name+" "+p.DriverName, nil)
		if isThermal {
			sensor.ImpactedPrinters = append(sensor.ImpactedPrinters, p.Name)
		}
	}
	return sensor
}

func collectPlatformPrintJobs(store *SQLiteStore, devices []DiscoveredDevice, spooler LocalSpoolerResult, hostname string) ([]PrintJobRecord, []JobSummary, *WPPReadiness) {
	wpp := ReadWPPReadinessSensor(spooler.AllPrinters)

	var lastRecID int64
	if store != nil {
		lastRecID, _, _ = store.GetCheckpoint("windows_printservice")
	}

	events, maxRecID := QueryPrintServiceEvents(lastRecID, 300)
	if len(events) == 0 {
		return nil, nil, wpp
	}

	lookup := BuildDeviceColorLookup(devices, spooler.PrinterIPMap)
	domain := ResolveHostDomain()
	jobs := CorrelatePrintJobs(events, lookup, domain, hostname)

	if store != nil && maxRecID > lastRecID {
		if err := store.SaveCheckpoint("windows_printservice", maxRecID, ""); err != nil {
			slog.Warn("failed to save printservice checkpoint", "error", err)
		}
	}
	return jobs, AggregateJobSummaries(jobs), wpp
}
