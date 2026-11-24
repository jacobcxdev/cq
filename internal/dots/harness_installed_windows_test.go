//go:build windows

package dots

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
	"github.com/jacobcxdev/cq/internal/httputil"
)

// TestHarnessWindowsNativeProfileContract qualifies the installed Windows
// app-server's per-chat controls without using an account or the live Codex home.
func TestHarnessWindowsNativeProfileContract(t *testing.T) {
	if os.Getenv("CQ_HARNESS_WINDOWS_NATIVE_ACCEPTANCE") != "1" {
		t.Skip("set CQ_HARNESS_WINDOWS_NATIVE_ACCEPTANCE=1 for isolated native acceptance")
	}
	executable := os.Getenv("CQ_HARNESS_WINDOWS_CODEX")
	if !filepath.IsAbs(executable) {
		t.Fatal("CQ_HARNESS_WINDOWS_CODEX must name the absolute native executable")
	}
	root := t.TempDir()
	for _, name := range []string{"codex", "home", "appdata", "localappdata", "tmp", "work"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	skill := filepath.Join(root, "codex", "skills", "probe")
	if err := os.MkdirAll(skill, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: probe\ndescription: CQ_WINDOWS_SKILL_MARKER\n---\nSynthetic skill.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	type observation struct{ skill, mcp bool }
	observed := make(chan observation, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			http.Error(w, "unexpected synthetic request", http.StatusBadRequest)
			return
		}
		body, err := httputil.ReadBody(r.Body)
		if err != nil {
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		var request struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Input []json.RawMessage `json:"input"`
		}
		if json.Unmarshal(body, &request) != nil {
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		entry := observation{}
		for _, tool := range request.Tools {
			entry.mcp = entry.mcp || tool.Name == "mcp__probe"
		}
		for _, item := range request.Input {
			entry.skill = entry.skill || bytes.Contains(item, []byte("CQ_WINDOWS_SKILL_MARKER"))
		}
		observed <- entry
		item := map[string]any{"id": "probe_message", "type": "message", "role": "assistant", "status": "completed", "content": []map[string]any{{"type": "output_text", "text": "CQ_WINDOWS_REPLY", "annotations": []any{}}}}
		response := map[string]any{"id": "probe_response", "object": "response", "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 30, "output_tokens": 1, "total_tokens": 31}}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []any{
			map[string]any{"type": "response.created", "response": map[string]any{"id": "probe_response", "object": "response", "status": "in_progress", "output": []any{}}},
			map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
			map[string]any{"type": "response.completed", "response": response},
		} {
			data, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}))
	defer upstream.Close()
	config := fmt.Sprintf(`model = "glm-5.3"
model_provider = "fixture"
approval_policy = "never"
sandbox_mode = "read-only"
cli_auth_credentials_store = "file"
check_for_update_on_startup = false
[analytics]
enabled = false
[features]
plugins = true
[model_providers.fixture]
name = "Synthetic"
base_url = %q
wire_api = "responses"
requires_openai_auth = false
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
[mcp_servers.probe]
command = %q
args = ["-test.run=^TestHarnessInstalledMCPHelper$"]
startup_timeout_sec = 5
[mcp_servers.probe.env]
CQ_HARNESS_INSTALLED_MCP_HELPER = "1"
`, upstream.URL, os.Args[0])
	if err := os.WriteFile(filepath.Join(root, "codex", "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "app-server", "--stdio", "--strict-config")
	command.Dir = filepath.Join(root, "work")
	for _, key := range []string{"PATH", "SystemRoot", "WINDIR", "COMSPEC", "PATHEXT"} {
		if value := os.Getenv(key); value != "" {
			command.Env = append(command.Env, key+"="+value)
		}
	}
	for key, value := range map[string]string{
		"CODEX_HOME": filepath.Join(root, "codex"), "USERPROFILE": filepath.Join(root, "home"),
		"APPDATA": filepath.Join(root, "appdata"), "LOCALAPPDATA": filepath.Join(root, "localappdata"),
		"TEMP": filepath.Join(root, "tmp"), "TMP": filepath.Join(root, "tmp"),
		"NO_PROXY": "127.0.0.1,localhost", "CI": "1",
	} {
		command.Env = append(command.Env, key+"="+value)
	}
	t.Run("cq-bridge", func(t *testing.T) {
		bridgeCtx, bridgeCancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer bridgeCancel()
		native := exec.CommandContext(bridgeCtx, executable, "app-server", "--stdio", "--strict-config")
		native.Dir, native.Env, native.Stderr = command.Dir, command.Env, io.Discard
		nativeInput, err := native.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		nativeOutput, err := native.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := native.Start(); err != nil {
			t.Fatal(err)
		}
		var waitOnce sync.Once
		var waitErr error
		wait := func() error { waitOnce.Do(func() { waitErr = native.Wait() }); return waitErr }
		defer func() { _ = native.Process.Kill(); _ = wait() }()
		bridgeInput, clientInput := io.Pipe()
		clientOutput, bridgeOutput := io.Pipe()
		policy := harness.Policy{Version: 1, Enabled: true, Projects: map[string]harness.Profile{command.Dir: harness.Minimal}}
		var policyMu sync.Mutex
		var eventMu sync.Mutex
		var profileEvents []string
		receiptFS := fsutil.NewMemFS()
		if err := receiptFS.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		receipts := harness.Receipts{FS: receiptFS, Dir: filepath.Join(root, "bridge-receipts")}
		deps := cloudStdioDependencies{
			localExecutable: executable, environment: command.Env, now: time.Now, statusInterval: 25 * time.Millisecond,
			harnessTimeout: 5 * time.Second, harnessReceipts: receipts,
			event: func(event map[string]any) {
				profile, ok := event["harness_profile"].(string)
				if !ok {
					return
				}
				eventMu.Lock()
				defer eventMu.Unlock()
				if len(profileEvents) < 32 {
					profileEvents = append(profileEvents, fmt.Sprintf("%s:%v", profile, event["state"]))
				}
			},
			readRoots: func(string, string) ([]byte, error) { return nil, os.ErrNotExist },
			readHarnessPolicy: func() (harness.Policy, error) {
				policyMu.Lock()
				defer policyMu.Unlock()
				return policy, nil
			},
			launch: func(context.Context, string, []string, []string, io.Writer) (*cloudLocal, error) {
				return &cloudLocal{input: nativeInput, output: nativeOutput, wait: wait,
					stop: func() { _ = nativeInput.Close(); _ = native.Process.Kill(); _ = nativeOutput.Close() }}, nil
			},
			stock: func(context.Context, string, []string, []string, io.Reader, io.Writer, io.Writer) error {
				t.Error("Windows harness fixture bypassed CQ bridge")
				return errors.New("Windows harness fixture bypassed CQ bridge")
			},
			dial: func(context.Context, Account) (cloudSocket, int, error) {
				t.Error("Windows harness fixture attempted external cloud socket")
				return nil, 0, errors.New("external cloud sockets forbidden")
			},
			http: cloudTestHTTP(func(*http.Request) (*http.Response, error) {
				t.Error("Windows harness fixture attempted external cloud HTTP")
				return nil, errors.New("external cloud HTTP forbidden")
			}),
		}
		finished := make(chan error, 1)
		go func() {
			finished <- runCloudStdio(bridgeCtx, &Settings{RuntimeRoot: root}, nil,
				[]string{"app-server"}, bridgeInput, bridgeOutput, io.Discard, deps)
			_ = bridgeOutput.Close()
		}()
		defer func() {
			// After the last RPC the bridge can still forward native notifications.
			go func() { _, _ = io.Copy(io.Discard, clientOutput) }()
			_ = clientInput.Close()
			select {
			case err := <-finished:
				if err != nil && bridgeCtx.Err() == nil {
					t.Errorf("Windows CQ bridge failed: %v", err)
				}
			case <-time.After(6 * time.Second):
				t.Error("Windows CQ bridge did not stop")
			}
			_ = bridgeInput.Close()
			_ = clientOutput.Close()
		}()
		rpc := &harnessInstalledRPC{t: t, encoder: json.NewEncoder(clientInput), decoder: json.NewDecoder(clientOutput)}
		rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "cq_windows_bridge_acceptance", "version": "0.0.0"}, "capabilities": map[string]any{"experimentalApi": true}})
		if err := rpc.encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
			t.Fatal(err)
		}
		result := rpc.call("thread/start", map[string]any{"cwd": command.Dir, "model": "glm-5.3", "modelProvider": "fixture"})
		var started struct{ Thread struct{ ID string } }
		if json.Unmarshal(result, &started) != nil || started.Thread.ID == "" {
			t.Fatal("bridge thread start returned no ID")
		}
		chat := started.Thread.ID
		turn := func() observation {
			t.Helper()
			rpc.call("turn/start", map[string]any{"threadId": chat, "input": []map[string]string{{"type": "text", "text": "Synthetic bridge turn"}}})
			for {
				if message := rpc.next(); message.Method == "turn/completed" && cloudThreadID(message.Params) == chat {
					break
				}
			}
			select {
			case entry := <-observed:
				return entry
			case <-bridgeCtx.Done():
				t.Fatal("CQ bridge turn did not reach the synthetic provider")
				return observation{}
			}
		}
		setProfile := func(profile harness.Profile) {
			policyMu.Lock()
			policy.Chats = map[string]harness.Profile{chat: profile}
			policyMu.Unlock()
		}
		assertApplied := func(profile harness.Profile) {
			t.Helper()
			deadline := time.Now().Add(20 * time.Second)
			for {
				receipt, err := receipts.Load(chat)
				if err == nil && receipt.Profile == profile && receipt.State == "applied" {
					return
				}
				if time.Now().After(deadline) {
					eventMu.Lock()
					events := append([]string(nil), profileEvents...)
					eventMu.Unlock()
					t.Fatalf("CQ bridge %s receipt = %+v, %v; observed transitions = %v", profile, receipt, err, events)
				}
				// An applied receipt can follow turn/completed. Keep reading the
				// stdio stream while waiting for the verifier to record its result.
				rpc.call("thread/read", map[string]any{"threadId": chat})
				time.Sleep(25 * time.Millisecond)
			}
		}
		if reduced := turn(); reduced.skill || reduced.mcp {
			t.Fatalf("CQ bridge did not apply fresh minimal profile: %+v", reduced)
		}
		setProfile(harness.Full)
		if restored := turn(); !restored.skill || !restored.mcp {
			t.Fatalf("CQ bridge did not restore skill and MCP: %+v", restored)
		}
		assertApplied(harness.Full)
		setProfile(harness.Minimal)
		if reduced := turn(); reduced.mcp {
			t.Fatalf("CQ bridge retained MCP after minimal reload: %+v", reduced)
		}
		assertApplied(harness.Minimal)
	})
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = io.Discard
	command.WaitDelay = time.Second
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = input.Close()
		_ = output.Close()
		if err := command.Wait(); err != nil && ctx.Err() == nil {
			t.Errorf("isolated native app-server exit: %v", err)
		}
	}()
	rpc := &harnessInstalledRPC{t: t, encoder: json.NewEncoder(input), decoder: json.NewDecoder(output)}
	rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "cq_windows_harness_acceptance", "version": "0.0.0"}, "capabilities": map[string]any{"experimentalApi": true}})
	if err := rpc.encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	start := func(config map[string]any) string {
		t.Helper()
		result := rpc.call("thread/start", map[string]any{"cwd": command.Dir, "model": "glm-5.3", "modelProvider": "fixture", "config": config})
		var started struct {
			Thread struct{ ID string } `json:"thread"`
		}
		if json.Unmarshal(result, &started) != nil || started.Thread.ID == "" {
			t.Fatal("native thread start returned no ID")
		}
		return started.Thread.ID
	}
	turn := func(chat string) observation {
		t.Helper()
		rpc.call("turn/start", map[string]any{"threadId": chat, "input": []map[string]string{{"type": "text", "text": "Synthetic local turn"}}})
		for {
			message := rpc.next()
			if message.Method == "turn/completed" {
				break
			}
		}
		select {
		case entry := <-observed:
			return entry
		case <-ctx.Done():
			t.Fatal("synthetic provider did not receive a turn")
			return observation{}
		}
	}
	minimal := map[string]any{"skills.include_instructions": false, "mcp_servers.probe.enabled": false}
	full := map[string]any{"skills.include_instructions": true, "mcp_servers.probe.enabled": true}
	resume := func(chat string, config map[string]any, wantPlugins []string) {
		t.Helper()
		result := rpc.call("thread/resume", map[string]any{"threadId": chat, "config": config})
		var response struct {
			DisabledPluginIDs []string `json:"disabledPluginIds"`
		}
		if json.Unmarshal(result, &response) != nil || len(response.DisabledPluginIDs) != len(wantPlugins) {
			t.Fatalf("native plugin exclusions after resume: %v, want %v", response.DisabledPluginIDs, wantPlugins)
		}
		for index, id := range wantPlugins {
			if response.DisabledPluginIDs[index] != id {
				t.Fatalf("native plugin exclusions after resume: %v, want %v", response.DisabledPluginIDs, wantPlugins)
			}
		}
	}
	chat := start(nil)
	if first := turn(chat); !first.skill || !first.mcp {
		t.Fatalf("native full baseline omitted fixture skill or MCP: %+v", first)
	}
	rpc.call("thread/settings/update", map[string]any{"threadId": chat, "disabledPluginIds": []string{"probe@fixture"}})
	rpc.call("thread/unsubscribe", map[string]string{"threadId": chat})
	resume(chat, minimal, []string{"probe@fixture"})
	if reduced := turn(chat); reduced.mcp {
		t.Fatalf("native minimal reload retained standalone MCP: %+v", reduced)
	}
	rpc.call("thread/settings/update", map[string]any{"threadId": chat, "disabledPluginIds": []string{}})
	rpc.call("thread/unsubscribe", map[string]string{"threadId": chat})
	resume(chat, full, nil)
	if restored := turn(chat); !restored.mcp {
		t.Fatalf("native full reload omitted restored standalone MCP: %+v", restored)
	}
	fresh := start(minimal)
	if reduced := turn(fresh); reduced.skill || reduced.mcp {
		t.Fatalf("fresh minimal chat retained fixture skill or MCP: %+v", reduced)
	}
	rpc.call("thread/unsubscribe", map[string]string{"threadId": fresh})
	resume(fresh, full, nil)
	if restored := turn(fresh); !restored.skill || !restored.mcp {
		t.Fatalf("native full reload did not restore fixture skill and MCP: %+v", restored)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, output); err != nil {
		t.Fatal(err)
	}
}
