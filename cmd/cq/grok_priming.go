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
	"github.com/jacobcxdev/cq/internal/provider/grok"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type GrokPrimeCmd struct {
	Status  struct{} `cmd:"" help:"Show Grok priming configuration and journal"`
	Enable  struct{} `cmd:"" help:"Enable confirmed dormant-window priming"`
	Disable struct{} `cmd:"" help:"Disable automatic Grok priming"`
	Now     struct{} `cmd:"" help:"Observe Grok windows and prime confirmed dormant timers"`
}
type grokPrimeDependencies struct {
	loadConfig func() (*proxy.Config, error)
	readConfig func() (*proxy.Config, error)
	saveConfig func(*proxy.Config) error
	primer     func() (*proxy.GrokPrimer, error)
	output     io.Writer
}

func defaultGrokPrimeDependencies() grokPrimeDependencies {
	return grokPrimeDependencies{loadConfig: proxy.LoadConfig, readConfig: proxy.LoadExistingConfig, saveConfig: proxy.SaveConfig, output: os.Stdout, primer: func() (*proxy.GrokPrimer, error) {
		roots, err := userdirs.Default()
		if err != nil {
			return nil, errors.New("resolve Grok priming directory")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, errors.New("resolve Grok credential sources")
		}
		fsys := fsutil.OSFileSystem{}
		return proxy.NewGrokPrimer(fsys, roots.State, grokReadAuthority(fsys, roots.Config, grokHTTPClient(30*time.Second), os.Getenv, home))
	}}
}
func runGrokPrimeCommand(ctx context.Context, command string, jsonOutput bool, deps grokPrimeDependencies) error {
	load := deps.loadConfig
	if command == "status" && deps.readConfig != nil {
		load = deps.readConfig
	}
	cfg, err := load()
	if command == "status" && errors.Is(err, os.ErrNotExist) {
		cfg, err = &proxy.Config{}, nil
	}
	if err != nil {
		return err
	}
	switch command {
	case "enable", "disable":
		cfg.GrokWindowPriming.Enabled = command == "enable"
		if err := deps.saveConfig(cfg); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(deps.output).Encode(cfg.GrokWindowPriming)
		}
		_, err = fmt.Fprintf(deps.output, "Grok window priming %sd. Restart CQ to apply automatic priming.\n", command)
		return err
	case "status", "now":
		primer, err := deps.primer()
		if err != nil {
			return err
		}
		if command == "now" {
			err = primer.PrimeNow(ctx)
		}
		if encodeErr := json.NewEncoder(deps.output).Encode(primer.Health(cfg.GrokWindowPriming.Enabled)); encodeErr != nil {
			return encodeErr
		}
		return err
	default:
		return errors.New("unknown Grok priming command")
	}
}
func buildGrokPrimer(cfg *proxy.Config, roots userdirs.Roots, fsys fsutil.FileSystem, authority grok.Authority) (*proxy.GrokPrimer, error) {
	if cfg == nil || !cfg.GrokWindowPriming.Enabled {
		return nil, nil
	}
	return proxy.NewGrokPrimer(fsys, roots.State, authority)
}
