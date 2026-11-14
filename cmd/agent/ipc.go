package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const ipcHeaderToken = "X-PrintMind-IPC-Token"

func generateIPCToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// StartIPCServer starts a strictly loopback (127.0.0.1) authenticated HTTP IPC server
// allowing the desktop Tray process to read agent status and request an immediate scan.
func StartIPCServer(
	ctx context.Context,
	onScanNow func() (alreadyPending bool),
	getStatus func() AgentStatusSnapshot,
) (addr string, token string, shutdown func(context.Context) error, err error) {
	token, err = generateIPCToken()
	if err != nil {
		return "", "", nil, fmt.Errorf("generate ipc token: %w", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", nil, fmt.Errorf("listen loopback ipc: %w", err)
	}

	mux := http.NewServeMux()
	authorize := func(r *http.Request) bool {
		provided := strings.TrimSpace(r.Header.Get(ipcHeaderToken))
		if len(provided) != len(token) {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
	}

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !authorize(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		snap := getStatus()
		snap.IPCToken = "" // never reflect token back over GET
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(snap); err != nil {
			http.Error(w, `{"error":"encode failed"}`, http.StatusInternalServerError)
		}
	})

	mux.HandleFunc("/scan-now", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !authorize(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		alreadyPending := false
		if onScanNow != nil {
			alreadyPending = onScanNow()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"queued":          true,
			"already_pending": alreadyPending,
		})
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
		WriteTimeout:      5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()

	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			_ = ln.Close()
		}
	}()

	return ln.Addr().String(), token, srv.Shutdown, nil
}

// SendIPCScanNow sends an authenticated POST /scan-now request to the running background service
// using the loopback address and ephemeral token saved in status.json.
func SendIPCScanNow(statusPath string) (alreadyPending bool, err error) {
	snap, err := ReadStatusSnapshot(statusPath)
	if err != nil {
		return false, fmt.Errorf("serviço indisponível (%w)", err)
	}
	if !strings.HasPrefix(snap.IPCAddr, "127.0.0.1:") || strings.TrimSpace(snap.IPCToken) == "" {
		return false, errors.New("o serviço PrintMind não publicou canal IPC local")
	}

	req, err := http.NewRequest(http.MethodPost, "http://"+snap.IPCAddr+"/scan-now", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set(ipcHeaderToken, snap.IPCToken)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("não foi possível contactar o serviço local: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("serviço retornou status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Queued         bool `json:"queued"`
		AlreadyPending bool `json:"already_pending"`
	}
	if uErr := json.Unmarshal(body, &out); uErr == nil {
		return out.AlreadyPending, nil
	}
	return false, nil
}

// ReadStatusSnapshot safely reads status.json for Tray or diagnostics.
func ReadStatusSnapshot(statusPath string) (AgentStatusSnapshot, error) {
	raw, err := os.ReadFile(statusPath)
	if err != nil {
		return AgentStatusSnapshot{}, err
	}
	var snap AgentStatusSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return AgentStatusSnapshot{}, err
	}
	return snap, nil
}
