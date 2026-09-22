package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type ModelEntry struct {
	Provider         string   `json:"provider"`
	ID               string   `json:"id"`
	Aliases          []string `json:"aliases"`
	DisplayName      *string  `json:"display_name"`
	Description      *string  `json:"description"`
	ContextWindow    *int     `json:"context_window"`
	MaxContextWindow *int     `json:"max_context_window"`
	MaxOutputTokens  *int     `json:"max_output_tokens"`
	Visibility       *string  `json:"visibility"`
	Priority         *int     `json:"priority"`
	Source           string   `json:"source"`
	CloneFrom        *string  `json:"clone_from"`
	InferredFrom     *string  `json:"inferred_from"`
}
type ModelCloneSelectionResult struct {
	Mode     string  `json:"mode"`
	SourceID *string `json:"source_id"`
}
type ModelPublication = modelregistry.Publication
type ModelSourceResult = modelregistry.PublicationSource
type ModelPublicationTarget = modelregistry.PublicationTarget
type ModelIdentity = modelregistry.ModelIdentity

type v2ModelsDependencies struct {
	Resolve func() (modelsDeps, error)
	Refresh func(context.Context, modelsDeps) (ModelPublication, []ModelIdentity, error)
}

// T27 registers these five leaves in the executable dispatcher.
func lookupV2Models(path string) (cli.Handler, bool) {
	switch path {
	case "models list", "models refresh", "models overlay add", "models overlay remove", "models overlay prune":
		return handleV2Models, true
	}
	return nil, false
}
func handleV2Models(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	return handleV2ModelsWithDependencies(ctx, inv, session, v2ModelsDependencies{
		Resolve: func() (modelsDeps, error) {
			roots, err := userdirs.Default(userdirs.ConfigRoot)
			if err != nil {
				return modelsDeps{}, err
			}
			cwd, err := userdirs.WorkingDirectory()
			if err != nil {
				return modelsDeps{}, err
			}
			return modelsDeps{FS: fsutil.OSFileSystem{}, Roots: roots, CWD: cwd, Env: os.Getenv}, nil
		}, Refresh: refreshV2Models,
	})
}
func modelOption(inv cli.Invocation, name string) string {
	if v := inv.Options[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}
func handleV2ModelsWithDependencies(ctx context.Context, inv cli.Invocation, _ *cli.Session, deps v2ModelsDependencies) cli.Outcome {
	providerName, id, clone := modelOption(inv, "provider"), modelOption(inv, "id"), modelOption(inv, "clone-from")
	provider := modelregistry.Provider(providerName)
	if providerName == "claude" {
		provider = modelregistry.ProviderAnthropic
	}
	mutation := inv.Path == "models overlay add" || inv.Path == "models overlay remove"
	if (providerName != "" && providerName != "claude" && providerName != "codex") || (mutation && (providerName == "" || modelregistry.ValidateModelID(id) != nil)) || (inv.Supplied["clone-from"] && modelregistry.ValidateModelID(clone) != nil) {
		return modelsFailure(2, "cli_invalid_usage", fmt.Sprintf("Invalid arguments: invalid model identity. Run cq %s --help.", inv.Path))
	}
	if ctx.Err() != nil {
		return modelsInterrupted()
	}
	state, err := deps.Resolve()
	if err != nil {
		return modelsStoreFailure(err)
	}
	state = normaliseModelsDeps(state)
	overlays, err := loadModelsOverlayFile(state)
	if err != nil {
		return modelsStoreFailure(err)
	}
	for _, e := range overlays.Models {
		if e.Source != modelregistry.SourceOverlay {
			return modelsStoreFailure(errors.New("invalid overlay source"))
		}
	}
	filter := modelregistry.Provider("")
	if inv.Path == "models list" {
		filter = provider
	}
	natives, err := loadCachedNativeEntries(state, filter)
	if err != nil {
		return modelsStoreFailure(err)
	}
	if inv.Path == "models list" {
		entries := filterModelEntries(modelregistry.Merge(natives, overlays.Models).Active, provider)
		rows := make([]ModelEntry, 0, len(entries))
		for _, e := range entries {
			rows = append(rows, modelEntryDTO(e))
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Provider != rows[j].Provider {
				return rows[i].Provider < rows[j].Provider
			}
			return rows[i].ID < rows[j].ID
		})
		var human strings.Builder
		human.WriteString("MODEL\tPROVIDER\tSOURCE\n")
		for _, r := range rows {
			fmt.Fprintf(&human, "%s\t%s\t%s\n", r.ID, r.Provider, r.Source)
		}
		return modelsData(cli.Outcome{Human: human.String()}, struct {
			Models   []ModelEntry `json:"models"`
			Provider *string      `json:"provider"`
		}{rows, modelString(providerName)})
	}
	var saved ModelEntry
	selection := ModelCloneSelectionResult{Mode: "none"}
	switch inv.Path {
	case "models overlay add":
		entry := modelregistry.Entry{Provider: provider, ID: id, Source: modelregistry.SourceOverlay, CloneFrom: clone}
		source, found := modelregistry.InferClone(entry, natives)
		if clone != "" && !found {
			return modelsFailure(3, "models_clone_not_found", fmt.Sprintf("Native clone source %s/%s was not found; run cq models refresh and cq models list --provider %s.", providerName, clone, providerName))
		}
		if found {
			selection.Mode = "inferred"
			if clone != "" {
				selection.Mode = "explicit"
			}
			selection.SourceID = &source.ID
		}
		entry = modelregistry.InferOverlayMetadata(entry, natives)
		saved = modelEntryDTO(entry)
		replaced := false
		for i, e := range overlays.Models {
			if e.Provider == provider && e.ID == id {
				overlays.Models[i] = entry
				replaced = true
				break
			}
		}
		if !replaced {
			overlays.Models = append(overlays.Models, entry)
		}
		if err := modelregistry.ValidateSnapshot(modelregistry.Snapshot{Entries: modelregistry.Merge(natives, overlays.Models).Active}); err != nil {
			return modelsConflict(err)
		}
		if ctx.Err() != nil {
			return modelsInterrupted()
		}
		if err := saveModelsOverlayFile(state, overlays); err != nil {
			return modelsStoreFailure(err)
		}
	case "models overlay remove":
		kept := make([]modelregistry.Entry, 0, len(overlays.Models))
		removed := false
		for _, e := range overlays.Models {
			if e.Provider == provider && e.ID == id {
				removed = true
			} else {
				kept = append(kept, e)
			}
		}
		if !removed {
			return modelsFailure(3, "models_overlay_not_found", fmt.Sprintf("Overlay %s/%s was not found.", providerName, id))
		}
		overlays.Models = kept
		if ctx.Err() != nil {
			return modelsInterrupted()
		}
		if err := saveModelsOverlayFile(state, overlays); err != nil {
			return modelsStoreFailure(err)
		}
	case "models refresh", "models overlay prune":
	default:
		return modelsFailure(2, "cli_invalid_usage", "Invalid models command.")
	}
	publication, prunable, refreshErr := deps.Refresh(ctx, state)
	outcome := modelsPublicationOutcome(publication, refreshErr)
	if ctx.Err() == context.Canceled {
		outcome = modelsInterrupted()
	}
	switch inv.Path {
	case "models overlay add":
		source := "none"
		if selection.SourceID != nil {
			source = *selection.SourceID
		}
		outcome.Human = fmt.Sprintf("Overlay saved: %s/%s\nMetadata source: %s (%s)\nPublication: %s\n", providerName, id, source, selection.Mode, publication.Status)
		return modelsData(outcome, struct {
			Model        ModelEntry                `json:"model"`
			OverlaySaved bool                      `json:"overlay_saved"`
			Selection    ModelCloneSelectionResult `json:"selection"`
			Publication  ModelPublication          `json:"publication"`
		}{saved, true, selection, publication})
	case "models overlay remove":
		outcome.Human = fmt.Sprintf("Overlay removed: %s/%s\nPublication: %s\n", providerName, id, publication.Status)
		return modelsData(outcome, struct {
			Provider       string           `json:"provider"`
			ID             string           `json:"id"`
			OverlayRemoved bool             `json:"overlay_removed"`
			Publication    ModelPublication `json:"publication"`
		}{providerName, id, true, publication})
	case "models overlay prune":
		removed := []ModelIdentity{}
		if refreshErr == nil && ctx.Err() == nil {
			// Receipt evidence is specific to this refresh; never infer freshness from a cache.
			fresh := map[ModelIdentity]bool{}
			for _, e := range prunable {
				fresh[e] = true
			}
			kept := make([]modelregistry.Entry, 0, len(overlays.Models))
			for _, e := range overlays.Models {
				key := ModelIdentity{Provider: modelregistry.PublicProvider(e.Provider), ID: e.ID}
				if fresh[key] {
					removed = append(removed, key)
				} else {
					kept = append(kept, e)
				}
			}
			if len(removed) > 0 {
				overlays.Models = kept
				if err := saveModelsOverlayFile(state, overlays); err != nil {
					outcome = modelsStoreFailure(err)
					removed = []ModelIdentity{}
				}
			}
		}
		sort.Slice(removed, func(i, j int) bool {
			if removed[i].Provider != removed[j].Provider {
				return removed[i].Provider < removed[j].Provider
			}
			return removed[i].ID < removed[j].ID
		})
		outcome.Human = fmt.Sprintf("Pruned %d overlays.\n", len(removed))
		return modelsData(outcome, struct {
			Removed      []ModelIdentity  `json:"removed"`
			RemovedCount int              `json:"removed_count"`
			Publication  ModelPublication `json:"publication"`
		}{removed, len(removed), publication})
	default:
		var human strings.Builder
		fmt.Fprintf(&human, "Models refreshed: %d\n", publication.ActiveCount)
		for _, target := range publication.Targets {
			fmt.Fprintf(&human, "%s: %s\n", target.Target, target.Status)
		}
		outcome.Human = human.String()
		return modelsData(outcome, struct {
			Publication ModelPublication `json:"publication"`
		}{publication})
	}
}
func modelString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func modelPositive(n int) *int {
	if n <= 0 {
		return nil
	}
	return &n
}
func modelEntryDTO(e modelregistry.Entry) ModelEntry {
	aliases := append([]string{}, e.Aliases...)
	var priority *int
	if e.Priority != 0 || e.PriorityKnown {
		priority = &e.Priority
	}
	return ModelEntry{modelregistry.PublicProvider(e.Provider), e.ID, aliases, modelString(e.DisplayName), modelString(e.Description), modelPositive(e.ContextWindow), modelPositive(e.MaxContextWindow), modelPositive(e.MaxOutputTokens), modelString(e.Visibility), priority, string(e.Source), modelString(e.CloneFrom), modelString(e.InferredFrom)}
}
func modelsData(o cli.Outcome, data any) cli.Outcome { o.Data, _ = json.Marshal(data); return o }
func modelsFailure(exit int, code, message string) cli.Outcome {
	return cli.Outcome{ExitCode: exit, Errors: []cli.Diagnostic{{Code: code, Message: message}}}
}
func modelsInterrupted() cli.Outcome {
	return modelsFailure(130, "interrupted", "Operation interrupted; inspect state before retrying.")
}
func modelsStoreFailure(err error) cli.Outcome {
	var environment *userdirs.EnvironmentError
	if errors.As(err, &environment) {
		return modelsFailure(environment.ExitCode, environment.Code, environment.Error())
	}
	return modelsFailure(1, "models_store_failed", "Cannot access model registry state: read, validation or atomic write failed.")
}
func modelsConflict(err error) cli.Outcome {
	var conflict *modelregistry.ConflictError
	if errors.As(err, &conflict) {
		providers := make([]string, 0, len(conflict.Providers))
		for _, p := range conflict.Providers {
			providers = append(providers, modelregistry.PublicProvider(modelregistry.Provider(p)))
		}
		return modelsFailure(6, "models_conflict", fmt.Sprintf("Model ID %s conflicts across providers: %s.", conflict.ID, strings.Join(providers, ", ")))
	}
	return modelsFailure(6, "models_conflict", "Model identities conflict across providers.")
}
func modelsPublicationOutcome(p ModelPublication, err error) cli.Outcome {
	var store *modelregistry.OverlayError
	if errors.As(err, &store) {
		return modelsStoreFailure(err)
	}
	var conflict *modelregistry.ConflictError
	if errors.As(err, &conflict) {
		return modelsConflict(err)
	}
	if p.Status == "partial" {
		return modelsFailure(8, "models_refresh_partial", "Model refresh completed partially; inspect source and publication results.")
	}
	if p.Status != "complete" || err != nil {
		return modelsFailure(1, "models_refresh_failed", "Model refresh failed: refresh or publication failed.")
	}
	return cli.Outcome{}
}

func refreshV2Models(ctx context.Context, state modelsDeps) (ModelPublication, []ModelIdentity, error) {
	cfg, err := proxy.LoadExistingConfig()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return failedModelsPublication("local"), nil, err
	}
	return refreshV2ModelsFromConfig(ctx, state, cfg, &http.Client{}, buildCanonicalLocalRegistry)
}
func refreshV2ModelsFromConfig(ctx context.Context, state modelsDeps, cfg *proxy.Config, client httputil.Doer, build func(context.Context, *proxy.Config, modelsDeps) (*localRegistry, error)) (ModelPublication, []ModelIdentity, error) {
	if cfg != nil {
		phase, cancel := context.WithTimeout(ctx, 5*time.Second)
		p, prunable, handled, err := attemptV2ProxyRegistryRefresh(phase, client, cfg.Port, cfg.LocalToken)
		cancel()
		if handled || err != nil {
			return p, prunable, err
		}
	} else {
		cfg = &proxy.Config{ClaudeUpstream: proxy.DefaultUpstream, CodexUpstream: proxy.DefaultCodexUpstream}
	}
	if err := ctx.Err(); err != nil {
		return failedModelsPublication("local"), nil, err
	}
	reg, err := build(ctx, cfg, state)
	if err != nil {
		return failedModelsPublication("local"), nil, err
	}
	defer reg.Close()
	return refreshV2LocalRegistry(ctx, reg)
}
func buildCanonicalLocalRegistry(ctx context.Context, cfg *proxy.Config, state modelsDeps) (*localRegistry, error) {
	home := state.HomeDir
	var err error
	if home == "" {
		home, err = state.FS.UserHomeDir()
		if err != nil {
			return nil, err
		}
	}
	client := newHTTPClientFn(30*time.Second, version)
	durable, ok := state.FS.(fsutil.DurableFileSystem)
	if !ok {
		return nil, errors.New("durable filesystem unavailable")
	}
	control, err := codexprov.OpenDefaultCanonicalCredentialRefreshControl(ctx, durable, client)
	if err != nil {
		return nil, err
	}
	reg, err := buildLocalRegistryFromAuthority(cfg, localRegistryDependencies{FS: state.FS, HomeDir: home, CWD: state.CWD, Roots: state.Roots, HTTPClient: client, CodexClientVersion: defaultCodexClientVersion(), ClaudeToken: firstClaudeAccessToken, CredentialAuthority: newCanonicalCodexRegistryControlAdapter(control), Env: state.Env, Stderr: io.Discard, Close: control.Close})
	if err != nil {
		_ = control.Close()
	}
	return reg, err
}
func refreshV2LocalRegistry(ctx context.Context, reg *localRegistry) (ModelPublication, []ModelIdentity, error) {
	phase, cancel := context.WithTimeout(ctx, 30*time.Second)
	diag, err := reg.Refresher.Refresh(phase)
	cancel()
	var targets []ModelPublicationTarget
	if err == nil && ctx.Err() == nil {
		targets = reg.PublishReport(diag.Snapshot)
	}
	p := modelregistry.NewPublication(diag, len(diag.Snapshot.Entries), targets, "local")
	prunable := []ModelIdentity{}
	for _, e := range diag.Prunable {
		prunable = append(prunable, ModelIdentity{Provider: modelregistry.PublicProvider(e.Provider), ID: e.ID})
	}
	if err == nil {
		err = ctx.Err()
	}
	return p, prunable, err
}
func failedModelsPublication(via string) ModelPublication {
	return modelregistry.NewPublication(modelregistry.RefreshDiagnostics{}, 0, nil, via)
}

// Only a failed dial proves that no proxy accepted the operation. EOF, response
// errors and timeouts retain that authority; they never trigger another refresh.
func attemptV2ProxyRegistryRefresh(ctx context.Context, client httputil.Doer, port int, token string) (ModelPublication, []ModelIdentity, bool, error) {
	failed := failedModelsPublication("proxy")
	if err := ctx.Err(); err != nil {
		return failed, nil, true, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/registry/refresh", port), nil)
	if err != nil {
		return failed, nil, true, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if ctx.Err() != nil {
			return failed, nil, true, ctx.Err()
		}
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" && (errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)) {
			return failed, nil, false, nil
		}
		return failed, nil, true, errors.New("proxy registry refresh transport failed")
	}
	if resp == nil || resp.Body == nil {
		return failed, nil, true, errors.New("proxy registry refresh missing response")
	}
	defer resp.Body.Close()
	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return failed, nil, true, errors.New("proxy registry refresh unreadable response")
	}
	var receipt struct {
		Publication *ModelPublication            `json:"publication"`
		Prunable    *[]ModelIdentity             `json:"prunable"`
		ErrorCode   string                       `json:"error_code"`
		Conflict    *modelregistry.ConflictError `json:"conflict"`
	}
	if !validModelReceiptFields(body) || json.Unmarshal(body, &receipt) != nil || receipt.Publication == nil || receipt.Prunable == nil || !validModelReceipt(*receipt.Publication, *receipt.Prunable) {
		return failed, nil, true, errors.New("proxy registry refresh missing or invalid publication receipt")
	}
	p := *receipt.Publication
	if receipt.ErrorCode == "models_conflict" {
		if receipt.Conflict == nil || modelregistry.ValidateModelID(receipt.Conflict.ID) != nil || len(receipt.Conflict.Providers) != 2 || receipt.Conflict.Providers[0] != "anthropic" || receipt.Conflict.Providers[1] != "codex" {
			return failed, nil, true, errors.New("proxy registry conflict receipt invalid")
		}
		return p, nil, true, receipt.Conflict
	}
	if receipt.ErrorCode == "models_store_failed" {
		return p, nil, true, &modelregistry.OverlayError{Err: errors.New("proxy model store unavailable")}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return p, *receipt.Prunable, true, errors.New("proxy registry refresh failed")
	}
	return p, *receipt.Prunable, true, nil
}
func validModelReceipt(p ModelPublication, prunable []ModelIdentity) bool {
	if p.Via != "proxy" || p.ActiveCount < 0 || len(p.Sources) != 2 || p.Targets == nil {
		return false
	}
	for i, s := range p.Sources {
		if s.Provider != []string{"claude", "codex"}[i] || s.NativeCount < 0 || s.MalformedCount < 0 {
			return false
		}
		if s.Status == "refreshed" {
			if s.ErrorCode != nil || s.Message != nil {
				return false
			}
		} else if s.Status != "failed" || s.ErrorCode == nil || s.Message == nil {
			return false
		}
	}
	if len(p.Targets) != 0 && len(p.Targets) != 3 {
		return false
	}
	for i, t := range p.Targets {
		if t.Target != []string{"codex_cache", "claude_capabilities", "claude_picker"}[i] || !filepath.IsAbs(t.Path) {
			return false
		}
		switch t.Status {
		case "written":
			if t.Reason != "published" || t.ErrorCode != nil || t.Message != nil {
				return false
			}
		case "skipped":
			if i == 0 || t.Reason != "optional_client_absent" || t.ErrorCode != nil || t.Message != nil {
				return false
			}
		case "failed":
			if t.Reason != "write_failed" || t.ErrorCode == nil || t.Message == nil {
				return false
			}
		default:
			return false
		}
	}
	for _, e := range prunable {
		if modelregistry.ValidateModelID(e.ID) != nil {
			return false
		}
		fresh := false
		for _, s := range p.Sources {
			if s.Provider == e.Provider && s.Status == "refreshed" && s.NativeCount > 0 {
				fresh = true
			}
		}
		if !fresh {
			return false
		}
	}
	recomputed := p
	recomputed.RecomputeStatus()
	return recomputed.Status == p.Status
}

// Required receipt fields cannot be supplied by Go zero values. Unknown fields
// remain forward compatible, but absent/null required fields are protocol errors.
func validModelReceiptFields(body []byte) bool {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return false
	}
	var publication map[string]json.RawMessage
	if json.Unmarshal(envelope["publication"], &publication) != nil {
		return false
	}
	required := func(row map[string]json.RawMessage, names []string, nullable map[string]bool) bool {
		for _, name := range names {
			raw, ok := row[name]
			if !ok || (!nullable[name] && string(raw) == "null") {
				return false
			}
		}
		return true
	}
	if !required(publication, []string{"status", "via", "active_count", "sources", "targets"}, nil) {
		return false
	}
	for _, key := range []string{"sources", "targets"} {
		var rows []map[string]json.RawMessage
		if json.Unmarshal(publication[key], &rows) != nil {
			return false
		}
		names := []string{"provider", "status", "native_count", "malformed_count", "error_code", "message"}
		if key == "targets" {
			names = []string{"target", "path", "status", "reason", "error_code", "message"}
		}
		for _, row := range rows {
			if !required(row, names, map[string]bool{"error_code": true, "message": true}) {
				return false
			}
		}
	}
	return true
}
