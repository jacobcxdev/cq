package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/keyring"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type v2SelectionDependencies struct {
	LoadConfig func() (*proxy.Config, error)
	SaveConfig func(*proxy.Config) error
	Codex      func(context.Context) (codexprov.Inventory, codexprov.AccountAliasIndex, error)
	Claude     func(context.Context) ([]keyring.ClaudeAccountInspection, error)
}

func lookupV2Selection(path string) (cli.Handler, bool) {
	switch path {
	case "claude proxy pin show", "claude proxy pin set", "claude proxy pin clear",
		"codex proxy pin show", "codex proxy pin set", "codex proxy pin clear",
		"codex proxy fallback show", "codex proxy fallback set", "codex proxy fallback clear",
		"codex proxy prime status", "codex proxy prime enable", "codex proxy prime disable":
		return handleV2Selection, true
	}
	return nil, false
}

func handleV2Selection(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	roots, err := userdirs.Default(userdirs.ConfigRoot, userdirs.StateRoot)
	if err != nil {
		return v2SelectionFailure(1, "routing_io_failed", "Routing operation failed: resolve proxy paths.")
	}
	paths := proxy.PathsForRoots(roots)
	accounts := &codexprov.Accounts{FS: fsutil.OSFileSystem{}, StateDir: roots.State}
	return handleV2SelectionWithDependencies(ctx, inv, session, v2SelectionDependencies{
		LoadConfig: func() (*proxy.Config, error) { return proxy.LoadExistingConfigAt(paths) },
		SaveConfig: func(cfg *proxy.Config) error { return proxy.SaveConfigAt(paths, cfg) },
		Codex: func(ctx context.Context) (codexprov.Inventory, codexprov.AccountAliasIndex, error) {
			inventory, err := accounts.Inspect(ctx)
			if err != nil {
				return inventory, codexprov.AccountAliasIndex{}, err
			}
			aliases, err := accounts.InspectAliases(ctx)
			return inventory, aliases, err
		},
		Claude: (&claudeprov.Accounts{}).Inspect,
	})
}

func v2Intent(inv cli.Invocation) proxySelectionIntent {
	parts := strings.Fields(inv.Path)
	intent := proxySelectionIntent{provider: parts[0], kind: parts[2], action: parts[3], legacyUUID: inv.LegacySelector == "claude_uuid"}
	if values := inv.Arguments["account"]; len(values) > 0 {
		intent.reference = values[0]
	}
	return intent
}

func handleV2SelectionWithDependencies(parent context.Context, inv cli.Invocation, _ *cli.Session, deps v2SelectionDependencies) cli.Outcome {
	intent := v2Intent(inv)
	timeout, err := time.ParseDuration(inv.Options["timeout"][0])
	if err != nil {
		return v2SelectionFailure(2, "routing_invalid_argument", "Invalid argument: timeout.")
	}
	budget := cli.BeginBudget(parent, timeout, 0)
	defer budget.Close()
	ctx := budget.Work()
	if out, stopped := v2SelectionStopped(parent, ctx); stopped {
		return out
	}
	cfg, err := deps.LoadConfig()
	if out, stopped := v2SelectionStopped(parent, ctx); stopped {
		return out
	}
	if err != nil || cfg == nil {
		return v2SelectionFailure(1, "routing_io_failed", "Routing operation failed: load config.")
	}

	if intent.kind == "prime" {
		if intent.action != "status" {
			cfg.CodexWindowPriming.Enabled = intent.action == "enable"
			if err := deps.SaveConfig(cfg); err != nil {
				return v2SelectionSaveFailure(parent, ctx)
			}
		}
		if out, stopped := v2SelectionStopped(parent, ctx); stopped {
			return out
		}
		return v2PrimeOutcome(cfg, intent.action)
	}

	var selected string
	if intent.action == "set" {
		if intent.provider == "claude" {
			rows, inventoryErr := deps.Claude(ctx)
			if out, stopped := v2SelectionStopped(parent, ctx); stopped {
				return out
			}
			if inventoryErr != nil {
				return v2SelectionInventoryFailure()
			}
			selected, err = v2ResolveClaudeSelection(rows, intent.reference, intent.legacyUUID)
		} else {
			inventory, aliases, inventoryErr := deps.Codex(ctx)
			if out, stopped := v2SelectionStopped(parent, ctx); stopped {
				return out
			}
			if inventoryErr != nil || proxyCodexDefaultInventoryIncomplete(inventory) {
				return v2SelectionInventoryFailure()
			}
			var key codexprov.AccountKey
			key, err = codexprov.ResolveAccountReference(inventory, aliases, intent.reference)
			selected = string(key)
		}
		if err != nil {
			return v2SelectionAccountFailure(intent.reference, err)
		}
	}

	if intent.action != "show" {
		applyProxySelection(cfg, intent, selected)
		if err := deps.SaveConfig(cfg); err != nil {
			return v2SelectionSaveFailure(parent, ctx)
		}
	}
	if out, stopped := v2SelectionStopped(parent, ctx); stopped {
		return out
	}
	return v2SelectionOutcome(cfg, intent)
}

func v2ResolveClaudeSelection(rows []keyring.ClaudeAccountInspection, reference string, legacyUUID bool) (string, error) {
	if legacyUUID {
		matches := 0
		email := ""
		for _, row := range rows {
			if row.Account.AccountUUID == reference && row.Account.Email != "" {
				matches++
				email = row.Account.Email
			}
		}
		if matches == 0 {
			return "", &claudeprov.AccountReferenceError{Code: "missing"}
		}
		if matches != 1 {
			return "", &claudeprov.AccountReferenceError{Code: "ambiguous"}
		}
		reference = email
	}
	row, err := claudeprov.ResolveAccountReference(rows, reference)
	if err != nil {
		return "", err
	}
	return row.Account.Email, nil
}

func v2SelectionAccountFailure(reference string, err error) cli.Outcome {
	var codexErr *codexprov.AccountReferenceError
	var claudeErr *claudeprov.AccountReferenceError
	if (errors.As(err, &codexErr) && codexErr.Code == codexprov.AccountReferenceAmbiguous) || (errors.As(err, &claudeErr) && claudeErr.Code == "ambiguous") {
		return v2SelectionFailure(6, "routing_account_ambiguous", "Account reference is ambiguous; use a unique alias or AccountKey.")
	}
	return v2SelectionFailure(3, "routing_account_not_found", fmt.Sprintf("Account not found: %s.", reference))
}

func v2SelectionInventoryFailure() cli.Outcome {
	return v2SelectionFailure(4, "routing_inventory_unavailable", "Account inventory is unavailable; no routing change was saved.")
}

func v2SelectionSaveFailure(parent, ctx context.Context) cli.Outcome {
	if out, stopped := v2SelectionStopped(parent, ctx); stopped {
		return out
	}
	return v2SelectionFailure(1, "routing_io_failed", "Routing operation failed: configuration update may have committed; inspect current state.")
}

func v2SelectionStopped(parent, ctx context.Context) (cli.Outcome, bool) {
	if parent.Err() == context.Canceled || ctx.Err() == context.Canceled {
		return v2SelectionFailure(130, "interrupted", "Operation interrupted; inspect state before retrying."), true
	}
	if ctx.Err() == context.DeadlineExceeded {
		return v2SelectionFailure(7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation."), true
	}
	return cli.Outcome{}, false
}

func v2SelectionFailure(exit int, code, message string) cli.Outcome {
	return cli.Outcome{ExitCode: exit, Errors: []cli.Diagnostic{{Code: code, Message: message}}}
}

type v2SelectionData struct {
	Provider        string  `json:"provider"`
	Kind            string  `json:"kind"`
	Configured      bool    `json:"configured"`
	Account         *string `json:"account"`
	Application     string  `json:"application"`
	RestartRequired bool    `json:"restart_required"`
}

func v2SelectionOutcome(cfg *proxy.Config, intent proxySelectionIntent) cli.Outcome {
	account := ""
	switch {
	case intent.provider == "claude":
		account = cfg.PinnedClaudeAccount
	case intent.kind == "fallback":
		account = string(cfg.CodexRoutingDefaultAccountKey)
	default:
		account = string(cfg.CodexRoutingPinnedAccountKey)
	}
	data := v2SelectionData{Provider: intent.provider, Kind: intent.kind, Configured: account != "", Application: "configured_only", RestartRequired: intent.provider == "codex" && intent.action != "show"}
	if account != "" {
		data.Account = &account
	}
	if intent.provider == "claude" && intent.action != "show" {
		data.Application = "hot_reload"
	}
	label := account
	if label == "" {
		label = "not configured"
	}
	raw, _ := json.Marshal(data)
	return cli.Outcome{Data: raw, Human: fmt.Sprintf("%s proxy %s: %s\nApplication: %s\nRestart required: %t\n", intent.provider, intent.kind, label, data.Application, data.RestartRequired)}
}

func v2PrimeOutcome(cfg *proxy.Config, action string) cli.Outcome {
	overrides := cfg.CodexWindowPriming.ModelOverrides
	if overrides == nil {
		overrides = map[string]string{}
	}
	data := struct {
		Enabled         bool              `json:"enabled"`
		ModelOverrides  map[string]string `json:"model_overrides"`
		RestartRequired bool              `json:"restart_required"`
	}{cfg.CodexWindowPriming.Enabled, overrides, action != "status"}
	raw, _ := json.Marshal(data)
	state := "disabled"
	if data.Enabled {
		state = "enabled"
	}
	return cli.Outcome{Data: raw, Human: fmt.Sprintf("Codex window priming: %s\nModel overrides: %d\nRestart required: %t\n", state, len(overrides), data.RestartRequired)}
}
