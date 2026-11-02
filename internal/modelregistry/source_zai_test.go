package modelregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
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
	if err := PublishCodexRegistryCache(fsys, userdirs.Roots{Cache: "/cq/cache"}, Snapshot{Entries: result.Entries}); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := LoadCodexRegistryCache(fsys, userdirs.Roots{Cache: "/cq/cache"})
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

func TestZAISourceDisplayNamesPreserveCodexMetadata(t *testing.T) {
	for _, tc := range []struct {
		id, displayName, want string
	}{
		{"glm-5.3", "glm-5.3", "GLM-5.3"},
		{"glm-5.3-flash", "glm-5.3-flash", "GLM-5.3-Flash"},
		{"glm-5-turbo", "glm-5-turbo", "GLM-5-Turbo"},
		{"glm-6.1-flash-lite", "glm-6.1-flash-lite", "GLM-6.1-Flash-Lite"},
		{"glm-7-preview", "", "GLM-7-Preview"},
		{"glm-5.3", "Z.ai Flagship", "Z.ai Flagship"},
		{"glm-5-turbo", "GLM-5-Turbo", "GLM-5-Turbo"},
		{"glm-6-vl", "GLM-6-VL", "GLM-6-VL"},
		{"glm-future", "Future model", "Future model"},
		{"embedding-3", "embedding-3", "embedding-3"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			metadata := map[string]any{
				"slug": tc.id, "display_name": tc.displayName,
				"context_window": float64(1048576), "input_modalities": []any{"text", "image"},
				"supported_reasoning_levels": []any{map[string]any{"effort": "max", "description": "Deep reasoning"}},
				"future_capability":          map[string]any{"enabled": true},
			}
			raw, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			src := &ZAISource{
				Token: func(context.Context) (string, error) { return "fake-key", nil },
				Client: zaiModelsDoer(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"models":[` + string(raw) + `]}`))}, nil
				}),
			}
			result, err := src.Fetch(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Entries) != 1 || result.Entries[0].ID != tc.id || result.Entries[0].DisplayName != tc.want {
				t.Fatalf("unexpected model identity or display name: %+v", result.Entries)
			}
			projected := CodexModelsResponse(Snapshot{Entries: result.Entries, CodexRawByID: result.CodexRawByID})
			var got map[string]any
			if err := json.Unmarshal(projected.Models[0], &got); err != nil {
				t.Fatal(err)
			}
			metadata["display_name"] = tc.want
			if !reflect.DeepEqual(got, metadata) {
				t.Fatalf("display name change altered native metadata: got %v, want %v", got, metadata)
			}
		})
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

type zaiModelsClosedBody struct {
	io.Reader
	closed *int
}

func (b zaiModelsClosedBody) Close() error { *b.closed++; return nil }

func TestZAISourceAccountFallback(t *testing.T) {
	for _, test := range []struct {
		name       string
		firstCode  int
		lastCode   int
		wantCalls  int
		requestErr bool
		wantErr    bool
	}{
		{"unauthorised", 401, 200, 2, false, false},
		{"forbidden", 403, 200, 2, false, false},
		{"rate limited", 429, 200, 2, false, false},
		{"all rejected", 429, 429, 2, false, true},
		{"bad request", 400, 200, 1, false, true},
		{"server error", 500, 200, 1, false, true},
		{"transport error", 0, 200, 1, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls, closed := 0, 0
			source := &ZAISource{
				Tokens: func(context.Context) ([]string, error) {
					return []string{"", "rejected-key", "rejected-key", "working-key"}, nil
				},
				Token: func(context.Context) (string, error) {
					t.Fatal("ordered account reader fell back to legacy token")
					return "", nil
				},
				Client: zaiModelsDoer(func(r *http.Request) (*http.Response, error) {
					calls++
					if test.requestErr {
						return nil, errors.New("private-key")
					}
					status, key := test.firstCode, "Bearer rejected-key"
					if calls == 2 {
						status, key = test.lastCode, "Bearer working-key"
					}
					if r.Header.Get("Authorization") != key {
						t.Error("unexpected model credential order")
					}
					return &http.Response{StatusCode: status, Body: zaiModelsClosedBody{Reader: strings.NewReader(`{"models":[{"slug":"glm-5.3"}]}`), closed: &closed}}, nil
				}),
			}
			result, err := source.Fetch(t.Context())
			if (err != nil) != test.wantErr || calls != test.wantCalls {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if err != nil && strings.Contains(err.Error(), "private-key") {
				t.Fatal("model discovery exposed credential details")
			}
			if !test.requestErr && closed != calls {
				t.Fatalf("unclosed model responses: calls=%d closed=%d", calls, closed)
			}
			if !test.wantErr && (len(result.Entries) != 1 || result.Entries[0].ID != "glm-5.3") {
				t.Fatal("fallback did not return model catalogue")
			}
		})
	}
}

func TestZAISourceOrderedReaderFailureDoesNotUseLegacyToken(t *testing.T) {
	for _, read := range []func(context.Context) ([]string, error){
		func(context.Context) ([]string, error) { return nil, errors.New("private-key") },
		func(context.Context) ([]string, error) { return []string{"", " "}, nil },
	} {
		source := &ZAISource{
			Tokens: read,
			Token: func(context.Context) (string, error) {
				t.Fatal("failed ordered reader used legacy token")
				return "", nil
			},
			Client: zaiModelsDoer(func(*http.Request) (*http.Response, error) {
				t.Fatal("unavailable credentials reached upstream")
				return nil, nil
			}),
		}
		_, err := source.Fetch(t.Context())
		if err == nil || strings.Contains(err.Error(), "private-key") {
			t.Fatal("ordered credential failure missing or unsafe")
		}
	}
}
