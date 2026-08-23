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

func TestCodexTurnReceiptControlV1AndV2HitAndMiss(t *testing.T) {
	store, err := NewCodexTurnReceiptStore(bytes.NewReader(bytes.Repeat([]byte{0x31}, 32)), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	store.register([]byte("session-a"), []byte("turn-a"), testCodexTurnReceipt())
	server := &Server{CodexTurnReceipts: store}

	for _, version := range []struct {
		path          string
		schemaVersion int
		shadow        bool
	}{
		{path: RuntimeCodexTurnReceiptPath, schemaVersion: 1},
		{path: RuntimeCodexTurnReceiptV2Path, schemaVersion: 2, shadow: true},
	} {
		for _, test := range []struct {
			name  string
			body  string
			found bool
		}{
			{name: "hit", body: `{"session_id":"session-a","turn_id":"turn-a"}`, found: true},
			{name: "miss", body: `{"session_id":"session-a","turn_id":"turn-b"}`, found: false},
		} {
			t.Run(test.name+version.path, func(t *testing.T) {
				request := httptest.NewRequest(http.MethodPost, version.path, strings.NewReader(test.body))
				writer := httptest.NewRecorder()
				server.handleCodexTurnReceipt(writer, request)
				if writer.Code != http.StatusOK || writer.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("response = %d %q: %s", writer.Code, writer.Header().Get("Content-Type"), writer.Body.String())
				}
				var envelope struct {
					SchemaVersion int             `json:"schema_version"`
					Found         bool            `json:"found"`
					Receipt       json.RawMessage `json:"receipt"`
				}
				if err := json.Unmarshal(writer.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.SchemaVersion != version.schemaVersion || envelope.Found != test.found || (len(envelope.Receipt) != 0) != test.found {
					t.Fatalf("response = %+v", envelope)
				}
				if got := bytes.Contains(writer.Body.Bytes(), []byte("shadow_comparison")); got != (test.found && version.shadow) {
					t.Fatalf("shadow fields present = %t, want %t: %s", got, test.found && version.shadow, writer.Body.String())
				}
				if strings.Contains(writer.Body.String(), "session-a") || strings.Contains(writer.Body.String(), "turn-a") {
					t.Fatalf("response leaked raw identity: %s", writer.Body.String())
				}
			})
		}
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
	for _, path := range []string{RuntimeCodexTurnReceiptPath, RuntimeCodexTurnReceiptV2Path} {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		if got := normalCallerPolicy(request); got != normalCallerRouteLocal {
			t.Fatalf("route policy for %s = %d, want local", path, got)
		}
	}
}
