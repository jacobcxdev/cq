package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const (
	candidateConfigMaxBytes     = 1 << 20
	candidateRegistryMaxBytes   = 64 << 10
	candidateReleaseMaxBytes    = 16 << 20
	candidateExecutableMaxBytes = 512 << 20
)

type candidateCommandDependencies struct {
	FS             fsutil.FileSystem
	Random         io.Reader
	Now            func() time.Time
	Start          func(context.Context, string, proxy.CandidateLifecycleStateV1, []byte) ([]byte, error)
	Stop           func(context.Context, string, proxy.CandidateLifecycleStateV1, []byte) ([]byte, error)
	Barrier        func(context.Context, fsutil.FileSystem, string, proxy.CandidateLifecycleStateV1, []byte, string) ([]byte, error)
	ArtifactSwitch func(context.Context, string, proxy.CandidateLifecycleStateV1, []byte) ([]byte, error)
	Validate       func(context.Context, fsutil.FileSystem, CandidateValidateReleaseArgumentsV1, proxy.CandidateLifecycleStateV1, []byte) (string, error)
	Remove         func(context.Context, fsutil.FileSystem, string, proxy.CandidateLifecycleStateV1) error
}

type candidateLifecycleResultV1 struct {
	OperationID                      string                         `json:"operation_id"`
	ValidationRunID                  string                         `json:"validation_run_id"`
	ProxyInstanceID                  string                         `json:"proxy_instance_id"`
	Port                             int                            `json:"port"`
	SourceConfigDigest               string                         `json:"source_config_digest"`
	TargetReleaseBundleDigest        string                         `json:"target_release_bundle_digest"`
	TargetReleaseSetDigest           string                         `json:"target_release_set_digest"`
	ActiveReleaseSetDigest           string                         `json:"active_release_set_digest"`
	ClientBuild                      string                         `json:"client_build"`
	ClientExecutableDigest           string                         `json:"client_executable_digest"`
	LocalTokenClientRegistryDigest   string                         `json:"local_token_client_registry_digest"`
	CredentialMode                   string                         `json:"credential_mode"`
	CredentialManifestDigest         string                         `json:"credential_manifest_digest,omitempty"`
	PolicySnapshotDigest             string                         `json:"policy_snapshot_digest,omitempty"`
	PayloadCapture                   bool                           `json:"payload_capture"`
	Phase                            proxy.CandidateLifecyclePhase  `json:"phase"`
	Generation                       uint64                         `json:"generation"`
	PendingAction                    proxy.CandidateLifecycleAction `json:"pending_action,omitempty"`
	EffectStarted                    bool                           `json:"effect_started"`
	EffectReceiptDigest              string                         `json:"effect_receipt_digest,omitempty"`
	ClientBearerBarrierReceiptDigest string                         `json:"client_bearer_barrier_receipt_digest,omitempty"`
	ValidationReceiptDigest          string                         `json:"validation_receipt_digest,omitempty"`
	UpdatedAt                        time.Time                      `json:"updated_at"`
	MAC                              string                         `json:"-"`
}

type candidateLifecycleEnvelopeV1 struct {
	SchemaVersion int                        `json:"schema_version"`
	Kind          string                     `json:"kind"`
	OK            bool                       `json:"ok"`
	State         string                     `json:"state"`
	Result        candidateLifecycleResultV1 `json:"result"`
	Warnings      []ProxyWarningV1           `json:"warnings"`
	Errors        []ProxyErrorV1             `json:"errors"`
}

func defaultCandidateCommandDependencies() candidateCommandDependencies {
	return candidateCommandDependencies{
		FS: fsutil.OSFileSystem{}, Random: rand.Reader, Now: time.Now,
		Start: startCandidateRuntime, Stop: stopCandidateRuntime,
		Barrier: refreshCandidateBearerBarrier, ArtifactSwitch: switchCandidateRuntimeArtifact,
		Validate: validateCandidateRelease, Remove: removeCandidateStateRoot,
	}
}

func runProxyCandidateCommand(ctx context.Context, output io.Writer, authority OrdinaryCommandAuthorityV1, deps candidateCommandDependencies) (int, error) {
	if ctx == nil || output == nil || deps.FS == nil || deps.Random == nil || deps.Now == nil {
		return 1, proxy.ErrCandidateLifecycleInvalid
	}
	var state proxy.CandidateLifecycleStateV1
	jsonOutput := false
	switch arguments := authority.Arguments.(type) {
	case CandidatePrepareArgumentsV1:
		jsonOutput = arguments.JSON
		input, err := prepareCandidateInput(deps.FS, arguments)
		if err != nil {
			return 1, err
		}
		store, prepared, err := proxy.PrepareCandidateLifecycle(ctx, deps.FS, input, deps.Random, deps.Now)
		if err != nil {
			return 1, err
		}
		state = prepared
		if err := store.Close(); err != nil {
			return 1, err
		}
	case CandidateStatusArgumentsV1:
		jsonOutput = arguments.JSON
		store, current, err := proxy.OpenCandidateLifecycle(ctx, deps.FS, arguments.InstanceStateRoot)
		if err != nil {
			return 1, err
		}
		state = current
		if err := store.Close(); err != nil {
			return 1, err
		}
	case CandidateMutationArgumentsV1:
		jsonOutput = arguments.JSON
		store, _, err := proxy.OpenCandidateLifecycle(ctx, deps.FS, arguments.InstanceStateRoot)
		if err != nil {
			return 1, err
		}
		defer store.Close()
		var action proxy.CandidateLifecycleAction
		var effect func(context.Context, string, proxy.CandidateLifecycleStateV1, []byte) ([]byte, error)
		switch authority.Row {
		case "candidate_start":
			action, effect = proxy.CandidateActionStart, deps.Start
		case "candidate_stop":
			action, effect = proxy.CandidateActionStop, deps.Stop
		default:
			return 64, errors.New("candidate command usage")
		}
		if effect == nil {
			return 1, errors.New("candidate runtime effect unavailable")
		}
		controlToken, err := store.RuntimeControlToken()
		if err != nil {
			return 1, err
		}
		defer zeroCandidateBytes(controlToken)
		state, err = store.Apply(ctx, action, func(current proxy.CandidateLifecycleStateV1) (string, error) {
			material, effectErr := effect(ctx, arguments.InstanceStateRoot, current, controlToken)
			if effectErr != nil {
				return "", effectErr
			}
			return proxy.CandidateEffectReceiptDigest(action, material), nil
		})
		if err != nil {
			return 1, err
		}
	case CandidateBarrierArgumentsV1:
		jsonOutput = arguments.JSON
		if deps.Barrier == nil {
			return 1, errors.New("candidate barrier effect unavailable")
		}
		store, current, err := proxy.OpenCandidateLifecycle(ctx, deps.FS, arguments.InstanceStateRoot)
		if err != nil {
			return 1, err
		}
		defer store.Close()
		if current.ValidationRunID != arguments.ValidationRun {
			return 3, proxy.ErrCandidateLifecycleInvalid
		}
		controlToken, err := store.RuntimeControlToken()
		if err != nil {
			return 1, err
		}
		defer zeroCandidateBytes(controlToken)
		state, err = store.Apply(ctx, proxy.CandidateActionRefreshBarrier, func(current proxy.CandidateLifecycleStateV1) (string, error) {
			material, effectErr := deps.Barrier(ctx, deps.FS, arguments.InstanceStateRoot, current, controlToken, arguments.ValidationRun)
			if effectErr != nil {
				return "", effectErr
			}
			return proxy.CandidateEffectReceiptDigest(proxy.CandidateActionRefreshBarrier, material), nil
		})
		if err != nil {
			return 1, err
		}
	case CandidateArtifactSwitchArgumentsV1:
		jsonOutput = arguments.JSON
		if deps.ArtifactSwitch == nil {
			return 1, errors.New("candidate artifact switch unavailable")
		}
		store, current, err := proxy.OpenCandidateLifecycle(ctx, deps.FS, arguments.InstanceStateRoot)
		if err != nil {
			return 1, err
		}
		defer store.Close()
		if current.ValidationRunID != arguments.ValidationRun {
			return 3, proxy.ErrCandidateLifecycleInvalid
		}
		controlToken, err := store.RuntimeControlToken()
		if err != nil {
			return 1, err
		}
		defer zeroCandidateBytes(controlToken)
		state, err = store.ApplyTarget(ctx, proxy.CandidateActionArtifactSwitch, arguments.ReleaseSet, func(current proxy.CandidateLifecycleStateV1) (string, error) {
			material, effectErr := deps.ArtifactSwitch(ctx, arguments.InstanceStateRoot, current, controlToken)
			if effectErr != nil {
				return "", effectErr
			}
			return proxy.CandidateEffectReceiptDigest(proxy.CandidateActionArtifactSwitch, material), nil
		})
		if err != nil {
			return 1, err
		}
	case CandidateValidateReleaseArgumentsV1:
		jsonOutput = arguments.JSON
		if deps.Validate == nil {
			return 1, errors.New("candidate release validation unavailable")
		}
		store, current, err := proxy.OpenCandidateLifecycle(ctx, deps.FS, arguments.InstanceStateRoot)
		if err != nil {
			return 1, err
		}
		defer store.Close()
		if current.ValidationRunID != arguments.ValidationRun {
			return 3, proxy.ErrCandidateLifecycleInvalid
		}
		controlToken, err := store.RuntimeControlToken()
		if err != nil {
			return 1, err
		}
		defer zeroCandidateBytes(controlToken)
		state, err = store.Apply(ctx, proxy.CandidateActionValidateRelease, func(current proxy.CandidateLifecycleStateV1) (string, error) {
			return deps.Validate(ctx, deps.FS, arguments, current, controlToken)
		})
		if err != nil {
			return 1, err
		}
	case CandidateRemoveArgumentsV1:
		jsonOutput = arguments.JSON
		if deps.Remove == nil {
			return 1, errors.New("candidate removal unavailable")
		}
		store, current, err := proxy.OpenCandidateLifecycle(ctx, deps.FS, arguments.InstanceStateRoot)
		if err != nil {
			return 1, err
		}
		if current.Phase == proxy.CandidatePhaseRemoved {
			state = current
		} else {
			state, err = store.Apply(ctx, proxy.CandidateActionRemove, func(current proxy.CandidateLifecycleStateV1) (string, error) {
				return proxy.CandidateEffectReceiptDigest(proxy.CandidateActionRemove, []byte(current.OperationID)), nil
			})
		}
		closeErr := store.Close()
		if err != nil {
			return 1, errors.Join(err, closeErr)
		}
		if closeErr != nil {
			return 1, closeErr
		}
		if err := deps.Remove(ctx, deps.FS, arguments.InstanceStateRoot, state); err != nil {
			return 1, err
		}
	default:
		return 64, errors.New("candidate command usage")
	}
	return renderCandidateLifecycle(output, state, jsonOutput)
}

func prepareCandidateInput(fsys fsutil.FileSystem, arguments CandidatePrepareArgumentsV1) (proxy.CandidatePrepareInputV1, error) {
	digest := func(path string, maxBytes int64) (string, error) {
		body, err := readCandidateInputFile(fsys, path, maxBytes)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(body)
		return hex.EncodeToString(sum[:]), nil
	}
	sourceDigest, err := digest(arguments.SourceConfig, candidateConfigMaxBytes)
	if err != nil {
		return proxy.CandidatePrepareInputV1{}, fmt.Errorf("read source config: %w", err)
	}
	releaseDigest, err := digest(arguments.TargetReleaseBundle, candidateReleaseMaxBytes)
	if err != nil {
		return proxy.CandidatePrepareInputV1{}, fmt.Errorf("read target release bundle: %w", err)
	}
	executableDigest, err := digestCandidateExecutable(arguments.ClientExecutable, candidateExecutableMaxBytes)
	if err != nil {
		return proxy.CandidatePrepareInputV1{}, fmt.Errorf("read client executable: %w", err)
	}
	registryBody, err := readCandidateInputFile(fsys, arguments.LocalTokenClientRegistry, candidateRegistryMaxBytes)
	if err != nil {
		return proxy.CandidatePrepareInputV1{}, fmt.Errorf("read local-token client registry: %w", err)
	}
	registrySum := sha256.Sum256(registryBody)
	registryDigest := hex.EncodeToString(registrySum[:])
	input := proxy.CandidatePrepareInputV1{
		Root: arguments.InstanceStateRoot, Port: arguments.Port,
		SourceConfigDigest: sourceDigest, TargetReleaseBundleDigest: releaseDigest,
		TargetReleaseSetDigest: arguments.TargetReleaseSet, ClientBuild: arguments.ClientBuild,
		ClientExecutableDigest: executableDigest, LocalTokenClientRegistryDigest: registryDigest,
		LocalTokenClientRegistry: registryBody,
		CredentialMode:           arguments.CredentialMode, PayloadCapture: arguments.ConfirmPayloadCapture,
	}
	if arguments.CredentialManifest != "" {
		input.CredentialManifestDigest, err = digest(arguments.CredentialManifest, candidateConfigMaxBytes)
		if err != nil {
			return proxy.CandidatePrepareInputV1{}, fmt.Errorf("read credential manifest: %w", err)
		}
	}
	if arguments.PolicySnapshot != "" {
		input.PolicySnapshotDigest, err = digest(arguments.PolicySnapshot, candidateConfigMaxBytes)
		if err != nil {
			return proxy.CandidatePrepareInputV1{}, fmt.Errorf("read policy snapshot: %w", err)
		}
	}
	return input, nil
}

func readCandidateInputFile(fsys fsutil.FileSystem, path string, maxBytes int64) ([]byte, error) {
	if !cleanAbsolutePath(path) || maxBytes <= 0 {
		return nil, proxy.ErrCandidateLifecycleInvalid
	}
	inspector, ok := fsys.(fsutil.SecurePathInspector)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	directoryPath := filepath.Dir(path)
	directory, err := fsutil.OpenOwnerControlledDirectory(fsys, directoryPath)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	body, _, err := fsutil.ReadOwnerControlledFileInDirectoryWithIdentity(inspector, directory, directoryPath, filepath.Base(path), maxBytes)
	return body, err
}

func digestCandidateExecutable(path string, maxBytes int64) (string, error) {
	if !cleanAbsolutePath(path) || maxBytes <= 0 {
		return "", proxy.ErrCandidateLifecycleInvalid
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 || pathInfo.Mode().Perm()&0o022 != 0 {
		return "", fsutil.ErrUnsafeSecurePath
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(pathInfo, openedInfo) {
		return "", fsutil.ErrUnsafeSecurePath
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maxBytes+1))
	if err != nil {
		return "", err
	}
	if written > maxBytes {
		return "", fsutil.ErrSecureFileTooLarge
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func renderCandidateLifecycle(output io.Writer, state proxy.CandidateLifecycleStateV1, jsonOutput bool) (int, error) {
	result := candidateLifecycleResultV1{
		OperationID: state.OperationID, ValidationRunID: state.ValidationRunID,
		ProxyInstanceID: state.ProxyInstanceID, Port: state.Port,
		SourceConfigDigest: state.SourceConfigDigest, TargetReleaseBundleDigest: state.TargetReleaseBundleDigest,
		TargetReleaseSetDigest: state.TargetReleaseSetDigest, ClientBuild: state.ClientBuild,
		ActiveReleaseSetDigest:         state.ActiveReleaseSetDigest,
		ClientExecutableDigest:         state.ClientExecutableDigest,
		LocalTokenClientRegistryDigest: state.LocalTokenClientRegistryDigest,
		CredentialMode:                 state.CredentialMode, CredentialManifestDigest: state.CredentialManifestDigest,
		PolicySnapshotDigest: state.PolicySnapshotDigest, PayloadCapture: state.PayloadCapture,
		Phase: state.Phase, Generation: state.Generation, PendingAction: state.PendingAction,
		EffectStarted: state.EffectStarted, EffectReceiptDigest: state.EffectReceiptDigest, UpdatedAt: state.UpdatedAt,
		ClientBearerBarrierReceiptDigest: state.ClientBearerBarrierReceiptDigest,
		ValidationReceiptDigest:          state.ValidationReceiptDigest,
	}
	if jsonOutput {
		envelope := candidateLifecycleEnvelopeV1{SchemaVersion: 1, Kind: "proxy_candidate_lifecycle", OK: true, State: string(state.Phase), Result: result, Warnings: []ProxyWarningV1{}, Errors: []ProxyErrorV1{}}
		return 0, json.NewEncoder(output).Encode(envelope)
	}
	_, err := fmt.Fprintf(output, "candidate: %s\nstate: %s\nport: %d\n", state.ProxyInstanceID, state.Phase, state.Port)
	return 0, err
}

func zeroCandidateBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
