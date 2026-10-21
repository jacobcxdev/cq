package dots

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Request is one user operation. Target is a stable handle, message or board ID.
type Request struct {
	Operation, Target   string
	All, Clear, Watcher bool
	Install             InstallOptions
}

// Controller coordinates the Go runtime without changing the signed app bundle.
type Controller struct {
	Root, Executable, BaseURL string
	Stdout, Stderr            io.Writer
	Catalogue                 *CatalogueClient
	Native                    NativeEvaluator
}

func (c Controller) settings() (*Settings, error) {
	s, err := LoadSettings(filepath.Join(c.Root, "settings.json"))
	if err != nil {
		return nil, err
	}
	if s.RuntimeRoot != filepath.Join(c.Root, "runtime") {
		return nil, errors.New("Dots runtime differs from installation")
	}
	return s, nil
}

// refreshAppPin preserves account and runtime state when a reviewed app update
// replaces the installed bundle. Callers hold the installation operation lock.
func (c Controller) refreshAppPin(s *Settings, pins []Settings) error {
	if AppCheck(s) == nil {
		return nil
	}
	pin, err := qualifiedAppPin(s.AppPath, pins)
	if err != nil {
		return err
	}
	next := *s
	next.AppVersion, next.AppBuild = pin.AppVersion, pin.AppBuild
	next.ASARSHA256, next.ModuleAsset = pin.ASARSHA256, pin.ModuleAsset
	if err := SaveJSON(filepath.Join(c.Root, "settings.json"), &next); err != nil {
		return err
	}
	*s = next
	return nil
}

func (c Controller) client(s *Settings) *CatalogueClient {
	if c.Catalogue != nil {
		return c.Catalogue
	}
	return &CatalogueClient{Settings: s}
}

func (c Controller) evaluator(s *Settings) NativeEvaluator {
	if c.Native != nil {
		return c.Native
	}
	return NewNativeEvaluator(s)
}

// Execute returns JSON-compatible results; the CLI owns output encoding.
func (c Controller) Execute(ctx context.Context, r Request) (map[string]any, error) {
	if !cleanAbsolute(c.Root) {
		return nil, errors.New("Dots root must be a clean absolute path")
	}
	if r.Operation == "install" {
		r.Install.Root = c.Root
		r.Install.Executable = c.Executable
		r.Install.BaseURL = c.BaseURL
		if _, err := ValidateInstallLocation(r.Install); err != nil {
			return nil, err
		}
		if err := PrivateDirectory(c.Root); err != nil {
			return nil, err
		}
	} else if err := privateDirectory(c.Root); err != nil {
		return nil, err
	}
	if r.Operation != "status" && r.Operation != "list" {
		wait := time.Duration(0)
		if r.Operation == "prime" || r.Operation == "broadcast" || r.Operation == "board" {
			wait = 45 * time.Second
		}
		lock, err := OperationLock(ctx, c.Root, wait)
		if err != nil {
			return nil, err
		}
		defer lock.Close()
	}
	if r.Operation == "install" {
		if prior, err := c.settings(); err == nil {
			processes, e := RuntimeProcesses(ctx, prior)
			if e != nil {
				return nil, e
			}
			if len(processes) > 0 {
				return nil, errors.New("Quit the desktop app and stop this Dots installation before reinstalling")
			}
		}
		result, err := Install(ctx, r.Install)
		if err != nil {
			return nil, err
		}
		if _, _, err = c.Refresh(ctx); err != nil {
			return nil, err
		}
		started, err := c.Start(ctx, false)
		if err != nil {
			return nil, err
		}
		for key, value := range result {
			started[key] = value
		}
		return started, nil
	}
	s, err := c.settings()
	if err != nil {
		return nil, err
	}
	switch r.Operation {
	case "start":
		return c.Start(ctx, false)
	case "stop":
		if err := StopRuntime(ctx, s, false); err != nil {
			return nil, err
		}
		return map[string]any{"stopped": true, "system_app_modified": false}, nil
	case "status":
		return c.Status(ctx)
	case "sync":
		return c.Sync(ctx, r.Watcher)
	case "list":
		return c.List(ctx, s)
	case "prime":
		rows, err := NewAwareness(s, c.client(s), c.evaluator(s)).Prime(ctx, r.Target, r.All, true)
		return map[string]any{"priming": rows}, err
	case "broadcast":
		result, err := NewAwareness(s, c.client(s), c.evaluator(s)).Broadcast(ctx, r.Target)
		return map[string]any{"broadcast": result}, err
	case "board":
		board := NewBoard(s, c.client(s), c.evaluator(s))
		if r.Clear {
			value, err := board.Clear()
			return map[string]any{"board": value}, err
		}
		if r.Target != "" {
			value, err := board.Bind(ctx, r.Target)
			if err != nil {
				return nil, err
			}
			rows, err := board.Announce(ctx)
			return map[string]any{"board": value, "notices": rows}, err
		}
		value, err := board.Configuration()
		return map[string]any{"board": value}, err
	default:
		return nil, errors.New("Unsupported Dots operation")
	}
}

// Refresh retains known profiles while their owner is temporarily unavailable.
func (c Controller) Refresh(ctx context.Context) (bool, []AccountOutcome, error) {
	s, err := c.settings()
	if err != nil {
		return false, nil, err
	}
	discovery, err := c.client(s).Discover(ctx)
	if err != nil {
		return false, nil, err
	}
	uncertain := false
	unavailable := map[string]bool{}
	authoritative := map[string]bool{}
	for _, row := range discovery.Accounts {
		if row.OwnerAlias == "" {
			uncertain = true
		}
		if row.Status == "unavailable" && row.OwnerAlias != "" {
			unavailable[row.OwnerAlias] = true
		}
		if row.Status == "ready" || row.Status == "no_dot" {
			authoritative[row.OwnerAlias] = true
		}
	}
	next := make(map[string]Dot)
	for handle, dot := range s.Dots {
		if !authoritative[dot.OwnerAlias] && (uncertain || unavailable[dot.OwnerAlias]) {
			next[handle] = dot
		}
	}
	for _, dot := range discovery.Dots {
		next[dot.Handle] = dot.Dot
	}
	oldRevision := s.CatalogueRevision()
	s.Dots = next
	if err = s.Validate(); err != nil {
		return false, nil, err
	}
	changed := oldRevision != s.CatalogueRevision()
	if changed {
		if err = SaveJSON(filepath.Join(c.Root, "settings.json"), s); err != nil {
			return false, nil, err
		}
	}
	err = SaveJSON(filepath.Join(c.Root, "accounts.json"), discovery.Accounts)
	return changed, discovery.Accounts, err
}

func (c Controller) restartRequired(s *Settings) bool {
	data, err := ReadOwned(filepath.Join(s.RuntimeRoot, "launch.json"), s.RuntimeRoot)
	if err != nil {
		return true
	}
	var launch struct {
		Revision string `json:"catalogue_revision"`
		Archive  string `json:"asar_sha256"`
	}
	return json.Unmarshal(data, &launch) != nil || launch.Revision != s.CatalogueRevision() || launch.Archive != s.ASARSHA256
}

func (c Controller) pending(ctx context.Context, s *Settings) (map[string]any, error) {
	if _, err := ensureDesktopExecutable(s, c.Executable); err != nil {
		return nil, err
	}
	watcher, err := c.ensureWatcher(ctx, s)
	if err != nil {
		return nil, err
	}
	watcher["installed"] = true
	watcher["started"] = false
	watcher["restart_required"] = true
	watcher["reason"] = "Quit the normal desktop app, then run cq codex dots start; CQ will not interrupt running work"
	watcher["system_app_modified"] = false
	return watcher, nil
}

func (c Controller) Sync(ctx context.Context, preserveWatcher bool) (map[string]any, error) {
	s, err := c.settings()
	if err != nil {
		return nil, err
	}
	if err := c.refreshAppPin(s, qualifiedAppPins); err != nil {
		return nil, err
	}
	processes, err := RuntimeProcesses(ctx, s)
	if err != nil {
		return nil, err
	}
	running := false
	for _, p := range processes {
		if p.Role != "account-watch" {
			running = true
		}
	}
	changed, accounts, err := c.Refresh(ctx)
	if err != nil {
		return nil, err
	}
	s, err = c.settings()
	if err != nil {
		return nil, err
	}
	app, err := NormalAppRunning(ctx, s)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if s.IntegrationMode == "native" && app && (changed || c.restartRequired(s)) {
		result, err = c.pending(ctx, s)
	} else if changed {
		if running {
			if err = StopRuntime(ctx, s, preserveWatcher); err != nil {
				return nil, err
			}
		}
		result, err = c.Start(ctx, preserveWatcher)
	} else if running {
		result, err = c.finishStart(ctx, s, nil)
	} else {
		result, err = c.Start(ctx, preserveWatcher)
	}
	if err != nil {
		return nil, err
	}
	complete, retryable := true, false
	for _, row := range accounts {
		if row.Status != "ready" && row.Status != "no_dot" {
			complete = false
		}
		if row.Status == "unavailable" && row.Source == "" && (row.HTTPStatus == 0 || row.HTTPStatus == 408 || row.HTTPStatus == 429 || row.HTTPStatus >= 500) {
			retryable = true
		}
	}
	result["catalogue_changed"] = changed
	result["accounts"] = accounts
	result["discovery_complete"] = complete
	result["discovery_retryable"] = retryable || result["onboarding_required"] == true
	return result, nil
}

func (c Controller) List(ctx context.Context, s *Settings) (map[string]any, error) {
	awareness := NewAwareness(s, c.client(s), c.evaluator(s))
	states, err := awareness.Summary()
	if err != nil {
		return nil, err
	}
	labels, err := awareness.Names(ctx, true)
	if err != nil {
		return nil, err
	}
	stateByID := map[string]string{}
	for _, row := range states {
		target, _ := row["target"].(string)
		outcome, _ := row["outcome"].(string)
		stateByID[target] = outcome
	}
	handles := make([]string, 0, len(s.Dots))
	for handle := range s.Dots {
		handles = append(handles, handle)
	}
	sort.Strings(handles)
	rows := make([]map[string]any, 0, len(handles))
	for _, handle := range handles {
		state := stateByID[handle]
		if state == "" {
			state = "not_attempted"
		}
		rows = append(rows, map[string]any{"id": handle, "name": labels[handle], "account": s.Dots[handle].OwnerAlias, "setup": state})
	}
	return map[string]any{"dots": rows}, nil
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func helperExecutable(s *Settings) string { return filepath.Join(s.RuntimeRoot, "cq-dots-runtime") }

func processRoles(rows []RuntimeProcess) []string {
	roles := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Role != "app" {
			roles = append(roles, row.Role)
		}
	}
	sort.Strings(roles)
	return roles
}

func (c Controller) spawn(s *Settings, role string, args ...string) (*os.Process, error) {
	if c.BaseURL == "" && s.IntegrationMode == "native" && (role == "native-broker" || role == "account-watch") {
		return nil, fmt.Errorf("CQ proxy base URL unavailable")
	}
	env := os.Environ()
	if c.BaseURL != "" {
		env = setEnvironment(env, "OPENAI_BASE_URL", c.BaseURL)
	}
	env = setEnvironment(env, "CQ_DOTS_CQ_PATH", c.Executable)
	return SpawnHelper(s, helperExecutable(s), role, args, env)
}
