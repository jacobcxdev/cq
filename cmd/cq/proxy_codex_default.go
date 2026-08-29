package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

const proxyCodexDefaultUsageMessage = "usage: cq proxy default codex [--clear | <account-reference>]"

type proxyCodexDefaultDependencies struct {
	ListInventory  func(context.Context) (codexprov.Inventory, error)
	LoadAliasIndex func() (codexprov.AccountAliasIndex, error)
	LoadConfig     func() (*proxy.Config, error)
	SaveConfig     func(*proxy.Config) error
	Stdout         io.Writer
}

func runProxyCodexDefault(args []string) error {
	fsys := fsutil.OSFileSystem{}
	var home string
	return runProxyCodexDefaultWithDependencies(context.Background(), args, proxyCodexDefaultDependencies{
		ListInventory: func(ctx context.Context) (codexprov.Inventory, error) {
			inventory, resolvedHome, err := listProxyCodexDefaultInventory(ctx, fsys)
			home = resolvedHome
			return inventory, err
		},
		LoadAliasIndex: func() (codexprov.AccountAliasIndex, error) {
			return (codexprov.Registry{FS: fsys, Home: home}).AccountAliasIndex()
		},
		LoadConfig: proxy.LoadConfig,
		SaveConfig: proxy.SaveConfig,
		Stdout:     os.Stdout,
	})
}

func listProxyCodexDefaultInventory(ctx context.Context, fsys fsutil.DurableFileSystem) (codexprov.Inventory, string, error) {
	roots, err := userdirs.Default()
	if err != nil {
		return codexprov.Inventory{}, "", fmt.Errorf("resolve CQ directories: %w", err)
	}
	store, err := codexprov.NewManagedStore(fsys)
	if err != nil {
		return codexprov.Inventory{}, "", err
	}
	coordinator, err := codexprov.NewCredentialCoordinator(store, roots.State)
	if err != nil {
		return codexprov.Inventory{}, store.Home, err
	}
	inventory, err := coordinator.List(ctx)
	return inventory, store.Home, err
}

func runProxyCodexDefaultWithDependencies(
	ctx context.Context,
	args []string,
	deps proxyCodexDefaultDependencies,
) error {
	if len(args) > 1 ||
		(len(args) == 1 && args[0] != "--clear" && strings.HasPrefix(args[0], "-")) {
		return errors.New(proxyCodexDefaultUsageMessage)
	}

	if len(args) == 0 {
		cfg, err := deps.LoadConfig()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		if cfg.CodexRoutingDefaultAccountKey == "" {
			fmt.Fprintln(deps.Stdout, "Codex routing default: not configured.")
		} else {
			fmt.Fprintf(deps.Stdout, "Codex routing default: %q\n", string(cfg.CodexRoutingDefaultAccountKey))
		}
		return nil
	}

	if args[0] == "--clear" {
		cfg, err := deps.LoadConfig()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		cfg.CodexRoutingDefaultAccountKey = ""
		if err := deps.SaveConfig(cfg); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		fmt.Fprintln(deps.Stdout, "Codex routing default cleared.")
		fmt.Fprintln(deps.Stdout, "Restart proxy to apply change.")
		return nil
	}

	inventory, err := deps.ListInventory(ctx)
	if err != nil || proxyCodexDefaultInventoryIncomplete(inventory) {
		return errors.New("list Codex account inventory: unavailable")
	}
	aliases, err := deps.LoadAliasIndex()
	if err != nil {
		return errors.New("load Codex account aliases: unavailable")
	}
	accountKey, err := codexprov.ResolveAccountReference(inventory, aliases, args[0])
	if err != nil {
		return err
	}

	cfg, err := deps.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg.CodexRoutingDefaultAccountKey = accountKey
	if err := deps.SaveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintf(deps.Stdout, "Codex routing default: %q\n", string(accountKey))
	fmt.Fprintln(deps.Stdout, "Restart proxy to apply change.")
	return nil
}

func proxyCodexDefaultInventoryIncomplete(inventory codexprov.Inventory) bool {
	for _, source := range inventory.ExternalSources {
		if source.ErrorCode != "" && !source.OptionalAbsent {
			return true
		}
	}
	return false
}
