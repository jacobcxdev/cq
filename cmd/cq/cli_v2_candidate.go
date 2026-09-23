package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func lookupV2Candidate(path string) (cli.Handler, bool) {
	switch path {
	case "proxy candidate prepare", "proxy candidate status", "proxy candidate stop", "proxy candidate remove", "proxy candidate receipt show", "proxy candidate start", "proxy candidate client-safety refresh", "proxy candidate release activate", "proxy candidate release validate":
		return handleV2Candidate, true
	}
	return nil, false
}

type v2CandidateDependencies struct {
	FS           fsutil.FileSystem
	PrepareInput func(context.Context, fsutil.FileSystem, CandidatePrepareArgumentsV1) (proxy.CandidatePrepareInputV1, error)
	PrepareStop  func(context.Context, proxy.CandidateLifecycleStateV1, []byte) (*candidateStopOperation, error)
	Absent       func(context.Context, int) error
}

var prepareV2CandidateDependencies = func(context.Context) (v2CandidateDependencies, error) {
	return v2CandidateDependencies{FS: fsutil.OSFileSystem{}, PrepareInput: prepareCandidateInputContext, PrepareStop: prepareCandidateRuntimeStop, Absent: candidateListenerAbsent}, nil
}

func handleV2Candidate(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	return handleV2CandidateWithPreparation(ctx, inv, session, prepareV2CandidateDependencies)
}

func handleV2CandidateWithPreparation(parent context.Context, inv cli.Invocation, _ *cli.Session, prepare func(context.Context) (v2CandidateDependencies, error)) cli.Outcome {
	// This boundary must precede budgets, dependency construction, paths and IO.
	switch inv.Path {
	case "proxy candidate start", "proxy candidate client-safety refresh", "proxy candidate release activate", "proxy candidate release validate":
		return v2SelectionFailure(4, "candidate_evidence_unavailable", "Candidate qualification evidence is unavailable; no transition was performed.")
	}
	option := func(name string) string {
		if values := inv.Options[name]; len(values) > 0 {
			return values[0]
		}
		return ""
	}
	total, reserve := 30*time.Second, 15*time.Second
	if value := option("timeout"); value != "" {
		total, _ = time.ParseDuration(value)
		reserve = 0
	}
	if inv.Path == "proxy candidate prepare" {
		reserve = 30 * time.Second
	}
	budget := cli.BeginBudget(parent, total, reserve)
	defer budget.Close()
	ctx := budget.Work()
	finish := func(err error) cli.Outcome {
		switch {
		case parent.Err() == context.Canceled || errors.Is(err, context.Canceled):
			return v2SelectionFailure(130, "interrupted", "Operation interrupted; inspect state before retrying.")
		case ctx.Err() == context.DeadlineExceeded || budget.Cleanup().Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded):
			return v2SelectionFailure(7, "candidate_timeout", "Candidate operation timed out; inspect candidate status before retrying.")
		case errors.Is(err, os.ErrNotExist):
			return v2SelectionFailure(3, "candidate_not_found", "Candidate state or receipt does not exist.")
		case errors.Is(err, proxy.ErrCandidateLifecycleInvalid), errors.Is(err, proxy.ErrCandidateLifecycleExists), errors.Is(err, proxy.ErrCandidateEffectIndeterminate), errors.Is(err, fsutil.ErrUnsafeSecurePath), errors.Is(err, fsutil.ErrExclusiveLockHeld):
			return v2SelectionFailure(6, "candidate_conflict", "Candidate state conflicts with this operation.")
		case err != nil:
			return v2SelectionFailure(1, "candidate_io_failed", "Candidate operation failed: local state or runtime operation failed.")
		default:
			return cli.Outcome{}
		}
	}
	if err := ctx.Err(); err != nil {
		return finish(err)
	}
	deps, err := prepare(ctx)
	if ctx.Err() != nil {
		return finish(ctx.Err())
	}
	if err != nil {
		return finish(err)
	}
	root := option("state-dir")
	parentDir, err := fsutil.OpenOwnerControlledDirectory(deps.FS, filepath.Dir(root))
	if err != nil {
		return finish(err)
	}
	if err = parentDir.Close(); err != nil {
		return finish(err)
	}
	checkpointState, checkpoint, receiptRoot, checkpointErr := inspectCandidateRemovalCheckpoint(ctx, deps.FS, root)
	if checkpointErr != nil {
		return finish(checkpointErr)
	}
	if checkpoint && inv.Path != "proxy candidate status" && inv.Path != "proxy candidate receipt show" {
		return finish(proxy.ErrCandidateLifecycleInvalid)
	}
	if inv.Path == "proxy candidate receipt show" {
		lookupRoot := root
		if checkpoint {
			if receiptRoot == "" {
				return finish(os.ErrNotExist)
			}
			lookupRoot = receiptRoot
		}
		if err := fsutil.ValidateSecureDirectory(deps.FS, lookupRoot); err != nil {
			return finish(err)
		}
		receipt, err := lookupCandidateReceipt(ctx, deps.FS, lookupRoot, option("attempt-id"))
		if ctx.Err() != nil {
			return finish(ctx.Err())
		}
		if err != nil {
			return finish(err)
		}
		if !receipt.Found {
			return finish(os.ErrNotExist)
		}
		out := cli.Outcome{}
		if receipt.Outcome == "conflicted" {
			out = v2SelectionFailure(6, "candidate_receipt_conflicted", "Candidate receipt is conflicted.")
		}
		out.Data, _ = json.Marshal(struct {
			Receipt v2CandidateReceipt `json:"receipt"`
		}{v2CandidateReceipt{receipt.AttemptID, receipt.Outcome, receipt.ReceiptDigest, receipt.PromotionDigest}})
		out.Human = fmt.Sprintf("Attempt: %s\nOutcome: %s\nReceipt digest: %s\nPromotion digest: %s\n", cli.HumanValue(receipt.AttemptID), cli.HumanValue(receipt.Outcome), cli.HumanValue(receipt.ReceiptDigest), cli.HumanValue(receipt.PromotionDigest))
		return out
	}
	var state proxy.CandidateLifecycleStateV1
	removalCommitted := false
	switch inv.Path {
	case "proxy candidate prepare":
		port, _ := strconv.Atoi(option("port"))
		args := CandidatePrepareArgumentsV1{InstanceStateRoot: root, Port: port, SourceConfig: option("source-config"), TargetReleaseBundle: option("target-release-bundle"), TargetReleaseSet: option("release-digest"), ClientBuild: option("client-build"), ClientExecutable: option("client-executable"), LocalTokenClientRegistry: option("client-registry"), CredentialMode: option("credential-mode"), CredentialManifest: option("credential-manifest"), ConfirmReadOnlyCredentials: option("confirm-read-only-credentials") == "true", PolicySnapshot: option("policy-snapshot"), ConfirmPayloadCapture: option("confirm-payload-capture") == "true"}
		var input proxy.CandidatePrepareInputV1
		input, err = deps.PrepareInput(ctx, deps.FS, args)
		if ctx.Err() != nil {
			return finish(ctx.Err())
		}
		if err != nil {
			return finish(err)
		}
		if err = deps.Absent(ctx, port); err != nil {
			return finish(err)
		}
		var store *proxy.CandidateLifecycleStore
		store, state, err = proxy.PrepareCandidateLifecycle(ctx, deps.FS, input, rand.Reader, time.Now)
		if store != nil {
			err = errors.Join(err, store.Close())
		}
	case "proxy candidate status":
		if checkpoint {
			state = checkpointState
		} else {
			state, err = proxy.InspectCandidateLifecycle(ctx, deps.FS, root)
		}
	case "proxy candidate stop", "proxy candidate remove":
		var store *proxy.CandidateLifecycleStore
		store, state, err = proxy.OpenCandidateLifecycle(ctx, deps.FS, root)
		if err != nil {
			return finish(err)
		}
		defer store.Close()
		if state.PendingAction != "" {
			return finish(proxy.ErrCandidateEffectIndeterminate)
		}
		if inv.Path == "proxy candidate stop" {
			if option("confirm-client-stopped") != "true" || (state.Phase != proxy.CandidatePhaseRunning && state.Phase != proxy.CandidatePhaseValidated) {
				return finish(proxy.ErrCandidateLifecycleInvalid)
			}
			token, tokenErr := store.RuntimeControlToken()
			if tokenErr != nil {
				return finish(tokenErr)
			}
			defer zeroCandidateBytes(token)
			operation, prepareErr := deps.PrepareStop(ctx, state, token)
			if prepareErr != nil {
				return finish(prepareErr)
			}
			defer operation.Close()
			state, err = store.Apply(budget.Cleanup(), proxy.CandidateActionStop, func(current proxy.CandidateLifecycleStateV1) (string, error) {
				material, stopErr := operation.Run(ctx, budget.Cleanup(), current)
				if stopErr != nil {
					return "", stopErr
				}
				return proxy.CandidateEffectReceiptDigest(proxy.CandidateActionStop, material), nil
			})
		} else {
			if option("confirm-candidate-state-loss") != "true" || !candidateRemovableState(state) {
				return finish(proxy.ErrCandidateLifecycleInvalid)
			}
			if err = deps.Absent(ctx, state.Port); err != nil {
				return finish(err)
			}
			if err = validateCandidateRemovalTree(ctx, deps.FS, root); err != nil {
				return finish(err)
			}
			state, err = store.Apply(budget.Cleanup(), proxy.CandidateActionRemove, func(current proxy.CandidateLifecycleStateV1) (string, error) {
				return proxy.CandidateEffectReceiptDigest(proxy.CandidateActionRemove, []byte(current.OperationID)), nil
			})
			if err == nil {
				err = store.Close()
			}
			if err == nil {
				removalCommitted, err = removeCandidateStateRootCommit(ctx, budget.Cleanup(), deps.FS, root, state)
			}
		}
	default:
		return finish(proxy.ErrCandidateLifecycleInvalid)
	}
	if err != nil {
		return finish(err)
	}
	if !removalCommitted && budget.Cleanup().Err() != nil {
		return finish(budget.Cleanup().Err())
	}
	out := cli.Outcome{}
	out.Data, _ = json.Marshal(struct {
		Candidate v2CandidateStatus `json:"candidate"`
	}{projectV2Candidate(root, state)})
	out.Human = fmt.Sprintf("Candidate: %s\nState directory: %s\nPhase: %s\nPort: %d\nValidation run: %s\n", cli.HumanValue(state.ProxyInstanceID), cli.HumanValue(root), cli.HumanValue(string(state.Phase)), state.Port, cli.HumanValue(state.ValidationRunID))
	return out
}

type v2CandidateStatus struct {
	StateDir                  string                        `json:"state_dir"`
	OperationID               string                        `json:"operation_id"`
	InstanceID                string                        `json:"instance_id"`
	ValidationRunID           string                        `json:"validation_run_id"`
	Port                      int                           `json:"port"`
	SourceConfigDigest        string                        `json:"source_config_digest"`
	TargetBundleFileDigest    string                        `json:"target_bundle_file_digest"`
	TargetReleaseDigest       string                        `json:"target_release_digest"`
	ActiveReleaseDigest       *string                       `json:"active_release_digest"`
	ClientBuild               string                        `json:"client_build"`
	ClientExecutableDigest    string                        `json:"client_executable_digest"`
	ClientRegistryDigest      string                        `json:"client_registry_digest"`
	CredentialMode            string                        `json:"credential_mode"`
	CredentialManifestDigest  *string                       `json:"credential_manifest_digest"`
	PolicySnapshotDigest      *string                       `json:"policy_snapshot_digest"`
	PayloadCapture            bool                          `json:"payload_capture"`
	Phase                     proxy.CandidateLifecyclePhase `json:"phase"`
	Generation                uint64                        `json:"generation"`
	PendingAction             *string                       `json:"pending_action"`
	EffectStarted             bool                          `json:"effect_started"`
	EffectReceiptDigest       *string                       `json:"effect_receipt_digest"`
	ClientSafetyReceiptDigest *string                       `json:"client_safety_receipt_digest"`
	ValidationReceiptDigest   *string                       `json:"validation_receipt_digest"`
	AttemptID                 *string                       `json:"attempt_id"`
	UpdatedAt                 string                        `json:"updated_at"`
}

type v2CandidateReceipt struct {
	AttemptID       string `json:"attempt_id"`
	Outcome         string `json:"outcome"`
	ReceiptDigest   string `json:"receipt_digest"`
	PromotionDigest string `json:"promotion_digest"`
}

func projectV2Candidate(root string, s proxy.CandidateLifecycleStateV1) v2CandidateStatus {
	// Legacy retained digest fields do not establish observed artefacts or measured
	// qualification. No available v2 transition can issue that evidence.
	return v2CandidateStatus{StateDir: root, OperationID: s.OperationID, InstanceID: s.ProxyInstanceID, ValidationRunID: s.ValidationRunID, Port: s.Port, SourceConfigDigest: s.SourceConfigDigest, TargetBundleFileDigest: s.TargetReleaseBundleDigest, TargetReleaseDigest: s.TargetReleaseSetDigest, ClientBuild: s.ClientBuild, ClientExecutableDigest: s.ClientExecutableDigest, ClientRegistryDigest: s.LocalTokenClientRegistryDigest, CredentialMode: s.CredentialMode, CredentialManifestDigest: v2AccountString(s.CredentialManifestDigest), PolicySnapshotDigest: v2AccountString(s.PolicySnapshotDigest), PayloadCapture: s.PayloadCapture, Phase: s.Phase, Generation: s.Generation, PendingAction: v2AccountString(string(s.PendingAction)), EffectStarted: s.EffectStarted, EffectReceiptDigest: v2AccountString(s.EffectReceiptDigest), UpdatedAt: s.UpdatedAt.UTC().Format(time.RFC3339Nano)}
}

func candidateNativeStopMaterial(s proxy.CandidateLifecycleStateV1) []byte {
	material, _ := proxy.CanonicalJSONV1(struct {
		Kind            string `json:"kind"`
		OperationID     string `json:"operation_id"`
		InstanceID      string `json:"instance_id"`
		ValidationRunID string `json:"validation_run_id"`
		Port            int    `json:"port"`
		Generation      uint64 `json:"generation"`
	}{"candidate_native_exit_v1", s.OperationID, s.ProxyInstanceID, s.ValidationRunID, s.Port, s.Generation})
	return material
}
func candidateRemovableState(s proxy.CandidateLifecycleStateV1) bool {
	if s.PendingAction != "" || s.EffectStarted {
		return false
	}
	if s.Phase == proxy.CandidatePhasePrepared {
		return s.Generation == 1 && s.EffectReceiptDigest == ""
	}
	if s.Phase != proxy.CandidatePhaseStopped || s.Generation < 4 {
		return false
	}
	s.Generation--
	return s.EffectReceiptDigest == proxy.CandidateEffectReceiptDigest(proxy.CandidateActionStop, candidateNativeStopMaterial(s))
}
