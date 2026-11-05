package harness

import (
	"github.com/jacobcxdev/cq/internal/fsutil"
	"testing"
)

func TestV2PolicyStorageValidation(t *testing.T) {
	valid := `{"version":2,"enabled":true,"profiles":{"research":{"changes":{"plugins":{"drive":"enabled"}}}},"global":{"all":{"profiles":["minimal","research"]}},"workspaces":{"/work":{"models":{"gpt-*":{"changes":{"skills":{"user:swift":"disabled"}}}}}},"tasks":{"019c6e27-e55b-73d1-87d8-4e01f1f75043":{"all":{"profiles":["full"]}}}}`
	policy, err := decodePolicy([]byte(valid))
	if err != nil {
		t.Fatalf("valid v2: %v", err)
	}
	fsys := fsutil.NewMemFS()
	if err := Save(fsys, "/state/harness.json", policy); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(fsys, "/state/harness.json")
	if err != nil || !loaded.ResolveEffective("gpt-6", "/work", "").Choose(PluginKind, "drive", false) {
		t.Fatalf("roundtrip = %#v, %v", loaded, err)
	}
	for name, raw := range map[string]string{
		"v1 layered fields":    `{"version":1,"profiles":{"research":{"changes":{}}}}`,
		"v2 legacy projects":   `{"version":2,"projects":{}}`,
		"v2 legacy chats null": `{"version":2,"chats":null}`,
		"unknown profile":      `{"version":2,"global":{"all":{"profiles":["missing"]}}}`,
		"reserved definition":  `{"version":2,"profiles":{"full":{"changes":{}}}}`,
		"profile chaining":     `{"version":2,"profiles":{"research":{"profiles":["minimal"],"changes":{}}}}`,
		"invalid decision":     `{"version":2,"global":{"all":{"changes":{"default":"auto"}}}}`,
		"empty item decision":  `{"version":2,"global":{"all":{"changes":{"plugins":{"drive":""}}}}}`,
		"blank identifier":     `{"version":2,"global":{"all":{"changes":{"plugins":{" ":"enabled"}}}}}`,
		"middle wildcard":      `{"version":2,"global":{"models":{"g*t":{"profiles":["full"]}}}}`,
		"spaced model":         `{"version":2,"global":{"models":{"gpt 6":{"profiles":["full"]}}}}`,
		"ambiguous selectors":  `{"version":2,"global":{"models":{"GLM-*":{"profiles":["full"]},"glm-*":{"profiles":["minimal"]}}}}`,
		"relative workspace":   `{"version":2,"workspaces":{"relative":{"all":{"profiles":["full"]}}}}`,
		"bad task":             `{"version":2,"tasks":{"invalid":{"all":{"profiles":["full"]}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodePolicy([]byte(raw)); err == nil {
				t.Fatal("accepted invalid layered policy")
			}
		})
	}
}
