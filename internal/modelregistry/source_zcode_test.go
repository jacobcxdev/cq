package modelregistry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
	"github.com/jacobcxdev/cq/internal/quota"
)

const zcodeStartCatalogueFixture = `{"code":0,"data":{"builtinModels":[
{"modelId":"GLM-5.3","name":"GLM-5.3","contextWindow":1000000,"priority":99,"maxCompletionTokens":128000,"reasoning":{"defaultLevel":"max","levels":{"low":{},"high":{},"max":{}}},"modalities":{}},
{"modelId":"GLM-5.3-Flash","name":"GLM-5.3-Flash","contextWindow":1000000,"maxCompletionTokens":128000,"capabilities":{"vision":true},"reasoning":{"defaultLevel":"high","levels":{"low":{},"high":{},"max":{}}},"modalities":{"input":["text","image","video"],"output":["text"]}},
{"modelId":"GLM-5.4-Future","name":"Next GLM","contextWindow":2000000,"maxCompletionTokens":256000,"reasoning":{"defaultLevel":"low","levels":{"low":{},"high":{}}},"modalities":{"input":["text"],"output":["text"]}},
{"modelId":"GLM-Unentitled","contextWindow":1000000,"maxCompletionTokens":128000}
]}}`

type zcodeCatalogueDoer func(*http.Request) (*http.Response, error)

func (d zcodeCatalogueDoer) Do(r *http.Request) (*http.Response, error) { return d(r) }

func zcodeStartCatalogue(t *testing.T, body string) *zcode.ModelCatalogue {
	t.Helper()
	return &zcode.ModelCatalogue{Version: "3.17.2", HTTP: zcodeCatalogueDoer(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "zcode.z.ai" || r.URL.Path != "/api/v1/client/configs" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			return nil, io.ErrUnexpectedEOF
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
}

func TestZCodeStartSourceDiscoveredWithoutPaidAPIKey(t *testing.T) {
	source := ZCodeStartSource{
		Catalogue: zcodeStartCatalogue(t, zcodeStartCatalogueFixture),
		Quota: func(context.Context) ([]quota.Result, error) {
			return []quota.Result{
				{Plan: "ZCode Start", Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
					"86399s:glm-5.3":       {RemainingPct: 100},
					"86399s:glm-5.3-flash": {RemainingPct: 0},
				}},
				{Plan: "ZCode Start", Status: quota.StatusExhausted, Windows: map[quota.WindowName]quota.Window{
					"1d:glm-5.4-future": {RemainingPct: 0},
				}},
				{Status: quota.StatusError, Windows: map[quota.WindowName]quota.Window{
					"1d:glm-unentitled": {RemainingPct: 100},
				}},
			}, nil
		},
	}
	got, err := source.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, entry := range got.Entries {
		ids = append(ids, entry.ID)
		if entry.Provider != ProviderZAI || entry.ContextWindow <= 0 || entry.MaxOutputTokens <= 0 {
			t.Fatalf("invalid Start projection: %+v", entry)
		}
	}
	if !reflect.DeepEqual(ids, []string{"glm-5.3", "glm-5.3-flash", "glm-5.4-future"}) {
		t.Fatalf("model-scoped entitlements lost or invented: %v", ids)
	}
	var raw struct {
		Context          int    `json:"context_window"`
		Output           int    `json:"max_output_tokens"`
		DefaultReasoning string `json:"default_reasoning_level"`
		Levels           []struct {
			Effort string `json:"effort"`
		} `json:"supported_reasoning_levels"`
		Shell      string   `json:"shell_type"`
		Patch      string   `json:"apply_patch_tool_type"`
		Search     bool     `json:"supports_search_tool"`
		Modalities []string `json:"input_modalities"`
	}
	if err := json.Unmarshal(got.Entries[1].Raw, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Context != 1000000 || raw.Output != 128000 || raw.DefaultReasoning != "high" || len(raw.Levels) != 3 ||
		raw.Shell != "shell_command" || raw.Patch != "freeform" || raw.Search || !reflect.DeepEqual(raw.Modalities, []string{"text", "image"}) {
		t.Fatalf("wrong vendor facts or CQ harness policy: %+v", raw)
	}
	// Existing Codex projection still applies the scoped GLM-5.3 70% policy.
	projected := CodexModelsResponse(Snapshot{Entries: got.Entries})
	var flagship map[string]any
	if err := json.Unmarshal(projected.Models[0], &flagship); err != nil || flagship["auto_compact_token_limit"] != float64(665000) {
		t.Fatalf("GLM-5.3 compaction policy lost: %s, %v", projected.Models[0], err)
	}
}

func TestZCodeStartSourceDidNotInventEntitlements(t *testing.T) {
	source := ZCodeStartSource{
		Catalogue: &zcode.ModelCatalogue{Version: "3.17.2", HTTP: zcodeCatalogueDoer(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("public metadata must not be fetched without a current entitlement")
		})},
		Quota: func(context.Context) ([]quota.Result, error) {
			return []quota.Result{
				{Plan: "ZCode Start", Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
					"1d":                {RemainingPct: 100},
					"arbitrary:glm-5.3": {RemainingPct: 100},
				}},
				{Plan: "Max", Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
					"1d:glm-5.3": {RemainingPct: 100},
				}},
				{Plan: "ZCode Start", Status: quota.StatusError, Windows: map[quota.WindowName]quota.Window{
					"1d:glm-5.3-flash": {RemainingPct: 100},
				}},
			}, nil
		},
	}
	got, err := source.Fetch(context.Background())
	if err != nil || len(got.Entries) != 0 || got.MalformedEntries != 0 {
		t.Fatalf("invented model availability: %+v,%v", got, err)
	}
}

func TestZCodeStartSourceSkippedUnsupportedOrMissingModelFacts(t *testing.T) {
	const fixture = `{"code":0,"data":{"builtinModels":[
{"modelId":"GLM-Missing","maxCompletionTokens":128000},
{"modelId":"GLM-Missing-Output","contextWindow":1000000},
{"modelId":"GLM-Video","contextWindow":1000000,"maxCompletionTokens":128000,"modalities":{"input":["video"],"output":["text"]}},
{"modelId":"GLM-Audio-Output","contextWindow":1000000,"maxCompletionTokens":128000,"modalities":{"input":["text"],"output":["audio"]}},
{"modelId":"GLM-Unknown-Reasoning","contextWindow":1000000,"maxCompletionTokens":128000,"reasoning":{"defaultLevel":"unsupported","levels":{"unsupported":{}}}},
{"modelId":"GLM-Text","contextWindow":500000,"maxCompletionTokens":64000,"reasoning":{"defaultLevel":"low","levels":{"low":{},"unsupported":{}}},"modalities":{"input":["text","audio","video"]}}
]}}`
	source := ZCodeStartSource{Catalogue: zcodeStartCatalogue(t, fixture), Quota: func(context.Context) ([]quota.Result, error) {
		windows := make(map[quota.WindowName]quota.Window)
		for _, id := range []string{"glm-missing", "glm-missing-output", "glm-video", "glm-audio-output", "glm-unknown-reasoning", "glm-text"} {
			windows[quota.WindowName("1d:"+id)] = quota.Window{RemainingPct: 100}
		}
		return []quota.Result{{Plan: "ZCode Start", Status: quota.StatusOK, Windows: windows}}, nil
	}}
	got, err := source.Fetch(context.Background())
	if err != nil || len(got.Entries) != 1 || got.Entries[0].ID != "glm-text" || got.MalformedEntries != 5 {
		t.Fatalf("unsupported metadata projected: %+v,%v", got, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(got.Entries[0].Raw, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["context_window"] != float64(500000) || raw["max_output_tokens"] != float64(64000) ||
		!reflect.DeepEqual(raw["input_modalities"], []any{"text"}) || len(raw["supported_reasoning_levels"].([]any)) != 1 {
		t.Fatalf("source facts or supported adapter subset lost: %s", got.Entries[0].Raw)
	}
}

func TestZCodeStartSourceFailedSafely(t *testing.T) {
	for _, mode := range []string{"quota error", "quota panic", "cancelled", "catalogue error"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source := ZCodeStartSource{Catalogue: zcodeStartCatalogue(t, "private-upstream-value"), Quota: func(context.Context) ([]quota.Result, error) {
				switch mode {
				case "quota error":
					return nil, errors.New("private-upstream-value")
				case "quota panic":
					panic("private-upstream-value")
				case "cancelled":
					cancel()
					return nil, nil
				}
				return []quota.Result{{Plan: "ZCode Start", Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
					"1d:glm-5.3": {RemainingPct: 100},
				}}}, nil
			}}
			got, err := source.Fetch(ctx)
			if err == nil || len(got.Entries) != 0 || strings.Contains(err.Error(), "private-upstream-value") {
				t.Fatalf("unsafe failed discovery: %+v,%v", got, err)
			}
		})
	}
}
