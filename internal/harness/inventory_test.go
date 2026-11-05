package harness

import "testing"

func TestStableSkillIDUsedPortableScopeAndPluginOwnership(t *testing.T) {
	for _, row := range []struct{ scope, name, plugin, want string }{
		{"user", "testing", "", "user:testing"},
		{"repo", "testing", "", "repo:testing"},
		{"system", "testing", "", "system:testing"},
		{"admin", "testing", "", "admin:testing"},
		{"user", "testing", "vendor@market", "plugin:vendor@market:testing"},
	} {
		got, err := StableSkillID(row.scope, row.name, row.plugin)
		if err != nil || got != row.want {
			t.Fatalf("skill identity = %q, %v; want %q", got, err, row.want)
		}
	}
	for _, row := range []struct{ scope, name, plugin string }{
		{"unknown", "testing", ""}, {"user", "", ""}, {"repo", " padded", ""}, {"user", "bad\nname", ""}, {"user", "name", "bad\x00plugin"},
	} {
		if _, err := StableSkillID(row.scope, row.name, row.plugin); err == nil {
			t.Fatal("invalid skill identity accepted")
		}
	}
}

func TestQualifiedSkillIDUsedLogicalRootsWithoutHomeOrCacheVersion(t *testing.T) {
	for _, row := range []struct{ scope, name, plugin, path, cwd, want string }{
		{"user", "plain", "", "/Users/one/.agents/skills/plain/SKILL.md", "/work", "user:plain@agents/plain"},
		{"user", "plain", "", "/home/two/.agents/skills/plain/SKILL.md", "/work", "user:plain@agents/plain"},
		{"user", "plain", "", "/home/two/.agents/skills/plain/skills/plain/SKILL.md", "/work", "user:plain@agents/plain/skills/plain"},
		{"user", "plain", "", "/home/two/.codex/skills/plain/SKILL.md", "/work", "user:plain@codex/plain"},
		{"user", "plain", "one@market", "/Users/one/.codex/plugins/cache/market/one/1.0.0/skills/plain/SKILL.md", "/work", "plugin:one@market:plain@cache/market/one/skills/plain"},
		{"user", "plain", "one@market", "/home/two/.codex/plugins/cache/market/one/9.9.9/skills/plain/SKILL.md", "/work", "plugin:one@market:plain@cache/market/one/skills/plain"},
		{"repo", "plain", "", "/work/.agents/skills/plain/SKILL.md", "/work/sub", "repo:plain@repo/.agents/skills/plain"},
	} {
		id, err := QualifiedSkillID(row.scope, row.name, row.plugin, row.path, row.cwd)
		if err != nil || id != row.want {
			t.Fatalf("qualified ID = %q, %v; want %q", id, err, row.want)
		}
	}
	if _, err := QualifiedSkillID("user", "plain", "", "/unknown/root/plain/SKILL.md", "/work"); err == nil {
		t.Fatal("unknown origin invented a qualifier")
	}
}
