package proxy

import (
	"bytes"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestHarnessObservedNativeModelsWithoutChangingRequests(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		for _, model := range []string{"glm-5.3", "gpt-6.1-sol"} {
			body := []byte(`{"model":"` + model + `","input":"preserved history"}`)
			if compressed {
				encoder, err := zstd.NewWriter(nil)
				if err != nil {
					t.Fatal(err)
				}
				body = encoder.EncodeAll(body, nil)
				encoder.Close()
			}
			request := httptest.NewRequest("POST", "/responses", bytes.NewReader(body))
			request.Header.Set("Session_id", "01a198eb-f8e9-72a1-856c-0e986d10f6f7")
			if compressed {
				request.Header.Set("Content-Encoding", "zstd")
			}
			calls := 0
			server := Server{HarnessObserver: func(chat, actualModel string) {
				calls++
				if chat != request.Header.Get("Session_id") || actualModel != model {
					t.Fatalf("unexpected observation: %s %s", chat, actualModel)
				}
			}}
			if handled, _ := server.tryHandleZAI(httptest.NewRecorder(), request); handled {
				t.Fatal("observer claimed routing")
			}
			got, err := io.ReadAll(request.Body)
			if err != nil || !bytes.Equal(got, body) || calls != 1 {
				t.Fatalf("request changed or observation missing: calls=%d err=%v", calls, err)
			}
		}
	}
}

func TestHarnessObserverNeedsModelAndNativeSession(t *testing.T) {
	calls := 0
	server := Server{HarnessObserver: func(string, string) { calls++ }}
	request := httptest.NewRequest("POST", "/responses", nil)
	server.observeHarness(request.Header, "glm-5.3", nil)
	request.Header.Set("X-Session-Id", "native-chat")
	server.observeHarness(request.Header, "", nil)
	if calls != 0 {
		t.Fatal("observed request without identity/model")
	}
	server.observeHarness(request.Header, "glm-5.3", nil)
	if calls != 1 {
		t.Fatal("alternate native session header was ignored")
	}
}

func TestHarnessPrefersNativeThreadMetadataOverSharedSession(t *testing.T) {
	const chat = "01a198eb-f8e9-72a1-856c-0e986d10f6f7"
	for _, nested := range []bool{false, true} {
		header := httptest.NewRequest("POST", "/responses", nil).Header
		header.Set("Session_id", "shared-root-session")
		metadata := `{"thread_id":"` + chat + `","session_id":"shared-root-session","turn_id":"synthetic-turn","request_kind":"turn"}`
		var body []byte
		if nested {
			body = []byte(`{"client_metadata":{"x-codex-turn-metadata":` + metadata + `}}`)
		} else {
			header.Set("X-Codex-Turn-Metadata", metadata)
		}
		called := false
		server := Server{HarnessObserver: func(actual, model string) {
			called = true
			if actual != chat {
				t.Fatalf("targeted %q, want child chat", actual)
			}
		}}
		server.observeHarness(header, "glm-5.3", body)
		if !called {
			t.Fatal("native metadata request was not observed")
		}
	}
}
