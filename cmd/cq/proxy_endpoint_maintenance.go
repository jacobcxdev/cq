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
)

const legacyEndpointProofMaxBytes = 16 << 10

type proxyEndpointMaintenanceDependencies struct {
	homeDir    func() (string, error)
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	stdinIsTTY func() bool
}

func defaultProxyEndpointMaintenanceDependencies() proxyEndpointMaintenanceDependencies {
	return proxyEndpointMaintenanceDependencies{
		homeDir: os.UserHomeDir, stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, stdinIsTTY: isStdinTerminal,
	}
}

func isReadOnlyLegacyEndpointInspectCommand(args []string) bool {
	return len(args) >= 3 && args[0] == "proxy" && args[1] == "endpoint" && args[2] == "inspect-legacy"
}

func runProxyEndpoint(args []string) error {
	return runProxyEndpointWithDependencies(context.Background(), args, defaultProxyEndpointMaintenanceDependencies())
}

func runProxyEndpointWithDependencies(ctx context.Context, args []string, deps proxyEndpointMaintenanceDependencies) error {
	if deps.homeDir == nil || deps.stdin == nil || deps.stdout == nil || deps.stderr == nil || deps.stdinIsTTY == nil {
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
	home, err := deps.homeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	endpointPath := codexprov.DefaultCredentialControlPath(home)
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

func inspectLegacyEndpointCommand(ctx context.Context, path string, out io.Writer) error {
	snapshot, snapshotErr := codexprov.InspectLegacyCredentialEndpoint(ctx, path)
	if snapshotErr == nil {
		return encodeLegacyEndpointCommandResult(out, snapshot)
	}
	if !errors.Is(snapshotErr, codexprov.ErrLegacyCredentialEndpointArtifacts) && !errors.Is(snapshotErr, codexprov.ErrCredentialEndpointMaintenancePending) {
		return snapshotErr
	}
	status, statusErr := codexprov.InspectLegacyCredentialEndpointTransition(ctx, path)
	if statusErr == nil {
		return encodeLegacyEndpointCommandResult(out, status)
	}
	return errors.Join(snapshotErr, statusErr)
}

type legacyEndpointTransitionOptions struct {
	action                   string
	snapshotFile             string
	ticketFile               string
	confirmStoppedAndDrained bool
	nonInteractive           bool
}

func transitionLegacyEndpointCommand(ctx context.Context, path string, args []string, deps proxyEndpointMaintenanceDependencies) (resultErr error) {
	opts, err := parseLegacyEndpointTransitionOptions(args)
	if err != nil {
		return err
	}
	if err := confirmLegacyEndpointStoppedAndDrained(opts, deps); err != nil {
		return err
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
		data, err := fsutil.ReadSecureFile(fsutil.OSFileSystem{}, opts.snapshotFile, legacyEndpointProofMaxBytes)
		if err != nil {
			return fmt.Errorf("read snapshot file: %w", err)
		}
		snapshot, err := codexprov.ParseLegacyCredentialEndpointSnapshot(data)
		if err != nil {
			return err
		}
		if snapshot.Path != path {
			return codexprov.ErrCredentialEndpointMaintenanceSnapshotChanged
		}
		transition, err = codexprov.PrepareLegacyCredentialEndpointTransition(ctx, path, snapshot, authority)
		if err != nil {
			return err
		}
	case "resume", "commit", "rollback":
		data, err := fsutil.ReadSecureFile(fsutil.OSFileSystem{}, opts.ticketFile, legacyEndpointProofMaxBytes)
		if err != nil {
			return fmt.Errorf("read ticket file: %w", err)
		}
		ticket, err := codexprov.ParseLegacyCredentialEndpointTransitionTicket(data)
		if err != nil {
			return err
		}
		if ticket.Path != path {
			return codexprov.ErrCredentialEndpointMaintenanceTicketMismatch
		}
		transition, err = codexprov.ResumeLegacyCredentialEndpointTransition(ctx, path, ticket, authority)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown transition action: %s", opts.action)
	}
	defer func() { resultErr = errors.Join(resultErr, transition.Close()) }()
	switch opts.action {
	case "commit":
		if err := transition.Commit(ctx); err != nil {
			return err
		}
	case "rollback":
		if err := transition.Rollback(ctx); err != nil {
			return err
		}
	}
	return encodeLegacyEndpointCommandResult(deps.stdout, codexprov.LegacyCredentialEndpointTransitionStatus{
		State: transition.State(), Ticket: transition.Ticket(),
	})
}

func parseLegacyEndpointTransitionOptions(args []string) (legacyEndpointTransitionOptions, error) {
	var opts legacyEndpointTransitionOptions
	if len(args) == 0 {
		return opts, fmt.Errorf("usage: cq proxy endpoint transition-legacy <prepare|resume|commit|rollback>")
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
	case "resume", "commit", "rollback":
		if opts.ticketFile == "" || opts.snapshotFile != "" {
			return opts, fmt.Errorf("transition-legacy %s requires --ticket-file only", opts.action)
		}
		if !filepath.IsAbs(opts.ticketFile) || filepath.Clean(opts.ticketFile) != opts.ticketFile {
			return opts, fmt.Errorf("transition-legacy requires an absolute clean ticket file path")
		}
	default:
		return opts, fmt.Errorf("unknown transition action: %s", opts.action)
	}
	if !opts.confirmStoppedAndDrained {
		return opts, fmt.Errorf("transition-legacy requires --confirm-stopped-and-drained")
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

func encodeLegacyEndpointCommandResult(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
