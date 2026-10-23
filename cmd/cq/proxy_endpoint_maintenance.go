package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

const legacyEndpointProofMaxBytes = 16 << 10

type proxyEndpointMaintenanceDependencies struct {
	resolveRoots func() (userdirs.Roots, error)
	stdin        io.Reader
	stdout       io.Writer
	stderr       io.Writer
	stdinIsTTY   func() bool
}

func defaultProxyEndpointMaintenanceDependencies() proxyEndpointMaintenanceDependencies {
	return proxyEndpointMaintenanceDependencies{
		resolveRoots: func() (userdirs.Roots, error) { return userdirs.Default() }, stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, stdinIsTTY: isStdinTerminal,
	}
}

func isReadOnlyLegacyEndpointInspectCommand(args []string) bool {
	return len(args) >= 3 && args[0] == "proxy" && args[1] == "endpoint" && args[2] == "inspect-legacy"
}

func runProxyEndpoint(args []string) error {
	return runProxyEndpointWithDependencies(context.Background(), args, defaultProxyEndpointMaintenanceDependencies())
}

func runProxyEndpointWithDependencies(ctx context.Context, args []string, deps proxyEndpointMaintenanceDependencies) error {
	if deps.resolveRoots == nil || deps.stdin == nil || deps.stdout == nil || deps.stderr == nil || deps.stdinIsTTY == nil {
		return fmt.Errorf("proxy endpoint: missing command dependency")
	}
	if len(args) == 0 || helpRequested(args) {
		path := []string{"proxy", "endpoint"}
		if len(args) > 0 && args[0] == "inspect-legacy" {
			path = append(path, "inspect-legacy")
		} else if len(args) > 0 && args[0] == "transition-legacy" {
			path = append(path, "transition-legacy")
		}
		return writeManualHelp(deps.stdout, path)
	}
	roots, err := deps.resolveRoots()
	if err != nil {
		return fmt.Errorf("resolve CQ directories: %w", err)
	}
	endpointPath := codexprov.DefaultCredentialControlPath(roots.State)
	switch args[0] {
	case "inspect-legacy":
		if len(args) != 1 {
			return fmt.Errorf("usage: cq proxy endpoint inspect-legacy")
		}
		return inspectLegacyEndpointCommand(ctx, endpointPath, deps.stdout)
	case "transition-legacy":
		return transitionLegacyEndpointCommand(ctx, endpointPath, args[1:], deps)
	default:
		return fmt.Errorf("unknown proxy endpoint command: %s", args[0])
	}
}

// The typed seam keeps v1 encoding separate from the v2 resource envelope.
type legacyEndpointInspection struct {
	Snapshot   *codexprov.LegacyCredentialEndpointSnapshot
	Transition *codexprov.LegacyCredentialEndpointTransitionStatus
}

func inspectLegacyEndpointCommand(ctx context.Context, path string, out io.Writer) error {
	result, err := inspectLegacyEndpoint(ctx, path)
	if err != nil {
		return err
	}
	if result.Snapshot != nil {
		return encodeLegacyEndpointCommandResult(out, result.Snapshot)
	}
	return encodeLegacyEndpointCommandResult(out, result.Transition)
}

func inspectLegacyEndpoint(ctx context.Context, path string) (legacyEndpointInspection, error) {
	snapshot, snapshotErr := codexprov.InspectLegacyCredentialEndpoint(ctx, path)
	if snapshotErr == nil {
		return legacyEndpointInspection{Snapshot: &snapshot}, nil
	}
	if !errors.Is(snapshotErr, codexprov.ErrLegacyCredentialEndpointArtifacts) && !errors.Is(snapshotErr, codexprov.ErrCredentialEndpointMaintenancePending) {
		return legacyEndpointInspection{}, snapshotErr
	}
	status, statusErr := codexprov.InspectLegacyCredentialEndpointTransition(ctx, path)
	if statusErr == nil {
		return legacyEndpointInspection{Transition: &status}, nil
	}
	return legacyEndpointInspection{}, errors.Join(snapshotErr, statusErr)
}

type legacyEndpointTransitionOptions struct {
	action                   string
	snapshotFile             string
	ticketFile               string
	confirmStoppedAndDrained bool
	confirmCandidateHealthy  bool
	nonInteractive           bool
}

func transitionLegacyEndpointCommand(ctx context.Context, path string, args []string, deps proxyEndpointMaintenanceDependencies) (resultErr error) {
	opts, err := parseLegacyEndpointTransitionOptions(args)
	if err != nil {
		return err
	}
	if opts.action == "finalise" {
		if err := confirmLegacyEndpointCandidateHealthy(opts, deps); err != nil {
			return err
		}
	} else {
		if err := confirmLegacyEndpointStoppedAndDrained(opts, deps); err != nil {
			return err
		}
	}
	status, err := executeLegacyEndpointTransition(ctx, path, opts, legacyEndpointTransitionOperations{
		ReadProof: func(_ context.Context, file string) ([]byte, error) {
			return fsutil.ReadSecureFile(fsutil.OSFileSystem{}, file, legacyEndpointProofMaxBytes)
		},
		Resume: func(ctx context.Context, path string, ticket codexprov.LegacyCredentialEndpointTransitionTicket, drain codexprov.DrainAuthority, _ codexprov.LegacyCredentialEndpointAction) (*codexprov.LegacyCredentialEndpointTransition, error) {
			return codexprov.ResumeLegacyCredentialEndpointTransition(ctx, path, ticket, drain)
		},
	})
	if err != nil {
		return err
	}
	return encodeLegacyEndpointCommandResult(deps.stdout, status)
}

type legacyEndpointProofError struct{ error }

func (e *legacyEndpointProofError) Unwrap() error { return e.error }

type legacyEndpointTransitionOperations struct {
	ReadProof func(context.Context, string) ([]byte, error)
	Reopen    func(context.Context, string, codexprov.LegacyCredentialEndpointTransitionTicket, codexprov.DrainAuthority) (codexprov.LegacyCredentialEndpointTransitionStatus, error)
	Resume    func(context.Context, string, codexprov.LegacyCredentialEndpointTransitionTicket, codexprov.DrainAuthority, codexprov.LegacyCredentialEndpointAction) (*codexprov.LegacyCredentialEndpointTransition, error)
}

// Consent is checked by the command before entering this synchronous operation.
// Finalise uses the live owner's verifier; no drain authority is passed to it.
func executeLegacyEndpointTransition(ctx context.Context, path string, opts legacyEndpointTransitionOptions, ops legacyEndpointTransitionOperations) (status codexprov.LegacyCredentialEndpointTransitionStatus, resultErr error) {
	if err := ctx.Err(); err != nil {
		return status, err
	}
	authority := codexprov.DrainAuthorityFunc(func(assertCtx context.Context, assertedPath string) error {
		if assertedPath != path {
			return codexprov.ErrCredentialEndpointMaintenanceDrainRequired
		}
		return assertCtx.Err()
	})
	var transition *codexprov.LegacyCredentialEndpointTransition
	switch opts.action {
	case "prepare":
		data, err := ops.ReadProof(ctx, opts.snapshotFile)
		if err != nil {
			return status, fmt.Errorf("read snapshot file: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return status, err
		}
		snapshot, err := codexprov.ParseLegacyCredentialEndpointSnapshot(data)
		if err != nil {
			return status, &legacyEndpointProofError{err}
		}
		if snapshot.Path != path {
			return status, codexprov.ErrCredentialEndpointMaintenanceSnapshotChanged
		}
		transition, err = codexprov.PrepareLegacyCredentialEndpointTransition(ctx, path, snapshot, authority)
		if err != nil {
			return status, err
		}
	case "resume", "activate", "rollback", "finalise":
		data, err := ops.ReadProof(ctx, opts.ticketFile)
		if err != nil {
			return status, fmt.Errorf("read ticket file: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return status, err
		}
		ticket, err := codexprov.ParseLegacyCredentialEndpointTransitionTicket(data)
		if err != nil {
			return status, &legacyEndpointProofError{err}
		}
		if ticket.Path != path {
			return status, codexprov.ErrCredentialEndpointMaintenanceTicketMismatch
		}
		if opts.action == "finalise" {
			if err := codexprov.FinaliseLegacyCredentialEndpointTransition(ctx, path, ticket); err != nil {
				return status, err
			}
			return codexprov.LegacyCredentialEndpointTransitionStatus{State: codexprov.CredentialEndpointMaintenanceCommitted, Ticket: ticket}, nil
		}
		if opts.action == "resume" && ops.Reopen != nil {
			return ops.Reopen(ctx, path, ticket, authority)
		}
		transition, err = ops.Resume(ctx, path, ticket, authority, codexprov.LegacyCredentialEndpointAction(opts.action))
		if err != nil {
			return status, err
		}
	default:
		return status, fmt.Errorf("unknown transition action: %s", opts.action)
	}
	defer func() { resultErr = errors.Join(resultErr, transition.Close()) }()
	switch opts.action {
	case "activate":
		if err := transition.Activate(ctx); err != nil {
			return status, err
		}
	case "rollback":
		if err := transition.Rollback(ctx); err != nil {
			return status, err
		}
	}
	return codexprov.LegacyCredentialEndpointTransitionStatus{State: transition.State(), Ticket: transition.Ticket()}, nil
}

func parseLegacyEndpointTransitionOptions(args []string) (legacyEndpointTransitionOptions, error) {
	var opts legacyEndpointTransitionOptions
	if len(args) == 0 {
		return opts, fmt.Errorf("usage: cq proxy endpoint transition-legacy <prepare|resume|activate|finalise|rollback>")
	}
	opts.action = args[0]
	for index := 1; index < len(args); index++ {
		switch args[index] {
		case "--snapshot-file":
			if opts.snapshotFile != "" || index+1 >= len(args) {
				return opts, fmt.Errorf("transition-legacy: --snapshot-file requires exactly one value")
			}
			index++
			opts.snapshotFile = args[index]
		case "--ticket-file":
			if opts.ticketFile != "" || index+1 >= len(args) {
				return opts, fmt.Errorf("transition-legacy: --ticket-file requires exactly one value")
			}
			index++
			opts.ticketFile = args[index]
		case "--confirm-stopped-and-drained":
			if opts.confirmStoppedAndDrained {
				return opts, fmt.Errorf("transition-legacy: duplicate --confirm-stopped-and-drained")
			}
			opts.confirmStoppedAndDrained = true
		case "--confirm-candidate-healthy":
			if opts.confirmCandidateHealthy {
				return opts, fmt.Errorf("transition-legacy: duplicate --confirm-candidate-healthy")
			}
			opts.confirmCandidateHealthy = true
		case "--non-interactive":
			if opts.nonInteractive {
				return opts, fmt.Errorf("transition-legacy: duplicate --non-interactive")
			}
			opts.nonInteractive = true
		default:
			return opts, fmt.Errorf("transition-legacy: unexpected argument %q", args[index])
		}
	}
	switch opts.action {
	case "prepare":
		if opts.snapshotFile == "" || opts.ticketFile != "" {
			return opts, fmt.Errorf("transition-legacy prepare requires --snapshot-file only")
		}
		if !filepath.IsAbs(opts.snapshotFile) || filepath.Clean(opts.snapshotFile) != opts.snapshotFile {
			return opts, fmt.Errorf("transition-legacy requires an absolute clean snapshot file path")
		}
	case "resume", "activate", "rollback", "finalise":
		if opts.ticketFile == "" || opts.snapshotFile != "" {
			return opts, fmt.Errorf("transition-legacy %s requires --ticket-file only", opts.action)
		}
		if !filepath.IsAbs(opts.ticketFile) || filepath.Clean(opts.ticketFile) != opts.ticketFile {
			return opts, fmt.Errorf("transition-legacy requires an absolute clean ticket file path")
		}
	default:
		return opts, fmt.Errorf("unknown transition action: %s", opts.action)
	}
	if opts.action == "finalise" {
		if opts.confirmStoppedAndDrained {
			return opts, fmt.Errorf("transition-legacy finalise does not accept --confirm-stopped-and-drained")
		}
		if !opts.confirmCandidateHealthy {
			return opts, fmt.Errorf("transition-legacy finalise requires --confirm-candidate-healthy")
		}
	} else {
		if opts.confirmCandidateHealthy {
			return opts, fmt.Errorf("transition-legacy %s does not accept --confirm-candidate-healthy", opts.action)
		}
		if !opts.confirmStoppedAndDrained {
			return opts, fmt.Errorf("transition-legacy requires --confirm-stopped-and-drained")
		}
	}
	return opts, nil
}

func confirmLegacyEndpointStoppedAndDrained(opts legacyEndpointTransitionOptions, deps proxyEndpointMaintenanceDependencies) error {
	if opts.nonInteractive {
		return nil
	}
	if !deps.stdinIsTTY() {
		return fmt.Errorf("transition-legacy requires --non-interactive when stdin is not a TTY")
	}
	if _, err := fmt.Fprint(deps.stderr, "Type stopped-and-drained to confirm the proxy remains stopped and drained: "); err != nil {
		return err
	}
	line, err := bufio.NewReader(io.LimitReader(deps.stdin, 256)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.TrimSpace(line) != "stopped-and-drained" {
		return fmt.Errorf("stopped-and-drained confirmation declined")
	}
	return nil
}

func confirmLegacyEndpointCandidateHealthy(opts legacyEndpointTransitionOptions, deps proxyEndpointMaintenanceDependencies) error {
	if opts.nonInteractive {
		return nil
	}
	if !deps.stdinIsTTY() {
		return fmt.Errorf("transition-legacy finalise requires --non-interactive when stdin is not a TTY")
	}
	if _, err := fmt.Fprint(deps.stderr, "Type candidate-healthy to confirm the exact live candidate passed health verification: "); err != nil {
		return err
	}
	line, err := bufio.NewReader(io.LimitReader(deps.stdin, 256)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.TrimSpace(line) != "candidate-healthy" {
		return fmt.Errorf("candidate-healthy confirmation declined")
	}
	return nil
}

func encodeLegacyEndpointCommandResult(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
