// Package harness stores opt-in context profiles for GLM models.
package harness

import (
	"path/filepath"
	"strings"
)

// Profile selects how much of the normal Codex context is attached.
type Profile string

const (
	Minimal Profile = "minimal"
	Full    Profile = "full"
)

// Policy contains explicit overrides. Removing an override restores inheritance.
type Policy struct {
	Version  int                `json:"version"`
	Enabled  bool               `json:"enabled"`
	Projects map[string]Profile `json:"projects,omitempty"`
	Chats    map[string]Profile `json:"chats,omitempty"`
}

// Selection explains the effective profile without changing any Codex setting.
type Selection struct {
	Managed bool    `json:"managed"`
	Profile Profile `json:"profile"`
	Source  string  `json:"source"`
	Scope   string  `json:"scope,omitempty"`
}

// Resolve selects a profile for the effective model, working directory and chat.
func (policy Policy) Resolve(model, cwd, chatID string) Selection {
	if !strings.HasPrefix(strings.ToLower(model), "glm-") {
		return Selection{Profile: Full, Source: "model"}
	}
	if !policy.Enabled {
		return Selection{Profile: Full, Source: "disabled"}
	}
	if profile, ok := policy.Chats[chatID]; ok {
		return Selection{Managed: true, Profile: profile, Source: "chat", Scope: chatID}
	}
	selection := Selection{Managed: true, Profile: Minimal, Source: "default"}
	if !filepath.IsAbs(cwd) {
		return selection
	}
	cwd = filepath.Clean(cwd)
	for project, profile := range policy.Projects {
		relative, err := filepath.Rel(project, cwd)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		if len(project) > len(selection.Scope) {
			selection = Selection{Managed: true, Profile: profile, Source: "project", Scope: project}
		}
	}
	return selection
}
