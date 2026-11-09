package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/harness"
)

func TestCodexHarnessRetryRequestedSameFailedBindingOnce(t *testing.T) {
	runner, fsys := harnessProfilesFixture(t)
	policy := harness.Policy{Version: 2, Enabled: true, Tasks: map[string]harness.Scope{harnessProfilesChat: {All: harness.Binding{Changes: harness.Changes{Default: harness.Disabled}}}}}
	if err := harness.Save(fsys, "/cq/harness.json", policy); err != nil {
		t.Fatal(err)
	}
	selection := policy.Resolve("gpt-5.5", "/work/project", harnessProfilesChat)
	receipt := harness.Receipt{Version: 1, ChatID: harnessProfilesChat, Mode: "full", RetrySupported: true, Profile: selection.Profile, Source: selection.Source, State: "failed", Fingerprint: selection.Fingerprint, Model: "gpt-5.5", Cwd: "/work/project", UpdatedAt: time.Now().UTC()}
	store := harness.Receipts{FS: fsys, Dir: "/state/harness"}
	if err := store.Save(receipt); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.Load(harnessProfilesChat)
	if err != nil {
		t.Fatal(err)
	}
	runner.receipt = store.Load
	runner.requestRetry = store.RequestRetry
	runner.apply = func(context.Context, string) error { t.Fatal("retry bypassed existing full-mode owner"); return nil }
	originalPolicy, _ := fsys.ReadFile("/cq/harness.json")
	for range 2 {
		if err := runHarnessTestCLI(t, runner, io.Discard, "retry", "--chat", "current", "--project", "/work/project", "--model", "gpt-5.5"); err != nil {
			t.Fatal(err)
		}
	}
	unchangedPolicy, _ := fsys.ReadFile("/cq/harness.json")
	if !bytes.Equal(originalPolicy, unchangedPolicy) {
		t.Fatal("retry intent changed shared policy seen by older controllers")
	}
	got, err := harness.Load(fsys, "/cq/harness.json")
	if err != nil {
		t.Fatal(err)
	}
	request, err := store.LoadRetry(harnessProfilesChat)
	if err != nil {
		t.Fatal(err)
	}
	if request.Nonce == "" || !request.Matches(receipt) || got.Resolve("gpt-5.5", "/work/project", harnessProfilesChat) != selection {
		t.Fatal("retry changed selection or omitted intent")
	}
	first, _ := fsys.ReadFile("/cq/harness.json")
	if err := runHarnessTestCLI(t, runner, io.Discard, "retry", "--chat", "current", "--project", "/work/project", "--model", "gpt-5.5"); err != nil {
		t.Fatal(err)
	}
	second, _ := fsys.ReadFile("/cq/harness.json")
	if !bytes.Equal(first, second) {
		t.Fatal("duplicate request changed retry nonce")
	}
}

func TestCodexHarnessRetryMatchedAttemptedContextAfterRollback(t *testing.T) {
	for _, target := range []struct{ name, model, cwd string }{
		{"model", "glm-5.3-flash", "/fixture"},
		{"project", "glm-5.3", "/other"},
		{"model_and_project", "glm-5.3-flash", "/other"},
	} {
		t.Run(target.name, func(t *testing.T) {
			runner, fsys := harnessProfilesFixture(t)
			policy := harness.Policy{Version: 2, Enabled: true, Global: harness.Scope{All: harness.Binding{Changes: harness.Changes{Default: harness.Enabled}}, Models: map[string]harness.Binding{"glm-5.3-flash": {Changes: harness.Changes{Default: harness.Disabled}}}}, Workspaces: map[string]harness.Scope{"/other": {All: harness.Binding{Changes: harness.Changes{Default: harness.Disabled}}}}}
			if err := harness.Save(fsys, "/cq/harness.json", policy); err != nil {
				t.Fatal(err)
			}
			selection := policy.Resolve(target.model, target.cwd, harnessProfilesChat)
			store := harness.Receipts{FS: fsys, Dir: "/state/harness"}
			receipt := harness.Receipt{Version: 1, ChatID: harnessProfilesChat, Mode: "full", RetrySupported: true, Profile: selection.Profile, Source: selection.Source, State: "failed", Fingerprint: selection.Fingerprint, Model: "glm-5.3", Cwd: "/fixture", AttemptedContext: &harness.ApplicationContext{Model: target.model, Cwd: target.cwd}, Failure: &harness.ApplicationFailure{Stage: "resume", Reason: "rpc_rejected", Rollback: "restored"}}
			if err := store.Save(receipt); err != nil {
				t.Fatal(err)
			}
			runner.receipt, runner.requestRetry = store.Load, store.RequestRetry
			runner.apply = func(context.Context, string) error { t.Fatal("retry bypassed native owner"); return nil }
			before, _ := fsys.ReadFile("/cq/harness.json")
			var explanation bytes.Buffer
			if err := runHarnessTestCLI(t, runner, &explanation, "explain", "--chat", "current", "--project", target.cwd, "--model", target.model, "--json"); err != nil {
				t.Fatal(err)
			}
			var explained harnessExplanation
			if err := json.Unmarshal(explanation.Bytes(), &explained); err != nil || explained.ApplicationStatus.State != "failed" || explained.ApplicationStatus.Reason != "receipt_matches" || explained.LastApplication.Model != "glm-5.3" || explained.LastApplication.Cwd != "/fixture" {
				t.Fatalf("explain lost observed/attempted distinction: %s %v", explanation.Bytes(), err)
			}
			for range 2 {
				if err := runHarnessTestCLI(t, runner, io.Discard, "retry", "--chat", "current", "--project", target.cwd, "--model", target.model); err != nil {
					t.Fatal(err)
				}
			}
			intent, err := store.LoadRetry(harnessProfilesChat)
			if err != nil || intent.Model != target.model || intent.Cwd != target.cwd {
				t.Fatalf("wrong retry context: %#v %v", intent, err)
			}
			if err := runHarnessTestCLI(t, runner, io.Discard, "retry", "--chat", "current", "--project", "/fixture", "--model", "glm-5.3"); err == nil {
				t.Fatal("rolled-back context was accepted as the failed attempt")
			}
			after, _ := fsys.ReadFile("/cq/harness.json")
			if !bytes.Equal(before, after) {
				t.Fatal("context retry changed curation policy")
			}
		})
	}
}

func TestCodexHarnessRetryRefusedUnrelatedOrUnfailedReceipt(t *testing.T) {
	for _, state := range []string{"pending", "applied", "partial", "failed-wrong-model", "failed-wrong-project", "failed-wrong-fingerprint", "failed-plugins", "failed-unsupported-runtime"} {
		t.Run(state, func(t *testing.T) {
			runner, fsys := harnessProfilesFixture(t)
			policy := harness.Policy{Version: 2, Enabled: true, Tasks: map[string]harness.Scope{harnessProfilesChat: {All: harness.Binding{Changes: harness.Changes{Default: harness.Disabled}}}}}
			if err := harness.Save(fsys, "/cq/harness.json", policy); err != nil {
				t.Fatal(err)
			}
			selection := policy.Resolve("gpt-5.5", "/work/project", harnessProfilesChat)
			receipt := harness.Receipt{ChatID: harnessProfilesChat, Mode: "full", RetrySupported: true, Profile: selection.Profile, State: state, Fingerprint: selection.Fingerprint, Model: "gpt-5.5", Cwd: "/work/project", UpdatedAt: time.Now().UTC()}
			switch state {
			case "failed-wrong-model":
				receipt.State, receipt.Model = "failed", "other"
			case "failed-wrong-project":
				receipt.State, receipt.Cwd = "failed", "/other"
			case "failed-wrong-fingerprint":
				receipt.State, receipt.Fingerprint = "failed", "other"
			case "failed-unsupported-runtime":
				receipt.State, receipt.RetrySupported = "failed", false
			case "failed-plugins":
				receipt.State, receipt.Mode = "failed", "plugins"
			}
			runner.receipt = func(string) (harness.Receipt, error) { return receipt, nil }
			before, _ := fsys.ReadFile("/cq/harness.json")
			if err := runHarnessTestCLI(t, runner, io.Discard, "retry", "--chat", "current", "--project", "/work/project", "--model", "gpt-5.5"); err == nil {
				t.Fatal("unrelated or unfailed application accepted")
			}
			after, _ := fsys.ReadFile("/cq/harness.json")
			if !bytes.Equal(before, after) {
				t.Fatal("refused retry changed policy")
			}
		})
	}
}
