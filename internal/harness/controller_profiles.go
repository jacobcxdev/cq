package harness

import (
	"context"
	"fmt"
	"slices"
	"sort"
)

func (controller *Controller) selectPlugins(ctx context.Context, snapshot Snapshot, baseline []string, effective Effective) ([]string, []string, []string, error) {
	if controller.Inventory == nil {
		return nil, nil, nil, fmt.Errorf("native capability inventory unavailable")
	}
	inventory, err := controller.Inventory.Snapshot(ctx, snapshot.Cwd)
	if err != nil {
		return nil, nil, nil, err
	}
	nativeID := func(capability Capability) string {
		if capability.NativeID != "" {
			return capability.NativeID
		}
		return capability.ID
	}
	selected := SelectCapabilities(effective, inventory, func(capability Capability) bool {
		if capability.Kind == PluginKind {
			return capability.Enabled && !slices.Contains(baseline, nativeID(capability))
		}
		if capability.Kind == SkillKind && capability.PluginID != "" {
			for _, owner := range inventory.Capabilities {
				if owner.Kind == PluginKind && owner.ID == capability.PluginID {
					return capability.Enabled && owner.Enabled && !slices.Contains(baseline, nativeID(owner))
				}
			}
		}
		return capability.Enabled
	})
	disabled := make(map[string]bool, len(baseline))
	for _, id := range baseline {
		disabled[id] = true
	}
	var unsupported []string
	for _, capability := range inventory.Capabilities {
		wanted := selected.Wants(capability.Kind, capability.ID)
		if capability.Kind == PluginKind {
			if !wanted {
				disabled[nativeID(capability)] = true
			} else if !capability.Enabled {
				unsupported = append(unsupported, "plugin:"+capability.ID+":native-disabled")
			} else {
				delete(disabled, nativeID(capability))
			}
			continue
		}
		actual := capability.Enabled
		if capability.Kind == SkillKind && capability.PluginID != "" && !selected.Wants(PluginKind, capability.PluginID) {
			actual = false
		}
		if wanted != actual {
			unsupported = append(unsupported, string(capability.Kind)+":"+capability.ID)
		}
	}
	desired := make([]string, 0, len(disabled))
	for id := range disabled {
		desired = append(desired, id)
	}
	sort.Strings(desired)
	sort.Strings(unsupported)
	return desired, slices.Compact(unsupported), selected.Issues, nil
}
