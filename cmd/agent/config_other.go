//go:build !windows

package main

// readInstallerPairingToken returns empty on non-Windows platforms where MSI registry keys do not exist.
func readInstallerPairingToken() string {
	return ""
}
