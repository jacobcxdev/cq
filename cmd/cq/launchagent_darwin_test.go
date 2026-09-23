package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPlistTemplate(t *testing.T) {
	data, err := renderDarwinLaunchAgent(darwinLaunchAgentDefinition{
		Label:             "dev.jacobcx.cq.refresh",
		ProgramArguments:  []string{"/opt/homebrew/bin/cq", "refresh"},
		StartInterval:     1800,
		RunAtLoad:         true,
		StandardErrorPath: "/Users/test/Library/Logs/cq/refresh.log",
	})
	if err != nil {
		t.Fatalf("render plist: %v", err)
	}
	out := string(data)

	for _, want := range []string{
		"<string>dev.jacobcx.cq.refresh</string>",
		"<string>/opt/homebrew/bin/cq</string>",
		"<string>refresh</string>",
		"<integer>1800</integer>",
		"<true/>",
		"<string>Background</string>",
		"<string>/Users/test/Library/Logs/cq/refresh.log</string>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing %q", want)
		}
	}
}

func TestAgentPlistPath(t *testing.T) {
	path, err := agentPlistPath()
	if err != nil {
		t.Fatalf("agentPlistPath: %v", err)
	}
	if filepath.Base(path) != agentLabel+".plist" {
		t.Errorf("base = %q, want %q", filepath.Base(path), agentLabel+".plist")
	}
	if !strings.Contains(path, "LaunchAgents") {
		t.Errorf("path %q missing LaunchAgents component", path)
	}
}

func TestAgentLogPath(t *testing.T) {
	logs := filepath.Join(string(filepath.Separator), "cq", "logs")
	path := agentLogPath(logs)
	if want := filepath.Join(logs, "refresh.log"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestUninstallAgentNoOp(t *testing.T) {
	// When no plist exists, uninstall should succeed silently.
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Ensure LaunchAgents dir exists but no plist.
	if err := os.MkdirAll(filepath.Join(dir, "Library", "LaunchAgents"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := uninstallAgent(); err != nil {
		t.Errorf("uninstallAgent with no plist: %v", err)
	}
}

func TestDarwinServiceRefreshCompletion(t *testing.T) {
	p, _ := newSelectedDarwinHarness(t)
	d := readDarwinDefinition(t, p.plistPath(agentLabel))
	before := time.Now().UTC().Add(-time.Second)
	after := before.Add(time.Millisecond)
	calls := 0
	clock := func() time.Time {
		calls++
		if calls == 1 {
			return before
		}
		return after
	}
	sentinel := errors.New("refresh failed")
	if err := recordDarwinServiceRefresh(d, func() error { return sentinel }, clock); !errors.Is(err, sentinel) {
		t.Fatalf("run error=%v", err)
	}
	receipt, err := readDarwinRefreshCompletion(d, time.Now())
	if err != nil || receipt.ExitCode != 1 || !receipt.StartedAt.Equal(before) || !receipt.CompletedAt.Equal(after) || receipt.Roots != p.roots || receipt.Executable != p.executable {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	path := filepath.Join(p.roots.State, darwinRefreshCompletionName)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("receipt mode=%v err=%v", info, err)
	}
	if err := recordDarwinServiceRefresh(d, func() error { return nil }, time.Now); err != nil {
		t.Fatal(err)
	}
	receipt, err = readDarwinRefreshCompletion(d, time.Now())
	if err != nil || receipt.ExitCode != 0 || !receipt.CompletedAt.After(after) {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}
func TestDarwinServiceRefreshRejectsInvalidCompletion(t *testing.T) {
	p, _ := newSelectedDarwinHarness(t)
	d := readDarwinDefinition(t, p.plistPath(agentLabel))
	if err := recordDarwinServiceRefresh(d, func() error { return nil }, time.Now); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.roots.State, darwinRefreshCompletionName)
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"malformed": "{", "null": "null", "trailing": string(valid) + "{}", "unknown": strings.Replace(string(valid), `"schema_version":1`, `"unknown":1,"schema_version":1`, 1),
		"duplicate":      strings.Replace(string(valid), `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1),
		"null exit":      strings.Replace(string(valid), `"exit_code":0`, `"exit_code":null`, 1),
		"missing exit":   strings.Replace(string(valid), `"exit_code":0,`, "", 1),
		"case alias":     strings.Replace(string(valid), `"schema_version"`, `"Schema_Version"`, 1),
		"duplicate root": strings.Replace(string(valid), `"roots":{`, `"roots":{"Config":"/wrong",`, 1),
		"null root":      strings.Replace(string(valid), `"roots":{`, `"roots":{"Config":null,`, 1),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readDarwinRefreshCompletion(d, time.Now()); err == nil {
				t.Fatal("invalid completion accepted")
			}
		})
	}
	var receipt darwinRefreshCompletion
	if err := json.Unmarshal(valid, &receipt); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"future", "backwards", "wrong executable", "wrong roots", "negative exit"} {
		t.Run(kind, func(t *testing.T) {
			r := receipt
			switch kind {
			case "future":
				r.CompletedAt = time.Now().Add(time.Hour)
			case "backwards":
				r.CompletedAt = r.StartedAt.Add(-time.Second)
			case "wrong executable":
				r.Executable = "/different/cq"
			case "wrong roots":
				r.Roots.State = "/different/state"
			case "negative exit":
				r.ExitCode = -1
			}
			data, _ := json.Marshal(r)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readDarwinRefreshCompletion(d, time.Now()); err == nil {
				t.Fatal("mismatched completion accepted")
			}
		})
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "other")
	if err := os.WriteFile(target, valid, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := readDarwinRefreshCompletion(d, time.Now()); err == nil {
		t.Fatal("symlink receipt accepted")
	}
}
func TestDarwinServiceRefreshPublicationFailurePreservesError(t *testing.T) {
	p, _ := newSelectedDarwinHarness(t)
	d := readDarwinDefinition(t, p.plistPath(agentLabel))
	sentinel := errors.New("refresh failure")
	err := recordDarwinServiceRefresh(d, func() error {
		if err := os.Mkdir(filepath.Join(p.roots.State, darwinRefreshCompletionName), 0o700); err != nil {
			t.Fatal(err)
		}
		return sentinel
	}, time.Now)
	if !errors.Is(err, sentinel) || err == sentinel {
		t.Fatalf("publication failure lost: %v", err)
	}
	if _, err := readDarwinRefreshCompletion(d, time.Now()); err == nil {
		t.Fatal("publication failure became completion")
	}
}
func TestDarwinServiceRefreshHealthAge(t *testing.T) {
	p, _ := newSelectedDarwinHarness(t)
	d := readDarwinDefinition(t, p.plistPath(agentLabel))
	now := time.Now().UTC()
	for _, tc := range []struct {
		name string
		age  time.Duration
		fail bool
		want bool
	}{{"fresh", time.Minute, false, true}, {"boundary", 35 * time.Minute, false, true}, {"stale", 35*time.Minute + time.Nanosecond, false, false}, {"failed", time.Minute, true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			then := now.Add(-tc.age)
			if err := recordDarwinServiceRefresh(d, func() error {
				if tc.fail {
					return errors.New("failure")
				}
				return nil
			}, func() time.Time { return then }); err != nil && !tc.fail {
				t.Fatal(err)
			}
			status, err := p.InspectSelected(context.Background(), serviceRefresh)
			if err != nil {
				t.Fatal(err)
			}
			data := projectV2ServiceComponent(serviceRefresh, status.Refresh, now)
			if data.Healthy == nil || *data.Healthy != tc.want {
				t.Fatalf("health=%+v", data)
			}
		})
	}
}
func TestDarwinServiceRefreshOrdinaryInvocation(t *testing.T) {
	t.Setenv("CQ_SERVICE_REFRESH", "")
	calls := 0
	sentinel := errors.New("ordinary error")
	if err := runDarwinServiceRefresh(func() error { calls++; return sentinel }); err != sentinel || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
func TestDarwinServiceRefreshOnceCancellation(t *testing.T) {
	p, _ := newSelectedDarwinHarness(t)
	d := readDarwinDefinition(t, p.plistPath(agentLabel))
	pidFile := filepath.Join(p.home, "child.pid")
	script := "#!/bin/sh\n/bin/sleep 30 &\necho $! > \"$PID_FILE\"\nwait\n"
	if err := os.WriteFile(p.executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	d.EnvironmentVariables["PID_FILE"] = pidFile
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	observed := make(chan struct{})
	go func() {
		defer close(observed)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := os.Stat(pidFile); err == nil {
					cancel()
					return
				}
			}
		}
	}()
	if err := runDarwinRefreshOnce(ctx, d); !errors.Is(err, context.Canceled) {
		t.Fatalf("run error=%v", err)
	}
	<-observed
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("child %d survived cancellation", pid)
	}
	if _, err := readDarwinRefreshCompletion(d, time.Now()); err == nil {
		t.Fatal("killed run published completion")
	}
}

func TestDarwinServiceRefreshLogAuthority(t *testing.T) {
	for _, scenario := range []string{"existing-0644", "writable", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			p, _ := newSelectedDarwinHarness(t)
			path := filepath.Join(p.roots.Logs, "refresh.log")
			if err := os.WriteFile(path, []byte("existing diagnostics\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if scenario == "writable" {
				if err := os.Chmod(path, 0o666); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "symlink" {
				other := filepath.Join(p.roots.Logs, "other")
				if err := os.Rename(path, other); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, path); err != nil {
					t.Fatal(err)
				}
			}
			file, err := openDarwinRefreshLog(path)
			if file != nil {
				file.Close()
			}
			if (err == nil) != (scenario == "existing-0644") {
				t.Fatalf("open=%v", err)
			}
		})
	}
}
