package proxy

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type RouteEvent struct {
	Time        time.Time `json:"time"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	Provider    string    `json:"provider"`
	RouteKind   string    `json:"route_kind,omitempty"`
	Model       string    `json:"model,omitempty"`
	AccountHint string    `json:"account_hint,omitempty"`
	PinActive   bool      `json:"pin_active,omitempty"`
	Failover    bool      `json:"failover,omitempty"`
	StatusCode  int       `json:"status_code,omitempty"`
	LatencyMS   int64     `json:"latency_ms,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type DiagnosticsWriter struct {
	mu   sync.Mutex
	file *os.File
}

func OpenDiagnosticsWriter(path string) (*DiagnosticsWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &DiagnosticsWriter{file: f}, nil
}

func (w *DiagnosticsWriter) Write(event RouteEvent) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	return json.NewEncoder(w.file).Encode(event)
}

func (w *DiagnosticsWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
