package proxy

import "testing"

func TestCodexHTTPRequestPolicyHasNoCQByteLimit(t *testing.T) {
	if codexHTTPRequestMaxBytes != 0 {
		t.Fatalf("HTTP request limit = %d, want unbounded", codexHTTPRequestMaxBytes)
	}
}

func TestCodexWebSocketPolicyMatchesInstalledClient(t *testing.T) {
	if codexWebSocketMessageMaxBytes != 64<<20 {
		t.Fatalf("WebSocket message limit = %d, want %d", codexWebSocketMessageMaxBytes, 64<<20)
	}
}
