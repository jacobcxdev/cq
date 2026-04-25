package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type modelsDeps struct {
	FS       fsutil.FileSystem
	HomeDir  string
	Env      func(string) string
	Stdout   io.Writer
	Stderr   io.Writer
	Natives  func() []modelregistry.Entry
	Refresh  func() error
	UseProxy func() bool
}

func runModelsCommand(args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	deps := modelsDeps{
		FS:      fsutil.OSFileSystem{},
		HomeDir: home,
		Env:     os.Getenv,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Natives: func() []modelregistry.Entry { return nil },
		Refresh: runModelsRefresh,
		UseProxy: func() bool {
			cfg, err := proxy.LoadConfig()
			if err != nil {
				return false
			}
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", cfg.Port))
			if err != nil {
				return false
			}
			defer resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		},
	}
	return runModels(args, deps)
}

// registryRefreshStrategy orchestrates cq models refresh. TryProxy returns
// (handled=true) when the running proxy accepted the refresh; (handled=false,
// err=nil) when the proxy is unreachable or does not support the endpoint, in
// which case LocalRefresh is invoked; (handled=false, err!=nil) when the proxy
// reported a real failure (auth, 5xx) that should surface rather than fall
// back silently.
type registryRefreshStrategy struct {
	TryProxy     func() (bool, error)
	LocalRefresh func() error
}

func runRegistryRefresh(s registryRefreshStrategy) error {
	handled, err := s.TryProxy()
	if err != nil {
		return err
	}
	if handled {
		return nil
	}
	return s.LocalRefresh()
}

// attemptProxyRegistryRefresh posts to the local proxy's /v1/registry/refresh
// endpoint. Returns handled=true on 2xx, (false, nil) on 404 or transport
// failure (so the caller can fall back to a local refresh), and (false, err)
// on any other non-2xx so auth/5xx errors are not silently swallowed.
func attemptProxyRegistryRefresh(ctx context.Context, client httputil.Doer, port int, token string) (bool, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/registry/refresh", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return false, fmt.Errorf("build proxy refresh request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		// If the caller's context was cancelled or timed out, surface that error
		// rather than silently falling back to a local refresh.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, fmt.Errorf("proxy registry refresh: %w", ctxErr)
		}
		// Proxy not reachable (connection refused, network timeout). Fall back locally.
		return false, nil
	}
	defer resp.Body.Close()
	body, readErr := httputil.ReadBody(resp.Body)
	if readErr != nil {
		body = []byte(readErr.Error())
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, nil
	case resp.StatusCode == http.StatusNotFound:
		// Older proxy that does not expose the registry endpoint.
		return false, nil
	default:
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			return false, fmt.Errorf("proxy registry refresh: HTTP %d", resp.StatusCode)
		}
		return false, fmt.Errorf("proxy registry refresh: HTTP %d: %s", resp.StatusCode, msg)
	}
}

func runModelsRefresh() error {
	deps := normaliseModelsDeps(modelsDeps{})
	if err := runRegistryRefresh(registryRefreshStrategy{
		TryProxy:     defaultTryProxyRegistryRefresh,
		LocalRefresh: defaultLocalRegistryRefresh,
	}); err != nil {
		return err
	}
	return pruneOverlayModels(deps)
}

func runModelsRefreshWithDeps(deps modelsDeps) error {
	if err := deps.Refresh(); err != nil {
		return err
	}
	return pruneOverlayModels(deps)
}

func defaultTryProxyRegistryRefresh() (bool, error) {
	cfg, err := proxy.LoadConfig()
	if err != nil {
		// No proxy config readable — treat as "no proxy to try" and let the
		// caller fall back to a local refresh.
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &http.Client{}
	return attemptProxyRegistryRefresh(ctx, client, cfg.Port, cfg.LocalToken)
}

func defaultLocalRegistryRefresh() error {
	cfg, err := proxy.LoadConfig()
	if err != nil {
		return fmt.Errorf("load proxy config: %w", err)
	}
	reg, err := buildLocalRegistry(cfg, version)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	diag, err := reg.Refresher.Refresh(ctx)
	if err != nil {
		return fmt.Errorf("local registry refresh: %w", err)
	}
	writeRegistrySourceDiagnostics(os.Stderr, diag)
	reg.Publish()
	total := 0
	for _, count := range diag.Counts {
		total += count
	}
	fmt.Fprintf(os.Stderr, "cq: registry: refreshed locally (%d entries)\n", total)
	return nil
}

func writeRegistrySourceDiagnostics(w io.Writer, diag modelregistry.RefreshDiagnostics) {
	providers := make([]string, 0, len(diag.SourceErrors))
	byName := make(map[string]error, len(diag.SourceErrors))
	for provider, err := range diag.SourceErrors {
		if err == nil {
			continue
		}
		name := string(provider)
		providers = append(providers, name)
		byName[name] = err
	}
	sort.Strings(providers)
	for _, provider := range providers {
		fmt.Fprintf(w, "cq: registry: %s source: %v\n", provider, byName[provider])
	}

	malformedProviders := make([]string, 0, len(diag.MalformedCounts))
	malformedByName := make(map[string]int, len(diag.MalformedCounts))
	for provider, count := range diag.MalformedCounts {
		if count <= 0 {
			continue
		}
		name := string(provider)
		malformedProviders = append(malformedProviders, name)
		malformedByName[name] = count
	}
	sort.Strings(malformedProviders)
	for _, provider := range malformedProviders {
		fmt.Fprintf(w, "cq: registry: %s source: skipped %d malformed model entries\n", provider, malformedByName[provider])
	}
}

func runModels(args []string, deps modelsDeps) error {
	deps = normaliseModelsDeps(deps)
	if len(args) == 0 {
		return fmt.Errorf("usage: cq models <list|refresh|overlay>")
	}

	switch args[0] {
	case "list":
		return runModelsList(args[1:], deps)
	case "refresh":
		if err := runModelsRefreshWithDeps(deps); err != nil {
			return fmt.Errorf("models refresh: %w", err)
		}
		fmt.Fprintln(deps.Stdout, "refreshed")
		return nil
	case "overlay":
		return runModelsOverlay(args[1:], deps)
	default:
		return fmt.Errorf("unknown models command: %s", args[0])
	}
}

func normaliseModelsDeps(deps modelsDeps) modelsDeps {
	if deps.FS == nil {
		deps.FS = fsutil.OSFileSystem{}
	}
	if deps.Env == nil {
		deps.Env = os.Getenv
	}
	if deps.Stdout == nil {
		deps.Stdout = io.Discard
	}
	if deps.Stderr == nil {
		deps.Stderr = io.Discard
	}
	if deps.Natives == nil {
		deps.Natives = func() []modelregistry.Entry { return nil }
	}
	if deps.Refresh == nil {
		deps.Refresh = func() error { return nil }
	}
	if deps.UseProxy == nil {
		deps.UseProxy = func() bool { return false }
	}
	return deps
}

func runModelsList(args []string, deps modelsDeps) error {
	jsonOut := false
	providerFilter := modelregistry.Provider("")
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--provider":
			if i+1 >= len(args) {
				return fmt.Errorf("models list: --provider requires a value")
			}
			provider, err := parseModelsProvider(args[i+1])
			if err != nil {
				return err
			}
			providerFilter = provider
			i++
		default:
			return fmt.Errorf("models list: unknown argument %s", args[i])
		}
	}

	overlays, err := loadModelsOverlayFile(deps)
	if err != nil {
		return err
	}
	natives, err := loadCachedNativeEntries(deps)
	if err != nil {
		return err
	}
	natives = removeNativesShadowedByOverlays(natives, overlays.Models)
	merged := modelregistry.Merge(natives, overlays.Models)
	entries := filterModelEntries(merged.Active, providerFilter)
	if jsonOut {
		enc := json.NewEncoder(deps.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}
	for _, entry := range entries {
		fmt.Fprintf(deps.Stdout, "%s\t%s\t%s\n", entry.Provider, entry.ID, entry.Source)
	}
	return nil
}

func runModelsOverlay(args []string, deps modelsDeps) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cq models overlay <add|remove|prune>")
	}
	switch args[0] {
	case "add":
		provider, id, cloneFrom, err := parseOverlayModelFlags(args[1:], true)
		if err != nil {
			return err
		}
		if err := addOverlayModel(deps, provider, id, cloneFrom); err != nil {
			return err
		}
		return deps.Refresh()
	case "remove":
		provider, id, _, err := parseOverlayModelFlags(args[1:], false)
		if err != nil {
			return err
		}
		if err := removeOverlayModel(deps, provider, id); err != nil {
			return err
		}
		return deps.Refresh()
	case "prune":
		if err := deps.Refresh(); err != nil {
			return err
		}
		return pruneOverlayModels(deps)
	default:
		return fmt.Errorf("unknown models overlay command: %s", args[0])
	}
}

func parseOverlayModelFlags(args []string, allowClone bool) (modelregistry.Provider, string, string, error) {
	provider := modelregistry.Provider("")
	id := ""
	cloneFrom := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--provider":
			if i+1 >= len(args) {
				return "", "", "", fmt.Errorf("--provider requires a value")
			}
			provider = modelregistry.Provider(args[i+1])
			i++
		case "--id":
			if i+1 >= len(args) {
				return "", "", "", fmt.Errorf("--id requires a value")
			}
			id = args[i+1]
			i++
		case "--clone-from":
			if !allowClone {
				return "", "", "", fmt.Errorf("--clone-from is not supported here")
			}
			if i+1 >= len(args) {
				return "", "", "", fmt.Errorf("--clone-from requires a value")
			}
			cloneFrom = args[i+1]
			i++
		default:
			return "", "", "", fmt.Errorf("unknown argument %s", args[i])
		}
	}
	entry := modelregistry.Entry{Provider: provider, ID: id, Source: modelregistry.SourceOverlay}
	if err := entry.Validate(); err != nil {
		return "", "", "", err
	}
	return provider, id, cloneFrom, nil
}

func addOverlayModel(deps modelsDeps, provider modelregistry.Provider, id, cloneFrom string) error {
	overlays, err := loadModelsOverlayFile(deps)
	if err != nil {
		return err
	}
	entry := modelregistry.Entry{Provider: provider, ID: id, Source: modelregistry.SourceOverlay, CloneFrom: cloneFrom}
	updated := false
	for i, existing := range overlays.Models {
		if existing.Provider == provider && existing.ID == id {
			overlays.Models[i] = entry
			updated = true
			break
		}
	}
	if !updated {
		overlays.Models = append(overlays.Models, entry)
	}
	return saveModelsOverlayFile(deps, overlays)
}

func removeOverlayModel(deps modelsDeps, provider modelregistry.Provider, id string) error {
	overlays, err := loadModelsOverlayFile(deps)
	if err != nil {
		return err
	}
	kept := make([]modelregistry.Entry, 0, len(overlays.Models))
	removed := false
	for _, entry := range overlays.Models {
		if entry.Provider == provider && entry.ID == id {
			removed = true
			continue
		}
		kept = append(kept, entry)
	}
	if !removed {
		return fmt.Errorf("models overlay remove: %s/%s not found", provider, id)
	}
	overlays.Models = kept
	return saveModelsOverlayFile(deps, overlays)
}

func pruneOverlayModels(deps modelsDeps) error {
	overlays, err := loadModelsOverlayFile(deps)
	if err != nil {
		return err
	}
	natives := deps.Natives()
	if natives == nil {
		var err error
		natives, err = loadCachedNativeEntries(deps)
		if err != nil {
			return err
		}
	}
	pruned, removed := modelregistry.PruneOverlays(overlays, natives)
	if err := saveModelsOverlayFile(deps, pruned); err != nil {
		return err
	}
	fmt.Fprintf(deps.Stdout, "pruned %d overlay models\n", len(removed))
	return nil
}

func loadModelsOverlayFile(deps modelsDeps) (modelregistry.OverlayFile, error) {
	path := modelregistry.OverlayPath(deps.Env, deps.HomeDir)
	overlays, err := modelregistry.LoadOverlays(deps.FS, path)
	if err != nil {
		return modelregistry.OverlayFile{}, err
	}
	if overlays.Version == 0 {
		overlays.Version = 1
	}
	return overlays, nil
}

func saveModelsOverlayFile(deps modelsDeps, overlays modelregistry.OverlayFile) error {
	path := modelregistry.OverlayPath(deps.Env, deps.HomeDir)
	return modelregistry.SaveOverlays(deps.FS, path, overlays)
}

// loadCachedNativeEntries reads the Codex models_cache.json and Claude Code
// model-capabilities.json files published by the registry refresh and returns
// their entries. Missing cache files are not an error; they just yield no
// entries for that provider. Malformed caches surface as errors.
func loadCachedNativeEntries(deps modelsDeps) ([]modelregistry.Entry, error) {
	var all []modelregistry.Entry

	codexHome := deps.Env("CODEX_HOME")
	if codexHome == "" && deps.HomeDir != "" {
		codexHome = filepath.Join(deps.HomeDir, ".codex")
	}
	if codexHome != "" {
		codex, err := modelregistry.LoadCodexEntriesFromCache(deps.FS, filepath.Join(codexHome, "models_cache.json"))
		if err != nil {
			return nil, err
		}
		all = append(all, codex...)
	}

	claudeHome := deps.Env("CLAUDE_CONFIG_DIR")
	if claudeHome == "" && deps.HomeDir != "" {
		claudeHome = filepath.Join(deps.HomeDir, ".claude")
	}
	if claudeHome != "" {
		claude, err := modelregistry.LoadClaudeEntriesFromCapabilities(deps.FS, filepath.Join(claudeHome, "cache", "model-capabilities.json"))
		if err != nil {
			return nil, err
		}
		all = append(all, claude...)
	}

	return all, nil
}

func removeNativesShadowedByOverlays(natives, overlays []modelregistry.Entry) []modelregistry.Entry {
	type key struct {
		provider modelregistry.Provider
		id       string
	}
	overlaySet := make(map[key]struct{}, len(overlays))
	for _, overlay := range overlays {
		overlaySet[key{overlay.Provider, overlay.ID}] = struct{}{}
	}

	out := make([]modelregistry.Entry, 0, len(natives))
	for _, native := range natives {
		if _, ok := overlaySet[key{native.Provider, native.ID}]; ok {
			continue
		}
		out = append(out, native)
	}
	return out
}

func filterModelEntries(entries []modelregistry.Entry, provider modelregistry.Provider) []modelregistry.Entry {
	out := make([]modelregistry.Entry, 0, len(entries))
	for _, entry := range entries {
		if provider != "" && entry.Provider != provider {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func parseModelsProvider(value string) (modelregistry.Provider, error) {
	provider := modelregistry.Provider(value)
	if provider != modelregistry.ProviderAnthropic && provider != modelregistry.ProviderCodex {
		return "", fmt.Errorf("models: unknown provider %q (want anthropic or codex)", value)
	}
	return provider, nil
}
