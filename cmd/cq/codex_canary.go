package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func runCodexCanary(args []string) error {
	if len(args) == 0 || helpRequested(args) {
		fmt.Fprintln(os.Stdout, "Usage: cq codex canary start|status|stop")
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("Codex canary %s takes no arguments", args[0])
	}
	command := args[0]
	if command != "start" && command != "status" && command != "stop" {
		return fmt.Errorf("unknown Codex canary command: %s", command)
	}

	fsys := fsutil.OSFileSystem{}
	home, err := fsys.UserHomeDir()
	if err != nil {
		return err
	}
	path := proxy.DefaultCodexCanaryPath()
	configDirectory := filepath.Dir(path)
	protected, err := codexCanaryProtections(home, configDirectory)
	if err != nil {
		return err
	}

	switch command {
	case "start":
		cfg, err := proxy.LoadConfig()
		if err != nil {
			return err
		}
		if err := validateCodexCanaryStartConfig(cfg); err != nil {
			return err
		}
		clientBuild := defaultCodexRoutingClientBuild()
		marker, err := proxy.LoadCodexReadinessMarker(configDirectory, proxy.CodexRoutingHTTP)
		if err != nil {
			return fmt.Errorf("load HTTP readiness marker: %w", err)
		}
		tuple, err := proxy.BuildCurrentCodexCanaryTuple(version, clientBuild, marker)
		if err != nil {
			return err
		}
		recorder, err := proxy.StartCodexCanary(fsys, path, protected, tuple, time.Now())
		if err != nil {
			return err
		}
		if err := recorder.Close(); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "Codex routing canary started")
		return nil
	case "status":
		recorder, err := proxy.OpenCodexCanary(fsys, path, protected)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(recorder.State())
	case "stop":
		if err := proxy.RequestCodexCanaryStop(fsys, path, protected, time.Now()); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "Codex routing canary stop requested; awaiting installed service drain")
		return nil
	default:
		panic("validated Codex canary command")
	}
}

func validateCodexCanaryStartConfig(cfg *proxy.Config) error {
	if cfg == nil {
		return errors.New("Codex canary configuration is unavailable")
	}
	if cfg.PayloadDiagnosticsLog != "" {
		return errors.New("Codex canary requires payload diagnostics to be disabled")
	}
	if cfg.CodexTurnRouting != proxy.CodexRoutingEnforce {
		return errors.New("Codex canary requires HTTP routing enforcement")
	}
	return nil
}
