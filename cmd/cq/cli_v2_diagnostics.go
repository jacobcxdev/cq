package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func lookupV2RoutingDiagnostics(path string) (cli.Handler, bool) {
	switch path {
	case "codex proxy lease invalidate", "codex proxy trace":
		return handleV2RoutingDiagnostics, true
	}
	return nil, false
}

type v2DiagnosticsDependencies struct {
	Lease proxyLeaseDependencies
	Trace proxyTraceDependencies
}

func handleV2RoutingDiagnostics(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return handleV2RoutingDiagnosticsWithPreparation(ctx, inv, session, time.Now, func(context.Context) (v2DiagnosticsDependencies, error) {
		roots, err := userdirs.Default(userdirs.ConfigRoot, userdirs.StateRoot)
		if err != nil {
			return v2DiagnosticsDependencies{}, err
		}
		paths := proxy.PathsForRoots(roots)
		load := func() (*proxy.Config, error) { return proxy.LoadExistingConfigAt(paths) }
		return v2DiagnosticsDependencies{Lease: proxyLeaseDependencies{LoadConfig: load, Doer: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("proxy lease redirect refused") }}}, Trace: proxyTraceDependencies{LoadConfig: load}}, nil
	})
}
func handleV2RoutingDiagnosticsWithDependencies(ctx context.Context, inv cli.Invocation, session *cli.Session, deps v2DiagnosticsDependencies) cli.Outcome {
	now := deps.Trace.Now
	if now == nil {
		now = time.Now
	}
	return handleV2RoutingDiagnosticsWithPreparation(ctx, inv, session, now, func(context.Context) (v2DiagnosticsDependencies, error) { return deps, nil })
}
func handleV2RoutingDiagnosticsWithPreparation(parent context.Context, inv cli.Invocation, session *cli.Session, now func() time.Time, prepare func(context.Context) (v2DiagnosticsDependencies, error)) cli.Outcome {
	// Capture both the budget and --since origin before path or config preparation.
	started := now()
	ctx := parent
	if values := inv.Options["timeout"]; len(values) > 0 {
		timeout, _ := time.ParseDuration(values[0])
		budget := cli.BeginBudget(parent, timeout, 0)
		defer budget.Close()
		ctx = budget.Work()
	}
	trace := inv.Path == "codex proxy trace"
	records := 0
	outputFailed := false
	finish := func(out cli.Outcome) cli.Outcome {
		if !trace {
			return out
		}
		if ctx.Err() == context.DeadlineExceeded {
			out = v2SelectionFailure(7, "trace_timeout", "Trace reading timed out.")
		} else if ctx.Err() == context.Canceled {
			out = v2SelectionFailure(130, "trace_interrupted", "Trace following interrupted.")
		}
		if out.ExitCode == 0 || out.ExitCode == 7 || out.ExitCode == 130 {
			reason := "completed"
			if out.ExitCode == 7 {
				reason = "timeout"
			}
			if out.ExitCode == 130 {
				reason = "interrupted"
			}
			out.Data, _ = json.Marshal(struct {
				End v2TraceEnd `json:"end"`
			}{v2TraceEnd{records, reason}})
		}
		if outputFailed {
			out = v2SelectionFailure(1, "routing_io_failed", "Routing operation failed: write trace output.")
		} else if inv.JSON {
			out.Warnings = inv.Warnings
			if err := cli.WriteJSON(session.Out, inv.Path, out); err != nil {
				out = v2SelectionFailure(1, "routing_io_failed", "Routing operation failed: write trace output.")
			}
			out.Warnings = nil // cli.Run owns the single stderr warning rendering.
		}
		out.Streamed = true
		return out
	}
	if out, stopped := v2SelectionStopped(parent, ctx); stopped {
		return finish(out)
	}
	deps, err := prepare(ctx)
	if out, stopped := v2SelectionStopped(parent, ctx); stopped {
		return finish(out)
	}
	if err != nil {
		if out, ok := v2EnvironmentFailure(err); ok {
			return finish(out)
		}
		return finish(v2SelectionFailure(1, "routing_io_failed", "Routing operation failed: resolve proxy paths."))
	}
	if !trace {
		port := 0
		if values := inv.Options["port"]; len(values) > 0 {
			port, _ = strconv.Atoi(values[0])
		}
		result, err := requestProxyLeaseInvalidation(ctx, port, deps.Lease)
		if out, stopped := v2SelectionStopped(parent, ctx); stopped {
			return out
		}
		if err != nil {
			return v2LeaseFailure(err)
		}
		data, _ := json.Marshal(result)
		return cli.Outcome{Data: data, Human: fmt.Sprintf("Invalidated leases: %d\nJournal generation: %d\n", result.InvalidatedLeases, result.JournalGeneration)}
	}
	cfg, err := deps.Trace.LoadConfig()
	if ctx.Err() != nil {
		return finish(cli.Outcome{})
	}
	if err != nil || cfg == nil {
		return finish(v2SelectionFailure(1, "routing_io_failed", "Routing operation failed: load proxy configuration."))
	}
	payload := inv.Options["payload"][0] == "true"
	path := cfg.DiagnosticsLog
	if payload {
		path = cfg.PayloadDiagnosticsLog
	}
	if path == "" {
		return finish(v2SelectionFailure(4, "trace_not_configured", "Selected trace log is not configured."))
	}
	filter := proxyTraceFilter{payload: payload, ctx: ctx, accept: func(record proxyTraceRecord) bool { return v2TraceRecordValid(record, payload) }}
	if v := inv.Options["trace"]; len(v) > 0 {
		filter.traceID = v[0]
	}
	if v := inv.Options["since"]; len(v) > 0 {
		duration, _ := time.ParseDuration(v[0])
		filter.since = started.Add(-duration)
	}
	if v := inv.Options["session"]; len(v) > 0 {
		keys := proxy.CodexTraceSessionKeys(v[0])
		if len(keys) == 0 {
			return finish(v2SelectionFailure(2, "routing_invalid_argument", "Invalid argument: empty session selector."))
		}
		filter.session = make(map[string]struct{}, len(keys))
		for _, key := range keys {
			filter.session[key] = struct{}{}
		}
	}
	emit := func(record proxyTraceRecord) error {
		record.Time = record.Time.UTC()
		if err := ctx.Err(); err != nil {
			return err
		}
		if inv.JSON {
			data, err := v2TraceRecordData(record, payload)
			if err != nil {
				return err
			}
			err = cli.WriteJSON(session.Out, inv.Path, cli.Outcome{Data: data, Warnings: inv.Warnings})
			if err != nil {
				outputFailed = true
				return err
			}
		} else if err := writeProxyTraceRecord(session.Out, record, false); err != nil {
			outputFailed = true
			return err
		}
		records++
		return nil
	}
	follow := inv.Options["follow"][0] == "true"
	var initial []proxyTraceRecord
	var follower *proxyTraceFollower
	if follow {
		initial, follower, err = readProxyTraceRecordsForFollow(path, filter)
	} else {
		initial, err = readProxyTraceRecords(path, filter)
	}
	if err == nil {
		tail, _ := strconv.Atoi(inv.Options["tail"][0])
		if tail > 0 && len(initial) > tail {
			initial = initial[len(initial)-tail:]
		}
		for _, record := range initial {
			if err = emit(record); err != nil {
				break
			}
		}
	}
	if err == nil && follow {
		err = followProxyTraceWithEmitter(ctx, path, filter, emit, follower)
	} else if follower != nil {
		err = errors.Join(err, follower.Close())
	}
	if err != nil {
		var gap *proxyTraceHistoryGap
		if errors.As(err, &gap) {
			return finish(v2SelectionFailure(8, "trace_history_gap", "Trace history has a gap: "+gap.detail+"."))
		}
		return finish(v2SelectionFailure(1, "routing_io_failed", "Routing operation failed: read trace log."))
	}
	return finish(cli.Outcome{})
}

func v2LeaseFailure(err error) cli.Outcome {
	var failure *proxyLeaseError
	if !errors.As(err, &failure) {
		return v2SelectionFailure(1, "routing_io_failed", "Routing operation failed: lease control.")
	}
	switch {
	case failure.kind == "auth" || failure.status == 401 || failure.status == 403:
		return v2SelectionFailure(5, "routing_auth_failed", "Local proxy authentication failed.")
	case failure.kind == "io":
		return v2SelectionFailure(1, "routing_io_failed", "Routing operation failed: lease control.")
	case failure.status == 503 && failure.code == "lease_journal_unavailable":
		return v2SelectionFailure(4, "lease_journal_unavailable", "Lease journal is unavailable for invalidation.")
	case failure.status == 503 && failure.code == "routing_io_failed":
		return v2SelectionFailure(1, "routing_io_failed", "Routing operation failed: persist lease invalidation; inspect current state before retrying.")
	case failure.status == 409 || (failure.status == 503 && failure.code == "routing_conflict"):
		return v2SelectionFailure(6, "routing_conflict", "Routing state conflict: lease invalidation rejected.")
	default:
		return v2SelectionFailure(4, "routing_control_unavailable", "Running CQ proxy control is unavailable.")
	}
}

type v2TraceEnd struct {
	Records int    `json:"records"`
	Reason  string `json:"reason"`
}

type v2PayloadRecord struct {
	Kind          string              `json:"kind"`
	Time          time.Time           `json:"time"`
	EventType     string              `json:"event_type"`
	TraceID       string              `json:"trace_id"`
	ConnectionID  string              `json:"connection_id"`
	Transport     string              `json:"transport"`
	Direction     string              `json:"direction"`
	Method        string              `json:"method"`
	Path          string              `json:"path"`
	Provider      string              `json:"provider"`
	RouteKind     string              `json:"route_kind"`
	Model         string              `json:"model"`
	ClientKind    string              `json:"client_kind"`
	SessionKey    string              `json:"session_key"`
	SessionSource string              `json:"session_source"`
	ThreadKey     string              `json:"thread_key"`
	SessionSignal string              `json:"session_signal"`
	FrameType     string              `json:"frame_type"`
	AccountHint   string              `json:"account_hint"`
	BodyEncoding  string              `json:"body_encoding"`
	FrameIndex    int                 `json:"frame_index"`
	Attempt       int                 `json:"attempt"`
	StatusCode    int                 `json:"status_code"`
	BodyBytes     int                 `json:"body_bytes"`
	Headers       map[string][]string `json:"headers"`
	Complete      bool                `json:"complete"`
	Truncated     bool                `json:"truncated"`
	Body          json.RawMessage     `json:"body"`
}

// Apply public resource validation before tail, keeping malformed records out
// of the result without letting them displace valid retained history.
func v2TraceRecordValid(record proxyTraceRecord, payload bool) bool {
	if !payload {
		return record.Attempt >= 0 && record.StatusCode >= 0 && record.UpstreamStatus >= 0 && record.CloseCode >= 0
	}
	var value v2PayloadRecord
	return json.Unmarshal(record.Raw, &value) == nil && value.FrameIndex >= 0 && value.Attempt >= 0 && value.StatusCode >= 0 && value.BodyBytes >= 0
}

func v2TraceRecordData(record proxyTraceRecord, payload bool) (json.RawMessage, error) {
	record.Time = record.Time.UTC()
	if !payload {
		return json.Marshal(struct {
			Record struct {
				Kind string `json:"kind"`
				proxyTraceRecord
			} `json:"record"`
		}{Record: struct {
			Kind string `json:"kind"`
			proxyTraceRecord
		}{"route", record}})
	}
	var value v2PayloadRecord
	if err := json.Unmarshal(record.Raw, &value); err != nil {
		return nil, err
	}
	value.Kind = "payload"
	value.Time = value.Time.UTC()
	if value.Headers == nil {
		value.Headers = map[string][]string{}
	}
	for key := range value.Headers {
		if value.Headers[key] == nil {
			value.Headers[key] = []string{}
		}
		if v2TraceAuthKey(key) {
			value.Headers[key] = []string{"[REDACTED]"}
		}
	}
	if len(value.Body) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(value.Body))
		decoder.UseNumber()
		var body any
		if err := decoder.Decode(&body); err != nil {
			return nil, err
		}
		v2RedactTraceBody(body)
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		value.Body = encoded
	}
	return json.Marshal(struct {
		Record v2PayloadRecord `json:"record"`
	}{value})
}
func v2TraceAuthKey(key string) bool {
	switch strings.ToLower(key) {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key", "access_token", "refresh_token", "id_token", "api_key", "password", "client_secret":
		return true
	}
	return false
}
func v2RedactTraceBody(value any) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if v2TraceAuthKey(key) {
				v[key] = "[REDACTED]"
			} else {
				v2RedactTraceBody(child)
			}
		}
	case []any:
		for _, child := range v {
			v2RedactTraceBody(child)
		}
	}
}
