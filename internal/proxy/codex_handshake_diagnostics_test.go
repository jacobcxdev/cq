package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"

	"os"
	"path/filepath"
	"strings"
	"testing"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func handshakeRouteEvent(t *testing.T, response *http.Response, body []byte) map[string]any {
	t.Helper()
	ctx, diag := withRouteDiagnostics(context.Background())
	emitCodexWSHandshakePayload(ctx, response, body, RouteChoice{}, CandidateAttempt{}, nil)
	event := RouteEvent{Provider: "codex", StatusCode: http.StatusSwitchingProtocols}
	event.applyRouteDiagnostics(diag)
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

// Losing the upstream status or projecting raw upstream strings must fail this test.
func TestCodexWSHandshakeDiagnosticsSafeDetails(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{
		"X-Request-Id":  {"734468b2-abbe-4449-956b-18217fd984a2"},
		"Cf-Ray":        {"9daeb5f17c50e513-TXL"},
		"Authorization": {"Bearer header-secret"},
		"Set-Cookie":    {"session=cookie-secret"},
		"X-Private":     {"private-header"},
	}}
	body := []byte(`{"error":{"type":"permission_error","code":"access_denied","message":"Bearer message-secret user@example.com","token":"body-secret"},"private":"private-body"}`)
	fields := handshakeRouteEvent(t, response, body)
	if fields["status_code"] != float64(101) || fields["upstream_status"] != float64(403) {
		t.Fatalf("downstream/upstream statuses = %v/%v, want 101/403", fields["status_code"], fields["upstream_status"])
	}
	details, ok := fields["upstream_error"].(map[string]any)
	if !ok {
		t.Fatalf("missing upstream_error: %v", fields)
	}
	for key, want := range map[string]any{
		"body_kind": "json", "body_bytes": float64(len(body)), "error_type": "permission_error", "error_code": "access_denied",
		"request_id": "734468b2-abbe-4449-956b-18217fd984a2", "cf_ray": "9daeb5f17c50e513-TXL",
	} {
		if details[key] != want {
			t.Errorf("%s = %v, want %v", key, details[key], want)
		}
	}
	encoded, _ := json.Marshal(fields)
	for _, secret := range []string{"header-secret", "cookie-secret", "private-header", "message-secret", "user@example.com", "body-secret", "private-body"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("private upstream data persisted: %s", encoded)
		}
	}
}

func TestCodexWSHandshakeDiagnosticsRejectsUntrustedDetails(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		kind string
	}{
		{"empty", "", "empty"},
		{"html", "<!DOCTYPE html><html>Bearer private-secret</html>", "html"},
		{"malformed", `{"error": "private-secret"`, "text"},
		{"unknown symbols", `{"error":{"type":"private-secret","code":"private-secret","message":"private-secret"}}`, "json"},
		{"duplicate authority", `{"error":{"type":"permission_error","code":"access_denied"},"error":{"type":"private-secret"}}`, "json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fields := handshakeRouteEvent(t, &http.Response{StatusCode: 403, Header: http.Header{
				"X-Request-Id": {"Bearer private-secret"}, "Cf-Ray": {"private-secret"},
			}}, []byte(test.body))
			details, ok := fields["upstream_error"].(map[string]any)
			if !ok {
				t.Fatalf("missing upstream_error: %v", fields)
			}
			if details["body_kind"] != test.kind {
				t.Errorf("body kind = %v, want %s", details["body_kind"], test.kind)
			}
			if _, ok := details["request_id"]; ok {
				t.Error("unsafe request id retained")
			}
			if _, ok := details["cf_ray"]; ok {
				t.Error("unsafe cf ray retained")
			}
			if _, ok := details["error_type"]; ok {
				t.Error("untrusted error type retained")
			}
			if _, ok := details["error_code"]; ok {
				t.Error("untrusted error code retained")
			}
			encoded, _ := json.Marshal(fields)
			if strings.Contains(string(encoded), "private-secret") {
				t.Fatalf("secret persisted: %s", encoded)
			}
		})
	}
}

func TestCodexWSHandshakeDiagnosticsClearsRejectedAttempt(t *testing.T) {
	ctx, diag := withRouteDiagnostics(context.Background())
	emitCodexWSHandshakePayload(ctx, &http.Response{StatusCode: 403}, []byte(`{"error":{"type":"permission_error","code":"access_denied"}}`), RouteChoice{}, CandidateAttempt{}, nil)
	emitCodexWSHandshakePayload(ctx, &http.Response{StatusCode: 101}, nil, RouteChoice{}, CandidateAttempt{}, nil)
	event := RouteEvent{Provider: "codex", StatusCode: 101}
	event.applyRouteDiagnostics(diag)
	encoded, _ := json.Marshal(event)
	var fields map[string]any
	_ = json.Unmarshal(encoded, &fields)
	if fields["upstream_status"] != float64(101) {
		t.Fatalf("upstream status = %v, want 101", fields["upstream_status"])
	}
	if _, ok := fields["upstream_error"]; ok {
		t.Fatalf("stale rejected attempt: %s", encoded)
	}
}

func TestCodexWSHandshakeDiagnosticsCompressedAndOversized(t *testing.T) {
	var compressed bytes.Buffer
	compressor := gzip.NewWriter(&compressed)
	_, _ = compressor.Write([]byte(`{"error":{"type":"api_error","code":"invalid_workspace_selected","message":"private-secret"}}`))
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		body     []byte
		encoding string
		kind     string
		code     string
	}{
		{"gzip", compressed.Bytes(), "gzip", "json", "invalid_workspace_selected"},
		{"invalid gzip", []byte("private-secret"), "gzip", "undecodable", ""},
		{"unknown encoding", []byte("private-secret"), "br", "undecodable", ""},
		{"oversized", bytes.Repeat([]byte("x"), codexAttemptResponseLimit+1), "identity", "oversized", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			fields := handshakeRouteEvent(t, &http.Response{StatusCode: 403, Header: http.Header{"Content-Encoding": {test.encoding}}}, test.body)
			details, ok := fields["upstream_error"].(map[string]any)
			if !ok {
				t.Fatalf("missing details: %v", fields)
			}
			if details["body_kind"] != test.kind || details["body_bytes"] != float64(len(test.body)) {
				t.Fatalf("body classification = %v", details)
			}
			code, _ := details["error_code"].(string)
			if code != test.code {
				t.Errorf("code = %q, want %q", code, test.code)
			}
		})
	}
}

func TestCodexWSHandshakeDiagnosticsWriterRejectsUnsafeDetails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.jsonl")
	writer, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	for _, details := range []*CodexUpstreamErrorDetails{
		{BodyKind: "private-secret"},
		{BodyKind: "json", ErrorType: "private-secret"},
		{BodyKind: "json", ErrorCode: "private-secret"},
		{BodyKind: "empty", RequestID: "Bearer private-secret"},
		{BodyKind: "empty", CFRay: "private-secret"},
		{BodyKind: "empty", BodyBytes: -1},
	} {
		if err := writer.Write(RouteEvent{Provider: "codex", UpstreamError: details}); !errors.Is(err, ErrUnsafeCodexDiagnostics) {
			t.Fatalf("route bypass accepted: %v", err)
		}
		if err := writer.WriteTrace(CodexTraceEvent{UpstreamError: details}); !errors.Is(err, ErrUnsafeCodexDiagnostics) {
			t.Fatalf("trace bypass accepted: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Fatalf("unsafe details persisted: %s", raw)
	}
}

func TestCodexWSHandshakeDiagnosticsRealRejectedCapture(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "734468b2-abbe-4449-956b-18217fd984a2")
		w.Header().Set("Content-Length", "2097152")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 2<<20))
	}))
	defer upstream.Close()
	identity := codex.AccountIdentity{AccountID: "account", UserID: "user"}
	resolver := &testExactSecretResolver{materials: map[codex.Revision]codex.CredentialMaterial{"revision": testExactCredentialMaterial(identity, "selected")}}
	executor := NewCodexWebSocketAttemptExecutor(nil, resolver)
	choice := RouteChoice{AccountKey: "identity"}
	attempt := CandidateAttempt{AccountKey: "identity", Candidate: codex.CandidateRef{AccountKey: "identity", CandidateID: "candidate"}, Revision: "revision", Source: codex.SourceSystem, Identity: identity}
	ctx, diag := withRouteDiagnostics(context.Background())
	conn, response, _, _, err := executeCodexWebSocketAttempt(executor, ctx, choice, attempt, "ws"+strings.TrimPrefix(upstream.URL, "http"), nil, nil)
	if conn != nil {
		conn.Close()
		t.Fatal("unexpected WebSocket success")
	}
	if err == nil || response == nil || response.StatusCode != 403 {
		t.Fatalf("rejection = %v, %v", response, err)
	}
	event := RouteEvent{StatusCode: 101}
	event.applyRouteDiagnostics(diag)
	encoded, _ := json.Marshal(event)
	var fields map[string]any
	_ = json.Unmarshal(encoded, &fields)
	details, ok := fields["upstream_error"].(map[string]any)
	if !ok || fields["upstream_status"] != float64(403) || details["body_kind"] != "oversized" || details["body_truncated"] != true || details["body_bytes"] != float64(1024) {
		t.Fatalf("real rejected capture must identify bounded prefix: %s", encoded)
	}
}

func TestCodexWSHandshakeDiagnosticsInvalidUpgrade(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusSwitchingProtocols) }))
	defer upstream.Close()
	identity := codex.AccountIdentity{AccountID: "account", UserID: "user"}
	resolver := &testExactSecretResolver{materials: map[codex.Revision]codex.CredentialMaterial{"revision": testExactCredentialMaterial(identity, "selected")}}
	executor := NewCodexWebSocketAttemptExecutor(nil, resolver)
	choice := RouteChoice{AccountKey: "identity"}
	attempt := CandidateAttempt{AccountKey: "identity", Candidate: codex.CandidateRef{AccountKey: "identity", CandidateID: "candidate"}, Revision: "revision", Source: codex.SourceSystem, Identity: identity}
	ctx, diag := withRouteDiagnostics(context.Background())
	conn, response, _, _, err := executeCodexWebSocketAttempt(executor, ctx, choice, attempt, "ws"+strings.TrimPrefix(upstream.URL, "http"), nil, nil)
	if conn != nil {
		conn.Close()
		t.Fatal("unexpected WebSocket success")
	}
	if err == nil || response == nil || response.StatusCode != 101 {
		t.Fatalf("invalid upgrade = %v, %v", response, err)
	}
	event := RouteEvent{StatusCode: 101}
	event.applyRouteDiagnostics(diag)
	encoded, _ := json.Marshal(event)
	var fields map[string]any
	_ = json.Unmarshal(encoded, &fields)
	if fields["upstream_status"] != float64(101) || fields["upstream_error"] == nil {
		t.Fatalf("invalid upgrade treated as successful: %s", encoded)
	}
}
