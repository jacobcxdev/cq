package dots

import (
	"context"
	_ "embed"
	"sort"
	"strings"
)

// The vendor disabled the Dots Hub in this particular renderer archive.
// Restrict the renderer-only override to the qualified app bytes.
const dotHubArchive29803 = "b9e516de7c5ef608bf83a6313c815f5a7ef8819e4f08aef04c69744ecfc59cf8"

//go:embed native_dot_hub.js
var nativeDotHubJS string

func nativeDotHubSupported(s *Settings) bool {
	return s.IntegrationMode == "native" && s.AppBuild == "29803" &&
		s.ASARSHA256 == dotHubArchive29803 && len(s.Dots) > 1 &&
		s.NativeBindings != nil && s.NativeBindings.Rspack != nil
}

func enableNativeDotHub(ctx context.Context, s *Settings, e NativeEvaluator) bool {
	if !nativeDotHubSupported(s) {
		return false
	}
	profiles := make([]string, 0, len(s.Dots))
	for _, dot := range s.Dots {
		profiles = append(profiles, dot.ProfileID)
	}
	sort.Strings(profiles)
	expression := strings.NewReplacer(
		"CQ_DOT_PROFILE_IDS", nativeJSON(profiles),
		"CQ_SYSTEM_ACCOUNT", nativeJSON(s.SystemAccountID),
	).Replace(nativeDotHubJS)
	values, err := e.EvaluateAll(ctx, nativeAsset(expression, s))
	if err != nil {
		return false
	}
	for _, value := range values {
		if result, ok := value.(map[string]any); ok && result["enabled"] == true {
			return true
		}
	}
	return false
}
