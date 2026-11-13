package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

const (
	DefaultMaxLogBytes   = 20 * 1024 * 1024 // 20 MB
	DefaultMaxLogBackups = 5
)

// EventSink receives Warn/Error messages for OS-native event logging (e.g. Windows Event Log).
type EventSink interface {
	LogEvent(level slog.Level, msg string)
	Close()
}

// RotatingFileWriter writes log lines and rotates files when maxBytes is exceeded.
type RotatingFileWriter struct {
	mu         sync.Mutex
	path       string
	maxBytes   int64
	maxBackups int
	size       int64
	file       *os.File
}

// NewRotatingFileWriter initializes a size-based rotating log file writer.
func NewRotatingFileWriter(path string, maxBytes int64, maxBackups int) (*RotatingFileWriter, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxLogBytes
	}
	if maxBackups <= 0 {
		maxBackups = DefaultMaxLogBackups
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	var sz int64
	if info, err := f.Stat(); err == nil {
		sz = info.Size()
	}
	return &RotatingFileWriter{
		path:       path,
		maxBytes:   maxBytes,
		maxBackups: maxBackups,
		size:       sz,
		file:       f,
	}, nil
}

// Write implements io.Writer with automatic file rotation.
func (w *RotatingFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return 0, err
		}
		w.file = f
		if info, err := f.Stat(); err == nil {
			w.size = info.Size()
		}
	}

	if w.size+int64(len(p)) > w.maxBytes && w.size > 0 {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *RotatingFileWriter) rotateLocked() error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	oldest := fmt.Sprintf("%s.%d", w.path, w.maxBackups)
	_ = os.Remove(oldest)

	for i := w.maxBackups - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.path, i)
		dst := fmt.Sprintf("%s.%d", w.path, i+1)
		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, dst)
		}
	}
	if _, err := os.Stat(w.path); err == nil {
		_ = os.Rename(w.path, w.path+".1")
	}

	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	w.file = f
	w.size = 0
	return nil
}

// Close closes the underlying log file handle.
func (w *RotatingFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

type eventBridgeHandler struct {
	inner slog.Handler
	sink  EventSink
}

func (h *eventBridgeHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.inner.Enabled(ctx, lvl)
}

func (h *eventBridgeHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.inner.Handle(ctx, r)
	if h.sink != nil && r.Level >= slog.LevelWarn {
		h.sink.LogEvent(r.Level, r.Message)
	}
	return err
}

func (h *eventBridgeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &eventBridgeHandler{inner: h.inner.WithAttrs(attrs), sink: h.sink}
}

func (h *eventBridgeHandler) WithGroup(name string) slog.Handler {
	return &eventBridgeHandler{inner: h.inner.WithGroup(name), sink: h.sink}
}

// SetupLogger configures structured JSON logging to a rotating file (and stdout in console mode).
func SetupLogger(logPath string, console bool, sink EventSink) (*slog.Logger, io.Closer) {
	rfw, err := NewRotatingFileWriter(logPath, DefaultMaxLogBytes, DefaultMaxLogBackups)
	var out io.Writer
	var closer io.Closer
	if err == nil {
		closer = rfw
		if console {
			out = io.MultiWriter(os.Stdout, rfw)
		} else {
			out = rfw
		}
	} else {
		out = os.Stdout
	}

	baseHandler := slog.NewJSONHandler(out, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	var handler slog.Handler = baseHandler
	if sink != nil {
		handler = &eventBridgeHandler{inner: baseHandler, sink: sink}
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger, closer
}
