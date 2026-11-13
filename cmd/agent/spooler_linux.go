//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalSpoolerResult holds Linux local USB / ipp-usb printers and port->IP mappings.
type LocalSpoolerResult struct {
	USBDevices     []DiscoveredDevice
	WSDResolvedIPs map[string]string
	PrinterIPMap   map[string]string
}

func discoverLocalSpooler(enableUSB bool) LocalSpoolerResult {
	res := LocalSpoolerResult{
		WSDResolvedIPs: make(map[string]string),
		PrinterIPMap:   make(map[string]string),
	}
	if !enableUSB {
		return res
	}

	// 1. Discover USB printers exposed via ipp-usb daemon (localhost:60000-65535)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := newLocalHTTPClient(2 * time.Second)
	res.USBDevices = append(res.USBDevices, discoverLinuxIPPUSB(ctx, client)...)

	// 2. Inspect Linux sysfs IEEE 1284 Device IDs (/sys/class/usbmisc/lp*/device/ieee1284_id)
	matches, _ := filepath.Glob("/sys/class/usbmisc/lp*/device/ieee1284_id")
	for _, idPath := range matches {
		raw, err := os.ReadFile(idPath)
		if err != nil || len(raw) == 0 {
			continue
		}
		info := ParseIEEE1284DeviceID(raw)
		if info.Model == "" && info.Manufacturer == "" {
			continue
		}
		vendor, pen := detectVendorFromModel(info.Manufacturer + " " + info.Model)
		langs, colorCapable, _ := ClassifyPrinterCapabilities(vendor, info.Model, info.CommandSet)
		model := strings.TrimSpace(info.Manufacturer + " " + info.Model)
		serial := info.SerialNumber
		if serial == "" {
			serial = sanitizeSerial("USB-" + model)
		}
		already := false
		for _, d := range res.USBDevices {
			if strings.EqualFold(d.SerialNumber, serial) || strings.Contains(strings.ToLower(d.Model), strings.ToLower(info.Model)) {
				already = true
				break
			}
		}
		if already {
			continue
		}
		port := filepath.Base(filepath.Dir(filepath.Dir(idPath)))
		res.USBDevices = append(res.USBDevices, DiscoveredDevice{
			DiscoverySource:    "usb_ieee1284",
			IPAddress:          "0.0.0.0",
			USBPort:            port,
			SerialNumber:       serial,
			VendorPEN:          pen,
			Vendor:             vendor,
			Model:              model,
			Status:             "online",
			IsColorCapable:     colorCapable,
			SupportedLanguages: langs,
			Supplies:           []SupplyItem{},
		})
	}
	return res
}
