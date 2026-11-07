package dots

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jacobcxdev/cq/internal/harness"
)

func TestCloudHarnessProfileKeptOnlySelectedPluginApps(t *testing.T) {
	inventory := harness.Inventory{Capabilities: []harness.Capability{
		{Kind: harness.PluginKind, ID: "keep@market", NativeID: "keep@market", AppIDs: []string{"keep", "shared", "blocked"}, Enabled: true, Available: true},
		{Kind: harness.PluginKind, ID: "drop@market", NativeID: "drop@market", AppIDs: []string{"drop", "shared"}, Enabled: true, Available: true},
	}}
	for _, test := range []struct {
		name                           string
		gate, defaultEnabled, selected bool
		wantGate                       bool
		wantApps                       map[string]bool
	}{
		{"minimal", true, true, false, false, map[string]bool{"keep": false, "shared": false, "blocked": false, "drop": false, "unowned": false}},
		{"selected owner", true, true, true, true, map[string]bool{"keep": true, "shared": true, "blocked": false, "drop": false, "unowned": false}},
		{"global feature disabled", false, true, true, false, map[string]bool{"keep": false, "shared": false, "blocked": false, "drop": false, "unowned": false}},
		{"global default disabled", true, false, true, true, map[string]bool{"keep": false, "shared": true, "blocked": false, "drop": false, "unowned": false}},
	} {
		t.Run(test.name, func(t *testing.T) {
			baseline := cloudMessage{"features.apps": cloudHarnessRaw(test.gate), "apps": cloudHarnessRaw(map[string]any{
				"_default": map[string]any{"enabled": test.defaultEnabled, "default_tools_approval_mode": "approve"},
				"shared":   map[string]any{"enabled": true},
				"blocked":  map[string]any{"enabled": false},
				"drop":     map[string]any{"enabled": true},
				"unowned":  map[string]any{"enabled": true, "tools": map[string]any{"read": map[string]any{"approval_mode": "approve", "enabled": false}}, "links": map[string]any{"account": map[string]any{"approvals_reviewer": "user"}}},
			})}
			before := cloudHarnessRaw(baseline)
			effective := harness.Effective{Changes: harness.Changes{Default: harness.Disabled}}
			if test.selected {
				effective.Changes.Plugins = map[string]harness.Decision{"keep@market": harness.Enabled}
			}
			plan := cloudHarnessCompileProfile(effective, inventory, baseline, nil)
			if !bytes.Equal(plan.config["features.apps"], cloudHarnessRaw(test.wantGate)) {
				t.Fatalf("connected app feature gate = %s, want %t", plan.config["features.apps"], test.wantGate)
			}
			apps := cloudHarnessObject(plan.config["apps"])
			if !bytes.Equal(cloudHarnessObject(apps["_default"])["enabled"], []byte("false")) {
				t.Fatal("unconfigured apps were not suppressed")
			}
			for id, wanted := range test.wantApps {
				if !bytes.Equal(cloudHarnessObject(apps[id])["enabled"], cloudHarnessRaw(wanted)) {
					t.Errorf("app %s enabled = %s, want %t", id, cloudHarnessObject(apps[id])["enabled"], wanted)
				}
			}
			originalApps := cloudHarnessObject(baseline["apps"])
			for _, key := range []string{"tools", "links"} {
				if !bytes.Equal(cloudHarnessObject(apps["unowned"])[key], cloudHarnessObject(originalApps["unowned"])[key]) {
					t.Fatalf("app %s settings changed", key)
				}
			}
			if !bytes.Equal(cloudHarnessObject(apps["_default"])["default_tools_approval_mode"], []byte(`"approve"`)) {
				t.Fatal("default app approval policy changed")
			}
			if !bytes.Equal(before, cloudHarnessRaw(baseline)) {
				t.Fatal("compilation mutated restoration baseline")
			}
		})
	}
}

func TestCloudHarnessAppBaselineRestoredEmptyAndOriginalSettings(t *testing.T) {
	for _, test := range []struct {
		name, native, original string
		want                   map[string]any
	}{
		{"absent", `{"config":{"features":{"plugins":true,"apps":true}},"layers":[]}`, `{}`, map[string]any{}},
		{"empty", `{"config":{"features":{"plugins":true,"apps":true},"apps":{}},"layers":[]}`, `{}`, map[string]any{}},
		{"original app override", `{"config":{"features":{"plugins":true,"apps":false},"apps":{"_default":{"enabled":false},"keep":{"enabled":true,"default_tools_approval_mode":"approve"}}},"layers":[]}`, `{"apps.keep.enabled":false}`, map[string]any{"_default": map[string]any{"enabled": false}, "keep": map[string]any{"enabled": false, "default_tools_approval_mode": "approve"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := &cloudHarness{}
			baseline, err := h.baselineConfig(cloudHarnessObject(json.RawMessage(test.native)), json.RawMessage(test.original), "")
			if err != nil {
				t.Fatal(err)
			}
			var apps map[string]any
			if json.Unmarshal(baseline["apps"], &apps) != nil || !reflect.DeepEqual(apps, test.want) {
				t.Fatalf("app baseline = %s, want %v", baseline["apps"], test.want)
			}
			plan := cloudHarnessCompileProfile(harness.Effective{Changes: harness.Changes{Default: harness.Disabled}}, harness.Inventory{}, baseline, nil)
			if !bytes.Equal(plan.config["features.apps"], []byte("false")) {
				t.Fatal("minimal profile did not disable connected apps")
			}
			if !bytes.Equal(baseline["apps"], cloudHarnessRaw(test.want)) {
				t.Fatal("minimal profile changed app restoration settings")
			}
		})
	}
}

func TestCloudHarnessLegacyMinimalSuppressedConnectedApps(t *testing.T) {
	config := cloudHarnessMinimalConfig(nil, json.RawMessage(`{"features.apps":true,"apps":{"existing":{"enabled":true,"default_tools_approval_mode":"approve"}}}`))
	if !bytes.Equal(config["features.apps"], []byte("false")) {
		t.Fatal("legacy minimal still exposes connected apps")
	}
	apps := cloudHarnessObject(config["apps"])
	if !bytes.Equal(cloudHarnessObject(apps["_default"])["enabled"], []byte("false")) || !bytes.Equal(cloudHarnessObject(apps["existing"])["enabled"], []byte("false")) {
		t.Fatal("legacy minimal retained per-app enables")
	}
}

func TestCloudHarnessAppBaselineRejectedInvalidSettings(t *testing.T) {
	for _, apps := range []string{`false`, `{"_default":{"enabled":null}}`, `{"existing":{"enabled":"true"}}`, `{"existing":false}`} {
		t.Run(apps, func(t *testing.T) {
			native := cloudHarnessObject(json.RawMessage(`{"config":{"features":{"plugins":true,"apps":true},"apps":` + apps + `},"layers":[]}`))
			if _, err := (&cloudHarness{}).baselineConfig(native, nil, ""); err == nil {
				t.Fatal("malformed native app policy was silently replaced")
			}
		})
	}
}

func TestCloudHarnessPluginDeltaPreservedUnownedAndSharedApps(t *testing.T) {
	baseline := cloudMessage{"features.apps": cloudHarnessRaw(true), "apps": json.RawMessage(`{"shared":{"enabled":true},"drop":{"enabled":true},"unowned":{"enabled":true},"_default":{"enabled":true}}`)}
	inventory := harness.Inventory{Capabilities: []harness.Capability{
		{Kind: harness.PluginKind, ID: "keep", NativeID: "keep", AppIDs: []string{"shared"}, Enabled: true, Available: true},
		{Kind: harness.PluginKind, ID: "drop", NativeID: "drop", AppIDs: []string{"shared", "drop"}, Enabled: true, Available: true},
	}}
	plan := cloudHarnessCompileProfile(harness.Effective{Changes: harness.Changes{Plugins: map[string]harness.Decision{"drop": harness.Disabled}}}, inventory, baseline, nil)
	apps := cloudHarnessObject(plan.config["apps"])
	for id, wanted := range map[string]bool{"shared": true, "drop": false, "unowned": true, "_default": true} {
		if !bytes.Equal(cloudHarnessObject(apps[id])["enabled"], cloudHarnessRaw(wanted)) {
			t.Errorf("app %s = %s, want %t", id, cloudHarnessObject(apps[id])["enabled"], wanted)
		}
	}
}

func TestCloudHarnessAppBaselineRejectedConflictingEnabledAliases(t *testing.T) {
	for _, original := range []string{
		`{"apps.foo":{"enabled":false},"apps.foo.enabled":true}`,
		`{"apps.foo.enabled":false,"apps.\"foo\".enabled":true}`,
		`{"apps.foo.enabled":false,"apps.'foo'.enabled":true}`,
		`{"apps":{"foo":{"enabled":false}},"apps.foo.enabled":true}`,
		`{"apps.\"foo.bar\"":{"enabled":false},"apps.'foo.bar'.enabled":true}`,
	} {
		t.Run(original, func(t *testing.T) {
			native := cloudHarnessObject(json.RawMessage(`{"config":{"features":{"plugins":true}},"layers":[]}`))
			if _, err := (&cloudHarness{}).baselineConfig(native, json.RawMessage(original), ""); err == nil {
				t.Fatal("contradictory native app overrides were accepted")
			}
		})
	}
}
