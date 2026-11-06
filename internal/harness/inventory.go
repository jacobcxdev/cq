package harness

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

// StableSkillID excludes installation roots and versioned plugin cache paths.
// Native application still uses Capability.Path from the same inventory row.
func StableSkillID(scope, name, pluginID string) (string, error) {
	if !inventoryIdentifier(name) || pluginID != "" && !inventoryIdentifier(pluginID) {
		return "", fmt.Errorf("invalid native skill identity")
	}
	switch scope {
	case "user", "repo", "system", "admin":
	default:
		return "", fmt.Errorf("unknown native skill scope")
	}
	if pluginID != "" {
		return "plugin:" + pluginID + ":" + name, nil
	}
	return scope + ":" + name, nil
}

// QualifiedSkillID identifies a known logical source from first discovery,
// excluding HOME and plugin cache versions. NativeInventory consistently uses
// this identity for non-plugin skills, even before any name collision exists.
func QualifiedSkillID(scope, name, pluginID, path, cwd string) (string, error) {
	base, err := StableSkillID(scope, name, pluginID)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') {
		return "", fmt.Errorf("skill collision has no portable source")
	}
	directory := filepath.Dir(filepath.Clean(path))
	segments := strings.Split(filepath.ToSlash(directory), "/")
	for index := 0; index+4 < len(segments); index++ {
		if segments[index] == "plugins" && segments[index+1] == "cache" {
			qualifier := "cache/" + strings.Join(segments[index+2:index+4], "/")
			if suffix := strings.Join(segments[index+5:], "/"); suffix != "" {
				qualifier += "/" + suffix
			}
			return base + "@" + qualifier, nil
		}
	}
	for index := 0; index+2 < len(segments); index++ {
		if (segments[index] != ".agents" && segments[index] != ".codex") || segments[index+1] != "skills" {
			continue
		}
		if scope == "repo" {
			project := filepath.FromSlash(strings.Join(segments[:index], "/"))
			if project == "" {
				project = string(filepath.Separator)
			}
			if relative, err := filepath.Rel(project, cwd); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return base + "@repo/" + strings.Join(segments[index:], "/"), nil
			}
			return "", fmt.Errorf("skill collision has no portable repository source")
		}
		return base + "@" + strings.TrimPrefix(segments[index], ".") + "/" + strings.Join(segments[index+2:], "/"), nil
	}
	if scope == "repo" && filepath.IsAbs(cwd) {
		if relative, err := filepath.Rel(cwd, directory); err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return base + "@repo/" + filepath.ToSlash(relative), nil
		}
	}
	return "", fmt.Errorf("skill collision has no portable source")
}

func inventoryIdentifier(value string) bool {
	return value != "" && value != "*" && len(value) <= 4096 && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

// Capability describes native metadata, independently of a per-chat profile.
// Available means metadata is present in the active native configuration or
// installation. It does not establish health or permission to enable a feature.
type Capability struct {
	ID           string   `json:"id"`
	Kind         Kind     `json:"kind"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	NativeID     string   `json:"native_id"`
	Path         string   `json:"path,omitempty"`
	PluginID     string   `json:"plugin_id,omitempty"`
	MCPServers   []string `json:"mcp_servers,omitempty"`
	Enabled      bool     `json:"enabled"`
	Available    bool     `json:"available"`
	Dependencies []string `json:"dependencies,omitempty"`
}

// Inventory contains all discovered rows, including disabled capabilities.
type Inventory struct {
	Capabilities []Capability     `json:"capabilities"`
	Issues       []InventoryIssue `json:"issues,omitempty"`
}

// InventoryIssue deliberately excludes raw config and native error messages.
type InventoryIssue struct {
	Code string `json:"code"`
	Kind Kind   `json:"kind"`
	ID   string `json:"id,omitempty"`
}
