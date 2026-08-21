package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestProxyCandidatePrepareStatusStartStop(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "candidate")
	files := map[string]string{
		"source.json":   `{"port":29280}`,
		"release.json":  `{"release":"target"}`,
		"codex":         "synthetic executable",
		"registry.json": `{"clients":[]}`,
		"policy.json":   `{"generation":1}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(base, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	deps := candidateCommandDependencies{
		FS: fsutil.OSFileSystem{}, Random: rand.Reader, Now: time.Now,
		Start: func(_ context.Context, _ string, state proxy.CandidateLifecycleStateV1, token []byte) ([]byte, error) {
			if len(token) != 32 {
				t.Fatalf("control token length = %d", len(token))
			}
			return []byte("started\x00" + state.ProxyInstanceID), nil
		},
		Stop: func(_ context.Context, _ string, state proxy.CandidateLifecycleStateV1, token []byte) ([]byte, error) {
			if len(token) != 32 {
				t.Fatalf("control token length = %d", len(token))
			}
			return []byte("stopped\x00" + state.ProxyInstanceID), nil
		},
	}
	prepare, err := ClassifyProxyCommand([]string{
		"proxy", "candidate", "prepare", "--instance-state-root", root, "--port", "29280",
		"--source-config", filepath.Join(base, "source.json"),
		"--target-release-bundle", filepath.Join(base, "release.json"),
		"--target-release-set", strings.Repeat("a", 64), "--client-build", "codex-test",
		"--client-executable", filepath.Join(base, "codex"),
		"--local-token-client-registry", filepath.Join(base, "registry.json"),
		"--credential-mode", "none", "--policy-snapshot", filepath.Join(base, "policy.json"),
		"--json", "150s",
	})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if exit, runErr := runProxyCandidateCommand(context.Background(), &output, prepare, deps); runErr != nil || exit != 0 {
		t.Fatalf("prepare = exit %d, err %v", exit, runErr)
	}
	var prepared candidateLifecycleEnvelopeV1
	if err := json.Unmarshal(output.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	if !prepared.OK || prepared.State != string(proxy.CandidatePhasePrepared) || prepared.Result.Port != 29280 || prepared.Result.MAC != "" {
		t.Fatalf("prepared = %#v", prepared)
	}

	for _, fixture := range []struct {
		row  string
		args any
		want proxy.CandidateLifecyclePhase
	}{
		{"candidate_status", CandidateStatusArgumentsV1{InstanceStateRoot: root, JSON: true}, proxy.CandidatePhasePrepared},
		{"candidate_start", CandidateMutationArgumentsV1{InstanceStateRoot: root, JSON: true, Timeout: 30 * time.Second}, proxy.CandidatePhaseRunning},
		{"candidate_stop", CandidateMutationArgumentsV1{InstanceStateRoot: root, JSON: true, Timeout: 30 * time.Second, ConfirmClientStopped: true}, proxy.CandidatePhaseStopped},
	} {
		output.Reset()
		authority := OrdinaryCommandAuthorityV1{Catalogue: "proxy", Row: fixture.row, Arguments: fixture.args, Deadline: CommandDeadlineV1{Total: 30 * time.Second, Forward: 30 * time.Second}}
		if exit, runErr := runProxyCandidateCommand(context.Background(), &output, authority, deps); runErr != nil || exit != 0 {
			t.Fatalf("%s = exit %d, err %v", fixture.row, exit, runErr)
		}
		var envelope candidateLifecycleEnvelopeV1
		if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Result.Phase != fixture.want {
			t.Fatalf("%s phase = %q", fixture.row, envelope.Result.Phase)
		}
	}
}

func TestProxyCandidateBarrierArtifactValidationAndRemoval(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "candidate")
	for name, body := range map[string]string{
		"source.json": `{"port":29281}`, "release.json": `{"release":"target"}`,
		"codex": "synthetic executable", "registry.json": `{"clients":[]}`,
	} {
		if err := os.WriteFile(filepath.Join(base, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removed := false
	deps := candidateCommandDependencies{
		FS: fsutil.OSFileSystem{}, Random: rand.Reader, Now: time.Now,
		Start: func(context.Context, string, proxy.CandidateLifecycleStateV1, []byte) ([]byte, error) {
			return []byte("started"), nil
		},
		Stop: func(context.Context, string, proxy.CandidateLifecycleStateV1, []byte) ([]byte, error) {
			return []byte("stopped"), nil
		},
		Barrier: func(_ context.Context, _ fsutil.FileSystem, _ string, state proxy.CandidateLifecycleStateV1, token []byte, run string) ([]byte, error) {
			if run != state.ValidationRunID || len(token) != 32 {
				t.Fatal("barrier authority mismatch")
			}
			return []byte("barrier"), nil
		},
		ArtifactSwitch: func(_ context.Context, _ string, state proxy.CandidateLifecycleStateV1, _ []byte) ([]byte, error) {
			if state.PendingTargetDigest != strings.Repeat("b", 64) {
				t.Fatalf("pending target = %q", state.PendingTargetDigest)
			}
			return []byte("switched"), nil
		},
		Validate: func(_ context.Context, _ fsutil.FileSystem, arguments CandidateValidateReleaseArgumentsV1, state proxy.CandidateLifecycleStateV1, _ []byte) (string, error) {
			if arguments.ValidationRun != state.ValidationRunID || state.ClientBearerBarrierReceiptDigest == "" {
				t.Fatal("validation authority mismatch")
			}
			return strings.Repeat("d", 64), nil
		},
		Remove: func(_ context.Context, _ fsutil.FileSystem, gotRoot string, state proxy.CandidateLifecycleStateV1) error {
			removed = gotRoot == root && state.Phase == proxy.CandidatePhaseRemoved
			return nil
		},
	}
	prepare, err := ClassifyProxyCommand([]string{
		"proxy", "candidate", "prepare", "--instance-state-root", root, "--port", "29281",
		"--source-config", filepath.Join(base, "source.json"), "--target-release-bundle", filepath.Join(base, "release.json"),
		"--target-release-set", strings.Repeat("a", 64), "--client-build", "codex-test", "--client-executable", filepath.Join(base, "codex"),
		"--local-token-client-registry", filepath.Join(base, "registry.json"), "--credential-mode", "none", "150s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if exit, runErr := runProxyCandidateCommand(context.Background(), io.Discard, prepare, deps); runErr != nil || exit != 0 {
		t.Fatalf("prepare = %d, %v", exit, runErr)
	}
	store, state, err := proxy.OpenCandidateLifecycle(context.Background(), fsutil.OSFileSystem{}, root)
	if err != nil {
		t.Fatal(err)
	}
	run := state.ValidationRunID
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	commands := []OrdinaryCommandAuthorityV1{
		{Catalogue: "proxy", Row: "candidate_barrier_refresh", Arguments: CandidateBarrierArgumentsV1{InstanceStateRoot: root, ValidationRun: run, Timeout: time.Minute}},
		{Catalogue: "proxy", Row: "candidate_start", Arguments: CandidateMutationArgumentsV1{InstanceStateRoot: root, Timeout: time.Minute}},
		{Catalogue: "proxy", Row: "candidate_artifact_switch", Arguments: CandidateArtifactSwitchArgumentsV1{InstanceStateRoot: root, Role: "runtime-bundle", ReleaseSet: strings.Repeat("b", 64), ValidationRun: run, ConfirmArtifactSwitch: true, Timeout: time.Minute}},
		{Catalogue: "proxy", Row: "candidate_validate_release", Arguments: CandidateValidateReleaseArgumentsV1{InstanceStateRoot: root, ValidationRun: run}},
		{Catalogue: "proxy", Row: "candidate_remove", Arguments: CandidateRemoveArgumentsV1{InstanceStateRoot: root, ConfirmCandidateStateLoss: true, Timeout: time.Minute}},
	}
	for _, command := range commands {
		if exit, runErr := runProxyCandidateCommand(context.Background(), io.Discard, command, deps); runErr != nil || exit != 0 {
			t.Fatalf("%s = %d, %v", command.Row, exit, runErr)
		}
	}
	if !removed {
		t.Fatal("candidate root removal was not authorised")
	}
}

func TestCandidateBarrierPersistsAuthenticatedReceipt(t *testing.T) {
	registry := proxy.ClientSenderRegistryV1{SchemaVersion: 1, Revision: 1, Senders: []proxy.ClientRequestSenderV1{{SenderID: "cq", AdapterID: "cq_config_read_per_call_v1", CredentialDomains: []string{"cq_local_token"}, Transports: []string{"http"}, HookSupported: true}}}
	registryBody, err := proxy.CanonicalJSONV1(registry)
	if err != nil {
		t.Fatal(err)
	}
	registrySum := sha256.Sum256(registryBody)
	root := filepath.Join(t.TempDir(), "candidate")
	store, state, err := proxy.PrepareCandidateLifecycle(context.Background(), fsutil.OSFileSystem{}, proxy.CandidatePrepareInputV1{
		Root: root, Port: 29282, SourceConfigDigest: strings.Repeat("1", 64), TargetReleaseBundleDigest: strings.Repeat("2", 64),
		TargetReleaseSetDigest: strings.Repeat("3", 64), ClientBuild: "codex-test", ClientExecutableDigest: strings.Repeat("4", 64),
		LocalTokenClientRegistryDigest: hex.EncodeToString(registrySum[:]), LocalTokenClientRegistry: registryBody, CredentialMode: "none",
	}, rand.Reader, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.RuntimeControlToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	receiptBody, err := refreshCandidateBearerBarrier(context.Background(), fsutil.OSFileSystem{}, root, state, token, state.ValidationRunID)
	if err != nil {
		t.Fatal(err)
	}
	var receipt proxy.ClientBearerBarrierReceiptV1
	if err := json.Unmarshal(receiptBody, &receipt); err != nil || receipt.Digest == "" || receipt.RegistryDigest == "" {
		t.Fatalf("receipt = %#v, %v", receipt, err)
	}
	persisted, err := os.ReadFile(filepath.Join(root, candidateBarrierReceiptName))
	if err != nil || !bytes.Equal(persisted, receiptBody) {
		t.Fatalf("persisted receipt mismatch: %v", err)
	}
}

func TestCandidateRemovalRetiresRoot(t *testing.T) {
	registry := []byte(`{"schema_version":1}`)
	registrySum := sha256.Sum256(registry)
	root := filepath.Join(t.TempDir(), "candidate")
	store, _, err := proxy.PrepareCandidateLifecycle(context.Background(), fsutil.OSFileSystem{}, proxy.CandidatePrepareInputV1{
		Root: root, Port: 29283, SourceConfigDigest: strings.Repeat("1", 64), TargetReleaseBundleDigest: strings.Repeat("2", 64),
		TargetReleaseSetDigest: strings.Repeat("3", 64), ClientBuild: "codex-test", ClientExecutableDigest: strings.Repeat("4", 64),
		LocalTokenClientRegistryDigest: hex.EncodeToString(registrySum[:]), LocalTokenClientRegistry: registry, CredentialMode: "none",
	}, rand.Reader, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Apply(context.Background(), proxy.CandidateActionRemove, func(proxy.CandidateLifecycleStateV1) (string, error) { return strings.Repeat("a", 64), nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := removeCandidateStateRoot(context.Background(), fsutil.OSFileSystem{}, root, state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate root remains: %v", err)
	}
}

func TestCandidateReleaseValidationExportsAuthenticatedReceipt(t *testing.T) {
	base := t.TempDir()
	targetBody, target := operationalCandidateBundleForTest(t, "target", "1123456789abcdef0123456789abcdef01234567")
	floorBody, _ := operationalCandidateBundleForTest(t, "floor", "0123456789abcdef0123456789abcdef01234567")
	targetPath, floorPath := filepath.Join(base, "target.json"), filepath.Join(base, "floor.json")
	floorReceiptPath, outputPath := filepath.Join(base, "floor-receipt.json"), filepath.Join(base, "promotion.json")
	floorReceipt := []byte(`{"kind":"synthetic-floor-acceptance"}`)
	clientPath := filepath.Join(base, "client")
	for path, body := range map[string][]byte{targetPath: targetBody, floorPath: floorBody, floorReceiptPath: floorReceipt} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(clientPath, []byte("synthetic client"), 0o700); err != nil {
		t.Fatal(err)
	}
	clientDigest, err := digestCandidateExecutable(clientPath, candidateExecutableMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	registry := []byte(`{"schema_version":1}`)
	registrySum, targetSum := sha256.Sum256(registry), sha256.Sum256(targetBody)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	root := filepath.Join(base, "candidate")
	store, _, err := proxy.PrepareCandidateLifecycle(context.Background(), fsutil.OSFileSystem{}, proxy.CandidatePrepareInputV1{
		Root: root, Port: port, SourceConfigDigest: strings.Repeat("1", 64), TargetReleaseBundleDigest: hex.EncodeToString(targetSum[:]),
		TargetReleaseSetDigest: target.Digest, ClientBuild: "codex-test", ClientExecutableDigest: clientDigest,
		LocalTokenClientRegistryDigest: hex.EncodeToString(registrySum[:]), LocalTokenClientRegistry: registry, CredentialMode: "none",
	}, rand.Reader, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.RuntimeControlToken()
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Apply(context.Background(), proxy.CandidateActionRefreshBarrier, func(proxy.CandidateLifecycleStateV1) (string, error) { return strings.Repeat("b", 64), nil })
	if err != nil {
		t.Fatal(err)
	}
	previousExecutable, previousCommand := candidateRuntimeExecutable, candidateRuntimeCommand
	candidateRuntimeExecutable = func() (string, error) { return os.Executable() }
	candidateRuntimeCommand = func(_ string, arguments ...string) *exec.Cmd {
		command := exec.Command(os.Args[0], append([]string{"-test.run=^TestCandidateRuntimeProcessHelper$", "--"}, arguments...)...)
		command.Env = append(os.Environ(), "CQ_CANDIDATE_RUNTIME_HELPER=1")
		return command
	}
	t.Cleanup(func() { candidateRuntimeExecutable, candidateRuntimeCommand = previousExecutable, previousCommand })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	state, err = store.Apply(ctx, proxy.CandidateActionStart, func(current proxy.CandidateLifecycleStateV1) (string, error) {
		material, startErr := startCandidateRuntime(ctx, root, current, token)
		return proxy.CandidateEffectReceiptDigest(proxy.CandidateActionStart, material), startErr
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = stopCandidateRuntime(context.Background(), root, state, token) }()
	arguments := CandidateValidateReleaseArgumentsV1{
		InstanceStateRoot: root, TargetReleaseBundle: targetPath, FloorReleaseBundle: floorPath,
		FloorAcceptanceReceiptFile: floorReceiptPath, FloorAcceptanceReceipt: candidateDomainDigest("cq/release-import-floor/v1\x00", floorReceipt),
		ClientBuild: "codex-test", ClientExecutable: clientPath, ValidationRun: state.ValidationRunID, ReceiptOut: outputPath,
		ConfirmControlHealth: true,
	}
	digest, err := validateCandidateRelease(ctx, fsutil.OSFileSystem{}, arguments, state, token)
	if err != nil || len(digest) != 64 {
		t.Fatalf("validation = %q, %v", digest, err)
	}
	if output, err := os.ReadFile(outputPath); err != nil || len(output) == 0 || output[len(output)-1] != '\n' {
		t.Fatalf("promotion output = %q, %v", output, err)
	}
	inspection, err := proxy.InspectCandidateReceiptState(ctx, fsutil.OSFileSystem{}, root, state.OperationID)
	if err != nil || !inspection.Found || inspection.PromotionDigest != digest {
		t.Fatalf("receipt inspection = %#v, %v", inspection, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func operationalCandidateBundleForTest(t *testing.T, purpose, sourceCommit string) ([]byte, candidateOperationalReleaseV1) {
	t.Helper()
	seed := sha256.Sum256([]byte("candidate-" + purpose))
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	roles := []candidateOperationalReleaseRoleV1{{Role: "supervisor", ArtifactDigest: strings.Repeat("1", 64), ByteCount: 1}, {Role: "worker", ArtifactDigest: strings.Repeat("2", 64), ByteCount: 1}}
	if purpose == "target" {
		roles = append([]candidateOperationalReleaseRoleV1{{Role: "launcher", ArtifactDigest: strings.Repeat("3", 64), ByteCount: 1}}, roles...)
	}
	bundle := candidateOperationalReleaseV1{SchemaVersion: 1, Kind: "operational_release_bundle_v1", Purpose: purpose, AuthorityDigest: strings.Repeat("4", 64), SourceCommit: sourceCommit, SourceTreeDigest: strings.Repeat("5", 64), Roles: roles, BuiltAt: time.Unix(1_700_000_000, 0).UTC().Format(time.RFC3339), SignerPublicKey: hex.EncodeToString(privateKey.Public().(ed25519.PublicKey))}
	signable, err := proxy.CanonicalJSONV1(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Signature = hex.EncodeToString(ed25519.Sign(privateKey, signable))
	digestable, err := proxy.CanonicalJSONV1(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Digest = candidateDomainDigest("cq/operational-release-bundle/v1\x00", digestable)
	body, err := proxy.CanonicalJSONV1(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return body, bundle
}
