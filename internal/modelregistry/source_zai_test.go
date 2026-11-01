package modelregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type zaiModelsDoer func(*http.Request) (*http.Response, error)

func (f zaiModelsDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestZAISourcePreservesNativeCodexMetadata(t *testing.T) {
	const raw = `{"slug":"glm-5.3","display_name":"GLM-5.3","context_window":1048576,"default_reasoning_level":"max","supported_reasoning_levels":[{"effort":"low","description":"Light reasoning"},{"effort":"high","description":"Enhanced reasoning"},{"effort":"max","description":"Deep reasoning"}],"input_modalities":["text"],"shell_type":"shell_command","truncation_policy":{"mode":"bytes","limit":10000}}`
	src := &ZAISource{
		Token: func(context.Context) (string, error) { return "fake-key", nil },
		Client: zaiModelsDoer(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != "https://api.z.ai/api/v1/models" || r.Header.Get("Authorization") != "Bearer fake-key" {
				t.Fatalf("unexpected model discovery request: %s", r.URL)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"models":[` + raw + `,{"slug":"glm-5-turbo","context_window":204800},{},null,{"slug":123}]}`))}, nil
		}),
	}
	result, err := src.Fetch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 2 {
		t.Fatalf("models = %d", len(result.Entries))
	}
	if result.MalformedEntries != 3 {
		t.Fatalf("malformed entries = %d", result.MalformedEntries)
	}
	for _, e := range result.Entries {
		if e.Provider != ProviderZAI || e.Validate() != nil {
			t.Fatalf("bad entry: %+v", e)
		}
	}
	projected := CodexModelsResponse(Snapshot{Entries: result.Entries, CodexRawByID: result.CodexRawByID})
	if len(projected.Models) != 2 || string(projected.Models[0]) != raw {
		t.Fatalf("native metadata changed: %s", projected.Models)
	}

	fsys := fsutil.NewMemFS()
	if err := PublishCodexCache(fsys, "/codex/models_cache.json", Snapshot{Entries: result.Entries, CodexRawByID: result.CodexRawByID}, time.Now(), "test"); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCodexEntriesFromCache(fsys, "/codex/models_cache.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 || loaded[0].Provider != ProviderZAI || loaded[1].Provider != ProviderZAI {
		t.Fatalf("provider ownership lost: %+v", loaded)
	}
	roundTrip := CodexModelsResponse(Snapshot{Entries: loaded})
	var compact bytes.Buffer
	if err := json.Compact(&compact, roundTrip.Models[0]); err != nil {
		t.Fatal(err)
	}
	if compact.String() != raw {
		t.Fatalf("cache lost native metadata: %s", roundTrip.Models[0])
	}
}

func TestZAISourceRejectsErrorsWithoutCredentials(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		requestErr error
	}{
		{name: "auth", status: 401, body: `{"error":"fake-key"}`},
		{name: "invalid JSON", status: 200, body: `fake-key`},
		{name: "wrong envelope", status: 200, body: `{"error":"fake-key"}`},
		{name: "transport", requestErr: errors.New("fake-key")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &ZAISource{Token: func(context.Context) (string, error) { return "fake-key", nil }, Client: zaiModelsDoer(func(*http.Request) (*http.Response, error) {
				if tc.requestErr != nil {
					return nil, tc.requestErr
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			_, err := src.Fetch(t.Context())
			if err == nil || strings.Contains(err.Error(), "fake-key") {
				t.Fatalf("unsafe error: %v", err)
			}
		})
	}
}

type zaiPanicBody struct {
	close bool
}

func (b zaiPanicBody) Read(p []byte) (int, error) {
	if b.close {
		return 0, io.EOF
	}
	panic("fake-secret-key")
}

func (b zaiPanicBody) Close() error {
	if b.close {
		panic("fake-secret-key")
	}
	return nil
}

func TestZAISourcePanicDiagnosticsOmitCredentials(t *testing.T) {
	for _, stage := range []string{"token", "request", "body read", "body close"} {
		t.Run(stage, func(t *testing.T) {
			source := &ZAISource{
				Token: func(context.Context) (string, error) {
					if stage == "token" {
						panic("fake-secret-key")
					}
					return "fake-secret-key", nil
				},
				Client: zaiModelsDoer(func(*http.Request) (*http.Response, error) {
					if stage == "request" {
						panic("fake-secret-key")
					}
					return &http.Response{StatusCode: 200, Body: zaiPanicBody{close: stage == "body close"}}, nil
				}),
			}
			refresher := Refresher{Catalog: NewCatalog(Snapshot{}), ZAI: source}
			diag, err := refresher.Refresh(t.Context())
			if err == nil || strings.Contains(err.Error(), "fake-secret-key") {
				t.Fatal("source panic was not safely classified")
			}
			if sourceErr := diag.SourceErrors[ProviderZAI]; sourceErr == nil || strings.Contains(sourceErr.Error(), "fake-secret-key") {
				t.Fatal("source diagnostic exposed panic details")
			}
		})
	}
}

func TestZAIRegistrySurvivesPartialRefresh(t *testing.T) {
	raw := json.RawMessage(`{"slug":"glm-5.3","context_window":1048576}`)
	cat := NewCatalog(Snapshot{Entries: []Entry{{Provider: ProviderZAI, ID: "glm-5.3", Source: SourceNative}}, CodexRawByID: map[string]json.RawMessage{"glm-5.3": raw}})
	r := &Refresher{Catalog: cat, ZAI: SourceFunc(func(context.Context) (SourceResult, error) { return SourceResult{}, errors.New("offline") }), Codex: SourceFunc(func(context.Context) (SourceResult, error) {
		return SourceResult{Entries: []Entry{{Provider: ProviderCodex, ID: "gpt-test", Source: SourceNative}}}, nil
	})}
	diag, err := r.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if diag.SourceErrors[ProviderZAI] == nil {
		t.Fatal("missing Z.ai diagnostic")
	}
	projected := CodexModelsResponse(cat.Snapshot())
	if len(projected.Models) != 2 {
		t.Fatalf("models lost after partial refresh: %s", projected.Models)
	}
}
