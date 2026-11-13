//go:build windows

package main

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	printerEnumLocal       = 0x00000002
	printerEnumConnections = 0x00000004
	digcfPresent           = 0x00000002
	digcfDeviceInterface   = 0x00000010
	ioctlUSBPrintGet1284ID = 0x00220034 // CTL_CODE(FILE_DEVICE_UNKNOWN=0x22, 13, METHOD_BUFFERED=0, FILE_ANY_ACCESS=0)
	maxUSB1284BufferSize   = 4094
)

var (
	winspoolDLL = windows.NewLazySystemDLL("winspool.drv")
	procEnumPrintersW = winspoolDLL.NewProc("EnumPrintersW")
	procOpenPrinterW  = winspoolDLL.NewProc("OpenPrinterW")
	procClosePrinter  = winspoolDLL.NewProc("ClosePrinter")
	procEnumJobsW     = winspoolDLL.NewProc("EnumJobsW")

	setupapiDLL                      = windows.NewLazySystemDLL("setupapi.dll")
	procSetupDiGetClassDevsW         = setupapiDLL.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDeviceInterfaces  = setupapiDLL.NewProc("SetupDiEnumDeviceInterfaces")
	procSetupDiGetDeviceInterfaceDet = setupapiDLL.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procSetupDiDestroyDeviceInfoList = setupapiDLL.NewProc("SetupDiDestroyDeviceInfoList")
)

var guidDevInterfaceUSBPrint = windows.GUID{
	Data1: 0x28D78FAD,
	Data2: 0x5A12,
	Data3: 0x11d1,
	Data4: [8]byte{0xAE, 0x5B, 0x00, 0x00, 0xF8, 0x03, 0xA8, 0xC2},
}

type printerInfo2W struct {
	pServerName         *uint16
	pPrinterName        *uint16
	pShareName          *uint16
	pPortName           *uint16
	pDriverName         *uint16
	pComment            *uint16
	pLocation           *uint16
	pDevMode            *devModeWHeader
	pSepFile            *uint16
	pPrintProcessor     *uint16
	pDatatype           *uint16
	pParameters         *uint16
	pSecurityDescriptor uintptr
	Attributes          uint32
	Priority            uint32
	DefaultPriority     uint32
	StartTime           uint32
	UntilTime           uint32
	Status              uint32
	cJobs               uint32
	AveragePPM          uint32
}

type jobInfo2W struct {
	JobId              uint32
	pPrinterName       *uint16
	pMachineName       *uint16
	pUserName          *uint16
	pDocument          *uint16
	pNotifyName        *uint16
	pDatatype          *uint16
	pPrintProcessor    *uint16
	pParameters        *uint16
	pDriverName        *uint16
	pDevMode           *devModeWHeader
	pStatus            *uint16
	pSecurityDescriptor uintptr
	Status             uint32
	Priority           uint32
	Position           uint32
	StartTime          uint32
	UntilTime          uint32
	TotalPages         uint32
	Size               uint32
	Submitted          windows.Systemtime
	Time               uint32
	PagesPrinted       uint32
}

type devModeWHeader struct {
	dmDeviceName       [32]uint16
	dmSpecVersion      uint16
	dmDriverVersion    uint16
	dmSize             uint16
	dmDriverExtra      uint16
	dmFields           uint32
	dmOrientation      int16
	dmPaperSize        int16
	dmPaperLength      int16
	dmPaperWidth       int16
	dmScale            int16
	dmCopies           int16
	dmDefaultSource    int16
	dmPrintQuality     int16
	dmColor            int16
	dmDuplex           int16
}

type spDeviceInterfaceData struct {
	cbSize             uint32
	InterfaceClassGuid windows.GUID
	Flags              uint32
	Reserved           uintptr
}

type winSpoolPrinter struct {
	Name       string
	PortName   string
	DriverName string
	Status     uint32
	WorkOffline bool
	IsColor    bool
}

type spoolerJobMeta struct {
	JobID        int
	PrinterName  string
	UserName     string
	MachineName  string
	DocumentName string
	Copies       int
	Color        int
	Duplex       bool
	TotalPages   int
	PagesPrinted int
	SizeBytes    int64
}

// LocalSpoolerResult holds enumerated USB printers and WSD-resolved network IPs.
type LocalSpoolerResult struct {
	USBDevices     []DiscoveredDevice
	WSDResolvedIPs map[string]string // PortName -> IPv4
	PrinterIPMap   map[string]string // PrinterName -> IPv4
	AllPrinters    []winSpoolPrinter
}

func ptrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	return windows.UTF16PtrToString(p)
}

func enumWindowsPrinters() []winSpoolPrinter {
	flags := uint32(printerEnumLocal | printerEnumConnections)
	var needed, returned uint32
	_, _, _ = procEnumPrintersW.Call(
		uintptr(flags),
		0,
		2, // Level 2
		0,
		0,
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&returned)),
	)
	if needed == 0 {
		return nil
	}
	buf := make([]byte, needed)
	r1, _, _ := procEnumPrintersW.Call(
		uintptr(flags),
		0,
		2,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(needed),
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&returned)),
	)
	if r1 == 0 || returned == 0 {
		return nil
	}

	elemSize := unsafe.Sizeof(printerInfo2W{})
	out := make([]winSpoolPrinter, 0, returned)
	for i := uint32(0); i < returned; i++ {
		off := uintptr(i) * elemSize
		if off+elemSize > uintptr(len(buf)) {
			break
		}
		pi := (*printerInfo2W)(unsafe.Pointer(&buf[off]))
		isColor := false
		if pi.pDevMode != nil {
			if pi.pDevMode.dmColor == 2 {
				isColor = true
			}
		}
		const printerAttributeWorkOffline = 0x00000400
		const printerStatusOffline = 0x00000080
		workOffline := (pi.Attributes&printerAttributeWorkOffline) != 0 || (pi.Status&printerStatusOffline) != 0
		out = append(out, winSpoolPrinter{
			Name:        strings.TrimSpace(ptrToString(pi.pPrinterName)),
			PortName:    strings.TrimSpace(ptrToString(pi.pPortName)),
			DriverName:  strings.TrimSpace(ptrToString(pi.pDriverName)),
			Status:      pi.Status,
			WorkOffline: workOffline,
			IsColor:     isColor,
		})
	}
	return out
}

// enumSpoolerActiveJobs collects active JOB_INFO_2 metadata for a printer via winspool.drv.
func enumSpoolerActiveJobs(printerName string) []spoolerJobMeta {
	pNamePtr, err := windows.UTF16PtrFromString(printerName)
	if err != nil {
		return nil
	}
	var hPrinter windows.Handle
	r1, _, _ := procOpenPrinterW.Call(
		uintptr(unsafe.Pointer(pNamePtr)),
		uintptr(unsafe.Pointer(&hPrinter)),
		0,
	)
	if r1 == 0 || hPrinter == 0 {
		return nil
	}
	defer procClosePrinter.Call(uintptr(hPrinter))

	var needed, returned uint32
	_, _, _ = procEnumJobsW.Call(
		uintptr(hPrinter),
		0,
		64,
		2, // Level 2 JOB_INFO_2
		0,
		0,
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&returned)),
	)
	if needed == 0 {
		return nil
	}
	buf := make([]byte, needed)
	r2, _, _ := procEnumJobsW.Call(
		uintptr(hPrinter),
		0,
		64,
		2,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(needed),
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&returned)),
	)
	if r2 == 0 || returned == 0 {
		return nil
	}

	elemSize := unsafe.Sizeof(jobInfo2W{})
	jobs := make([]spoolerJobMeta, 0, returned)
	for i := uint32(0); i < returned; i++ {
		off := uintptr(i) * elemSize
		if off+elemSize > uintptr(len(buf)) {
			break
		}
		ji := (*jobInfo2W)(unsafe.Pointer(&buf[off]))
		copies := 1
		color := 1
		duplex := false
		if ji.pDevMode != nil {
			dm := ji.pDevMode
			if dm.dmCopies > 0 {
				copies = int(dm.dmCopies)
			}
			if dm.dmColor == 2 {
				color = 2
			}
			if dm.dmDuplex > 1 {
				duplex = true
			}
		}
		jobs = append(jobs, spoolerJobMeta{
			JobID:        int(ji.JobId),
			PrinterName:  printerName,
			UserName:     ptrToString(ji.pUserName),
			MachineName:  strings.TrimPrefix(ptrToString(ji.pMachineName), `\\`),
			DocumentName: ptrToString(ji.pDocument),
			Copies:       copies,
			Color:        color,
			Duplex:       duplex,
			TotalPages:   int(ji.TotalPages),
			PagesPrinted: int(ji.PagesPrinted),
			SizeBytes:    int64(ji.Size),
		})
	}
	return jobs
}

func readWSDRegistryEntries() []WSDRegistryEntry {
	const basePath = `SYSTEM\CurrentControlSet\Enum\SWD\DAFWSDProvider`
	root, err := registry.OpenKey(registry.LOCAL_MACHINE, basePath, registry.READ)
	if err != nil {
		return nil
	}
	defer root.Close()

	names, err := root.ReadSubKeyNames(-1)
	if err != nil {
		return nil
	}
	var entries []WSDRegistryEntry
	for _, subName := range names {
		sub, sErr := registry.OpenKey(root, subName, registry.READ)
		if sErr != nil {
			continue
		}
		entry := WSDRegistryEntry{KeyName: subName}
		if v, _, err := sub.GetStringValue("FriendlyName"); err == nil {
			entry.FriendlyName = v
		}
		if v, _, err := sub.GetStringValue("LocationInformation"); err == nil {
			entry.Location = v
		}
		if v, _, err := sub.GetStringValue("ipaddress"); err == nil {
			entry.IPAddress = v
		}
		if entry.IPAddress == "" {
			if dp, dpErr := registry.OpenKey(sub, "Device Parameters", registry.READ); dpErr == nil {
				if v, _, err := dp.GetStringValue("ipaddress"); err == nil {
					entry.IPAddress = v
				}
				if entry.IPAddress == "" {
					if v, _, err := dp.GetStringValue("IPAddress"); err == nil {
						entry.IPAddress = v
					}
				}
				_ = dp.Close()
			}
		}
		_ = sub.Close()
		entries = append(entries, entry)
	}
	return entries
}

func readUSBPrintIEEE1284Devices() []IEEE1284Info {
	hDevInfo, _, _ := procSetupDiGetClassDevsW.Call(
		uintptr(unsafe.Pointer(&guidDevInterfaceUSBPrint)),
		0,
		0,
		uintptr(digcfPresent|digcfDeviceInterface),
	)
	if hDevInfo == uintptr(windows.InvalidHandle) || hDevInfo == 0 {
		return nil
	}
	defer procSetupDiDestroyDeviceInfoList.Call(hDevInfo)

	var results []IEEE1284Info
	for idx := uint32(0); idx < 64; idx++ {
		var ifData spDeviceInterfaceData
		ifData.cbSize = uint32(unsafe.Sizeof(ifData))
		ok, _, _ := procSetupDiEnumDeviceInterfaces.Call(
			hDevInfo,
			0,
			uintptr(unsafe.Pointer(&guidDevInterfaceUSBPrint)),
			uintptr(idx),
			uintptr(unsafe.Pointer(&ifData)),
		)
		if ok == 0 {
			break
		}

		var reqSize uint32
		_, _, _ = procSetupDiGetDeviceInterfaceDet.Call(
			hDevInfo,
			uintptr(unsafe.Pointer(&ifData)),
			0,
			0,
			uintptr(unsafe.Pointer(&reqSize)),
			0,
		)
		if reqSize < 8 || reqSize > 4096 {
			continue
		}

		detBuf := make([]byte, reqSize)
		// SP_DEVICE_INTERFACE_DETAIL_DATA_W.cbSize is 8 on amd64/arm64, 6 on 386
		cbSize := uint32(8)
		if unsafe.Sizeof(uintptr(0)) == 4 {
			cbSize = 6
		}
		*(*uint32)(unsafe.Pointer(&detBuf[0])) = cbSize

		okDet, _, _ := procSetupDiGetDeviceInterfaceDet.Call(
			hDevInfo,
			uintptr(unsafe.Pointer(&ifData)),
			uintptr(unsafe.Pointer(&detBuf[0])),
			uintptr(reqSize),
			0,
			0,
		)
		if okDet == 0 {
			continue
		}

		pathPtr := (*uint16)(unsafe.Pointer(&detBuf[4]))
		devPath := windows.UTF16PtrToString(pathPtr)
		if devPath == "" {
			continue
		}

		handle, err := windows.CreateFile(
			pathPtr,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil,
			windows.OPEN_EXISTING,
			0,
			0,
		)
		if err != nil {
			handle, err = windows.CreateFile(
				pathPtr,
				windows.GENERIC_READ,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
				nil,
				windows.OPEN_EXISTING,
				0,
				0,
			)
		}
		if err != nil {
			continue
		}

		outBuf := make([]byte, maxUSB1284BufferSize)
		var bytesReturned uint32
		ioErr := windows.DeviceIoControl(
			handle,
			ioctlUSBPrintGet1284ID,
			nil,
			0,
			&outBuf[0],
			uint32(len(outBuf)),
			&bytesReturned,
			nil,
		)
		_ = windows.CloseHandle(handle)
		if ioErr != nil || bytesReturned == 0 {
			continue
		}

		info := ParseIEEE1284DeviceID(outBuf[:bytesReturned])
		if info.SerialNumber == "" {
			// Fallback: extract USB serial segment from device path if present
			parts := strings.Split(devPath, "#")
			if len(parts) >= 3 {
				cand := sanitizeSerial(parts[2])
				if cand != "" && !strings.Contains(cand, "&") {
					info.SerialNumber = cand
				}
			}
		}
		if info.Model != "" || info.Manufacturer != "" || info.Raw != "" {
			results = append(results, info)
		}
	}
	return results
}

func discoverLocalSpooler(enableUSB bool) LocalSpoolerResult {
	res := LocalSpoolerResult{
		WSDResolvedIPs: make(map[string]string),
		PrinterIPMap:   make(map[string]string),
	}
	printers := enumWindowsPrinters()
	res.AllPrinters = printers
	if len(printers) == 0 {
		return res
	}

	wsdEntries := readWSDRegistryEntries()
	var usbIDs []IEEE1284Info
	if enableUSB {
		usbIDs = readUSBPrintIEEE1284Devices()
	}

	for _, p := range printers {
		if isVirtualPrinter(p.Name, p.PortName, p.DriverName) {
			continue
		}
		portUpper := strings.ToUpper(p.PortName)

		// WSD ports MUST be resolved via Registry DAFWSDProvider and NOT misclassified as USB!
		if strings.HasPrefix(portUpper, "WSD") {
			if ip := MatchWSDPortIP(p.PortName, p.Name, wsdEntries); ip != "" {
				res.WSDResolvedIPs[p.PortName] = ip
				res.PrinterIPMap[p.Name] = ip
			}
			continue
		}

		// Direct TCP/IP port (e.g. IP_192.168.1.50 or 192.168.1.50)
		if ip := firstValidIPv4(p.PortName); ip != "" {
			res.PrinterIPMap[p.Name] = ip
			continue
		}

		if !enableUSB {
			continue
		}

		isUSB := strings.HasPrefix(portUpper, "USB") || strings.HasPrefix(portUpper, "DOT4")
		if !isUSB {
			continue
		}

		model := strings.TrimSpace(p.Name)
		vendor, pen := detectVendorFromModel(p.Name + " " + p.DriverName)
		serial := ""
		var cmdSet []string

		// Match against IEEE 1284 USB devices
		for _, u := range usbIDs {
			uCombined := strings.ToLower(u.Manufacturer + " " + u.Model)
			pCombined := strings.ToLower(p.Name + " " + p.DriverName)
			if (u.Model != "" && strings.Contains(pCombined, strings.ToLower(u.Model))) ||
				(vendor != "Genérico" && strings.Contains(uCombined, strings.ToLower(vendor))) {
				if u.Model != "" {
					model = strings.TrimSpace(u.Manufacturer + " " + u.Model)
				}
				if u.SerialNumber != "" {
					serial = u.SerialNumber
				}
				if len(u.CommandSet) > 0 {
					cmdSet = u.CommandSet
				}
				if u.Manufacturer != "" {
					vendor, pen = detectVendorFromModel(u.Manufacturer + " " + u.Model)
				}
				break
			}
		}
		if serial == "" {
			serial = sanitizeSerial("USB-" + p.Name + "-" + p.PortName)
		}

		langs, colorCapable, _ := ClassifyPrinterCapabilities(vendor, model, cmdSet)
		if p.IsColor {
			colorCapable = true
		}

		status := "online"
		if p.WorkOffline {
			status = "offline"
		}

		source := "usb_spooler"
		if len(cmdSet) > 0 || serial != sanitizeSerial("USB-"+p.Name+"-"+p.PortName) {
			source = "usb_ieee1284"
		}

		res.USBDevices = append(res.USBDevices, DiscoveredDevice{
			DiscoverySource:    source,
			IPAddress:          "0.0.0.0",
			USBPort:            p.PortName,
			SerialNumber:       serial,
			VendorPEN:          pen,
			Vendor:             vendor,
			Model:              model,
			Status:             status,
			IsColorCapable:     colorCapable,
			SupportedLanguages: langs,
			Supplies:           []SupplyItem{}, // Never fabricate 80% fake supply!
		})
	}

	// Capture any standalone USBPRINT IEEE 1284 device not matched to a spooler queue
	for _, u := range usbIDs {
		if u.Model == "" {
			continue
		}
		matched := false
		for _, existing := range res.USBDevices {
			if strings.Contains(strings.ToLower(existing.Model), strings.ToLower(u.Model)) {
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		vendor, pen := detectVendorFromModel(u.Manufacturer + " " + u.Model)
		langs, colorCapable, _ := ClassifyPrinterCapabilities(vendor, u.Model, u.CommandSet)
		serial := u.SerialNumber
		if serial == "" {
			serial = sanitizeSerial("USB-" + u.Model)
		}
		res.USBDevices = append(res.USBDevices, DiscoveredDevice{
			DiscoverySource:    "usb_ieee1284",
			IPAddress:          "0.0.0.0",
			USBPort:            "USBPRINT",
			SerialNumber:       serial,
			VendorPEN:          pen,
			Vendor:             vendor,
			Model:              strings.TrimSpace(u.Manufacturer + " " + u.Model),
			Status:             "online",
			IsColorCapable:     colorCapable,
			SupportedLanguages: langs,
			Supplies:           []SupplyItem{},
		})
	}

	return res
}

func collectActiveSpoolerJobsSnapshot(printers []winSpoolPrinter) []spoolerJobMeta {
	var all []spoolerJobMeta
	for _, p := range printers {
		if isVirtualPrinter(p.Name, p.PortName, p.DriverName) {
			continue
		}
		all = append(all, enumSpoolerActiveJobs(p.Name)...)
	}
	return all
}
