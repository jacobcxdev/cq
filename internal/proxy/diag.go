package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

type routeDiagnosticsContextKey struct{}

type routeDiagnostics struct {
	mu          sync.Mutex
	accountHint string
	failover    bool
}

func withRouteDiagnostics(ctx context.Context) (context.Context, *routeDiagnostics) {
	diag := &routeDiagnostics{}
	return context.WithValue(ctx, routeDiagnosticsContextKey{}, diag), diag
}

func noteRouteAccount(ctx context.Context, accountHint string, failover bool) {
	if ctx == nil {
		return
	}
	diag, _ := ctx.Value(routeDiagnosticsContextKey{}).(*routeDiagnostics)
	if diag == nil {
		return
	}
	diag.mu.Lock()
	defer diag.mu.Unlock()
	if accountHint != "" {
		diag.accountHint = accountHint
	}
	diag.failover = diag.failover || failover
}

func (d *routeDiagnostics) fields() (accountHint string, failover bool) {
	if d == nil {
		return "", false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.accountHint, d.failover
}

func (event *RouteEvent) applyRouteDiagnostics(diag *routeDiagnostics) {
	if event == nil {
		return
	}
	accountHint, failover := diag.fields()
	if accountHint != "" {
		event.AccountHint = accountHint
	}
	if failover {
		event.Failover = true
	}
}

func redactedAccountHint(prefix string, identifiers ...string) string {
	for _, identifier := range identifiers {
		if identifier == "" {
			continue
		}
		sum := sha256.Sum256([]byte(identifier))
		return prefix + ":" + hex.EncodeToString(sum[:])[:12]
	}
	return ""
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
