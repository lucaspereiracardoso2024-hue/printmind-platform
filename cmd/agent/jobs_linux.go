//go:build linux

package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"
)

// ResolveHostDomain returns the DNS domain suffix or WORKGROUP on Linux.
func ResolveHostDomain() string {
	if host, err := os.Hostname(); err == nil {
		if idx := strings.IndexByte(host, '.'); idx != -1 && idx+1 < len(host) {
			return host[idx+1:]
		}
	}
	return "LOCAL"
}

func collectPlatformPrintJobs(store *SQLiteStore, devices []DiscoveredDevice, spooler LocalSpoolerResult, hostname string) ([]PrintJobRecord, []JobSummary, *WPPReadiness) {
	var lastLine int64
	if store != nil {
		lastLine, _, _ = store.GetCheckpoint("cups_page_log")
	}

	lookup := BuildDeviceColorLookup(devices, spooler.PrinterIPMap)
	var jobs []PrintJobRecord

	if raw, err := os.ReadFile("/var/log/cups/page_log"); err == nil && len(raw) > 0 {
		parsed, newLine := ParseCUPSPageLog(string(raw), lastLine, lookup, hostname)
		jobs = append(jobs, parsed...)
		if store != nil && newLine > lastLine {
			if sErr := store.SaveCheckpoint("cups_page_log", newLine, ""); sErr != nil {
				slog.Warn("failed to save cups page_log checkpoint", "error", sErr)
			}
		}
	}

	// Query local CUPS IPP Get-Jobs on localhost:631 for completed/active job metadata if page_log was empty
	if len(jobs) == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		client := newLocalHTTPClient(1500 * time.Millisecond)
		reqBytes := BuildIPPGetJobs("ipp://localhost:631/printers", 3001, "completed")
		if resp, err := sendIPPRequest(ctx, client, "http://127.0.0.1:631/printers", reqBytes); err == nil && resp.StatusCode < 0x0400 {
			var lastIPPJobID int64
			if store != nil {
				lastIPPJobID, _, _ = store.GetCheckpoint("cups_ipp_jobs")
			}
			maxJobID := lastIPPJobID
			for _, g := range resp.Groups {
				if g.Tag != ippTagJobAttrs {
					continue
				}
				jids := ippGetInts(g.Attrs, "job-id")
				if len(jids) == 0 || int64(jids[0]) <= lastIPPJobID {
					continue
				}
				if int64(jids[0]) > maxJobID {
					maxJobID = int64(jids[0])
				}
				pages := 1
				if imp := ippGetInts(g.Attrs, "job-impressions-completed"); len(imp) > 0 && imp[0] > 0 {
					pages = imp[0]
				}
				copies := 1
				if cp := ippGetInts(g.Attrs, "copies"); len(cp) > 0 && cp[0] > 0 {
					copies = cp[0]
				}
				jobs = append(jobs, PrintJobRecord{
					JobID:          jids[0],
					PrinterName:    "CUPS",
					UserName:       ippGetString(g.Attrs, "job-originating-user-name"),
					MachineName:    hostname,
					DocumentName:   ippGetString(g.Attrs, "job-name"),
					PagesPrinted:   pages,
					Copies:         copies,
					EffectivePages: pages * copies,
					MonoPages:      pages * copies,
					Source:         "cups_ipp",
				})
			}
			if store != nil && maxJobID > lastIPPJobID {
				_ = store.SaveCheckpoint("cups_ipp_jobs", maxJobID, "")
			}
		}
	}

	if len(jobs) > 0 {
		EnrichPrintJobsDepartment(context.Background(), jobs, defaultLDAPResolver)
	}
	return jobs, AggregateJobSummaries(jobs), &WPPReadiness{Supported: false}
}
