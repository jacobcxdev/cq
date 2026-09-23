package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func lookupV2Validation(path string) (cli.Handler, bool) {
	switch path {
	case "codex proxy fixture create", "codex proxy readiness show", "codex proxy validate http", "codex proxy validate websocket":
		return handleV2Validation, true
	}
	return nil, false
}

type v2ValidationDependencies struct {
	stateDir   func() (string, error)
	loadMarker func(string, proxy.CodexRoutingTransport) (proxy.CodexReadinessMarker, error)
	http       func(context.Context, int, string) error
	websocket  func(context.Context, context.Context, string, string, string, string) (proxy.CodexReadinessMarker, error)
}

func handleV2Validation(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return handleV2ValidationWithPreparation(ctx, inv, session, func(context.Context) (v2ValidationDependencies, error) {
		return v2ValidationDependencies{
			stateDir: func() (string, error) {
				paths, err := proxy.ResolveDefaultPaths(userdirs.StateRoot)
				return paths.StateDir, err
			},
			loadMarker: proxy.LoadCodexReadinessMarker,
			http:       runCanonicalProxyValidateHTTP,
			websocket:  proxy.RunCodexInstalledWebSocketValidationWithCleanup,
		}, nil
	})
}
func handleV2ValidationWithPreparation(parent context.Context, inv cli.Invocation, _ *cli.Session, prepare func(context.Context) (v2ValidationDependencies, error)) cli.Outcome {
	option := func(name string) string {
		if v := inv.Options[name]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	if inv.Path == "codex proxy fixture create" {
		return createV2Fixture(inv)
	}
	ctx, cleanup := parent, parent
	if timeout := option("timeout"); timeout != "" {
		duration, _ := time.ParseDuration(timeout)
		reserve := time.Duration(0)
		if inv.Path == "codex proxy validate websocket" {
			reserve = 5 * time.Second
		}
		budget := cli.BeginBudget(parent, duration, reserve)
		defer budget.Close()
		ctx, cleanup = budget.Work(), budget.Cleanup()
	}
	finish := func(out cli.Outcome, err error) cli.Outcome {
		if parent.Err() == context.Canceled || ctx.Err() == context.Canceled || errors.Is(err, context.Canceled) {
			return v2SelectionFailure(130, "interrupted", "Operation interrupted; inspect state before retrying.")
		}
		if ctx.Err() == context.DeadlineExceeded || cleanup.Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) {
			message := "The HTTP validation operation timed out; inspect readiness before retrying."
			if inv.Path == "codex proxy validate websocket" {
				message = "WebSocket validation timed out; readiness was not recorded."
			}
			return v2SelectionFailure(7, "validation_timeout", message)
		}
		return out
	}
	if ctx.Err() != nil {
		return finish(cli.Outcome{}, ctx.Err())
	}
	port, _ := strconv.Atoi(option("port"))
	if inv.Path == "codex proxy validate http" && (port < 1 || port > 65535 || port == proxy.DefaultPort) {
		return v2SelectionFailure(2, "invalid_option_value", "Invalid value for --port.")
	}
	deps, err := prepare(ctx)
	if ctx.Err() != nil {
		return finish(cli.Outcome{}, ctx.Err())
	}
	if err != nil {
		return finish(v2SelectionFailure(1, "validation_request_failed", "The HTTP validation request failed."), err)
	}
	if inv.Path == "codex proxy validate http" {
		err := deps.http(ctx, port, version)
		out := cli.Outcome{}
		switch {
		case errors.Is(err, errValidationCleanupFailed):
			out = v2SelectionFailure(1, "validation_request_failed", "The HTTP validation request failed.")
		case errors.Is(err, errValidationCandidateChanged):
			out = v2SelectionFailure(6, "validation_candidate_changed", "The candidate service binding changed; no validation was requested.")
		case errors.Is(err, errValidationCandidateUnavailable):
			out = v2SelectionFailure(4, "validation_candidate_unavailable", "The installed candidate service is unavailable.")
		case err != nil:
			out = v2SelectionFailure(1, "validation_request_failed", "The HTTP validation request failed.")
		default:
			out.Data, _ = json.Marshal(struct {
				State    string `json:"state"`
				Port     int    `json:"port"`
				Complete bool   `json:"validation_complete"`
				Outcome  string `json:"outcome"`
			}{"requested", port, false, "accepted"})
			out.Human = fmt.Sprintf("HTTP validation requested on candidate port %d.\nValidation has not completed.\n", port)
		}
		return finish(out, err)
	}
	stateDir := option("state-dir")
	if stateDir == "" {
		stateDir, err = deps.stateDir()
	}
	if err == nil {
		err = validateV2ReadinessDirectory(stateDir)
	}
	if ctx.Err() != nil {
		return finish(cli.Outcome{}, ctx.Err())
	}
	if err != nil {
		if inv.Path == "codex proxy readiness show" {
			return v2SelectionFailure(1, "readiness_invalid", "HTTP readiness evidence could not be read.")
		}
		return v2SelectionFailure(1, "validation_failed", "WebSocket validation failed; readiness was not recorded.")
	}
	build := option("client-build")
	var marker proxy.CodexReadinessMarker
	if inv.Path == "codex proxy readiness show" {
		marker, err = deps.loadMarker(stateDir, proxy.CodexRoutingHTTP)
		if errors.Is(err, os.ErrNotExist) {
			return v2SelectionFailure(3, "readiness_missing", "No HTTP readiness evidence is recorded.")
		}
		if err != nil {
			return v2SelectionFailure(1, "readiness_invalid", "HTTP readiness evidence could not be read.")
		}
		required, _ := proxy.DefaultCodexRoutingRequirements(version, build)
		if proxy.ValidateCodexReadinessMarker(marker, required) != nil {
			return v2SelectionFailure(6, "readiness_stale", "HTTP readiness evidence does not match the requested build.")
		}
		data, _ := json.Marshal(struct {
			Transport   string            `json:"transport"`
			ClientBuild string            `json:"client_build"`
			Current     bool              `json:"current"`
			ValidatedAt string            `json:"validated_at"`
			Marker      v2ReadinessMarker `json:"marker"`
		}{"http", build, true, marker.ValidatedAt.UTC().Format(time.RFC3339), projectV2ReadinessMarker(marker)})
		return cli.Outcome{Data: data, Human: fmt.Sprintf("HTTP readiness: current\nCodex build: %s\nValidated: %s\n", cli.HumanValue(build), marker.ValidatedAt.UTC().Format(time.RFC3339))}
	}
	executable := option("client-executable")
	if executable != "" {
		executable, err = filepath.Abs(executable)
	}
	if err == nil {
		marker, err = deps.websocket(ctx, cleanup, version, build, executable, stateDir)
	}
	out := cli.Outcome{}
	switch {
	case errors.Is(err, proxy.ErrCodexValidationClientUnavailable):
		out = v2SelectionFailure(4, "validation_client_unavailable", "The Codex client cannot be validated on this installation.")
	case errors.Is(err, proxy.ErrCodexValidationBuildMismatch):
		out = v2SelectionFailure(6, "validation_build_mismatch", "The Codex executable does not match --client-build.")
	case err != nil:
		out = v2SelectionFailure(1, "validation_failed", "WebSocket validation failed; readiness was not recorded.")
	default:
		out.Data, _ = json.Marshal(struct {
			Transport   string            `json:"transport"`
			ClientBuild string            `json:"client_build"`
			ValidatedAt string            `json:"validated_at"`
			Marker      v2ReadinessMarker `json:"marker"`
		}{"websocket", marker.ClientBuild, marker.ValidatedAt.UTC().Format(time.RFC3339), projectV2ReadinessMarker(marker)})
		out.Human = fmt.Sprintf("WebSocket validation: passed\nCodex build: %s\nValidated: %s\n", cli.HumanValue(marker.ClientBuild), marker.ValidatedAt.UTC().Format(time.RFC3339))
	}
	return finish(out, err)
}

func createV2Fixture(inv cli.Invocation) cli.Outcome {
	input, output := inv.Options["input"][0], inv.Options["output"][0]
	invalid := func() cli.Outcome {
		return v2SelectionFailure(2, "fixture_input_invalid", "The request cannot be converted to a sanitised fixture.")
	}
	ioFailed := func() cli.Outcome {
		return v2SelectionFailure(1, "fixture_io_failed", "The fixture file could not be read or written.")
	}
	exists := func() cli.Outcome {
		return v2SelectionFailure(6, "fixture_output_exists", "The output file already exists.")
	}
	// Lstat rejects dangling symlinks too; the atomic writer fences creation races.
	if _, err := os.Lstat(output); err == nil {
		return exists()
	} else if !errors.Is(err, os.ErrNotExist) {
		return ioFailed()
	}
	info, err := os.Stat(input)
	if err != nil {
		return ioFailed()
	}
	if !info.Mode().IsRegular() || info.Size() > 2<<20 {
		return invalid()
	}
	file, err := os.Open(input)
	if err != nil {
		return ioFailed()
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return ioFailed()
	}
	if !opened.Mode().IsRegular() {
		return invalid()
	}
	body, err := io.ReadAll(io.LimitReader(file, (2<<20)+1))
	if err != nil {
		return ioFailed()
	}
	encoding := "auto"
	if v := inv.Options["content-encoding"]; len(v) > 0 {
		encoding = v[0]
	}
	metadata := ""
	if v := inv.Options["metadata-json"]; len(v) > 0 {
		metadata = v[0]
		if metadata == "" {
			return invalid()
		}
	}
	fixture, err := proxy.BuildSanitisedCodexFixture(body, encoding, metadata, time.Now())
	if err != nil {
		return invalid()
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return ioFailed()
	}
	if err := proxy.WriteSanitisedCodexFixture(output, fixture); err != nil {
		if errors.Is(err, os.ErrExist) {
			return exists()
		}
		return ioFailed()
	}
	data, _ := json.Marshal(struct {
		OutputPath string                      `json:"output_path"`
		Fixture    proxy.SanitisedCodexFixture `json:"fixture"`
	}{output, fixture})
	return cli.Outcome{Data: data, Human: fmt.Sprintf("Created sanitised fixture: %s\n", cli.HumanValue(output))}
}

func validateV2ReadinessDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("noncanonical readiness directory")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return errors.New("unsafe readiness directory")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	err := fsutil.ValidateOwnerControlledDirectory(fsutil.OSFileSystem{}, path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

type v2ReadinessMarker struct {
	proxy.CodexReadinessMarker
	CQExecutableSHA256     *string `json:"cq_executable_sha256"`
	ClientExecutableSHA256 *string `json:"client_executable_sha256"`
	ServiceIdentitySHA256  *string `json:"service_identity_sha256"`
	ServiceKind            *string `json:"service_kind"`
}

func projectV2ReadinessMarker(marker proxy.CodexReadinessMarker) v2ReadinessMarker {
	marker.CompletedGates = append([]string{}, marker.CompletedGates...)
	sort.Strings(marker.CompletedGates)
	marker.ValidatedAt = marker.ValidatedAt.UTC()
	return v2ReadinessMarker{marker, v2AccountString(marker.CQExecutableSHA256), v2AccountString(marker.ClientExecutableSHA256), v2AccountString(marker.ServiceIdentitySHA256), v2AccountString(marker.ServiceKind)}
}
