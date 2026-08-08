package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func runCodexCanary(args []string) error {
	if len(args) == 0 || helpRequested(args) {
		fmt.Fprintln(os.Stdout, "Usage: cq codex canary start|status|stop|acknowledge-explicit-switch")
		return nil
	}
	fsys := fsutil.OSFileSystem{}
	home, err := fsys.UserHomeDir()
	if err != nil {
		return err
	}
	path := proxy.DefaultCodexCanaryPath()
	protected := []string{filepath.Join(home, ".codex", "auth.json"), filepath.Join(home, ".codex", "accounts", "registry.json")}
	switch args[0] {
	case "start":
		if len(args) != 1 {
			return fmt.Errorf("Codex canary start takes no arguments")
		}
		clientBuild := defaultCodexClientVersion()
		required, _ := proxy.DefaultCodexRoutingRequirements(version, clientBuild)
		marker, err := proxy.LoadCodexReadinessMarker(filepath.Dir(path), proxy.CodexRoutingHTTP)
		if err != nil {
			return fmt.Errorf("load HTTP readiness marker: %w", err)
		}
		if err := proxy.ValidateCodexReadinessMarker(marker, required); err != nil {
			return fmt.Errorf("HTTP readiness marker: %w", err)
		}
		_, err = proxy.StartCodexCanary(fsys, path, protected, proxy.CodexCanaryTuple{CQBuild: version, ClientBuild: clientBuild, ParserSchema: proxy.CurrentCodexParserSchema, LeaseSchema: proxy.CurrentCodexLeaseSchema, FixtureHash: marker.FixtureHash}, time.Now())
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "Codex routing canary started")
		return nil
	case "status", "stop", "acknowledge-explicit-switch":
		recorder, err := proxy.OpenCodexCanary(fsys, path, protected)
		if err != nil {
			return err
		}
		if args[0] == "stop" {
			if err := recorder.Stop(time.Now()); err != nil {
				return err
			}
		} else if args[0] == "acknowledge-explicit-switch" {
			if err := recorder.AcknowledgeExplicitSwitch(); err != nil {
				return err
			}
		} else if recorder.State().Active {
			if err := recorder.RecordHeartbeat(time.Now()); err != nil {
				return err
			}
		}
		state := recorder.State()
		return json.NewEncoder(os.Stdout).Encode(struct {
			Active                  bool                   `json:"active"`
			StartedAt               time.Time              `json:"started_at"`
			EndedAt                 time.Time              `json:"ended_at,omitempty"`
			LastObservedAt          time.Time              `json:"last_observed_at"`
			Tuple                   proxy.CodexCanaryTuple `json:"tuple"`
			AdmittedTurns           uint64                 `json:"admitted_turns"`
			KeyedMismatches         uint64                 `json:"keyed_mismatches"`
			AutomaticHashChanges    uint64                 `json:"automatic_auth_registry_hash_changes"`
			SecretLeaks             uint64                 `json:"secret_leaks"`
			UnexplainedLifecycles   uint64                 `json:"unexplained_lifecycles"`
			ConsecutiveCalendarDays int                    `json:"consecutive_calendar_days"`
		}{state.Active, state.StartedAt, state.EndedAt, state.LastObservedAt, state.Tuple, state.AdmittedTurns, state.KeyedMismatches, state.AutomaticHashChanges, state.SecretLeaks, state.UnexplainedLifecycles, state.ConsecutiveCalendarDays})
	default:
		return fmt.Errorf("unknown Codex canary command: %s", args[0])
	}
}
