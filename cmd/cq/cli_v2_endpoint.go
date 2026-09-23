package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func lookupV2Endpoint(path string) (cli.Handler, bool) {
	switch path {
	case "codex proxy credential-endpoint legacy inspect", "codex proxy credential-endpoint legacy prepare", "codex proxy credential-endpoint legacy resume", "codex proxy credential-endpoint legacy activate", "codex proxy credential-endpoint legacy finalise", "codex proxy credential-endpoint legacy rollback":
		return handleV2Endpoint, true
	}
	return nil, false
}

type v2EndpointDependencies struct {
	Path       string
	Inspect    func(context.Context, string) (legacyEndpointInspection, error)
	Transition func(context.Context, string, legacyEndpointTransitionOptions) (codexprov.LegacyCredentialEndpointTransitionStatus, error)
}

func v2EndpointDependenciesForPath(path string) v2EndpointDependencies {
	return v2EndpointDependencies{
		Path: path, Inspect: inspectLegacyEndpoint,
		Transition: func(ctx context.Context, path string, opts legacyEndpointTransitionOptions) (codexprov.LegacyCredentialEndpointTransitionStatus, error) {
			return executeLegacyEndpointTransition(ctx, path, opts, legacyEndpointTransitionOperations{
				ReadProof: func(ctx context.Context, path string) ([]byte, error) {
					return readV2EndpointProof(ctx, fsutil.OSFileSystem{}, path)
				},
				Reopen: codexprov.ReopenLegacyCredentialEndpointTransition,
				Resume: codexprov.ResumeLegacyCredentialEndpointTransitionForAction,
			})
		},
	}
}

var prepareV2EndpointDependencies = func(context.Context) (v2EndpointDependencies, error) {
	switch runtime.GOOS {
	case "darwin", "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
	default:
		return v2EndpointDependencies{}, codexprov.ErrCredentialEndpointMaintenanceUnsupported
	}

	roots, err := userdirs.Default(userdirs.StateRoot)
	if err != nil {
		return v2EndpointDependencies{}, err
	}
	return v2EndpointDependenciesForPath(codexprov.DefaultCredentialControlPath(roots.State)), nil
}

func handleV2Endpoint(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	return handleV2EndpointWithPreparation(ctx, inv, session, prepareV2EndpointDependencies)
}

func handleV2EndpointWithPreparation(parent context.Context, inv cli.Invocation, session *cli.Session, prepare func(context.Context) (v2EndpointDependencies, error)) cli.Outcome {
	option := func(name string) string {
		if v := inv.Options[name]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	timeout, _ := time.ParseDuration(option("timeout"))
	budget := cli.BeginBudget(parent, timeout, 0)
	defer budget.Close()
	ctx := budget.Work()
	finish := func(err error) cli.Outcome {
		if parent.Err() == context.Canceled || ctx.Err() == context.Canceled {
			return v2EndpointFailure(context.Canceled)
		}
		if ctx.Err() != nil {
			return v2EndpointFailure(ctx.Err())
		}
		return v2EndpointFailure(err)
	}
	if err := ctx.Err(); err != nil {
		return finish(err)
	}
	action := strings.TrimPrefix(inv.Path, "codex proxy credential-endpoint legacy ")
	opts := legacyEndpointTransitionOptions{action: action, snapshotFile: option("snapshot-file"), ticketFile: option("ticket-file"), confirmStoppedAndDrained: option("confirm-stopped-and-drained") == "true", confirmCandidateHealthy: option("confirm-candidate-healthy") == "true", nonInteractive: inv.JSON || option("non-interactive") == "true"}
	if action != "inspect" {
		confirmed := opts.confirmStoppedAndDrained
		phrase, prompt := "stopped-and-drained", "Type stopped-and-drained to confirm the proxy remains stopped and drained: "
		if action == "finalise" {
			confirmed = opts.confirmCandidateHealthy
			phrase, prompt = "candidate-healthy", "Type candidate-healthy to confirm the exact live candidate passed health verification: "
		}
		if !confirmed || (!opts.nonInteractive && (session == nil || !session.Interactive)) {
			return finish(codexprov.ErrCredentialEndpointMaintenanceDrainRequired)
		}
		if !opts.nonInteractive {
			budget.Pause()
			err := confirmV2EndpointPhrase(parent, session, phrase, prompt)
			budget.Resume()
			ctx = budget.Work()
			if err != nil {
				return finish(err)
			}
		}
	}
	deps, err := prepare(ctx)
	if ctx.Err() != nil {
		return finish(ctx.Err())
	}
	if err != nil {
		return finish(err)
	}
	var result v2EndpointResult
	if action == "inspect" {
		inspection, err := deps.Inspect(ctx, deps.Path)
		if err != nil {
			return finish(err)
		}
		if inspection.Snapshot != nil {
			result = v2EndpointResult{Kind: "snapshot", Path: inspection.Snapshot.Path, State: string(inspection.Snapshot.State), Snapshot: inspection.Snapshot, TicketID: "none"}
		} else if inspection.Transition != nil {
			result = v2EndpointTransitionResult(*inspection.Transition)
		} else {
			return finish(errors.New("empty inspection"))
		}
	} else {
		status, err := deps.Transition(ctx, deps.Path, opts)
		if err != nil {
			return finish(err)
		}
		result = v2EndpointTransitionResult(status)
	}
	if ctx.Err() != nil {
		return finish(ctx.Err())
	}
	data, err := json.Marshal(struct {
		Endpoint v2EndpointResult `json:"endpoint"`
	}{result})
	if err != nil {
		return finish(err)
	}
	return cli.Outcome{Data: data, Human: fmt.Sprintf("Credential endpoint: %s\nMigration state: %s\nTicket: %s\n", cli.HumanValue(result.Path), cli.HumanValue(result.State), cli.HumanValue(result.TicketID))}
}

type v2EndpointResult struct {
	Kind     string                                              `json:"kind"`
	Path     string                                              `json:"path"`
	State    string                                              `json:"state"`
	Snapshot *codexprov.LegacyCredentialEndpointSnapshot         `json:"snapshot"`
	Ticket   *codexprov.LegacyCredentialEndpointTransitionTicket `json:"ticket"`
	TicketID string                                              `json:"ticket_id_or_none"`
}

func v2EndpointTransitionResult(status codexprov.LegacyCredentialEndpointTransitionStatus) v2EndpointResult {
	return v2EndpointResult{Kind: "transition", Path: status.Ticket.Path, State: string(status.State), Ticket: &status.Ticket, TicketID: status.Ticket.ID}
}

func v2EndpointFailure(err error) cli.Outcome {
	var proof *legacyEndpointProofError
	var environment *userdirs.EnvironmentError
	switch {
	case errors.Is(err, context.Canceled):
		return v2SelectionFailure(130, "interrupted", "Operation interrupted; inspect state before retrying.")
	case errors.Is(err, context.DeadlineExceeded):
		return v2SelectionFailure(7, "endpoint_timeout", "Credential endpoint operation timed out; inspect migration state before continuing.")
	case errors.As(err, &environment):
		return v2SelectionFailure(2, "environment_invalid", environment.Error())
	case errors.As(err, &proof), errors.Is(err, fsutil.ErrSecureFileTooLarge):
		return v2SelectionFailure(2, "endpoint_invalid_proof", "Credential endpoint proof is invalid: expected the exact endpoint proof schema and size.")
	case errors.Is(err, codexprov.ErrCredentialEndpointMaintenanceUnsupported), errors.Is(err, fsutil.ErrSecureCapabilityUnavailable):
		return v2SelectionFailure(4, "endpoint_unsupported", "Credential endpoint maintenance is unavailable on this platform.")
	case errors.Is(err, codexprov.ErrCredentialEndpointMaintenanceConflict), errors.Is(err, codexprov.ErrCredentialEndpointMaintenanceTicketMismatch), errors.Is(err, codexprov.ErrCredentialEndpointMaintenanceSnapshotChanged), errors.Is(err, codexprov.ErrCredentialEndpointMaintenanceDrainRequired), errors.Is(err, codexprov.ErrCredentialEndpointMaintenanceVerifierRequired), errors.Is(err, codexprov.ErrCredentialEndpointMaintenanceVerification), errors.Is(err, codexprov.ErrLegacyCredentialEndpointArtifacts), errors.Is(err, codexprov.ErrLegacyCredentialEndpointNotRefused), errors.Is(err, codexprov.ErrCredentialEndpointMaintenancePending), errors.Is(err, codexprov.ErrCredentialEndpointIdentityChanged), errors.Is(err, codexprov.ErrCredentialEndpointIncompatible), errors.Is(err, codexprov.ErrCredentialEndpointLockHeld), errors.Is(err, fsutil.ErrUnsafeSecurePath), errors.Is(err, fsutil.ErrExclusiveLockHeld):
		return v2SelectionFailure(6, "endpoint_conflict", "Credential endpoint migration precondition failed: authority, identity or migration state does not match.")
	case errors.Is(err, os.ErrNotExist):
		return v2SelectionFailure(3, "endpoint_not_found", "Credential endpoint or transition does not exist.")
	default:
		return v2SelectionFailure(1, "endpoint_io_failed", "Credential endpoint operation failed: local state or owner operation failed.")
	}
}

// Only the confirmation reader may outlive cancellation; it cannot access the
// endpoint or start work. Operational mutations always run synchronously.
func confirmV2EndpointPhrase(ctx context.Context, session *cli.Session, phrase, prompt string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := io.WriteString(session.Err, prompt); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		var result error
		defer func() {
			if recover() != nil {
				result = errors.New("confirmation input failed")
			}
			done <- result
		}()
		var answer []byte
		var one [1]byte
		for len(answer) < 256 {
			if _, err := io.ReadFull(session.In, one[:]); err != nil {
				result = codexprov.ErrCredentialEndpointMaintenanceDrainRequired
				return
			}
			if one[0] == '\n' {
				if strings.TrimSpace(string(answer)) != phrase {
					result = codexprov.ErrCredentialEndpointMaintenanceDrainRequired
				}
				return
			}
			answer = append(answer, one[0])
		}
		result = codexprov.ErrCredentialEndpointMaintenanceDrainRequired
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Proof files are owner-controlled inputs, not necessarily private 0600
// credential stores. Retain the parent and recheck the exact descriptor and
// pathname identity before using bounded bytes as transition authority.
func readV2EndpointProof(ctx context.Context, fsys fsutil.FileSystem, path string) (data []byte, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !cleanAbsolutePath(path) {
		return nil, fsutil.ErrUnsafeSecurePath
	}
	inspector, ok := fsys.(fsutil.SecurePathInspector)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	parentPath := filepath.Dir(path)
	parent, err := fsutil.OpenOwnerControlledDirectory(fsys, parentPath)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.Close()) }()
	before, err := inspector.Lstat(path)
	if err != nil {
		return nil, err
	}
	validate := func(info os.FileInfo) error {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
			return fsutil.ErrUnsafeSecurePath
		}
		if err := fsutil.ValidateSecureOwner(inspector, info); err != nil {
			return err
		}
		if info.Size() < 1 || info.Size() > legacyEndpointProofMaxBytes {
			return &legacyEndpointProofError{errors.New("invalid proof size")}
		}
		return nil
	}
	if err := validate(before); err != nil {
		return nil, err
	}
	file, err := parent.OpenNoFollow(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	same := func(info os.FileInfo) bool {
		a, aok := inspector.FileIdentity(before)
		b, bok := inspector.FileIdentity(info)
		return aok && bok && a == b && before.Mode() == info.Mode() && before.Size() == info.Size() && before.ModTime() == info.ModTime()
	}
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !same(opened) {
		return nil, fsutil.ErrUnsafeSecurePath
	}
	data, err = io.ReadAll(io.LimitReader(file, legacyEndpointProofMaxBytes+1))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	current, err := inspector.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !same(after) || !same(current) {
		return nil, fsutil.ErrUnsafeSecurePath
	}
	if err := fsutil.ValidateOwnerControlledDirectoryHandle(inspector, parent, parentPath); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) < 1 || len(data) > legacyEndpointProofMaxBytes || !utf8.Valid(data) {
		return nil, &legacyEndpointProofError{errors.New("invalid proof bytes")}
	}
	// Frozen v1 parsers validate field names, duplicates and identities. V2 also
	// rejects null scalar fields (encoding/json otherwise silently accepts them).
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, &legacyEndpointProofError{err}
		}
		if token == nil {
			return nil, &legacyEndpointProofError{errors.New("null proof field")}
		}
	}
	return data, nil
}
