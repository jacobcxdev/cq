package main

import (
	"bytes"
	"context"
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
