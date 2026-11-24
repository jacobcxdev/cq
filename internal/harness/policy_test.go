package harness

import (
	"path/filepath"
	"testing"
)

const testChat = "019c6e27-e55b-73d1-87d8-4e01f1f75043"

func TestResolveKeepsOtherModelsAndDisabledPolicyUnmanaged(t *testing.T) {
	policy := Policy{Enabled: true, Projects: map[string]Profile{testAbsPath("/work"): Minimal}, Chats: map[string]Profile{testChat: Minimal}}
	for _, model := range []string{"gpt-6", "claude-sonnet", "prefix-glm-5", "glm", ""} {
		got := policy.Resolve(model, testAbsPath("/work/project"), testChat)
		if got.Managed || got.Profile != Full || got.Source != "model" {
			t.Errorf("model %q selection = %#v, want unmanaged full", model, got)
		}
	}
	policy.Enabled = false
	if got := policy.Resolve("GLM-5", testAbsPath("/work/project"), testChat); got.Managed || got.Profile != Full || got.Source != "disabled" {
		t.Fatalf("disabled selection = %#v, want unmanaged full", got)
	}
}

func TestResolveUsesChatThenDeepestProjectThenMinimal(t *testing.T) {
	policy := Policy{
		Enabled:  true,
		Projects: map[string]Profile{testAbsPath("/"): Full, testAbsPath("/work"): Full, testAbsPath("/work/project"): Minimal},
		Chats:    map[string]Profile{testChat: Full},
	}
	tests := []struct {
		name, cwd, chat string
		profile         Profile
		source, scope   string
	}{
		{"chat takes precedence", testAbsPath("/work/project/nested"), testChat, Full, "chat", testChat},
		{"deepest ancestor", testAbsPath("/work/project/nested"), "", Minimal, "project", testAbsPath("/work/project")},
		{"exact project", testAbsPath("/work/project"), "", Minimal, "project", testAbsPath("/work/project")},
		{"parent project", testAbsPath("/work/other"), "", Full, "project", testAbsPath("/work")},
		{"sibling prefix excluded", testAbsPath("/work/project-other"), "", Full, "project", testAbsPath("/work")},
		{"root ancestor", testAbsPath("/other"), "", Full, "project", testAbsPath("/")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := policy.Resolve("GlM-5.3", test.cwd, test.chat)
			want := Selection{Managed: true, Profile: test.profile, Source: test.source, Scope: test.scope}
			if got != want {
				t.Fatalf("selection = %#v, want %#v", got, want)
			}
		})
	}
	delete(policy.Projects, testAbsPath("/"))
	for _, cwd := range []string{testAbsPath("/elsewhere"), "relative/project", ""} {
		want := Selection{Managed: true, Profile: Minimal, Source: "default"}
		if got := policy.Resolve("glm-5", cwd, ""); got != want {
			t.Errorf("cwd %q selection = %#v, want %#v", cwd, got, want)
		}
	}
	if got := policy.Resolve("glm-5", filepath.Join(testAbsPath("/work"), "project", "..", "other"), ""); got.Profile != Full || got.Scope != testAbsPath("/work") {
		t.Fatalf("cleaned cwd selection = %#v", got)
	}
}
