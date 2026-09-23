package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/installstate"
)

func lookupV2Service(path string) (cli.Handler, bool) {
	switch path {
	case "service install", "service start", "service stop", "service restart", "service status", "service uninstall":
		return handleV2Service, true
	}
	return nil, false
}

var selectedServiceLifecycleFactory = func(context.Context, serviceAction, serviceSelection) (*serviceLifecycle, error) {
	return serviceLifecycleFactory("")
}

func handleV2Service(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	return handleV2ServiceWithPreparation(ctx, inv, session, func(ctx context.Context) (*serviceLifecycle, error) {
		return selectedServiceLifecycleFactory(ctx, serviceAction(strings.TrimPrefix(inv.Path, "service ")), serviceSelection(inv.Options["component"][0]))
	})
}

func handleV2ServiceWithPreparation(parent context.Context, inv cli.Invocation, _ *cli.Session, prepare func(context.Context) (*serviceLifecycle, error)) cli.Outcome {
	duration, _ := time.ParseDuration(inv.Options["timeout"][0])
	budget := cli.BeginBudget(parent, duration, 0)
	defer budget.Close()
	ctx := budget.Work()
	action := serviceAction(strings.TrimPrefix(inv.Path, "service "))
	selection := serviceSelection(inv.Options["component"][0])
	result := serviceOperationResult{Rollback: "not_needed"}
	finish := func(err error) cli.Outcome {
		data := v2ServiceData{Component: selection, Action: action, Components: []v2ServiceComponent{}, Rollback: result.Rollback}
		for _, id := range selection.components() {
			data.Components = append(data.Components, projectV2ServiceComponent(id, result.Status.component(id), time.Now()))
		}
		out := cli.Outcome{}
		switch {
		case parent.Err() == context.Canceled || ctx.Err() == context.Canceled || errors.Is(err, context.Canceled):
			out = v2SelectionFailure(130, "interrupted", "Operation interrupted; inspect state before retrying.")
		case ctx.Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded):
			out = v2SelectionFailure(7, "service_"+string(action)+"_timeout", "Operation timed out; inspect state before retrying.")
		case result.Rollback == "failed":
			out = v2SelectionFailure(8, "service_partial", "Service operation was only partially completed; inspect component results.")
		case errors.Is(err, installstate.ErrOwnershipConflict), errors.Is(err, installstate.ErrInvalidRecord), errors.Is(err, installstate.ErrUnknownSchema):
			out = v2SelectionFailure(6, "service_owner_conflict", "Service ownership conflicts with this operation.")
		case errors.Is(err, errServiceUnavailable):
			out = v2SelectionFailure(4, "service_unavailable", "The current-user service manager is unavailable.")
		case err != nil:
			component := selection
			var componentErr *serviceComponentError
			if errors.As(err, &componentErr) {
				component = componentErr.Component
			}
			if errors.Is(err, installstate.ErrNotInstalled) {
				out = v2SelectionFailure(3, "service_not_installed", fmt.Sprintf("Service component %s is not installed.", component))
			} else {
				out = v2SelectionFailure(1, "service_unhealthy", fmt.Sprintf("Service component %s is unhealthy.", component))
			}
		}
		out.Data, _ = json.Marshal(data)
		var human strings.Builder
		fmt.Fprintf(&human, "CQ services (%s): %s\n", selection, action)
		for _, c := range data.Components {
			fmt.Fprintf(&human, "%s: %s, enabled=%s, healthy=%s\n", c.ID, c.State, v2ServiceBool(c.Enabled), v2ServiceBool(c.Healthy))
		}
		fmt.Fprintf(&human, "\nRollback: %s\n", data.Rollback)
		out.Human = human.String()
		return out
	}
	if ctx.Err() != nil {
		return finish(ctx.Err())
	}
	lifecycle, err := prepare(ctx)
	if ctx.Err() != nil {
		return finish(ctx.Err())
	}
	if err != nil {
		return finish(errors.Join(errServiceUnavailable, err))
	}
	if lifecycle == nil {
		return finish(errServiceUnavailable)
	}
	result, err = lifecycle.Selected(ctx, action, selection, inv.Options["strict"] != nil && inv.Options["strict"][0] == "true")
	return finish(err)
}

type v2ServiceData struct {
	Component  serviceSelection     `json:"component"`
	Action     serviceAction        `json:"action"`
	Components []v2ServiceComponent `json:"components"`
	Rollback   string               `json:"rollback"`
}

type v2ServiceComponent struct {
	ID           serviceSelection `json:"id"`
	Manager      *string          `json:"manager"`
	Owner        string           `json:"owner"`
	Installed    bool             `json:"installed"`
	Enabled      *bool            `json:"enabled"`
	State        string           `json:"state"`
	Healthy      *bool            `json:"healthy"`
	PID          *int             `json:"pid"`
	Executable   *string          `json:"executable"`
	LastRunAt    *string          `json:"last_run_at"`
	LastExitCode *int             `json:"last_exit_code"`
	ErrorCode    *string          `json:"error_code"`
	ConfigDir    *string          `json:"config_dir"`
	StateDir     *string          `json:"state_dir"`
	CacheDir     *string          `json:"cache_dir"`
	RuntimeDir   *string          `json:"runtime_dir"`
	LogDir       *string          `json:"log_dir"`
}

func projectV2ServiceComponent(id serviceSelection, c componentStatus, now time.Time) v2ServiceComponent {
	out := v2ServiceComponent{ID: id, Owner: "none", Installed: c.Registered, State: "indeterminate"}
	switch c.Manager {
	case "launchd", "task-scheduler", "systemd":
		out.Manager = &c.Manager
	case "systemd-user":
		manager := "systemd"
		out.Manager = &manager
	}
	if c.Observed == nil {
		return out
	}
	obs := c.Observed
	switch obs.Owner {
	case "cq", "package", "foreign", "none":
		out.Owner = obs.Owner
	}
	out.Enabled = obs.Enabled
	out.ErrorCode = v2AccountString(obs.ErrorCode)
	if obs.Roots != nil {
		out.ConfigDir = v2ServicePath(obs.Roots.Config)
		out.StateDir = v2ServicePath(obs.Roots.State)
		out.CacheDir = v2ServicePath(obs.Roots.Cache)
		out.RuntimeDir = v2ServicePath(obs.Roots.Runtime)
		out.LogDir = v2ServicePath(obs.Roots.Logs)
	}
	if !c.Registered {
		out.State = "absent"
		out.Owner = "none"
		out.Enabled = serviceBool(false)
		out.Healthy = serviceBool(false)
		return out
	}
	if c.PID > 0 {
		out.PID = &c.PID
	}
	out.Executable = v2ServicePath(c.ConfiguredExecutable)
	if obs.LastRunAt != nil {
		value := obs.LastRunAt.UTC().Format(time.RFC3339)
		out.LastRunAt = &value
	}
	out.LastExitCode = obs.LastExitCode
	if out.Manager == nil || out.Enabled == nil || out.Executable == nil || out.ConfigDir == nil || out.StateDir == nil || out.CacheDir == nil || out.RuntimeDir == nil || out.LogDir == nil || (out.Owner != "cq" && out.Owner != "package") {
		return out
	}
	if c.Running {
		out.State = "running"
	} else if *out.Enabled && id == serviceRefresh {
		out.State = "idle"
	} else {
		out.State = "stopped"
	}
	if id == serviceProxy {
		out.Healthy = obs.Healthy
		if !c.Running {
			out.Healthy = serviceBool(false)
		}
	} else if !*out.Enabled {
		out.Healthy = serviceBool(false)
	} else if obs.LastRunAt != nil && obs.LastExitCode != nil {
		age := now.Sub(*obs.LastRunAt)
		out.Healthy = serviceBool(*obs.LastExitCode == 0 && age >= 0 && age <= 35*time.Minute)
		if *obs.LastExitCode != 0 {
			out.State = "failed"
		}
	}
	if obs.ErrorCode != "" {
		out.State = "failed"
		out.Healthy = serviceBool(false)
	}
	if out.Healthy == nil {
		out.State = "indeterminate"
	}
	return out
}
func serviceBool(value bool) *bool { return &value }
func v2ServicePath(value string) *string {
	if value == "" || !filepath.IsAbs(value) {
		return nil
	}
	return &value
}
func v2ServiceBool(value *bool) string {
	if value == nil {
		return "—"
	}
	if *value {
		return "true"
	}
	return "false"
}
