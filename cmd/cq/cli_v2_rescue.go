package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func lookupV2Rescue(path string) (cli.Handler, bool) {
	switch path {
	case "proxy operation status", "proxy rescue enter", "proxy rescue exit", "proxy rescue status":
		return handleV2Rescue, true
	}
	return nil, false
}

type v2RescueDependencies struct {
	LoadConfig func() (*proxy.Config, error)
	Doer       httputil.Doer
	Inspect    func(context.Context, string) (proxy.OperationCoordinatorInspectionV1, error)
}

// Injection is limited to preparation; the production handler owns the budget.
var prepareV2RescueDependencies = func(ctx context.Context, inv cli.Invocation) (v2RescueDependencies, error) {
	if inv.Path == "proxy operation status" {
		paths, err := proxy.ResolveDefaultPaths(userdirs.ConfigRoot)
		if err != nil {
			return v2RescueDependencies{}, err
		}
		return v2RescueDependencies{Inspect: func(ctx context.Context, id string) (proxy.OperationCoordinatorInspectionV1, error) {
			return inspectOperatorOperationAt(ctx, id, paths)
		}}, nil
	}
	roots, err := userdirs.Default(userdirs.ConfigRoot, userdirs.StateRoot)
	if err != nil {
		return v2RescueDependencies{}, err
	}
	paths := proxy.PathsForRoots(roots)
	return v2RescueDependencies{
		LoadConfig: func() (*proxy.Config, error) {
			cfg, err := proxy.LoadCanonicalProxyRescueBootstrapConfigAt(paths)
			if err != nil {
				return nil, err
			}
			return &proxy.Config{Port: cfg.Port, LocalToken: cfg.LocalToken}, nil
		},
		Doer: &http.Client{
			Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func handleV2Rescue(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return handleV2RescueWithPreparation(ctx, inv, session, prepareV2RescueDependencies)
}

func handleV2RescueWithPreparation(parent context.Context, inv cli.Invocation, _ *cli.Session, prepare func(context.Context, cli.Invocation) (v2RescueDependencies, error)) cli.Outcome {
	ctx := parent
	operation := inv.Path == "proxy operation status"
	if !operation {
		timeout := proxyRescueControlTimeout
		if values := inv.Options["timeout"]; len(values) > 0 {
			timeout, _ = time.ParseDuration(values[0])
		}
		budget := cli.BeginBudget(parent, timeout, 0)
		defer budget.Close()
		ctx = budget.Work()
	}
	finish := func(out cli.Outcome, err error) cli.Outcome {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			return v2SelectionFailure(130, "interrupted", "Operation interrupted; inspect state before retrying.")
		}
		if !operation && (errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)) {
			return v2SelectionFailure(7, "rescue_timeout", "The proxy rescue operation timed out; inspect status before retrying.")
		}
		if operation && ctx.Err() != nil {
			err = ctx.Err()
		}
		if err == nil {
			return out
		}
		if operation {
			return v2SelectionFailure(4, "operation_state_unavailable", "The operation state is unavailable.")
		}
		var control *proxyRescueError
		switch {
		case errors.Is(err, proxy.ErrLocalTokenRequired):
			return v2SelectionFailure(5, "rescue_auth_failed", "Proxy rescue control was not authorised.")
		case errors.As(err, &control):
			switch {
			case control.status == 401 || control.status == 403:
				return v2SelectionFailure(5, "rescue_auth_failed", "Proxy rescue control was not authorised.")
			case control.status == 409:
				return v2SelectionFailure(6, "rescue_transition_conflict", "The proxy cannot perform this rescue transition.")
			case control.kind == "response":
				return v2SelectionFailure(1, "rescue_response_invalid", "Proxy rescue control returned an invalid response.")
			}
		}
		return v2SelectionFailure(4, "rescue_unavailable", "Proxy rescue control is unavailable.")
	}
	if ctx.Err() != nil {
		return finish(cli.Outcome{}, ctx.Err())
	}
	deps, err := prepare(ctx, inv)
	if ctx.Err() != nil {
		return finish(cli.Outcome{}, ctx.Err())
	}
	if err != nil {
		return finish(cli.Outcome{}, err)
	}
	if operation {
		id := ""
		if values := inv.Arguments["operation-id"]; len(values) > 0 {
			id = values[0]
		}
		inspection, err := deps.Inspect(ctx, id)
		if err != nil {
			return finish(cli.Outcome{}, err)
		}
		if !inspection.Found && id != "" {
			return finish(v2SelectionFailure(3, "operation_not_found", "The requested operation does not exist."), nil)
		}
		data := v2OperationStatus{State: "idle"}
		humanID, humanPhase := "idle", "—"
		if inspection.Found {
			if !lowerHexArgument(inspection.OperationID, 32) || (inspection.ValueDigest != "" && !lowerHexArgument(inspection.ValueDigest, 64)) {
				return finish(cli.Outcome{}, errors.New("invalid operation identity"))
			}
			switch inspection.Phase {
			case "intent", "anchor", "receipt", "terminal":
			default:
				return finish(cli.Outcome{}, errors.New("invalid operation phase"))
			}
			data.OperationID, data.Phase, data.ResultDigest = &inspection.OperationID, &inspection.Phase, v2ProxyNullable(inspection.ValueDigest)
			data.ResultAvailable = inspection.Receipt || inspection.Phase == "receipt" || inspection.Phase == "terminal"
			data.State = "pending"
			if data.ResultAvailable {
				data.State = "result_available"
			}
			humanID, humanPhase = cli.HumanValue(inspection.OperationID), cli.HumanValue(inspection.Phase)
		}
		return finish(v2ProxyResource(data, fmt.Sprintf("Operation: %s\nState: %s\nPhase: %s\nResult available: %t\nRecovery supported: false\n", humanID, data.State, humanPhase, data.ResultAvailable)), nil)
	}
	port := 0
	if values := inv.Options["port"]; len(values) > 0 {
		port, _ = strconv.Atoi(values[0])
	}
	body, err := requestProxyRescue(ctx, strings.TrimPrefix(inv.Path, "proxy rescue "), port, deps.LoadConfig, deps.Doer)
	if err != nil {
		return finish(cli.Outcome{}, err)
	}
	data, err := decodeV2RescueState(body)
	if err != nil {
		return finish(cli.Outcome{}, &proxyRescueError{kind: "response", cause: err})
	}
	return finish(v2ProxyResource(data, fmt.Sprintf("Proxy mode: %s\nGeneration: %d\nActive rescue requests: %d\nDraining sessions: %d\n", data.Mode, data.Generation, data.ActiveRescueRequests, len(data.DrainingSessions))), nil)
}

type v2OperationStatus struct {
	OperationID       *string `json:"operation_id"`
	State             string  `json:"state"`
	Phase             *string `json:"phase"`
	ResultAvailable   bool    `json:"result_available"`
	ResultDigest      *string `json:"result_digest"`
	RecoverySupported bool    `json:"recovery_supported"`
}

type v2RescueState struct {
	Mode                 string   `json:"mode"`
	Generation           uint64   `json:"generation"`
	ActiveRescueRequests uint64   `json:"active_rescue_requests"`
	DrainingSessions     []string `json:"draining_sessions"`
}

func decodeV2RescueState(body []byte) (v2RescueState, error) {
	invalid := errors.New("invalid rescue response")
	result := v2RescueState{DrainingSessions: []string{}}
	if !utf8.Valid(body) {
		return result, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return result, invalid
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return result, invalid
		}
		seen[key] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(value, []byte("null")) {
			return result, invalid
		}
		switch key {
		case "mode":
			err = json.Unmarshal(value, &result.Mode)
		case "generation":
			err = json.Unmarshal(value, &result.Generation)
		case "active_rescue_requests":
			err = json.Unmarshal(value, &result.ActiveRescueRequests)
		case "draining_sessions":
			err = json.Unmarshal(value, &result.DrainingSessions)
		default:
			return result, invalid
		}
		if err != nil {
			return result, invalid
		}
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') || decoder.Decode(&struct{}{}) != io.EOF || !seen["mode"] || !seen["generation"] || !seen["active_rescue_requests"] {
		return result, invalid
	}
	switch result.Mode {
	case "normal", "drain", "rescue_draining", "rescue", "rescue_exit_draining":
	default:
		return result, invalid
	}
	for _, hint := range result.DrainingSessions {
		if hint == "" || strings.ContainsAny(hint, "\x00\r\n") {
			return result, invalid
		}
	}
	sort.Strings(result.DrainingSessions)
	return result, nil
}
