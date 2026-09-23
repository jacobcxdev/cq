package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
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

type v2ProxyDependencies struct {
	LoadConfig func() (*proxy.Config, error)
	Client     *http.Client
	Inspect    func(context.Context, string) proxy.ProxySnapshot
	Initialise func(context.Context, string, *proxy.Config) (v2ProxyState, error)
	Serve      func(context.Context, proxyCommandOptions, func(string, []string) error) error
}

func lookupV2Proxy(path string) (cli.Handler, bool) {
	switch path {
	case "proxy health", "proxy status", "proxy serve", "proxy state initialise":
		return handleV2Proxy, true
	}
	return nil, false
}

var errV2ProxyTerminated = errors.New("proxy terminated")

func handleV2Proxy(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	if inv.Path == "proxy serve" {
		signalCtx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(signals)
		go func() {
			defer func() {
				if recover() != nil {
					cancel(context.Canceled)
				}
			}()
			select {
			case sig := <-signals:
				if sig == syscall.SIGTERM {
					cancel(errV2ProxyTerminated)
				} else {
					cancel(context.Canceled)
				}
			case <-signalCtx.Done():
			}
		}()
		ctx = context.WithValue(signalCtx, proxyForegroundSignalsKey{}, true)
	}
	return handleV2ProxyWithPreparation(ctx, inv, session, func(ctx context.Context) (v2ProxyDependencies, error) {
		deps := v2ProxyDependencies{
			Inspect: func(ctx context.Context, root string) proxy.ProxySnapshot {
				return InspectProxy(ctx, proxyInspectionTargetForRoot(root))
			},
			Serve: runProxyStartWithContext,
		}
		// An explicit health port must not even resolve a configuration directory.
		if inv.Path == "proxy health" && inv.Supplied["port"] {
			return deps, nil
		}
		if inv.Path == "proxy serve" {
			return deps, nil
		}
		roots, err := userdirs.Default(userdirs.ConfigRoot, userdirs.StateRoot)
		if err != nil {
			return deps, err
		}
		paths := proxy.PathsForRoots(roots)
		deps.LoadConfig = func() (*proxy.Config, error) { return proxy.LoadExistingConfigAt(paths) }
		deps.Initialise = func(ctx context.Context, root string, cfg *proxy.Config) (v2ProxyState, error) {
			return initialiseProxyState(ctx, root, cfg, paths, deps.Inspect)
		}
		return deps, nil
	})
}
func handleV2ProxyWithDependencies(ctx context.Context, inv cli.Invocation, session *cli.Session, deps v2ProxyDependencies) cli.Outcome {
	return handleV2ProxyWithPreparation(ctx, inv, session, func(context.Context) (v2ProxyDependencies, error) { return deps, nil })
}
func handleV2ProxyWithPreparation(parent context.Context, inv cli.Invocation, session *cli.Session, prepare func(context.Context) (v2ProxyDependencies, error)) cli.Outcome {
	ctx := parent
	if values := inv.Options["timeout"]; len(values) > 0 {
		duration, _ := time.ParseDuration(values[0])
		budget := cli.BeginBudget(parent, duration, 0)
		defer budget.Close()
		ctx = budget.Work()
	}
	finish := func(out cli.Outcome) cli.Outcome {
		if inv.Path == "proxy serve" {
			if !out.Streamed && ctx.Err() == context.Canceled && !errors.Is(context.Cause(ctx), errV2ProxyTerminated) {
				return v2SelectionFailure(130, "interrupted", "Operation interrupted; inspect state before retrying.")
			}
			return out
		}
		var failure cli.Outcome
		if ctx.Err() == context.Canceled {
			failure = v2SelectionFailure(130, "interrupted", "Operation interrupted; inspect state before retrying.")
		} else if ctx.Err() == context.DeadlineExceeded {
			failure = v2SelectionFailure(7, strings.ReplaceAll(inv.Path, " ", "_")+"_timeout", "Operation timed out; inspect state before retrying.")
		} else {
			return out
		}
		failure.Data = out.Data
		failure.Human = out.Human
		return failure
	}
	if ctx.Err() != nil {
		return finish(cli.Outcome{})
	}
	deps, err := prepare(ctx)
	if ctx.Err() != nil {
		return finish(cli.Outcome{})
	}
	if err != nil {
		return v2ProxyIO()
	}
	var out cli.Outcome
	switch inv.Path {
	case "proxy health":
		out = executeV2ProxyHealth(ctx, inv, deps)
	case "proxy status":
		out = executeV2ProxyStatus(ctx, inv, deps)
	case "proxy state initialise":
		out = executeV2ProxyInitialise(ctx, inv, deps)
	case "proxy serve":
		out = executeV2ProxyServe(ctx, inv, session, deps)
	}
	return finish(out)
}
func v2ProxyIO() cli.Outcome {
	return v2SelectionFailure(1, "internal_error", "Operation failed because of an internal error.")
}
func v2ProxyResource(data any, human string) cli.Outcome {
	raw, _ := json.Marshal(data)
	return cli.Outcome{Data: raw, Human: human}
}

type v2ProxyHealth struct {
	Reachable  bool   `json:"reachable"`
	Healthy    bool   `json:"healthy"`
	HTTPStatus *int   `json:"http_status"`
	Address    string `json:"address"`
	DurationMS int64  `json:"duration_ms"`
}

func executeV2ProxyHealth(ctx context.Context, inv cli.Invocation, deps v2ProxyDependencies) cli.Outcome {
	started := time.Now()
	port := proxy.DefaultPort
	if inv.Supplied["port"] {
		port, _ = strconv.Atoi(inv.Options["port"][0])
	} else {
		cfg, err := deps.LoadConfig()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return v2ProxyIO()
		}
		if err == nil && cfg != nil {
			port = cfg.Port
		}
	}
	if ctx.Err() != nil {
		return cli.Outcome{}
	}
	if port < 1 || port > 65535 {
		return v2SelectionFailure(6, "proxy_config_invalid", "Proxy configuration is invalid: port.")
	}
	data := v2ProxyHealth{Address: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+data.Address+"/health", nil)
	transport := &http.Transport{Proxy: nil, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport}
	if deps.Client != nil {
		client = *deps.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	var out cli.Outcome
	if response != nil {
		data.Reachable = true
		if response.StatusCode >= 100 && response.StatusCode <= 599 {
			data.HTTPStatus = &response.StatusCode
		}
		if response.Body != nil {
			defer response.Body.Close()
		}
	}
	if err == nil && response != nil && response.StatusCode >= 200 && response.StatusCode < 300 && response.Body != nil {
		body, readErr := httputil.ReadBody(response.Body)
		if readErr == nil && utf8.Valid(body) {
			var value struct {
				Status *string `json:"status"`
			}
			decoder := json.NewDecoder(bytes.NewReader(body))
			data.Healthy = decoder.Decode(&value) == nil && errors.Is(decoder.Decode(&struct{}{}), io.EOF) && value.Status != nil && *value.Status == "ok"
		}
	}
	if !data.Reachable {
		out = v2SelectionFailure(4, "proxy_unreachable", fmt.Sprintf("Proxy is not reachable at %s.", data.Address))
	} else if !data.Healthy {
		out = v2SelectionFailure(1, "proxy_health_failed", "Proxy HTTP health check failed.")
	}
	data.DurationMS = time.Since(started).Milliseconds()
	status := "unavailable"
	if data.HTTPStatus != nil {
		status = strconv.Itoa(*data.HTTPStatus)
	}
	health := "failed"
	if data.Healthy {
		health = "ok"
	}
	result := v2ProxyResource(data, fmt.Sprintf("Proxy HTTP health: %s\nAddress: %s\nHTTP status: %s\n", health, data.Address, status))
	result.ExitCode = out.ExitCode
	result.Errors = out.Errors
	return result
}

type v2ProxyFact struct {
	Name      string           `json:"name"`
	State     proxy.FactStatus `json:"state"`
	Detail    string           `json:"detail"`
	ErrorCode *string          `json:"error_code"`
	Value     any              `json:"value"`
}
type v2ProxyStatus struct {
	State       string        `json:"state"`
	Scope       string        `json:"scope"`
	StateDir    *string       `json:"state_dir"`
	CollectedAt string        `json:"collected_at"`
	DurationMS  int64         `json:"duration_ms"`
	Facts       []v2ProxyFact `json:"facts"`
}

func v2ProxyFactValue[S, P any](name string, fact proxy.Fact[S], project func(S) P) v2ProxyFact {
	result := v2ProxyFact{Name: name, State: fact.Status, ErrorCode: fact.ErrorCode, Detail: "not observed"}
	if fact.Status == proxy.FactKnown && fact.Value != nil {
		result.Value = project(*fact.Value)
		raw, _ := json.Marshal(result.Value)
		result.Detail = string(raw)
	} else if fact.Status == proxy.FactAbsent {
		result.ErrorCode = nil
	} else if fact.ErrorCode != nil {
		result.Detail = *fact.ErrorCode
	}
	return result
}
func v2ProxyNullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func v2ProxyPID(i int) *int {
	if i <= 0 {
		return nil
	}
	return &i
}
func projectV2ProxySnapshot(snapshot proxy.ProxySnapshot, root string) v2ProxyStatus {
	snapshot = proxy.ReconcileProxySnapshot(snapshot)
	snapshot = attributeV2ProxyConflicts(snapshot)
	data := v2ProxyStatus{State: "indeterminate", Scope: "live", StateDir: v2ProxyNullable(root), CollectedAt: snapshot.CollectedAt, DurationMS: snapshot.DurationMS, Facts: []v2ProxyFact{}}
	if root != "" {
		data.Scope = "candidate"
	}
	if data.CollectedAt == "" {
		data.CollectedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	switch snapshot.Verdict {
	case proxy.ProxyVerdictHealthy:
		data.State = "ready"
	case proxy.ProxyVerdictLegacy, proxy.ProxyVerdictDegraded, proxy.ProxyVerdictConflicted:
		data.State = "degraded"
	case proxy.ProxyVerdictDown:
		data.State = "stopped"
		if (snapshot.Desired.Status == proxy.FactAbsent || (snapshot.Desired.Value != nil && !snapshot.Desired.Value.Configured)) && (snapshot.Service.Status == proxy.FactAbsent || (snapshot.Service.Value != nil && snapshot.Service.Value.State == "absent")) {
			data.State = "absent"
		}
	}
	data.Facts = append(data.Facts,
		v2ProxyFactValue("inspector", snapshot.Inspector, func(v proxy.InspectorIdentity) any {
			return struct {
				Executable *string `json:"executable"`
				Version    *string `json:"version"`
				Commit     *string `json:"commit"`
				Digest     *string `json:"digest"`
			}{v2ProxyNullable(v.Executable), v2ProxyNullable(v.Version), v2ProxyNullable(v.Commit), v2ProxyNullable(v.Digest)}
		}),
		v2ProxyFactValue("desired", snapshot.Desired, func(v proxy.DesiredProxyState) any {
			return struct {
				Manager    *string `json:"manager"`
				Configured bool    `json:"configured"`
				Listener   *string `json:"listener"`
			}{v2ProxyNullable(v.Manager), v.Configured, v2ProxyNullable(v.Listener)}
		}),
		v2ProxyFactValue("service", snapshot.Service, func(v proxy.ServiceState) any {
			return struct {
				Manager    *string `json:"manager"`
				State      *string `json:"state"`
				Executable *string `json:"executable"`
				PID        *int    `json:"pid"`
			}{v2ProxyNullable(v.Manager), v2ProxyNullable(v.State), v2ProxyNullable(v.Executable), v2ProxyPID(v.PID)}
		}),
		v2ProxyFactValue("listener", snapshot.Listener, func(v proxy.ListenerState) any {
			return struct {
				State      *string `json:"state"`
				Listener   *string `json:"listener"`
				Executable *string `json:"executable"`
				PID        *int    `json:"pid"`
			}{v2ProxyNullable(v.State), v2ProxyNullable(v.Listener), v2ProxyNullable(v.Executable), v2ProxyPID(v.PID)}
		}),
		v2ProxyFactValue("process", snapshot.Process, func(v proxy.ProcessState) any {
			return struct {
				PID        int    `json:"pid"`
				Executable string `json:"executable"`
			}{v.PID, v.Executable}
		}),
		v2ProxyFactValue("runtime", snapshot.Runtime, func(v proxy.RuntimeIdentity) any {
			return struct {
				Reachable  bool    `json:"reachable"`
				PID        *int    `json:"pid"`
				Executable *string `json:"executable"`
				Health     *string `json:"health"`
			}{v.Reachable, v2ProxyPID(v.PID), v2ProxyNullable(v.Executable), v2ProxyNullable(v.Health)}
		}),
		v2ProxyFactValue("data_plane", snapshot.DataPlane, func(v proxy.DataPlaneProof) any {
			return struct {
				Proven bool    `json:"proven"`
				Code   *string `json:"code"`
			}{v.Proven, v2ProxyNullable(v.Code)}
		}),
	)
	return data
}
func executeV2ProxyStatus(ctx context.Context, inv cli.Invocation, deps v2ProxyDependencies) cli.Outcome {
	root := ""
	if v := inv.Options["state-dir"]; len(v) > 0 {
		root = v[0]
		if err := validateProxyStatePath(root, true); err != nil {
			return v2SelectionFailure(2, "invalid_argument", "Invalid state-dir: existing clean absolute non-root directory; no symlink ancestors.")
		}
	}
	data := projectV2ProxySnapshot(deps.Inspect(ctx, root), root)
	if root == "" && deps.LoadConfig != nil {
		cfg, err := deps.LoadConfig()
		if err == nil && cfg != nil {
			data.StateDir = v2ProxyNullable(cfg.ResolvedProxyResilienceStateDir())
		}
	}
	var human strings.Builder
	fmt.Fprintf(&human, "Proxy: %s\nScope: %s\n", data.State, data.Scope)
	for _, fact := range data.Facts {
		fmt.Fprintf(&human, "%s: %s — %s\n", fact.Name, fact.State, cli.HumanValue(fact.Detail))
	}
	out := v2ProxyResource(data, human.String())
	if inv.Options["strict"][0] == "true" {
		var failure cli.Outcome
		switch data.State {
		case "degraded":
			failure = v2SelectionFailure(1, "proxy_unhealthy", "Proxy readiness is degraded.")
		case "absent", "stopped":
			failure = v2SelectionFailure(3, "proxy_absent", "No ready proxy is running.")
		case "indeterminate":
			failure = v2SelectionFailure(4, "proxy_indeterminate", "Proxy readiness could not be determined.")
		}
		out.ExitCode = failure.ExitCode
		out.Errors = failure.Errors
	}
	return out
}

type v2ProxyState struct {
	bound           bool   // positive configuration binding receipt; not a public field
	StateDir        string `json:"state_dir"`
	Created         bool   `json:"created"`
	RestartRequired bool   `json:"restart_required"`
}

func executeV2ProxyInitialise(ctx context.Context, inv cli.Invocation, deps v2ProxyDependencies) cli.Outcome {
	root := inv.Options["state-dir"][0]
	cfg, err := deps.LoadConfig()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return v2ProxyIO()
	}
	if cfg != nil && cfg.ResolvedProxyResilienceStateDir() != "" && cfg.ResolvedProxyResilienceStateDir() != root {
		return v2SelectionFailure(6, "proxy_state_conflict", "Proxy state ownership conflicts with the requested directory.")
	}
	if ctx.Err() != nil {
		return cli.Outcome{}
	}
	data, err := deps.Initialise(ctx, root, cfg)
	out := v2ProxyResource(data, fmt.Sprintf("Proxy state: %s\nCreated: %t\nRestart required: %t\n", cli.HumanValue(data.StateDir), data.Created, data.RestartRequired))
	if err != nil {
		failure := v2ProxyIO()
		if errors.Is(err, errProxyStateConflict) {
			failure = v2SelectionFailure(6, "proxy_state_conflict", "Proxy state ownership conflicts with the requested directory.")
		}
		if errors.Is(err, errProxyStateObservation) {
			failure = v2ProxyIO()
			failure.Warnings = []cli.Diagnostic{{Code: "proxy_state_observation_unavailable", Message: "Cannot verify proxy service state or state adoption; resolve service inspection before retrying."}}
		}
		if data.Created || data.bound {
			failure = v2SelectionFailure(8, "proxy_state_initialise_partial", "Proxy state initialisation partially completed; inspect state before retrying.")
			failure.Data = out.Data
			failure.Human = out.Human
		}
		return failure
	}
	return out
}

type v2ProxyEvent struct {
	Event         string   `json:"event"`
	ListenAddress string   `json:"listen_address"`
	PID           int      `json:"pid"`
	Providers     []string `json:"providers"`
	Reason        *string  `json:"reason"`
}

func executeV2ProxyServe(ctx context.Context, inv cli.Invocation, session *cli.Session, deps v2ProxyDependencies) cli.Outcome {
	opts := proxyCommandOptions{MigrateLegacyManaged: inv.Options["migrate-legacy-managed"][0] == "true"}
	if inv.Supplied["port"] {
		opts.Port, _ = strconv.Atoi(inv.Options["port"][0])
	}
	address := ""
	providers := []string{}
	outputFailed := false
	ready := func(value string, enabled []string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		address = value
		providers = append([]string{}, enabled...)
		out := v2ProxyResource(v2ProxyEvent{"ready", value, os.Getpid(), providers, nil}, fmt.Sprintf("Proxy listening on %s (PID %d).\nPress Ctrl-C to stop.\n", cli.HumanValue(value), os.Getpid()))
		var err error
		if inv.JSON {
			out.Warnings = inv.Warnings
			err = cli.WriteJSON(session.Out, inv.Path, out)
		} else {
			var n int
			n, err = io.WriteString(session.Out, out.Human)
			if err == nil && n != len(out.Human) {
				err = io.ErrShortWrite
			}
		}
		outputFailed = err != nil
		return err
	}
	err := deps.Serve(ctx, opts, ready)
	if outputFailed {
		out := v2ProxyIO()
		out.Streamed = true
		return out
	}
	out := cli.Outcome{}
	if err != nil {
		out = v2ProxyIO()
		if address == "" && err == context.Canceled && ctx.Err() == context.Canceled {
			out = cli.Outcome{}
			if !errors.Is(context.Cause(ctx), errV2ProxyTerminated) {
				out = v2SelectionFailure(130, "interrupted", "Operation interrupted; inspect state before retrying.")
			}
		}
		if errors.Is(err, syscall.EADDRINUSE) {
			out = v2SelectionFailure(6, "proxy_port_in_use", fmt.Sprintf("Port %d is already in use.", proxyBoundPort(err, opts.Port)))
		}
		var invalid *proxyStartConfigurationError
		if errors.As(err, &invalid) {
			out = v2SelectionFailure(6, "proxy_config_invalid", "Proxy configuration is invalid: startup preconditions.")
		}
	} else if ctx.Err() == context.Canceled && !errors.Is(context.Cause(ctx), errV2ProxyTerminated) {
		out = v2SelectionFailure(130, "interrupted", "Operation interrupted; inspect state before retrying.")
	}
	if address != "" && (out.ExitCode == 0 || out.ExitCode == 130) {
		reason := "shutdown"
		if ctx.Err() == context.Canceled {
			reason = "interrupted"
		}
		out.Data, _ = json.Marshal(v2ProxyEvent{"stopped", address, os.Getpid(), providers, &reason})
	}
	if inv.JSON {
		out.Warnings = inv.Warnings
		if err := cli.WriteJSON(session.Out, inv.Path, out); err != nil {
			out = v2ProxyIO()
		}
	}
	// The outer renderer emits invocation warnings to stderr once.
	out.Warnings = nil
	out.Streamed = true
	return out
}

func proxyBoundPort(err error, fallback int) int {
	var op *net.OpError
	if errors.As(err, &op) {
		if address, ok := op.Addr.(*net.TCPAddr); ok {
			return address.Port
		}
	}
	return fallback
}

// Attribute only a conflict already found by the frozen reconciler. This adds
// concrete fact diagnostics without introducing another readiness decision.
func attributeV2ProxyConflicts(s proxy.ProxySnapshot) proxy.ProxySnapshot {
	if s.Verdict != proxy.ProxyVerdictConflicted {
		return s
	}
	for _, state := range []proxy.FactStatus{s.Desired.Status, s.Service.Status, s.Listener.Status, s.Process.Status, s.Runtime.Status, s.DataPlane.Status} {
		if state == proxy.FactInvalid {
			return s
		}
	}
	desired, service, listener, process, runtime := s.Desired.Value, s.Service.Value, s.Listener.Value, s.Process.Value, s.Runtime.Value
	if listener != nil && listener.State == "foreign" {
		s.Listener = proxy.InvalidFact[proxy.ListenerState]("foreign_listener")
		return s
	}
	// Preserve untouched observations; mark both members of a disagreeing pair.
	if service != nil && listener != nil {
		if service.PID != 0 && listener.PID != 0 && service.PID != listener.PID {
			s.Service = proxy.InvalidFact[proxy.ServiceState]("pid_conflict")
			s.Listener = proxy.InvalidFact[proxy.ListenerState]("pid_conflict")
			return s
		}
		if service.Executable != "" && listener.Executable != "" && service.Executable != listener.Executable {
			s.Service = proxy.InvalidFact[proxy.ServiceState]("executable_conflict")
			s.Listener = proxy.InvalidFact[proxy.ListenerState]("executable_conflict")
			return s
		}
	}
	if listener != nil && process != nil {
		if listener.PID != 0 && process.PID != 0 && listener.PID != process.PID {
			s.Listener = proxy.InvalidFact[proxy.ListenerState]("pid_conflict")
			s.Process = proxy.InvalidFact[proxy.ProcessState]("pid_conflict")
			return s
		}
		if listener.Executable != "" && process.Executable != "" && listener.Executable != process.Executable {
			s.Listener = proxy.InvalidFact[proxy.ListenerState]("executable_conflict")
			s.Process = proxy.InvalidFact[proxy.ProcessState]("executable_conflict")
			return s
		}
	}
	if listener != nil && runtime != nil {
		if listener.PID != 0 && runtime.PID != 0 && listener.PID != runtime.PID {
			s.Listener = proxy.InvalidFact[proxy.ListenerState]("pid_conflict")
			s.Runtime = proxy.InvalidFact[proxy.RuntimeIdentity]("pid_conflict")
			return s
		}
		if listener.Executable != "" && runtime.Executable != "" && listener.Executable != runtime.Executable {
			s.Listener = proxy.InvalidFact[proxy.ListenerState]("executable_conflict")
			s.Runtime = proxy.InvalidFact[proxy.RuntimeIdentity]("executable_conflict")
			return s
		}
	}
	if desired != nil && service != nil && desired.Manager != "" && desired.Manager != service.Manager {
		s.Desired = proxy.InvalidFact[proxy.DesiredProxyState]("manager_conflict")
		s.Service = proxy.InvalidFact[proxy.ServiceState]("manager_conflict")
		return s
	}
	if desired != nil && listener != nil && desired.Listener != "" && listener.Listener != "" && desired.Listener != listener.Listener {
		s.Desired = proxy.InvalidFact[proxy.DesiredProxyState]("listener_conflict")
		s.Listener = proxy.InvalidFact[proxy.ListenerState]("listener_conflict")
		return s
	}
	if listener != nil && listener.State == "listening" && (service == nil || service.State != "running" || process == nil) {
		s.Listener = proxy.InvalidFact[proxy.ListenerState]("orphan_listener")
	}
	return s
}
