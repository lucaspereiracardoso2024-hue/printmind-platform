//go:build windows

package main

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// readInstallerPairingToken reads the optional silent-install PAIRINGTOKEN written by the MSI installer.
func readInstallerPairingToken() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\PrintMind\Agent`, registry.READ)
	if err != nil {
		return ""
	}
	defer k.Close()

	val, _, err := k.GetStringValue("PAIRINGTOKEN")
	if err != nil {
		return ""
	}
	tok := strings.TrimSpace(val)
	if tok == "" || tok == "pm_live_hosp_central_01" {
		return ""
	}
	return tok
}
