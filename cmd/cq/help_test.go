package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

func TestRootHelpShowsFullCLISurface(t *testing.T) {
	out := &bytes.Buffer{}
	var cli CLI
	kctx, err := kong.New(&cli,
		append(cliKongOptions(), kong.Writers(out, io.Discard), kong.Exit(func(int) {}))...,
	)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}

	_, _ = kctx.Parse([]string{"--help"})
	help := out.String()
	for _, want := range []string{
		"check [<providers> ...]",
		"claude login",
		"codex login",
		"gemini accounts",
		"refresh",
		"agent install",
		"agent uninstall",
		"proxy start",
		"proxy install",
		"proxy uninstall",
		"proxy restart",
		"proxy status",
		"proxy pin",
		"models list",
		"models refresh",
		"models overlay add",
		"models overlay remove",
		"models overlay prune",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("root help missing %q:\n%s", want, help)
		}
	}
}

func TestManualHelpTextDocumentsEachCommandPath(t *testing.T) {
	for _, tt := range []struct {
		name string
		path []string
		want []string
	}{
		{
			name: "refresh",
			path: []string{"refresh"},
			want: []string{"Usage: cq refresh", "Refresh stored OAuth tokens"},
		},
		{
			name: "agent",
			path: []string{"agent"},
			want: []string{"Usage: cq agent <command>", "agent install", "agent uninstall"},
		},
		{
			name: "proxy start",
			path: []string{"proxy", "start"},
			want: []string{"Usage: cq proxy start [--port PORT]", "Start local Claude and Codex proxy"},
		},
		{
			name: "proxy pin",
			path: []string{"proxy", "pin"},
			want: []string{"Usage: cq proxy pin [--clear | <email-or-account-uuid>]", "Pin Claude proxy routing"},
		},
		{
			name: "models list",
			path: []string{"models", "list"},
			want: []string{"Usage: cq models list [--json] [--provider PROVIDER]", "List active registry models"},
		},
		{
			name: "models overlay add",
			path: []string{"models", "overlay", "add"},
			want: []string{"Usage: cq models overlay add --provider PROVIDER --id MODEL [--clone-from MODEL]", "Add or update user model overlay"},
		},
		{
			name: "models overlay remove",
			path: []string{"models", "overlay", "remove"},
			want: []string{"Usage: cq models overlay remove --provider PROVIDER --id MODEL", "Remove user model overlay"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			help, ok := manualHelp(tt.path)
			if !ok {
				t.Fatalf("manualHelp(%v) missing entry", tt.path)
			}
			for _, want := range tt.want {
				if !strings.Contains(help, want) {
					t.Fatalf("manualHelp(%v) missing %q:\n%s", tt.path, want, help)
				}
			}
		})
	}
}

func TestRunModelsHelpDoesNotRefresh(t *testing.T) {
	_, stdout, _, deps := testModelsDeps()
	refreshCalls := 0
	deps.Refresh = func() error {
		refreshCalls++
		return nil
	}

	for _, args := range [][]string{
		{"--help"},
		{"list", "--help"},
		{"overlay", "add", "--help"},
	} {
		stdout.Reset()
		if err := runModels(args, deps); err != nil {
			t.Fatalf("runModels(%v): %v", args, err)
		}
		if refreshCalls != 0 {
			t.Fatalf("runModels(%v) called Refresh %d time(s), want 0", args, refreshCalls)
		}
		if !strings.Contains(stdout.String(), "Usage: cq models") {
			t.Fatalf("runModels(%v) did not print models help:\n%s", args, stdout.String())
		}
	}
}
