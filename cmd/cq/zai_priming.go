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
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type ZAIPrimeCmd struct {
	Status  struct{} `cmd:"" help:"Show Z.ai priming configuration and journal"`
	Enable  struct{} `cmd:"" help:"Enable automatic dormant-window priming"`
	Disable struct{} `cmd:"" help:"Disable automatic dormant-window priming"`
	Now     struct{} `cmd:"" help:"Check all accounts and prime dormant windows once"`
}

type zaiPrimeDependencies struct {
	loadConfig func() (*proxy.Config, error)
	saveConfig func(*proxy.Config) error
	primer     func() (*proxy.ZAIPrimer, error)
	output     io.Writer
}

func defaultZAIPrimeDependencies() zaiPrimeDependencies {
	return zaiPrimeDependencies{
		loadConfig: proxy.LoadConfig,
		saveConfig: proxy.SaveConfig,
		output:     os.Stdout,
		primer: func() (*proxy.ZAIPrimer, error) {
			roots, err := userdirs.Default()
			if err != nil {
				return nil, errors.New("resolve Z.ai priming directory")
			}
			fsys := fsutil.OSFileSystem{}
			models, _, err := modelregistry.LoadCodexRegistryCache(fsys, roots)
			if err != nil {
				return nil, errors.New("load Z.ai priming model catalogue")
			}
			client := zaiHTTPClient(30 * time.Second)
			primer, err := proxy.NewZAIPrimer(fsys, roots.State, client, func() ([]zaiprov.Account, error) {
				return zaiprov.ReadAccounts(fsys, os.Getenv, roots.Config)
			}, func() []modelregistry.Entry { return models })
			if err != nil {
				return nil, err
			}
			primer.ProjectTokens = &zcode.ProjectTokenClient{HTTP: client, Version: zcodeClientVersion}
			primer.Gateway = &zcode.Gateway{HTTP: client, Signer: &zcode.Signer{HTTP: client, Version: zcodeClientVersion}, Version: zcodeClientVersion, Catalogue: &zcode.ModelCatalogue{HTTP: client, Version: zcodeClientVersion}}
			return primer, nil
		},
	}
}

func runZAIPrimeCommand(ctx context.Context, command string, jsonOutput bool, deps zaiPrimeDependencies) error {
	cfg, err := deps.loadConfig()
	if err != nil {
		return err
	}
	switch command {
	case "enable", "disable":
		cfg.ZAIWindowPriming.Enabled = command == "enable"
		if err := deps.saveConfig(cfg); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(deps.output).Encode(cfg.ZAIWindowPriming)
		}
		_, err = fmt.Fprintf(deps.output, "Z.ai window priming %sd. Restart CQ to apply automatic priming.\n", command)
		return err
	case "status", "now":
		primer, err := deps.primer()
		if err != nil {
			return err
		}
		if command == "now" {
			_, err = primer.RunOnce(ctx, time.Now())
		}
		if encodeErr := json.NewEncoder(deps.output).Encode(primer.Health(cfg.ZAIWindowPriming.Enabled)); encodeErr != nil {
			return encodeErr
		}
		return err
	default:
		return errors.New("unknown Z.ai priming command")
	}
}

func buildZAIPrimer(cfg *proxy.Config, roots userdirs.Roots, fsys fsutil.FileSystem, client httputil.Doer, catalog *modelregistry.Catalog) (*proxy.ZAIPrimer, error) {
	if cfg == nil || !cfg.ZAIWindowPriming.Enabled {
		return nil, nil
	}
	if catalog == nil || client == nil {
		return nil, errors.New("Z.ai window priming dependencies unavailable")
	}
	return proxy.NewZAIPrimer(fsys, roots.State, client, func() ([]zaiprov.Account, error) {
		return zaiprov.ReadAccounts(fsys, os.Getenv, roots.Config)
	}, func() []modelregistry.Entry { return catalog.Snapshot().Entries })
}
