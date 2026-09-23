package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/quota"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func lookupV2Reserve(path string) (cli.Handler, bool) {
	switch path {
	case "codex proxy reserve clear", "codex proxy reserve disable", "codex proxy reserve enable", "codex proxy reserve set", "codex proxy reserve status", "codex proxy reserve windows":
		return handleV2Reserve, true
	}
	return nil, false
}

func handleV2Reserve(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	return handleV2ReserveWithPreparation(ctx, inv, session, func(context.Context) (proxyPolicyDependencies, error) {
		roots, err := userdirs.Default(userdirs.ConfigRoot, userdirs.StateRoot)
		if err != nil {
			return proxyPolicyDependencies{}, err
		}
		paths := proxy.PathsForRoots(roots)
		return proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return proxy.LoadExistingConfigAt(paths) }, Doer: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("proxy reserve redirect refused") }}}, nil
	})
}

func handleV2ReserveWithDependencies(ctx context.Context, inv cli.Invocation, session *cli.Session, deps proxyPolicyDependencies) cli.Outcome {
	return handleV2ReserveWithPreparation(ctx, inv, session, func(context.Context) (proxyPolicyDependencies, error) { return deps, nil })
}

func handleV2ReserveWithPreparation(parent context.Context, inv cli.Invocation, session *cli.Session, prepare func(context.Context) (proxyPolicyDependencies, error)) cli.Outcome {
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
	deps, err := prepare(ctx)
	if out, stopped := v2SelectionStopped(parent, ctx); stopped {
		return out
	}
	if err != nil {
		return v2SelectionFailure(1, "routing_io_failed", "Routing operation failed: resolve proxy paths.")
	}
	parts := strings.Fields(inv.Path)
	options := proxyReserveOptions{action: parts[len(parts)-1]}
	if values := inv.Options["port"]; len(values) > 0 {
		options.port, _ = strconv.Atoi(values[0])
	}
	if options.action == "set" {
		options.window = quota.WindowName(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(inv.Options["window"][0])), "_", "-"))
		options.percent, _ = strconv.ParseFloat(inv.Options["percent"][0], 64)
	}
	status, err := requestProxyReserve(ctx, options, deps)
	if out, stopped := v2SelectionStopped(parent, ctx); stopped {
		return out
	}
	if err != nil {
		return v2ReserveFailure(err)
	}
	data := v2ReserveResource(status)
	if options.action == "windows" {
		human := "No system-account quota windows available.\n"
		if len(data.Windows) > 0 {
			var b strings.Builder
			for _, window := range data.Windows {
				fmt.Fprintln(&b, cli.HumanValue(window.Selector))
			}
			human = b.String()
		}
		raw, _ := json.Marshal(struct {
			Windows []v2ReserveWindow `json:"windows"`
		}{data.Windows})
		return cli.Outcome{Data: raw, Human: human}
	}
	raw, _ := json.Marshal(struct {
		Reserve v2ReserveStatus `json:"reserve"`
	}{data})
	return cli.Outcome{Data: raw, Human: fmt.Sprintf("System account reserve: %t\nAccount: %s\nWindow: %s\nThreshold: %s%%\nEnabled: %t\nBlocked: %t\nReason: %s\nRemaining: %s\nReset: %s\nObserved: %s\n", data.Configured, v2ReserveText(data.Email, "unavailable"), v2ReserveText(data.Window, "none"), v2ReserveNumber(data.Percent), data.Enabled, data.Blocked, v2ReserveText(data.Reason, "none"), v2ReserveNumber(data.RemainingPct), v2ReserveText(data.ResetAt, "unknown"), v2ReserveText(data.ObservedAt, "unknown"))}
}

func v2ReserveFailure(err error) cli.Outcome {
	var failure *proxyReserveError
	if !errors.As(err, &failure) {
		return v2SelectionFailure(1, "routing_io_failed", "Routing operation failed: reserve control.")
	}
	switch {
	case failure.kind == "auth" || failure.status == 401 || failure.status == 403:
		return v2SelectionFailure(5, "routing_auth_failed", "Local proxy authentication failed.")
	case failure.kind == "io":
		return v2SelectionFailure(1, "routing_io_failed", "Routing operation failed: reserve control.")
	case failure.status == 409:
		switch failure.code {
		case "reserve_not_configured":
			return v2SelectionFailure(6, failure.code, "Reserve is not configured; use reserve set first.")
		case "reserve_evidence_required":
			return v2SelectionFailure(6, failure.code, "Fresh usage and reset evidence are required to disable the reserve.")
		case "reserve_window_unavailable":
			return v2SelectionFailure(6, failure.code, "Selected window is unavailable; inspect reserve windows.")
		case "routing_io_failed":
			return v2SelectionFailure(1, failure.code, "Routing operation failed: persist reserve state; inspect current state before retrying.")
		case "routing_invalid_argument":
			return v2SelectionFailure(2, failure.code, "Invalid argument: reserve control.")
		default:
			return v2SelectionFailure(6, "routing_conflict", "Routing state conflict: reserve control rejected.")
		}
	case failure.status == 400:
		return v2SelectionFailure(2, "routing_invalid_argument", "Invalid argument: reserve control.")
	default:
		return v2SelectionFailure(4, "routing_control_unavailable", "Running CQ proxy control is unavailable.")
	}
}

type v2ReserveWindow struct {
	Selector          string   `json:"selector"`
	RemainingPct      int      `json:"remaining_pct"`
	RemainingPctExact *float64 `json:"remaining_pct_exact"`
	ResetAt           *string  `json:"reset_at"`
}
type v2ReserveStatus struct {
	Configured   bool              `json:"configured"`
	Window       *string           `json:"window"`
	Percent      *float64          `json:"percent"`
	AccountKey   *string           `json:"account_key"`
	Email        *string           `json:"email"`
	Enabled      bool              `json:"enabled"`
	Blocked      bool              `json:"blocked"`
	Reason       *string           `json:"reason"`
	RemainingPct *float64          `json:"remaining_pct"`
	ResetAt      *string           `json:"reset_at"`
	ObservedAt   *string           `json:"observed_at"`
	Windows      []v2ReserveWindow `json:"windows"`
}

func v2ReserveResource(status proxy.CodexReserveStatus) v2ReserveStatus {
	data := v2ReserveStatus{Configured: status.Configured, AccountKey: v2ReserveString(string(status.AccountKey)), Email: v2ReserveString(status.Email), Enabled: status.Enabled, Blocked: status.Blocked, Reason: v2ReserveString(status.Reason), RemainingPct: status.RemainingPct, ResetAt: v2ReserveUnix(status.ResetAt), Windows: make([]v2ReserveWindow, 0, len(status.Windows))}
	if status.Configured {
		data.Window = v2ReserveString(string(status.Window))
		data.Percent = &status.Percent
	}
	if !status.ObservedAt.IsZero() {
		observed := status.ObservedAt.UTC().Format(time.RFC3339Nano)
		data.ObservedAt = &observed
	}
	for selector, window := range status.Windows {
		data.Windows = append(data.Windows, v2ReserveWindow{Selector: string(selector), RemainingPct: window.RemainingPct, RemainingPctExact: window.RemainingPctExact, ResetAt: v2ReserveUnix(window.ResetAtUnix)})
	}
	sort.Slice(data.Windows, func(i, j int) bool { return data.Windows[i].Selector < data.Windows[j].Selector })
	return data
}
func v2ReserveString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
func v2ReserveUnix(value int64) *string {
	if value <= 0 {
		return nil
	}
	s := time.Unix(value, 0).UTC().Format(time.RFC3339Nano)
	return &s
}
func v2ReserveText(value *string, unknown string) string {
	if value == nil {
		return unknown
	}
	return cli.HumanValue(*value)
}
func v2ReserveNumber(value *float64) string {
	if value == nil {
		return "unknown"
	}
	return strconv.FormatFloat(*value, 'g', -1, 64)
}
