package dots

import (
	"context"
	_ "embed"
	"runtime"
	"sort"
	"strings"
)

// Restrict each renderer override to the qualified app bytes.
const (
	dotHubArchive29803 = "b9e516de7c5ef608bf83a6313c815f5a7ef8819e4f08aef04c69744ecfc59cf8"
	dotHubArchive30607 = "e60f32facfaebf94310f18b95e4883799506541356204b9589ac373e192897e6"
	dotHubArchive30866 = "c6488597167abbbeff5a2422cdbe63f7466143b9770a224c9bc6ac2168631f13"
	dotHubArchive5083  = "7b5175d7932e26f8fca91fb0b25bc85e424304ce368c8ce21a68b1be2c174b28"
)

//go:embed native_dot_hub.js
var nativeDotHubJS string

func teamDotHubSupported(s *Settings) bool {
	return (s.AppBuild == "30607" && s.ASARSHA256 == dotHubArchive30607) ||
		(s.AppBuild == "30866" && s.ASARSHA256 == dotHubArchive30866) ||
		(runtime.GOOS == "windows" && s.AppVersion == "26.1120.5083.0" &&
			s.AppBuild == "5083" && s.ModuleAsset == "348410.477a16d4d9.js" && s.ASARSHA256 == dotHubArchive5083)
}

func nativeDotHubSupported(s *Settings) bool {
	return s.IntegrationMode == "native" && len(s.Dots) > 1 &&
		s.NativeBindings != nil && s.NativeBindings.Rspack != nil &&
		(s.AppBuild == "29803" && s.ASARSHA256 == dotHubArchive29803 ||
			teamDotHubSupported(s))
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
		"CQ_TEAM_DOT_HUB", nativeJSON(teamDotHubSupported(s)),
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
