//go:build !windows

package main

import (
	"fmt"
	"os"
)

func shouldRunWindowsBootstrapGUI(_ string, _ bool, _ Config) bool {
	return false
}

func runWindowsBootstrapGUI(_ Config, _ OSPaths) int {
	return 1
}

func runTrayMode(_ Config, _ OSPaths) int {
	fmt.Fprintln(os.Stderr, "Tray mode is only supported on Windows desktops.")
	return 1
}

func notifyGUIErrorIfInteractive(_ string) {
	// Non-Windows platforms log directly to stderr/journald.
}
