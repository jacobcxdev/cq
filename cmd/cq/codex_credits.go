package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

type CodexCreditsCmd struct {
	Enable  CodexCreditsAccountCmd `cmd:"" help:"Allow extra credit usage for one Codex account"`
	Disable CodexCreditsAccountCmd `cmd:"" help:"Require included quota for one Codex account"`
	Status  CodexCreditsAccountCmd `cmd:"" help:"Show one account's extra credit permission"`
}

type CodexCreditsAccountCmd struct {
	Reference string `arg:"" name:"account-reference" help:"Unique Codex email, CQ alias, or opaque AccountKey"`
}

type codexCreditsDependencies struct {
	Inventory func(context.Context) (codexprov.Inventory, error)
	Aliases   func() (codexprov.AccountAliasIndex, error)
	Policy    *codexprov.ExtraCreditsPolicy
	Out       io.Writer
}

type codexCreditsStatus struct {
	AccountKey codexprov.AccountKey `json:"account_key"`
	Email      string               `json:"email,omitempty"`
	Enabled    bool                 `json:"enabled"`
}

func runDefaultCodexCredits(action, reference string, asJSON bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fs := fsutil.OSFileSystem{}
	control, err := codexprov.OpenDefaultCredentialControl(ctx, fs)
	if err != nil {
		return err
	}
	defer control.Close()
	policy, err := codexprov.DefaultExtraCreditsPolicy()
	if err != nil {
		return err
	}
	home, err := fs.UserHomeDir()
	if err != nil {
		return err
	}
	return runCodexCredits(ctx, action, reference, asJSON, codexCreditsDependencies{
		Inventory: control.List,
		Aliases: func() (codexprov.AccountAliasIndex, error) {
			return (codexprov.Registry{FS: fs, Home: home}).AccountAliasIndex()
		},
		Policy: policy, Out: os.Stdout,
	})
}

func runCodexCredits(ctx context.Context, action, reference string, asJSON bool, deps codexCreditsDependencies) error {
	if ctx == nil || deps.Inventory == nil || deps.Aliases == nil || deps.Policy == nil || deps.Out == nil {
		return errors.New("Codex extra credits control unavailable")
	}
	if action != "enable" && action != "disable" && action != "status" {
		return errors.New("invalid Codex extra credits action")
	}
	inventory, err := deps.Inventory(ctx)
	if err != nil {
		return err
	}
	aliases, err := deps.Aliases()
	if err != nil {
		return err
	}
	key, err := codexprov.ResolveAccountReference(inventory, aliases, reference)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var identity codexprov.AccountIdentity
	for _, account := range inventory.Accounts {
		if account.Key == key {
			identity = account.Identity
			break
		}
	}
	policyKey, err := codexprov.ExtraCreditsIdentityKey(identity)
	if err != nil {
		return err
	}
	if action != "status" {
		if err := deps.Policy.Set(policyKey, action == "enable"); err != nil {
			return fmt.Errorf("save Codex extra credits permission: %w", err)
		}
	}
	allowed, err := deps.Policy.Allowed(policyKey)
	if err != nil {
		return err
	}
	status := codexCreditsStatus{AccountKey: key, Email: identity.Email, Enabled: allowed}
	if asJSON {
		return json.NewEncoder(deps.Out).Encode(status)
	}
	state := "disabled"
	if status.Enabled {
		state = "enabled"
	}
	label := status.Email
	if label == "" {
		label = string(key)
	}
	_, err = fmt.Fprintf(deps.Out, "Codex extra credits: %s\nAccount: %s\n", state, label)
	return err
}
