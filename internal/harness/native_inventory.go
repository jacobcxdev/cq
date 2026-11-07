package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const nativeCurationLimit = 8 << 20

// NativeInventory reads all native capability metadata independently of chat
// exclusions. Run, when injected, writes the combined plugins/skills/config
// response envelope; callers never receive that raw configuration envelope.
type NativeInventory struct {
	Executable string
	Run        func(context.Context, string, string, io.Writer) error
}

func (client NativeInventory) Snapshot(ctx context.Context, cwd string) (Inventory, error) {
	if !filepath.IsAbs(cwd) || filepath.Clean(cwd) != cwd {
		return Inventory{}, fmt.Errorf("inventory project must be absolute and clean")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	run := client.Run
	if run == nil {
		run = runNativeCurationInventory
	}
	output := &nativeCurationOutput{}
	if err := run(ctx, client.Executable, cwd, output); err != nil {
		if ctx.Err() != nil {
			return Inventory{}, fmt.Errorf("native capability inventory unavailable: %w", ctx.Err())
		}
		return Inventory{}, fmt.Errorf("native capability inventory unavailable")
	}
	if output.exceeded {
		return Inventory{}, fmt.Errorf("native capability inventory exceeds limit")
	}
	return decodeNativeInventory(output.Bytes(), cwd)
}

type nativePluginMetadata struct {
	ID             string                     `json:"id"`
	Name           string                     `json:"name"`
	Installed      *bool                      `json:"installed"`
	Enabled        *bool                      `json:"enabled"`
	Availability   *string                    `json:"availability"`
	DisabledReason *string                    `json:"disabledReason"`
	Interface      *nativeCapabilityInterface `json:"interface"`
	RemotePluginID string                     `json:"remotePluginId"`
}

type nativeCapabilityInterface struct {
	DisplayName      string `json:"displayName"`
	ShortDescription string `json:"shortDescription"`
}

type nativeSkillMetadata struct {
	Name             string                     `json:"name"`
	Description      *string                    `json:"description"`
	ShortDescription string                     `json:"shortDescription"`
	Enabled          *bool                      `json:"enabled"`
	Path             string                     `json:"path"`
	Scope            string                     `json:"scope"`
	PluginID         string                     `json:"pluginId"`
	Interface        *nativeCapabilityInterface `json:"interface"`
	Dependencies     *struct {
		Tools *[]struct {
			Type  *string `json:"type"`
			Value *string `json:"value"`
		} `json:"tools"`
	} `json:"dependencies"`
}

type nativeCurationResponse struct {
	Plugins       *nativePluginInventory     `json:"plugins"`
	PluginDetails map[string]json.RawMessage `json:"pluginDetails"`
	Skills        *struct {
		Data *[]struct {
			Cwd    string                 `json:"cwd"`
			Errors json.RawMessage        `json:"errors"`
			Skills *[]nativeSkillMetadata `json:"skills"`
		} `json:"data"`
	} `json:"skills"`
	Config *struct {
		Config  map[string]json.RawMessage    `json:"config"`
		Layers  *[]nativeInventoryConfigLayer `json:"layers"`
		Origins map[string]json.RawMessage    `json:"origins"`
	} `json:"config"`
}

type nativePluginInventory struct {
	Marketplaces *[]struct {
		Name    string                  `json:"name"`
		Path    *string                 `json:"path"`
		Plugins *[]nativePluginMetadata `json:"plugins"`
	} `json:"marketplaces"`
	LoadErrors json.RawMessage `json:"marketplaceLoadErrors"`
}

func nativePluginMCPServers(raw json.RawMessage, expected nativePluginMetadata) ([]string, bool) {
	var detail struct {
		Plugin *struct {
			Summary *struct{ ID, Name string } `json:"summary"`
			Servers *[]string                  `json:"mcpServers"`
		} `json:"plugin"`
	}
	if json.Unmarshal(raw, &detail) != nil || detail.Plugin == nil || detail.Plugin.Summary == nil || detail.Plugin.Servers == nil || detail.Plugin.Summary.ID != expected.ID || detail.Plugin.Summary.Name != expected.Name {
		return nil, false
	}
	servers := append([]string{}, (*detail.Plugin.Servers)...)
	sort.Strings(servers)
	for index, name := range servers {
		if !inventoryIdentifier(name) || index > 0 && name == servers[index-1] {
			return nil, false
		}
	}
	return servers, true
}

// App ownership remains part of the atomic plugin capability. Only stable app
// IDs are retained; app account, authentication and tool metadata stay native.
func nativePluginAppIDs(raw json.RawMessage, expected nativePluginMetadata) ([]string, bool) {
	var detail struct {
		Plugin *struct {
			Summary *struct{ ID, Name string } `json:"summary"`
			Apps    *[]struct {
				ID string `json:"id"`
			} `json:"apps"`
		} `json:"plugin"`
	}
	if json.Unmarshal(raw, &detail) != nil || detail.Plugin == nil || detail.Plugin.Summary == nil || detail.Plugin.Apps == nil || detail.Plugin.Summary.ID != expected.ID || detail.Plugin.Summary.Name != expected.Name {
		return nil, false
	}
	ids := make([]string, 0, len(*detail.Plugin.Apps))
	for _, app := range *detail.Plugin.Apps {
		if !inventoryIdentifier(app.ID) || app.ID == "_default" {
			return nil, false
		}
		ids = append(ids, app.ID)
	}
	sort.Strings(ids)
	for index := 1; index < len(ids); index++ {
		if ids[index] == ids[index-1] {
			return nil, false
		}
	}
	return ids, true
}

type nativeInventoryConfigLayer struct {
	Config         map[string]json.RawMessage `json:"config"`
	Name           json.RawMessage            `json:"name"`
	Version        string                     `json:"version"`
	DisabledReason *string                    `json:"disabledReason"`
}

type nativeMCPDefinition struct {
	Enabled  bool
	Explicit bool
}

type nativeMCPLayer struct {
	Identity string
	Active   bool
	Entries  map[string]nativeMCPDefinition
}

func decodeNativeInventory(data []byte, cwd string) (Inventory, error) {
	var response nativeCurationResponse
	if json.Unmarshal(data, &response) != nil || response.Plugins == nil || response.Plugins.Marketplaces == nil || response.Skills == nil || response.Skills.Data == nil || response.Config == nil || response.Config.Config == nil || response.Config.Layers == nil || response.Config.Origins == nil {
		return Inventory{}, fmt.Errorf("incomplete native capability inventory")
	}
	inventory := Inventory{Capabilities: []Capability{}}
	seen := make(map[string]bool)
	add := func(capability Capability) error {
		key := string(capability.Kind) + ":" + capability.ID
		if seen[key] {
			return fmt.Errorf("duplicate %s inventory ID %q", capability.Kind, capability.ID)
		}
		seen[key] = true
		inventory.Capabilities = append(inventory.Capabilities, capability)
		return nil
	}
	issue := func(code string, kind Kind, id string) {
		inventory.Issues = append(inventory.Issues, InventoryIssue{Code: code, Kind: kind, ID: id})
	}
	count, err := nativeMetadataErrorCount(response.Plugins.LoadErrors, true)
	if err != nil {
		return Inventory{}, err
	}
	if count > 0 {
		issue("plugin_load_error", PluginKind, "")
		issue("inventory_incomplete", PluginKind, "")
	}
	plugins := make(map[string]Capability)
	mcpOwners := make(map[string][]string)
	for _, marketplace := range *response.Plugins.Marketplaces {
		if marketplace.Plugins == nil {
			return Inventory{}, fmt.Errorf("incomplete native plugin metadata")
		}
		for _, plugin := range *marketplace.Plugins {
			if !inventoryIdentifier(plugin.ID) || !inventoryIdentifier(plugin.Name) || plugin.Installed == nil || plugin.Enabled == nil {
				return Inventory{}, fmt.Errorf("invalid native plugin metadata")
			}
			available := *plugin.Installed && *plugin.Enabled
			if plugin.Availability != nil && *plugin.Availability != "AVAILABLE" {
				available = false
			}
			if plugin.DisabledReason != nil && *plugin.DisabledReason != "" {
				available = false
			}
			capability := Capability{ID: plugin.ID, Kind: PluginKind, NativeID: plugin.ID, Name: plugin.Name, Enabled: *plugin.Enabled, Available: available}
			if *plugin.Installed {
				var known bool
				capability.MCPServers, known = nativePluginMCPServers(response.PluginDetails[plugin.ID], plugin)
				if !known {
					issue("plugin_mcp_ownership_unresolved", PluginKind, plugin.ID)
				}
				capability.AppIDs, known = nativePluginAppIDs(response.PluginDetails[plugin.ID], plugin)
				if !known {
					issue("plugin_app_ownership_unresolved", PluginKind, plugin.ID)
				}
				for _, server := range capability.MCPServers {
					mcpOwners[server] = append(mcpOwners[server], plugin.ID)
				}
			}
			if plugin.Interface != nil {
				if plugin.Interface.DisplayName != "" {
					capability.Name = plugin.Interface.DisplayName
				}
				capability.Description = plugin.Interface.ShortDescription
			}
			if err := add(capability); err != nil {
				return Inventory{}, err
			}
			plugins[plugin.ID] = capability
			if !*plugin.Installed {
				issue("plugin_not_installed", PluginKind, plugin.ID)
			}
		}
	}
	active, err := nativeMCPEntries(response.Config.Config)
	if err != nil {
		return Inventory{}, err
	}
	all := make(map[string]bool)
	for name := range active {
		all[name] = true
	}
	var layers []nativeMCPLayer
	for _, layer := range *response.Config.Layers {
		if layer.Config == nil {
			return Inventory{}, fmt.Errorf("incomplete native config layer")
		}
		entries, err := nativeMCPEntries(layer.Config)
		if err != nil {
			return Inventory{}, err
		}
		for name := range entries {
			all[name] = true
		}
		layers = append(layers, nativeMCPLayer{Identity: nativeConfigLayerIdentity(layer.Name, layer.Version), Active: layer.DisabledReason == nil || *layer.DisabledReason == "", Entries: entries})
	}
	for key := range response.Config.Origins {
		if !strings.HasPrefix(key, "mcp_servers.") {
			continue
		}
		name, ok := nativeMCPOriginName(key, all)
		if !ok {
			issue("mcp_origin_unresolved", MCPKind, "")
			issue("inventory_incomplete", MCPKind, "")
			continue
		}
		all[name] = true
	}
	// Some native versions omit untyped mcp_servers from the merged Config.
	// Recover its active definitions from raw layers, using enabled origins to
	// resolve conflicting flags rather than guessing layer ordering.
	if _, provided := response.Config.Config["mcp_servers"]; !provided {
		fallback, unresolved := nativeMCPFromLayers(layers, response.Config.Origins)
		for name, definition := range active {
			if previous, ok := fallback[name]; ok && !definition.Explicit {
				definition = previous
			}
			fallback[name] = definition
		}
		active = fallback
		for _, name := range unresolved {
			issue("mcp_enabled_unresolved", MCPKind, name)
			issue("inventory_incomplete", MCPKind, name)
		}
	}
	for name := range all {
		definition, present := active[name]
		if err := add(Capability{ID: name, Kind: MCPKind, NativeID: name, Name: name, Enabled: present && definition.Enabled, Available: present}); err != nil {
			return Inventory{}, err
		}
		if !present {
			issue("mcp_config_inactive", MCPKind, name)
		}
	}
	skillIDCounts := make(map[string]int)
	for _, entry := range *response.Skills.Data {
		if entry.Skills == nil {
			return Inventory{}, fmt.Errorf("incomplete native skill inventory")
		}
		for _, skill := range *entry.Skills {
			id, err := StableSkillID(skill.Scope, skill.Name, skill.PluginID)
			if err != nil {
				return Inventory{}, err
			}
			skillIDCounts[id]++
		}
	}
	matchedCwd := false
	for _, entry := range *response.Skills.Data {
		if entry.Cwd != cwd {
			return Inventory{}, fmt.Errorf("native skill inventory project mismatch")
		}
		matchedCwd = true
		if entry.Skills == nil {
			return Inventory{}, fmt.Errorf("incomplete native skill inventory")
		}
		count, err := nativeMetadataErrorCount(entry.Errors, false)
		if err != nil {
			return Inventory{}, err
		}
		if count > 0 {
			issue("skill_load_error", SkillKind, "")
			issue("inventory_incomplete", SkillKind, "")
		}
		for _, skill := range *entry.Skills {
			id, err := StableSkillID(skill.Scope, skill.Name, skill.PluginID)
			if err != nil || skill.Enabled == nil || skill.Description == nil || !filepath.IsAbs(skill.Path) || strings.ContainsRune(skill.Path, '\x00') {
				return Inventory{}, fmt.Errorf("invalid native skill metadata")
			}
			if skill.PluginID == "" {
				qualified, qualificationErr := QualifiedSkillID(skill.Scope, skill.Name, "", skill.Path, cwd)
				if qualificationErr == nil {
					id = qualified
				} else if skillIDCounts[id] > 1 {
					return Inventory{}, fmt.Errorf("duplicate skill inventory ID has no portable source")
				}
			}
			available := *skill.Enabled || skill.Scope == "user" || skill.Scope == "repo"
			capability := Capability{ID: id, Kind: SkillKind, NativeID: skill.Name, Name: skill.Name, Description: *skill.Description, Path: skill.Path, PluginID: skill.PluginID, Enabled: *skill.Enabled, Available: available}
			if skill.ShortDescription != "" {
				capability.Description = skill.ShortDescription
			}
			if skill.Interface != nil {
				if skill.Interface.DisplayName != "" {
					capability.Name = skill.Interface.DisplayName
				}
				if skill.Interface.ShortDescription != "" {
					capability.Description = skill.Interface.ShortDescription
				}
			}
			dependencies := make(map[string]bool)
			if skill.PluginID != "" {
				dependencies["plugin:"+skill.PluginID] = true
				owner, ok := plugins[skill.PluginID]
				if !ok {
					capability.Available = false
					issue("skill_plugin_unresolved", SkillKind, id)
				} else {
					capability.Available = capability.Available && owner.Available && owner.Enabled
				}
			}
			if skill.Dependencies != nil {
				if skill.Dependencies.Tools == nil {
					return Inventory{}, fmt.Errorf("incomplete native skill dependency metadata")
				}
				for _, dependency := range *skill.Dependencies.Tools {
					if dependency.Type == nil || dependency.Value == nil {
						return Inventory{}, fmt.Errorf("invalid native skill dependency metadata")
					}
					resolved := false
					if *dependency.Type == "mcp" {
						owners := mcpOwners[*dependency.Value]
						standalone := all[*dependency.Value]
						switch {
						case len(owners) > 1 || standalone && len(owners) > 0:
							issue("skill_dependency_ambiguous", SkillKind, id)
							resolved = true
						case standalone:
							dependencies["mcp:"+*dependency.Value] = true
							resolved = true
						case len(owners) == 1:
							dependencies["plugin:"+owners[0]] = true
							resolved = true
						}
					}
					if *dependency.Type == "plugin" {
						if _, ok := plugins[*dependency.Value]; ok {
							dependencies["plugin:"+*dependency.Value] = true
							resolved = true
						}
					}
					if !resolved {
						issue("skill_dependency_unresolved", SkillKind, id)
					}
				}
			}
			for dependency := range dependencies {
				capability.Dependencies = append(capability.Dependencies, dependency)
			}
			sort.Strings(capability.Dependencies)
			if err := add(capability); err != nil {
				return Inventory{}, err
			}
		}
	}
	if !matchedCwd {
		return Inventory{}, fmt.Errorf("native skill inventory project missing")
	}
	sort.Slice(inventory.Capabilities, func(i, j int) bool {
		left, right := inventory.Capabilities[i], inventory.Capabilities[j]
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.ID < right.ID
	})
	sort.Slice(inventory.Issues, func(i, j int) bool {
		left, right := inventory.Issues[i], inventory.Issues[j]
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.ID != right.ID {
			return left.ID < right.ID
		}
		return left.Code < right.Code
	})
	issues := inventory.Issues[:0]
	for _, current := range inventory.Issues {
		if len(issues) == 0 || issues[len(issues)-1] != current {
			issues = append(issues, current)
		}
	}
	inventory.Issues = issues
	return inventory, nil
}

func nativeMetadataErrorCount(raw json.RawMessage, optional bool) (int, error) {
	if len(raw) == 0 && optional {
		return 0, nil
	}
	var entries []json.RawMessage
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &entries) != nil {
		return 0, fmt.Errorf("invalid native inventory error metadata")
	}
	return len(entries), nil
}

func nativeMCPEntries(config map[string]json.RawMessage) (map[string]nativeMCPDefinition, error) {
	entries := make(map[string]nativeMCPDefinition)
	if raw, ok := config["mcp_servers"]; ok {
		var servers map[string]json.RawMessage
		if json.Unmarshal(raw, &servers) != nil || servers == nil {
			return nil, fmt.Errorf("invalid native MCP config metadata")
		}
		for name, raw := range servers {
			if !inventoryIdentifier(name) {
				return nil, fmt.Errorf("invalid native MCP name")
			}
			var server struct {
				Enabled json.RawMessage `json:"enabled"`
			}
			if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &server) != nil {
				return nil, fmt.Errorf("invalid native MCP config metadata")
			}
			enabled, err := nativeMCPEnabled(server.Enabled)
			if err != nil {
				return nil, err
			}
			entries[name] = nativeMCPDefinition{Enabled: enabled, Explicit: len(server.Enabled) > 0}
		}
	}
	for key, raw := range config {
		if !strings.HasPrefix(key, "mcp_servers.") {
			continue
		}
		segments, ok := nativeConfigKeySegments(key)
		if !ok || len(segments) < 2 || !inventoryIdentifier(segments[1]) {
			return nil, fmt.Errorf("invalid native MCP config key")
		}
		name := segments[1]
		if _, ok := entries[name]; !ok {
			entries[name] = nativeMCPDefinition{Enabled: true}
		}
		if len(segments) == 3 && segments[2] == "enabled" {
			enabled, err := nativeMCPEnabled(raw)
			if err != nil {
				return nil, err
			}
			entries[name] = nativeMCPDefinition{Enabled: enabled, Explicit: true}
		}
	}
	return entries, nil
}

func nativeConfigLayerIdentity(name json.RawMessage, version string) string {
	if len(name) == 0 || version == "" {
		return ""
	}
	var source map[string]any
	if json.Unmarshal(name, &source) != nil || source == nil {
		return ""
	}
	canonical, err := json.Marshal(source)
	if err != nil {
		return ""
	}
	return version + "\x00" + string(canonical)
}

func nativeMCPFromLayers(layers []nativeMCPLayer, origins map[string]json.RawMessage) (map[string]nativeMCPDefinition, []string) {
	definitions := make(map[string][]nativeMCPDefinition)
	for _, layer := range layers {
		if layer.Active {
			for name, entry := range layer.Entries {
				definitions[name] = append(definitions[name], entry)
			}
		}
	}
	effective := make(map[string]nativeMCPDefinition)
	var unresolved []string
	for name, entries := range definitions {
		values := make(map[bool]bool)
		for _, entry := range entries {
			if entry.Explicit {
				values[entry.Enabled] = true
			}
		}
		if len(values) < 2 {
			enabled := true
			for value := range values {
				enabled = value
			}
			effective[name] = nativeMCPDefinition{Enabled: enabled, Explicit: len(values) > 0}
			continue
		}
		originValues := make(map[bool]bool)
		for key, raw := range origins {
			segments, ok := nativeConfigKeySegments(key)
			if !(ok && len(segments) == 3 && segments[0] == "mcp_servers" && segments[1] == name && segments[2] == "enabled") && key != "mcp_servers."+name+".enabled" {
				continue
			}
			var origin struct {
				Name    json.RawMessage `json:"name"`
				Version string          `json:"version"`
			}
			if json.Unmarshal(raw, &origin) != nil {
				continue
			}
			identity := nativeConfigLayerIdentity(origin.Name, origin.Version)
			if identity == "" {
				continue
			}
			for _, layer := range layers {
				if layer.Active && layer.Identity == identity {
					if entry, ok := layer.Entries[name]; ok && entry.Explicit {
						originValues[entry.Enabled] = true
					}
				}
			}
		}
		if len(originValues) == 1 {
			for value := range originValues {
				effective[name] = nativeMCPDefinition{Enabled: value, Explicit: true}
			}
		} else {
			unresolved = append(unresolved, name)
		}
	}
	return effective, unresolved
}

func nativeMCPEnabled(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 {
		return true, nil
	}
	var enabled *bool
	if json.Unmarshal(raw, &enabled) != nil || enabled == nil {
		return false, fmt.Errorf("invalid native MCP enabled metadata")
	}
	return *enabled, nil
}

func nativeMCPOriginName(key string, known map[string]bool) (string, bool) {
	for name := range known {
		prefix := "mcp_servers." + name
		if key == prefix || strings.HasPrefix(key, prefix+".") {
			return name, true
		}
	}
	segments, ok := nativeConfigKeySegments(key)
	if !ok || len(segments) < 2 || !inventoryIdentifier(segments[1]) {
		return "", false
	}
	if len(segments) > 2 {
		switch segments[2] {
		case "command", "args", "enabled", "env", "env_vars", "cwd", "url", "http_headers", "env_http_headers", "bearer_token_env_var", "environment_id", "startup_timeout_sec", "tool_timeout_sec", "tools", "required", "enabled_tools", "disabled_tools":
		default:
			return "", false
		}
	}
	return segments[1], true
}

func nativeConfigKeySegments(key string) ([]string, bool) {
	var segments []string
	for key != "" {
		var value string
		if key[0] == '"' || key[0] == '\'' {
			quote := key[0]
			end := 1
			for end < len(key) {
				if quote == '"' && key[end] == '\\' {
					end += 2
					continue
				}
				if key[end] == quote {
					break
				}
				end++
			}
			if end >= len(key) {
				return nil, false
			}
			if quote == '"' {
				decoded, err := strconv.Unquote(key[:end+1])
				if err != nil {
					return nil, false
				}
				value = decoded
			} else {
				value = key[1:end]
			}
			key = key[end+1:]
		} else {
			index := strings.IndexByte(key, '.')
			if index < 0 {
				value, key = key, ""
			} else {
				value, key = key[:index], key[index:]
			}
		}
		if value == "" {
			return nil, false
		}
		segments = append(segments, value)
		if key != "" {
			if key[0] != '.' || len(key) == 1 {
				return nil, false
			}
			key = key[1:]
		}
	}
	return segments, len(segments) > 0
}

type nativeCurationOutput struct {
	data     bytes.Buffer
	exceeded bool
}

func (output *nativeCurationOutput) Bytes() []byte { return output.data.Bytes() }

func (output *nativeCurationOutput) Write(data []byte) (int, error) {
	if output.data.Len()+len(data) > nativeCurationLimit {
		output.exceeded = true
		return 0, fmt.Errorf("native inventory output exceeds limit")
	}
	return output.data.Write(data)
}

type nativeCurationRequest struct {
	method, name, pluginID string
	params                 any
}

// Plugin summaries identify the native read locator. Ambiguous or absent
// locators are left without details so decoding reports that plugin's gap.
// The protocol keeps its existing 128-message and 8 MiB bounds; at most 124
// detail reads can follow initialization and the three base inventory reads.
func nativePluginDetailRequests(raw json.RawMessage) []nativeCurationRequest {
	var plugins nativePluginInventory
	if json.Unmarshal(raw, &plugins) != nil || plugins.Marketplaces == nil {
		return nil
	}
	type candidate struct {
		id, locator string
		params      map[string]string
	}
	var candidates []candidate
	locators := make(map[string]int)
	ids := make(map[string]int)
	for _, marketplace := range *plugins.Marketplaces {
		if marketplace.Plugins == nil {
			continue
		}
		for _, plugin := range *marketplace.Plugins {
			if plugin.Installed == nil || !*plugin.Installed || !inventoryIdentifier(plugin.ID) || !inventoryIdentifier(plugin.Name) {
				continue
			}
			params := map[string]string{"pluginName": plugin.Name}
			if marketplace.Path != nil {
				path := *marketplace.Path
				if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, '\x00') {
					continue
				}
				params["marketplacePath"] = path
			} else {
				if !inventoryIdentifier(marketplace.Name) {
					continue
				}
				if plugin.RemotePluginID != "" {
					if !inventoryIdentifier(plugin.RemotePluginID) {
						continue
					}
					params["pluginName"] = plugin.RemotePluginID
				}
				params["remoteMarketplaceName"] = marketplace.Name
			}
			encoded, _ := json.Marshal(params)
			locator := string(encoded)
			locators[locator]++
			ids[plugin.ID]++
			candidates = append(candidates, candidate{id: plugin.ID, locator: locator, params: params})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].id < candidates[j].id })
	requests := make([]nativeCurationRequest, 0, min(len(candidates), 124))
	for _, candidate := range candidates {
		if locators[candidate.locator] == 1 && ids[candidate.id] == 1 {
			requests = append(requests, nativeCurationRequest{method: "plugin/read", pluginID: candidate.id, params: candidate.params})
			if len(requests) == 124 {
				break
			}
		}
	}
	return requests
}

func runNativeCurationInventory(ctx context.Context, executable, cwd string, output io.Writer) error {
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return fmt.Errorf("native inventory executable must be absolute and clean")
	}
	command := exec.CommandContext(ctx, executable, "app-server", "--stdio", "--strict-config")
	command.Dir, command.Stderr, command.WaitDelay = cwd, io.Discard, time.Second
	stdin, err := command.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	defer stdout.Close()
	if err := command.Start(); err != nil {
		return err
	}
	waited := false
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	cancellationDone := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		defer close(cancellationDone)
		defer func() { _ = recover() }()
		_ = stdin.Close()
		_ = stdout.Close()
	})
	defer func() {
		if !stopCancellation() {
			<-cancellationDone
		}
	}()
	encoder := json.NewEncoder(stdin)
	if err := encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "cq_capability_inventory", "version": "0.0.0"}, "capabilities": map[string]any{"experimentalApi": true}}}); err != nil {
		return err
	}
	requests := []nativeCurationRequest{
		{method: "plugin/installed", name: "plugins", params: map[string]any{"cwds": []string{cwd}}},
		{method: "skills/list", name: "skills", params: map[string]any{"cwds": []string{cwd}, "forceReload": true}},
		{method: "config/read", name: "config", params: map[string]any{"cwd": cwd, "includeLayers": true}},
	}
	phase, messages, totalBytes := 1, 0, 0
	complete := false
	responses := make(map[string]json.RawMessage)
	details := make(map[string]json.RawMessage)
	nextDetail := 3
	pendingDetails := make(map[string]nativeCurationRequest)
	// Remote details are independent metadata reads. Keep a fixed window so
	// installed catalogues fit the existing deadline without an unbounded fanout.
	sendDetails := func() error {
		for nextDetail < len(requests) && len(pendingDetails) < 8 {
			request := requests[nextDetail]
			id := nextDetail + 2
			pendingDetails[strconv.Itoa(id)] = request
			nextDetail++
			if err := encoder.Encode(map[string]any{"id": id, "method": request.method, "params": request.params}); err != nil {
				return err
			}
		}
		return nil
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		messages++
		totalBytes += len(line) + 1
		if messages > 128 || totalBytes > nativeCurationLimit {
			return fmt.Errorf("native inventory protocol exceeds limit")
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(line, &message) != nil {
			return fmt.Errorf("invalid native inventory protocol JSON")
		}
		if complete {
			continue
		}
		if phase == 5 {
			key := string(message.ID)
			request, ok := pendingDetails[key]
			if !ok {
				continue
			}
			delete(pendingDetails, key)
			failed := len(message.Error) > 0 && !bytes.Equal(bytes.TrimSpace(message.Error), []byte("null"))
			result := bytes.TrimSpace(message.Result)
			if !failed && len(result) > 0 && result[0] == '{' {
				details[request.pluginID] = append(json.RawMessage(nil), message.Result...)
			}
			if err := sendDetails(); err != nil {
				return err
			}
			if len(pendingDetails) == 0 {
				complete = true
				if err := stdin.Close(); err != nil {
					return err
				}
			}
			continue
		}
		if string(message.ID) != strconv.Itoa(phase) {
			continue
		}
		failed := len(message.Error) > 0 && !bytes.Equal(bytes.TrimSpace(message.Error), []byte("null"))
		if failed {
			return fmt.Errorf("native inventory request rejected")
		}
		result := bytes.TrimSpace(message.Result)
		valid := len(result) > 0 && result[0] == '{'
		if !valid {
			return fmt.Errorf("invalid native inventory response")
		}
		if phase == 1 {
			if err := encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
				return err
			}
		} else {
			request := requests[phase-2]
			responses[request.name] = append(json.RawMessage(nil), message.Result...)
			if request.name == "plugins" {
				requests = append(requests, nativePluginDetailRequests(message.Result)...)
			}
		}
		phase++
		if phase == 5 {
			if err := sendDetails(); err != nil {
				return err
			}
			if len(pendingDetails) == 0 {
				complete = true
				if err := stdin.Close(); err != nil {
					return err
				}
			}
			continue
		}
		request := requests[phase-2]
		if err := encoder.Encode(map[string]any{"id": phase, "method": request.method, "params": request.params}); err != nil {
			return err
		}
	}
	if scanner.Err() != nil || !complete {
		return fmt.Errorf("incomplete native inventory protocol")
	}
	err = command.Wait()
	waited = true
	if err != nil {
		return err
	}
	responses["pluginDetails"], err = json.Marshal(details)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(responses)
}
