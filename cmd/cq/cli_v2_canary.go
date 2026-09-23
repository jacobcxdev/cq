package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type v2CanaryDependencies struct {
	fs           fsutil.DurableFileSystem
	path         string
	protected    []proxy.CodexCanaryProtection
	loadConfig   func() (*proxy.Config, error)
	currentTuple func() (proxy.CodexCanaryTuple, error)
	now          func() time.Time
}

func lookupV2Canary(path string) (cli.Handler, bool) {
	switch path {
	case "codex proxy canary start", "codex proxy canary status", "codex proxy canary stop":
		return handleV2Canary, true
	}
	return nil, false
}
func handleV2Canary(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	paths, err := proxy.ResolveDefaultPaths(userdirs.ConfigRoot, userdirs.StateRoot)
	if err != nil {
		return v2CanaryFailure(err)
	}
	fsys := fsutil.OSFileSystem{}
	home, err := userdirs.UserHomeDir()
	if err != nil {
		return v2CanaryFailure(err)
	}
	protected, err := codexCanaryProtections(home, filepath.Dir(paths.ConfigFile))
	if err != nil {
		return v2CanaryFailure(err)
	}
	return handleV2CanaryWithDependencies(ctx, inv, session, v2CanaryDependencies{
		fs: fsys, path: proxy.CodexCanaryPath(paths.StateDir), protected: protected, now: time.Now,
		loadConfig: func() (*proxy.Config, error) { return proxy.LoadExistingConfigAt(paths) },
		currentTuple: func() (proxy.CodexCanaryTuple, error) {
			marker, err := proxy.LoadCodexReadinessMarker(paths.StateDir, proxy.CodexRoutingHTTP)
			if err != nil {
				return proxy.CodexCanaryTuple{}, err
			}
			return proxy.BuildCurrentCodexCanaryTuple(version, defaultCodexRoutingClientBuild(), marker)
		},
	})
}
func handleV2CanaryWithDependencies(ctx context.Context, inv cli.Invocation, _ *cli.Session, deps v2CanaryDependencies) cli.Outcome {
	if ctx.Err() != nil {
		return v2SelectionFailure(130, "interrupted", "Operation interrupted; inspect state before retrying.")
	}
	_, err := deps.fs.Stat(deps.path)
	missing := errors.Is(err, os.ErrNotExist)
	if missing && inv.Path != "codex proxy canary start" {
		return v2SelectionFailure(3, "canary_missing", "No Codex canary is recorded.")
	}
	if err != nil && !missing {
		return v2CanaryFailure(err)
	}
	var recorder *proxy.CodexCanaryRecorder
	if !missing {
		recorder, err = proxy.OpenCodexCanary(deps.fs, deps.path, deps.protected)
		if err != nil {
			return v2CanaryFailure(err)
		}
	}
	if recorder != nil {
		if err = recorder.ValidateProtectedState(); err != nil {
			return v2CanaryFailure(err)
		}
	}
	switch inv.Path {
	case "codex proxy canary start":
		if recorder != nil && recorder.State().Active {
			return v2CanaryFailure(proxy.ErrCodexCanaryActive)
		}
		cfg, err := deps.loadConfig()
		if err != nil {
			return v2CanaryFailure(err)
		}
		if validateCodexCanaryStartConfig(cfg) != nil {
			return v2SelectionFailure(6, "canary_precondition_failed", "The Codex canary preconditions are not satisfied.")
		}
		tuple, err := deps.currentTuple()
		if err != nil {
			return v2SelectionFailure(6, "canary_precondition_failed", "The Codex canary preconditions are not satisfied.")
		}
		recorder, err = proxy.StartCodexCanaryOnce(deps.fs, deps.path, deps.protected, tuple, deps.now())
		if err != nil {
			return v2CanaryFailure(err)
		}
		if err = recorder.Close(); err != nil {
			return v2CanaryFailure(err)
		}
	case "codex proxy canary stop":
		if recorder.State().Active {
			err = proxy.RequestCodexCanaryStopOnce(deps.fs, deps.path, deps.protected, deps.now())
			if err != nil {
				return v2CanaryFailure(err)
			}
			// The serving owner may have finalised while intent was published. Re-read
			// retained state; never infer drain completion from request acceptance.
			recorder, err = proxy.OpenCodexCanary(deps.fs, deps.path, deps.protected)
			if err != nil {
				return v2CanaryFailure(err)
			}
		}
	}
	state := projectV2Canary(recorder.State())
	data, _ := json.Marshal(struct {
		State v2CanaryState `json:"state"`
	}{state})
	return cli.Outcome{Data: data, Human: fmt.Sprintf("Canary: %s\nActive: %t\nFinalised: %t\nAdmitted turns: %d\n", cli.HumanValue(state.RunID), state.Active, state.Finalised, state.AdmittedTurns)}
}
func v2CanaryFailure(err error) cli.Outcome {
	switch {
	case errors.Is(err, proxy.ErrCodexCanaryActive), errors.Is(err, proxy.ErrCodexCanaryProtectedStateChanged):
		return v2SelectionFailure(6, "canary_precondition_failed", "The Codex canary preconditions are not satisfied.")
	default:
		return v2SelectionFailure(1, "canary_state_unavailable", "The Codex canary state is unavailable.")
	}
}

type v2CanaryState struct {
	RunID                   string                             `json:"run_id"`
	Active                  bool                               `json:"active"`
	Finalised               bool                               `json:"finalised"`
	StartedAt               string                             `json:"started_at"`
	EndedAt                 *string                            `json:"ended_at"`
	LastObservedAt          *string                            `json:"last_observed_at"`
	Tuple                   proxy.CodexCanaryTuple             `json:"tuple"`
	AdmittedTurns           uint64                             `json:"admitted_turns"`
	KeyedMismatches         uint64                             `json:"keyed_mismatches"`
	AutomaticHashChanges    uint64                             `json:"automatic_protected_state_changes"`
	SecretLeaks             uint64                             `json:"secret_leaks"`
	UnexplainedLifecycles   uint64                             `json:"unexplained_lifecycles"`
	LiveSessionRepairs      uint64                             `json:"live_session_repairs"`
	ProtectedStateFailures  uint64                             `json:"protected_state_failures"`
	ConsecutiveCalendarDays int                                `json:"consecutive_calendar_days"`
	ProtectedDigests        []proxy.CodexCanaryProtectedDigest `json:"protected_digests"`
	Finalisation            any                                `json:"finalisation"`
}

func projectV2Canary(state proxy.CodexCanaryState) v2CanaryState {
	timestamp := func(t time.Time) *string {
		if t.IsZero() {
			return nil
		}
		v := t.UTC().Format(time.RFC3339)
		return &v
	}
	digests := append([]proxy.CodexCanaryProtectedDigest{}, state.ProtectedDigests...)
	sort.Slice(digests, func(i, j int) bool { return digests[i].Kind < digests[j].Kind })
	return v2CanaryState{RunID: state.RunID, Active: state.Active, Finalised: state.Finalisation != nil, StartedAt: state.StartedAt.UTC().Format(time.RFC3339), EndedAt: timestamp(state.EndedAt), LastObservedAt: timestamp(state.LastObservedAt), Tuple: state.Tuple, AdmittedTurns: state.AdmittedTurns, KeyedMismatches: state.KeyedMismatches, AutomaticHashChanges: state.AutomaticHashChanges, SecretLeaks: state.SecretLeaks, UnexplainedLifecycles: state.UnexplainedLifecycles, LiveSessionRepairs: state.LiveSessionRepairs, ProtectedStateFailures: state.ProtectedStateFailures, ConsecutiveCalendarDays: state.ConsecutiveCalendarDays, ProtectedDigests: digests, Finalisation: state.Finalisation}
}
