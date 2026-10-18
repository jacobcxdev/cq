package dots

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Status proves catalogue and workspace reads, rather than implying delivery.
func (c Controller) Status(ctx context.Context) (map[string]any, error) {
	s, err := c.settings()
	if err != nil {
		return nil, err
	}
	asset, _ := json.Marshal("app://-/assets/" + s.ModuleAsset)
	expression := `(async()=>{if(document.querySelector('[data-testid="onboarding-progress-bar"]'))return {native_catalog_ready:false,onboarding_required:true};const m=await import(` + string(asset) + `);const c=await m.Ymr.safeGet('/tbo');const p=await m.Ymr.safeGet('/tbo/primary');return {profiles:c.items.map(x=>x.id),primary:p.profile?.id,available:p.selection?.available,workspaceError:[...document.querySelectorAll('aside[role="status"][aria-live="polite"] h3')].some(el=>el.textContent.trim().replace(/’/g,"'")==="Couldn't load workspace settings")};})()`
	values, err := c.evaluator(s).EvaluateAll(ctx, expression)
	if err != nil {
		return nil, errors.New("Native Dot status unavailable")
	}
	for _, raw := range values {
		value, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if value["onboarding_required"] == true && value["native_catalog_ready"] == false {
			return map[string]any{"native_catalog_ready": false, "onboarding_required": true}, nil
		}
		profiles, ok := value["profiles"].([]any)
		if !ok || len(profiles) != len(s.Dots) || value["workspaceError"] != false {
			continue
		}
		expected := map[string]bool{}
		for _, dot := range s.Dots {
			expected[dot.ProfileID] = true
		}
		primary, ok := value["primary"].(string)
		if !ok || !expected[primary] || (s.IntegrationMode != "native" && value["available"] != true) {
			continue
		}
		valid := true
		for _, profile := range profiles {
			id, ok := profile.(string)
			if !ok || !expected[id] {
				valid = false
				break
			}
			delete(expected, id)
		}
		if !valid || len(expected) > 0 {
			continue
		}
		rows, err := RuntimeProcesses(ctx, s)
		if err != nil {
			return nil, err
		}
		return map[string]any{"native_catalog_ready": true, "profiles": len(s.Dots), "research_components": processRoles(rows), "controller_runtime": "go", "system_app_modified": false}, nil
	}
	return nil, errors.New("Native catalogue or workspace status not ready")
}

func (c Controller) finishStart(ctx context.Context, s *Settings, ready map[string]any) (map[string]any, error) {
	var err error
	if ready == nil {
		ready, err = c.Status(ctx)
		if err != nil {
			return nil, err
		}
	}
	if ready["onboarding_required"] == true {
		watch, err := c.ensureWatcher(ctx, s)
		if err != nil {
			return nil, err
		}
		for key, value := range watch {
			ready[key] = value
		}
		ready["system_app_modified"] = false
		return ready, nil
	}
	if _, err = InstallNativeRoomCache(ctx, s, c.evaluator(s)); err != nil {
		return nil, err
	}
	awareness, err := NewAwareness(s, c.client(s), c.evaluator(s)).Reconcile(ctx)
	if err != nil {
		awareness = nil
		for handle := range s.Dots {
			awareness = append(awareness, map[string]any{"target": handle, "outcome": "unknown", "error": "awareness_reconciliation_failed"})
		}
	}
	rows, err := RuntimeProcesses(ctx, s)
	if err != nil {
		return nil, err
	}
	count := map[string]int{}
	for _, row := range rows {
		count[row.Role]++
	}
	for _, role := range []string{"relay", "executors"} {
		if count[role] > 1 {
			return nil, errors.New("Multiple verified Dot workers are running")
		}
	}
	if count["relay"] == 0 {
		_, err := c.spawn(s, "relay")
		if err != nil {
			return nil, err
		}
	}
	secondary := false
	for _, dot := range s.Dots {
		if dot.OwnerAlias != s.SystemAlias {
			secondary = true
		}
	}
	if s.IntegrationMode == "native" && secondary && count["executors"] == 0 {
		_, err := c.spawn(s, "executors")
		if err != nil {
			return nil, err
		}
	}
	if err = waitContext(ctx, 500*time.Millisecond); err != nil {
		return nil, err
	}
	rows, err = RuntimeProcesses(ctx, s)
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, row := range rows {
		live[row.Role] = true
	}
	if !live["relay"] || (s.IntegrationMode == "native" && secondary && !live["executors"]) {
		return nil, errors.New("Dot coordination worker failed to start")
	}
	watch, err := c.ensureWatcher(ctx, s)
	if err != nil {
		return nil, err
	}
	for key, value := range watch {
		ready[key] = value
	}
	ready["relay_started"] = true
	ready["relay_existing"] = count["relay"] > 0
	ready["secondary_executors_existing"] = count["executors"] > 0
	ready["awareness"] = awareness
	ready["system_app_modified"] = false
	return ready, nil
}

func (c Controller) Start(ctx context.Context, preserveWatcher bool) (result map[string]any, err error) {
	s, err := c.settings()
	if err != nil {
		return nil, err
	}
	if err = AppCheck(s); err != nil {
		return nil, err
	}
	if len(s.Dots) == 0 {
		if _, err = StageExecutable(c.Executable, s.RuntimeRoot); err != nil {
			return nil, err
		}
		result, err = c.ensureWatcher(ctx, s)
		if err != nil {
			return nil, err
		}
		result["started"] = false
		result["reason"] = "No onboarded Dots"
		result["system_app_modified"] = false
		return result, nil
	}
	primary := false
	for _, dot := range s.Dots {
		if dot.OwnerAlias == s.SystemAlias {
			primary = true
		}
	}
	if !primary {
		return nil, errors.New("Onboard the system account's Dot before starting the shared catalogue")
	}
	rows, err := RuntimeProcesses(ctx, s)
	if err != nil {
		return nil, err
	}
	if s.IntegrationMode == "native" {
		alive, err := NormalAppRunning(ctx, s)
		if err != nil {
			return nil, err
		}
		if alive {
			owned := false
			for _, row := range rows {
				if row.Role == "app" {
					owned = true
				}
			}
			if owned && !c.restartRequired(s) {
				return c.finishStart(ctx, s, nil)
			}
			return c.pending(ctx, s)
		}
	}
	running := false
	for _, row := range rows {
		if row.Role != "account-watch" {
			running = true
		}
	}
	if running {
		if err = StopRuntime(ctx, s, preserveWatcher); err != nil {
			return nil, err
		}
	}
	if _, err = StageExecutable(c.Executable, s.RuntimeRoot); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			alive, e := NormalAppRunning(context.Background(), s)
			if e == nil && !alive {
				cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				_ = StopRuntime(cleanup, s, preserveWatcher)
			}
		}
	}()
	for _, role := range []string{"probe", "realtime"} {
		_, e := c.spawn(s, role)
		if e != nil {
			return nil, e
		}
	}
	if err = c.waitProxies(ctx, s); err != nil {
		return nil, err
	}
	args, env, err := PrepareLaunch(s, c.BaseURL, c.Executable)
	if err != nil {
		return nil, err
	}
	_, err = SpawnHelper(s, helperExecutable(s), "native-broker", args, env)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		ready, e := c.Status(ctx)
		if e == nil {
			return c.finishStart(ctx, s, ready)
		}
		if time.Now().After(deadline) {
			return nil, errors.New("Native Dot startup failed")
		}
		if err = waitContext(ctx, 500*time.Millisecond); err != nil {
			return nil, err
		}
	}
}

func (c Controller) waitProxies(ctx context.Context, s *Settings) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, err := RuntimeProcesses(ctx, s)
		if err != nil {
			return err
		}
		live := map[string]bool{}
		for _, row := range rows {
			live[row.Role] = true
		}
		ready := live["probe"] && live["realtime"]
		for _, name := range []string{"port", "realtime-port"} {
			data, err := ReadOwned(filepath.Join(s.RuntimeRoot, name), s.RuntimeRoot)
			if err != nil {
				ready = false
				continue
			}
			port, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil || port < 1 || port > 65535 {
				ready = false
				continue
			}
			conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 100*time.Millisecond)
			if err != nil {
				ready = false
			} else {
				_ = conn.Close()
			}
		}
		if ready {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("Dot proxies failed to start")
		}
		if err = waitContext(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
}
