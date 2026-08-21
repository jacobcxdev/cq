package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCodexTurnReceiptControlHitAndMiss(t *testing.T) {
	store, err := NewCodexTurnReceiptStore(bytes.NewReader(bytes.Repeat([]byte{0x31}, 32)), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	store.register([]byte("session-a"), []byte("turn-a"), testCodexTurnReceipt())
	server := &Server{CodexTurnReceipts: store}

	for _, test := range []struct {
		name  string
		body  string
		found bool
	}{
		{name: "hit", body: `{"session_id":"session-a","turn_id":"turn-a"}`, found: true},
		{name: "miss", body: `{"session_id":"session-a","turn_id":"turn-b"}`, found: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, RuntimeCodexTurnReceiptPath, strings.NewReader(test.body))
			writer := httptest.NewRecorder()
			server.handleCodexTurnReceipt(writer, request)
			if writer.Code != http.StatusOK || writer.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("response = %d %q: %s", writer.Code, writer.Header().Get("Content-Type"), writer.Body.String())
			}
			var response CodexTurnReceiptLookupV1
			if err := json.Unmarshal(writer.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.SchemaVersion != 1 || response.Found != test.found || (response.Receipt != nil) != test.found {
				t.Fatalf("response = %+v", response)
			}
			if strings.Contains(writer.Body.String(), "session-a") || strings.Contains(writer.Body.String(), "turn-a") {
				t.Fatalf("response leaked raw identity: %s", writer.Body.String())
			}
		})
	}
}

func TestCodexTurnReceiptControlRejectsInvalidInput(t *testing.T) {
	store, err := NewCodexTurnReceiptStore(bytes.NewReader(bytes.Repeat([]byte{0x32}, 32)), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{CodexTurnReceipts: store}
	for _, body := range []string{
		`{}`,
		`{"session_id":"session","turn_id":"turn","extra":true}`,
		`{"session_id":"session","turn_id":"turn"}{}`,
		`{"session_id":"bad\nvalue","turn_id":"turn"}`,
		`{"session_id":"session","turn_id":"` + strings.Repeat("x", canonicalSessionIDMaxBytes+1) + `"}`,
	} {
		request := httptest.NewRequest(http.MethodPost, RuntimeCodexTurnReceiptPath, strings.NewReader(body))
		writer := httptest.NewRecorder()
		server.handleCodexTurnReceipt(writer, request)
		if writer.Code != http.StatusBadRequest {
			t.Fatalf("body %q returned %d: %s", body, writer.Code, writer.Body.String())
		}
	}

	writer := httptest.NewRecorder()
	(&Server{}).handleCodexTurnReceipt(writer, httptest.NewRequest(http.MethodPost, RuntimeCodexTurnReceiptPath, strings.NewReader(`{"session_id":"s","turn_id":"t"}`)))
	if writer.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable response = %d", writer.Code)
	}
}

func TestCodexTurnReceiptControlIsLocalRoute(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, RuntimeCodexTurnReceiptPath, nil)
	if got := normalCallerPolicy(request); got != normalCallerRouteLocal {
		t.Fatalf("route policy = %d, want local", got)
	}
}
