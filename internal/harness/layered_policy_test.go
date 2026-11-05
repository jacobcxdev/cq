package harness

import (
	"reflect"
	"testing"
)

func TestLayeredScopePrecedenceAndItemDecisions(t *testing.T) {
	policy := Policy{Version: 2, Enabled: true,
		Profiles:   map[string]Definition{"writing": {Changes: Changes{Plugins: map[string]Decision{"drive": Enabled}, MCP: map[string]Decision{"*": Disabled}}}},
		Global:     Scope{All: Binding{Profiles: []string{"minimal"}}, Models: map[string]Binding{"gpt-*": {Profiles: []string{"writing"}}, "gpt-6": {Changes: Changes{Skills: map[string]Decision{"swift": Enabled}}}}},
		Workspaces: map[string]Scope{"/work": {All: Binding{Changes: Changes{Plugins: map[string]Decision{"drive": Disabled}}}}, "/work/project": {All: Binding{Changes: Changes{Plugins: map[string]Decision{"drive": Inherit}, MCP: map[string]Decision{"docs": Enabled}}}}},
		Tasks:      map[string]Scope{testChat: {All: Binding{Changes: Changes{Plugins: map[string]Decision{"calendar": Enabled}}}}},
	}
	effective := policy.ResolveEffective("gpt-6", "/work/project/child", testChat)
	if !effective.Selection.Managed || effective.Selection.Profile != Custom || effective.Selection.Source != "task" || effective.Selection.Scope != testChat || effective.Selection.Fingerprint == "" {
		t.Fatalf("selection = %#v", effective.Selection)
	}
	for _, test := range []struct {
		kind           Kind
		id             string
		baseline, want bool
	}{{PluginKind, "drive", true, false}, {PluginKind, "calendar", false, true}, {PluginKind, "other", true, false}, {MCPKind, "docs", false, true}, {MCPKind, "other", true, false}, {SkillKind, "swift", false, true}} {
		if got := effective.Choose(test.kind, test.id, test.baseline); got != test.want {
			t.Errorf("%s/%s = %t, want %t", test.kind, test.id, got, test.want)
		}
	}
	if len(effective.Layers) != 6 {
		t.Fatalf("layers = %#v", effective.Layers)
	}
	if got := policy.Resolve("gpt-6", "/work/project/child", testChat); got != effective.Selection {
		t.Fatalf("Resolve differs: %#v", got)
	}
}

func TestLayeredFullResetAndMoreSpecificDefaults(t *testing.T) {
	policy := Policy{Version: 2, Enabled: true, Global: Scope{All: Binding{Profiles: []string{"minimal"}, Changes: Changes{Plugins: map[string]Decision{"drive": Enabled}}}}, Workspaces: map[string]Scope{"/work": {All: Binding{Profiles: []string{"full"}}}}}
	effective := policy.ResolveEffective("claude-sonnet", "/work/project", "")
	if effective.Selection.Profile != Full || !effective.Choose(PluginKind, "drive", true) || effective.Choose(PluginKind, "drive", false) {
		t.Fatalf("full reset did not restore baseline: %#v", effective)
	}
	policy.Workspaces["/work"] = Scope{All: Binding{Changes: Changes{Default: Enabled, Plugins: map[string]Decision{"*": Disabled, "drive": Enabled}, Skills: map[string]Decision{"*": Inherit}}}}
	effective = policy.ResolveEffective("claude-sonnet", "/work/project", "")
	if !effective.Choose(PluginKind, "drive", false) || effective.Choose(PluginKind, "other", true) || !effective.Choose(SkillKind, "other", false) {
		t.Fatalf("item/kind/default precedence wrong: %#v", effective.Changes)
	}
}

func TestLayeredSelectorsBoundariesAndFingerprint(t *testing.T) {
	policy := Policy{Version: 2, Enabled: true, Global: Scope{Models: map[string]Binding{"*": {Changes: Changes{Default: Enabled}}, "glm-*": {Profiles: []string{"minimal"}}, "glm-5*": {Changes: Changes{Plugins: map[string]Decision{"drive": Enabled}}}, "glm-5.3": {Changes: Changes{Plugins: map[string]Decision{"drive": Disabled}}}}}, Workspaces: map[string]Scope{"/work/app": {All: Binding{Profiles: []string{"full"}}}}}
	first := policy.ResolveEffective("GLM-5.3", "/work/app-other", "")
	if first.Choose(PluginKind, "drive", true) || first.Choose(SkillKind, "other", true) || len(first.Layers) != 4 {
		t.Fatalf("selectors = %#v", first)
	}
	for i := 0; i < 20; i++ {
		if got := policy.ResolveEffective("GLM-5.3", "/work/app-other", ""); got.Selection.Fingerprint != first.Selection.Fingerprint {
			t.Fatal("unstable fingerprint")
		}
	}
	policy.Global.Models["glm-5.3"] = Binding{Changes: Changes{Plugins: map[string]Decision{"drive": Enabled}}}
	if got := policy.ResolveEffective("GLM-5.3", "/work/app-other", ""); got.Selection.Fingerprint == first.Selection.Fingerprint {
		t.Fatal("changed decision retained fingerprint")
	}
	policy.Enabled = false
	if got := policy.ResolveEffective("glm-5.3", "/work/app", ""); got.Selection.Managed || !got.Choose(PluginKind, "drive", true) {
		t.Fatalf("disabled policy changed baseline: %#v", got)
	}
}

func TestPromotePreservesLegacyRules(t *testing.T) {
	policy := Policy{Version: 1, Enabled: true, Projects: map[string]Profile{"/work": Full, "/work/app": Minimal}, Chats: map[string]Profile{testChat: Full}}
	before := policy
	if err := Promote(&policy); err != nil {
		t.Fatal(err)
	}
	if policy.Version != 2 || policy.Projects != nil || policy.Chats != nil {
		t.Fatalf("promotion retained legacy fields: %#v", policy)
	}
	for _, test := range []struct{ model, cwd, chat string }{{"glm-5", "/other", ""}, {"GLM-5", "/work/project", ""}, {"glm-5", "/work/app", ""}, {"glm-5", "/work/app", testChat}, {"gpt-6", "/work/app", testChat}} {
		old := before.Resolve(test.model, test.cwd, test.chat)
		got := policy.ResolveEffective(test.model, test.cwd, test.chat)
		if got.Selection.Managed != old.Managed || got.Selection.Profile != old.Profile {
			t.Errorf("%#v: old %#v, promoted %#v", test, old, got.Selection)
		}
	}
	copy := policy
	if err := Promote(&policy); err != nil || !reflect.DeepEqual(copy, policy) {
		t.Fatalf("second promotion changed policy: %v", err)
	}
}

func TestPromoteDisabledPolicyDoesNotCreateGlobalRules(t *testing.T) {
	policy := Policy{Version: 1}
	if err := Promote(&policy); err != nil {
		t.Fatal(err)
	}
	policy.Enabled = true
	policy.Tasks[testChat] = Scope{All: Binding{Profiles: []string{"minimal"}}}
	if got := policy.Resolve("glm-5", "/work", testChat); !got.Managed || got.Profile != Minimal {
		t.Fatalf("explicit chat binding = %#v", got)
	}
	for _, model := range []string{"glm-5", "GLM-5.3", "gpt-6"} {
		if got := policy.Resolve(model, "/work", "019c7714-3b77-74d1-9866-e1f484aae2ab"); got.Managed || got.Profile != Full {
			t.Errorf("promoting disabled policy activated unrelated %s chat: %#v", model, got)
		}
	}
}

func TestPromoteDisabledPolicyRetainsOnlyExplicitLegacyScopes(t *testing.T) {
	policy := Policy{Version: 1, Projects: map[string]Profile{"/work": Minimal}, Chats: map[string]Profile{testChat: Full}}
	if err := Promote(&policy); err != nil {
		t.Fatal(err)
	}
	if policy.Enabled {
		t.Fatal("promotion enabled a disabled policy")
	}
	policy.Enabled = true
	for _, test := range []struct {
		model, cwd, chat string
		managed          bool
		profile          Profile
	}{
		{"glm-5", "/work/project", "", true, Minimal},
		{"glm-5", "/work/project", testChat, true, Full},
		{"glm-5", "/elsewhere", "", false, Full},
		{"gpt-6", "/work/project", testChat, false, Full},
	} {
		if got := policy.Resolve(test.model, test.cwd, test.chat); got.Managed != test.managed || got.Profile != test.profile {
			t.Errorf("%#v: selection = %#v", test, got)
		}
	}
}
