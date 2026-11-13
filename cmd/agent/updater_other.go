//go:build !windows

package main

import (
	"context"
	"net/http"
)

func checkAndApplyMSIUpdate(_ context.Context, _ *http.Client, _ Config, _ OSPaths, _ RemoteConfig) {
	// MSI auto-update only applies on Windows installations.
}
