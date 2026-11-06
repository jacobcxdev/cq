package grok

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseModelsPreservesAccountCatalogue(t *testing.T) {
	const body = `{"data":[{"id":"grok-4.7","name":"Grok 4.7","apiBackend":"responses","contextWindow":500000,"maxCompletionTokens":128000,"aliases":["grok-latest"],"reasoningEfforts":["low",{"effort":"high","description":"Think more"}],"defaultReasoningEffort":"high","vendorField":true},{"id":"legacy","context_window":256000},{"id":"messages","api_backend":"messages","_meta":{"contextWindow":1000000}},{"id":"unknown"}]}`
	models, err := ParseModels([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 4 {
		t.Fatalf("models=%#v", models)
	}
	m := models[0]
	if m.ID != "grok-4.7" || m.Backend != "responses" || m.ContextWindow != 500000 || m.MaxOutputTokens != 128000 || !reflect.DeepEqual(m.Aliases, []string{"grok-latest"}) || !reflect.DeepEqual(m.ReasoningEfforts, []string{"low", "high"}) || m.DefaultReasoningEffort != "high" {
		t.Fatalf("metadata=%#v", m)
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(m.Raw, &raw) != nil || string(raw["vendorField"]) != "true" {
		t.Fatal("lost native raw metadata")
	}
	if models[1].Backend != "chat_completions" || models[2].Backend != "messages" || models[2].ContextWindow != 1000000 || models[3].ContextWindow != 0 {
		t.Fatalf("incorrect defaults: %#v", models)
	}
}

func TestParseModelsRejectsMalformedOrAmbiguousCatalogue(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"data":null}`, `{"data":[null]}`, `{"data":[{"id":12}]}`, `{"data":[{"id":"same"},{"id":"SAME"}]}`, `{"data":[{"id":"bad","contextWindow":-1}]}`, `{"data":[{"id":"bad","apiBackend":"unknown"}]}`, `{"data":[{"id":"bad","baseUrl":"https://api.x.ai/v1"}]}`} {
		t.Run(body, func(t *testing.T) {
			if _, err := ParseModels([]byte(body)); err == nil {
				t.Fatal("invalid catalogue accepted")
			}
		})
	}
}

func TestParseModelsReadsNativeCapabilityEffortsAndContextChoices(t *testing.T) {
	models, err := ParseModels([]byte(`{"data":[{"id":"grok-4.7","apiBackend":"responses","contextWindows":[256000,500000],"capabilities":{"reasoning_effort":["low","high","xhigh"],"default_reasoning_effort":"high"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if models[0].ContextWindow != 256000 || models[0].MaxContextWindow != 500000 || !reflect.DeepEqual(models[0].ReasoningEfforts, []string{"low", "high", "xhigh"}) || models[0].DefaultReasoningEffort != "high" {
		t.Fatalf("metadata=%#v", models[0])
	}
}

func TestParseModelsDoesNotPublishProviderCredentialConfiguration(t *testing.T) {
	models, err := ParseModels([]byte(`{"data":[{"id":"grok-4.7","apiBackend":"responses","contextWindow":500000,"apiKey":"secret-value","extraHeaders":{"Authorization":"secret-value"},"_meta":{"api_key":"secret-value"},"capabilities":{"reasoning_effort":["high"]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(models[0].Raw), "secret-value") {
		t.Fatal("provider credential configuration retained in public model metadata")
	}
}

func TestParseModelsReadsLiveSubscriptionReasoningSchema(t *testing.T) {
	// Sanitized field shapes observed from the authenticated subscription
	// catalogue. No account identifiers or credentials are retained.
	const body = `{"data":[
		{"id":"grok-4.7","api_backend":"responses","context_window":256000,"context_windows":[256000,500000],"reasoning_effort":"high","reasoning_efforts":[{"id":"low","value":"low","label":"Low","description":"Less reasoning","default":false},{"id":"high","value":"high","label":"High","description":"More reasoning","default":true},{"id":"xhigh","value":"xhigh","label":"Extra High","description":"Extended reasoning","default":false}]},
		{"id":"grok-4.7-build-fast","api_backend":"responses","context_window":256000,"context_windows":[256000,500000],"reasoning_effort":"high","reasoning_efforts":[{"id":"high","value":"high","default":true},{"id":"xhigh","value":"xhigh","default":false}]},
		{"id":"grok-4.6","api_backend":"responses","context_window":256000,"context_windows":[256000,500000],"reasoning_efforts":[{"id":"high","value":"high","default":true},{"id":"xhigh","value":"xhigh","default":false}]},
		{"id":"grok-4.5","api_backend":"responses","context_window":256000,"context_windows":[256000,500000],"reasoning_effort":"high","reasoning_efforts":[{"id":"low","value":"low","default":false},{"id":"high","value":"high","default":true}]}
	]}`
	models, err := ParseModels([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 4 {
		t.Fatalf("models=%d", len(models))
	}
	for _, model := range models {
		if model.Backend != "responses" || model.ContextWindow != 256000 || model.MaxContextWindow != 500000 || model.DefaultReasoningEffort != "high" {
			t.Fatalf("metadata=%#v", model)
		}
	}
	if !reflect.DeepEqual(models[0].ReasoningEfforts, []string{"low", "high", "xhigh"}) || !reflect.DeepEqual(models[3].ReasoningEfforts, []string{"low", "high"}) {
		t.Fatalf("effort menu broadened: %#v", models)
	}
}

func TestParseModelsRejectsAmbiguousReasoningOptions(t *testing.T) {
	for _, fields := range []string{
		`"reasoning_efforts":[{"id":"high","value":"low"}]`,
		`"reasoning_efforts":[{"effort":"high","value":"low"}]`,
		`"reasoning_efforts":[{"value":"high","default":"true"}]`,
		`"reasoning_efforts":[{"value":"high","default":true},{"value":"low","default":true}]`,
		`"reasoning_efforts":[{"value":"high"},{"id":"high"}]`,
		`"reasoning_efforts":[{"id":"unsupported"}]`,
		`"reasoning_effort":"low","reasoning_efforts":[{"value":"high","default":true},{"value":"low"}]`,
		`"default_reasoning_effort":"high","reasoning_effort":"low","reasoning_efforts":["high","low"]`,
		`"reasoning_effort":"low","capabilities":{"reasoning_effort":["low","high"],"default_reasoning_effort":"high"}`,
	} {
		body := `{"data":[{"id":"grok-4.7",` + fields + `}]}`
		t.Run(fields, func(t *testing.T) {
			if _, err := ParseModels([]byte(body)); err == nil {
				t.Fatal("ambiguous reasoning metadata accepted")
			}
		})
	}
}
