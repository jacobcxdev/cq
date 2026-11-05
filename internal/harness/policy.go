// Package harness stores opt-in context profiles.
package harness

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// MarshalJSON keeps v1 files readable by older CQ versions.
func (policy Policy) MarshalJSON() ([]byte, error) {
	if policy.Version == 1 {
		return json.Marshal(struct {
			Version  int                `json:"version"`
			Enabled  bool               `json:"enabled"`
			Projects map[string]Profile `json:"projects,omitempty"`
			Chats    map[string]Profile `json:"chats,omitempty"`
		}{policy.Version, policy.Enabled, policy.Projects, policy.Chats})
	}
	type plainPolicy Policy
	return json.Marshal(plainPolicy(policy))
}

// Profile selects how much of the normal Codex context is attached.
type Profile string

const (
	Minimal Profile = "minimal"
	Full    Profile = "full"
	Custom  Profile = "custom"
)

// Policy contains explicit overrides. Removing an override restores inheritance.
type Policy struct {
	Version        int                   `json:"version"`
	Enabled        bool                  `json:"enabled"`
	Projects       map[string]Profile    `json:"projects,omitempty"`
	Chats          map[string]Profile    `json:"chats,omitempty"`
	Profiles       map[string]Definition `json:"profiles,omitempty"`
	Global         Scope                 `json:"global,omitempty"`
	Workspaces     map[string]Scope      `json:"workspaces,omitempty"`
	Tasks          map[string]Scope      `json:"tasks,omitempty"`
	workspaceFiles []workspaceLayer
}

// Selection explains the effective profile without changing any Codex setting.
type Selection struct {
	Managed     bool    `json:"managed"`
	Profile     Profile `json:"profile"`
	Source      string  `json:"source"`
	Scope       string  `json:"scope,omitempty"`
	Fingerprint string  `json:"fingerprint,omitempty"`
}

// Resolve selects a profile for the effective model, working directory and chat.
func (policy Policy) Resolve(model, cwd, chatID string) Selection {
	if policy.Version == 2 {
		return policy.ResolveEffective(model, cwd, chatID).Selection
	}
	return policy.resolveLegacy(model, cwd, chatID)
}

func (policy Policy) resolveLegacy(model, cwd, chatID string) Selection {
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
