//go:build !windows && !linux

package main

import (
	"context"
	"net/http"
)

func checkAndApplyUpdate(_ context.Context, _ *http.Client, _ Config, _ OSPaths, _ RemoteConfig) {
	// Auto-update only applies on Windows (MSI) and Linux (ELF atomic swap).
}
