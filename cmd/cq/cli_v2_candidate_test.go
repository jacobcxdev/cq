package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestCLIV2CandidateContract(t *testing.T) {
	runV2Case(t, v2Case{Name: "unavailable start does not read state", Scenario: "no-access", Args: []string{"proxy", "candidate", "start", "--state-dir", "/tmp/cq-v2-unopened-candidate", "--json"}, Exit: 4, Command: "proxy candidate start", Code: "candidate_evidence_unavailable", WantJSON: `null`, Forbid: []string{"filesystem-read", "filesystem-write", "credentials", "network", "service", "child", "consume"}})
}

func candidateReservedArgs() [][]string {
	root := "/tmp/cq-v2-unopened-candidate"
	digest := strings.Repeat("a", 64)
	return [][]string{
		{"proxy", "candidate", "start", "--state-dir", root},
		{"proxy", "candidate", "client-safety", "refresh", "--state-dir", root, "--validation-run-id", digest},
		{"proxy", "candidate", "release", "activate", "--state-dir", root, "--release-digest", digest, "--validation-run-id", digest, "--confirm-artifact-switch"},
		{"proxy", "candidate", "release", "validate", "--state-dir", root, "--target-release-bundle", "/missing/target", "--rollback-bundle", "/missing/floor", "--rollback-receipt", "/missing/receipt", "--rollback-receipt-digest", digest, "--client-build", "synthetic", "--client-executable", "/missing/client", "--validation-run-id", digest, "--receipt-file", "/missing/output", "--confirm-control-health"},
	}
}

func TestCLIV2CandidateReservedNoAccess(t *testing.T) {
	old := prepareV2CandidateDependencies
	prepareV2CandidateDependencies = func(context.Context) (v2CandidateDependencies, error) {
		panic("environment/files/credentials/services accessed")
	}
	defer func() { prepareV2CandidateDependencies = old }()
	for _, args := range candidateReservedArgs() {
		canonical := strings.Join(args[:3], " ")
		if args[2] != "start" {
			canonical = strings.Join(args[:4], " ")
		}
		variants := [][]string{args}
		legacy := append([]string(nil), args...)
		for i, value := range legacy {
			switch value {
			case "client-safety":
				legacy[i] = "client-bearer-barrier"
			case "--state-dir":
				legacy[i] = "--instance-state-root"
			case "--validation-run-id":
				legacy[i] = "--validation-run"
			case "--release-digest":
				legacy[i] = "--release-set"
			case "--rollback-bundle":
				legacy[i] = "--floor-release-bundle"
			case "--rollback-receipt":
				legacy[i] = "--floor-acceptance-receipt-file"
			case "--rollback-receipt-digest":
				legacy[i] = "--floor-acceptance-receipt"
			case "--receipt-file":
				legacy[i] = "--receipt-out"
			}
		}
		if args[2] == "release" && args[3] == "activate" {
			legacy[2], legacy[3] = "artifact", "switch"
			legacy = append(legacy, "--role", "runtime-bundle")
		}
		if args[2] == "release" && args[3] == "validate" {
			legacy = append([]string{"proxy", "candidate", "validate-release"}, legacy[4:]...)
		}
		variants = append(variants, legacy)
		for _, variant := range variants {
			for _, prefix := range []bool{false, true} {
				t.Run(strings.Join(variant[:3], " ")+fmt.Sprint(prefix), func(t *testing.T) {
					call := append([]string(nil), variant...)
					if prefix {
						call = append([]string{"--json"}, call...)
					} else {
						call = append(call, "--json")
					}
					exit, out := runCandidateV2(t, context.Background(), call, nil)
					if exit != 4 || out.Command != canonical || out.OK || string(out.Data) != "null" || len(out.Errors) != 1 || out.Errors[0].Code != "candidate_evidence_unavailable" || out.Errors[0].Message != "Candidate qualification evidence is unavailable; no transition was performed." {
						t.Fatalf("unexpected unavailable result: %d %+v", exit, out)
					}
				})
			}
			for _, extra := range [][]string{{"--unknown"}, {"--state-dir", "/duplicate"}, {"unexpected"}} {
				t.Run(canonical+strings.Join(extra, " "), func(t *testing.T) {
					exit, _ := runCandidateV2(t, context.Background(), append(append(append([]string(nil), variant...), extra...), "--json"), nil)
					if exit != 2 {
						t.Fatalf("invalid syntax exit=%d", exit)
					}
				})
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	inv, err := cli.Parse(candidateReservedArgs()[0])
	if err != nil {
		t.Fatal(err)
	}
	out := handleV2Candidate(ctx, inv, nil)
	if out.ExitCode != 4 {
		t.Fatalf("reserved handler consulted cancellation: %+v", out)
	}
}

type candidateV2Envelope struct {
	Command string                           `json:"command"`
	OK      bool                             `json:"ok"`
	Data    json.RawMessage                  `json:"data"`
	Errors  []struct{ Code, Message string } `json:"errors"`
}

func runCandidateV2(t *testing.T, ctx context.Context, args []string, deps *v2CandidateDependencies) (int, candidateV2Envelope) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	lookup := lookupV2Candidate
	if deps != nil {
		lookup = func(path string) (cli.Handler, bool) {
			_, ok := lookupV2Candidate(path)
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2CandidateWithPreparation(ctx, inv, s, func(context.Context) (v2CandidateDependencies, error) { return *deps, nil })
			}, ok
		}
	}
	exit := cli.Run(ctx, args, &cli.Session{In: noAccessV2Input{}, Out: &stdout, Err: &stderr}, lookup)
	var out candidateV2Envelope
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("JSON: %v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	return exit, out
}

func candidateTestRegistry(t *testing.T) []byte {
	t.Helper()
	body, err := proxy.CanonicalJSONV1(proxy.ClientSenderRegistryV1{SchemaVersion: 1, Revision: 1, Senders: []proxy.ClientRequestSenderV1{{SenderID: "cq", AdapterID: "cq_config_read_per_call_v1", CredentialDomains: []string{"cq_local_token"}, Transports: []string{"http"}, HookSupported: true}}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}
func candidateV2Inputs(t *testing.T) (CandidatePrepareArgumentsV1, v2CandidateDependencies) {
	t.Helper()
	base := t.TempDir()
	body, bundle := operationalCandidateBundleForTest(t, "target", strings.Repeat("a", 40))
	for name, data := range map[string][]byte{"source": []byte("opaque source\x00bytes"), "bundle": body, "registry": candidateTestRegistry(t), "client": []byte("synthetic executable"), "manifest": []byte("opaque credential attestation"), "policy": []byte("opaque policy")} {
		mode := os.FileMode(0o600)
		if name == "client" {
			mode = 0o700
		}
		if err := os.WriteFile(filepath.Join(base, name), data, mode); err != nil {
			t.Fatal(err)
		}
	}
	args := CandidatePrepareArgumentsV1{InstanceStateRoot: filepath.Join(base, "candidate"), Port: 29280, SourceConfig: filepath.Join(base, "source"), TargetReleaseBundle: filepath.Join(base, "bundle"), TargetReleaseSet: bundle.Digest, ClientBuild: " Exact-Build ", ClientExecutable: filepath.Join(base, "client"), LocalTokenClientRegistry: filepath.Join(base, "registry"), CredentialMode: "none"}
	deps := v2CandidateDependencies{FS: fsutil.OSFileSystem{}, PrepareInput: prepareCandidateInputContext, Absent: func(context.Context, int) error { return nil }, PrepareStop: func(context.Context, proxy.CandidateLifecycleStateV1, []byte) (*candidateStopOperation, error) {
		panic("unexpected stop")
	}}
	return args, deps
}
func candidatePrepareArgs(a CandidatePrepareArgumentsV1) []string {
	args := []string{"proxy", "candidate", "prepare", "--state-dir", a.InstanceStateRoot, "--port", fmt.Sprint(a.Port), "--source-config", a.SourceConfig, "--target-release-bundle", a.TargetReleaseBundle, "--release-digest", a.TargetReleaseSet, "--client-build", a.ClientBuild, "--client-executable", a.ClientExecutable, "--client-registry", a.LocalTokenClientRegistry, "--credential-mode", a.CredentialMode, "--json"}
	if a.CredentialManifest != "" {
		args = append(args, "--credential-manifest", a.CredentialManifest)
	}
	if a.ConfirmReadOnlyCredentials {
		args = append(args, "--confirm-read-only-credentials")
	}
	if a.PolicySnapshot != "" {
		args = append(args, "--policy-snapshot", a.PolicySnapshot)
	}
	if a.ConfirmPayloadCapture {
		args = append(args, "--confirm-payload-capture")
	}
	return args
}
func decodeCandidateV2(t *testing.T, out candidateV2Envelope) v2CandidateStatus {
	t.Helper()
	var data struct {
		Candidate v2CandidateStatus `json:"candidate"`
	}
	if err := json.Unmarshal(out.Data, &data); err != nil {
		t.Fatal(err)
	}
	return data.Candidate
}
func prepareCandidateV2Test(t *testing.T, a CandidatePrepareArgumentsV1, d v2CandidateDependencies) v2CandidateStatus {
	t.Helper()
	exit, out := runCandidateV2(t, context.Background(), candidatePrepareArgs(a), &d)
	if exit != 0 {
		t.Fatalf("prepare=%d %+v", exit, out)
	}
	return decodeCandidateV2(t, out)
}

func TestCLIV2CandidatePrepareStatusRemove(t *testing.T) {
	a, d := candidateV2Inputs(t)
	prepared := prepareCandidateV2Test(t, a, d)
	if prepared.Phase != proxy.CandidatePhasePrepared || prepared.Generation != 1 || prepared.ClientBuild != a.ClientBuild || prepared.TargetReleaseDigest != a.TargetReleaseSet || prepared.ActiveReleaseDigest != nil || prepared.PendingAction != nil || prepared.ClientSafetyReceiptDigest != nil || prepared.ValidationReceiptDigest != nil || prepared.AttemptID != nil || prepared.PayloadCapture {
		t.Fatalf("prepared: %+v", prepared)
	}
	entries, err := os.ReadDir(a.InstanceStateRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("private file %s mode=%v err=%v", entry.Name(), info.Mode(), err)
		}
	}
	before := candidateTreeBytes(t, a.InstanceStateRoot)
	store, _, err := proxy.OpenCandidateLifecycle(context.Background(), d.FS, a.InstanceStateRoot)
	if err != nil {
		t.Fatal(err)
	}
	// Status must work while the mutation lock is held and preserve all bytes.
	for i := 0; i < 2; i++ {
		exit, out := runCandidateV2(t, context.Background(), []string{"proxy", "candidate", "status", "--state-dir", a.InstanceStateRoot, "--json"}, &d)
		if exit != 0 || !reflect.DeepEqual(prepared, decodeCandidateV2(t, out)) {
			t.Fatalf("status=%d %+v", exit, out)
		}
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, candidateTreeBytes(t, a.InstanceStateRoot)) {
		t.Fatal("status changed state")
	}
	exit, _ := runCandidateV2(t, context.Background(), candidatePrepareArgs(a), &d)
	if exit != 6 {
		t.Fatalf("duplicate prepare=%d", exit)
	}
	exit, out := runCandidateV2(t, context.Background(), []string{"proxy", "candidate", "remove", "--state-dir", a.InstanceStateRoot, "--confirm-candidate-state-loss", "--json"}, &d)
	if exit != 0 || decodeCandidateV2(t, out).Phase != proxy.CandidatePhaseRemoved {
		t.Fatalf("remove=%d %+v", exit, out)
	}
	if _, err = os.Lstat(a.InstanceStateRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root remains: %v", err)
	}
	exit, _ = runCandidateV2(t, context.Background(), []string{"proxy", "candidate", "status", "--state-dir", a.InstanceStateRoot, "--json"}, &d)
	if exit != 3 {
		t.Fatalf("missing=%d", exit)
	}
}
func candidateTreeBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[path] = string(body)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCLIV2CandidateInvalidPrepareBeforeCreation(t *testing.T) {
	for _, name := range []string{"digest mismatch", "malformed bundle", "bundle null", "registry unknown", "registry missing field", "registry null", "registry duplicate", "empty attestation", "oversize attestation", "symlink input", "writable input", "empty executable", "non executable", "manifest consent", "occupied port"} {
		t.Run(name, func(t *testing.T) {
			a, d := candidateV2Inputs(t)
			write := func(path string, body []byte) {
				t.Helper()
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			switch name {
			case "digest mismatch":
				a.TargetReleaseSet = strings.Repeat("b", 64)
			case "malformed bundle":
				write(a.TargetReleaseBundle, []byte(`{}`))
			case "bundle null":
				body, _ := os.ReadFile(a.TargetReleaseBundle)
				write(a.TargetReleaseBundle, bytes.Replace(body, []byte(`"roles":[`), []byte(`"roles":null,"other":[`), 1))
			case "registry unknown":
				write(a.LocalTokenClientRegistry, []byte(`{"unknown":1}`))
			case "registry missing field":
				body, _ := os.ReadFile(a.LocalTokenClientRegistry)
				write(a.LocalTokenClientRegistry, bytes.Replace(body, []byte(`"stateful":false,`), nil, 1))
			case "registry null":
				body, _ := os.ReadFile(a.LocalTokenClientRegistry)
				write(a.LocalTokenClientRegistry, bytes.Replace(body, []byte(`"stateful":false`), []byte(`"stateful":null`), 1))
			case "registry duplicate":
				body, _ := os.ReadFile(a.LocalTokenClientRegistry)
				write(a.LocalTokenClientRegistry, bytes.Replace(body, []byte(`"revision":1`), []byte(`"revision":1,"revision":1`), 1))
			case "empty attestation":
				write(a.SourceConfig, nil)
			case "oversize attestation":
				write(a.SourceConfig, make([]byte, candidateConfigMaxBytes+1))
			case "symlink input":
				target := a.SourceConfig
				a.SourceConfig = filepath.Join(filepath.Dir(target), "link")
				if err := os.Symlink(target, a.SourceConfig); err != nil {
					t.Fatal(err)
				}
			case "writable input":
				if err := os.Chmod(a.SourceConfig, 0o666); err != nil {
					t.Fatal(err)
				}
			case "empty executable":
				write(a.ClientExecutable, nil)
			case "non executable":
				if err := os.Chmod(a.ClientExecutable, 0o600); err != nil {
					t.Fatal(err)
				}
			case "manifest consent":
				a.CredentialMode = "read-only"
				a.CredentialManifest = filepath.Join(filepath.Dir(a.SourceConfig), "manifest")
			case "occupied port":
				d.Absent = func(context.Context, int) error { return proxy.ErrCandidateLifecycleInvalid }
			}
			exit, _ := runCandidateV2(t, context.Background(), candidatePrepareArgs(a), &d)
			if exit == 0 {
				t.Fatal("invalid prepare succeeded")
			}
			if _, err := os.Lstat(a.InstanceStateRoot); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid prepare created state: %v", err)
			}
		})
	}
}

func TestCLIV2CandidateAttestations(t *testing.T) {
	a, d := candidateV2Inputs(t)
	a.CredentialMode = "read-only"
	a.CredentialManifest = filepath.Join(filepath.Dir(a.SourceConfig), "manifest")
	a.ConfirmReadOnlyCredentials = true
	a.PolicySnapshot = filepath.Join(filepath.Dir(a.SourceConfig), "policy")
	a.ConfirmPayloadCapture = true
	s := prepareCandidateV2Test(t, a, d)
	if s.CredentialManifestDigest == nil || s.PolicySnapshotDigest == nil || !s.PayloadCapture {
		t.Fatalf("attestations=%+v", s)
	}
}

func candidateStateTransition(t *testing.T, root string, actions ...proxy.CandidateLifecycleAction) proxy.CandidateLifecycleStateV1 {
	t.Helper()
	store, state, err := proxy.OpenCandidateLifecycle(context.Background(), fsutil.OSFileSystem{}, root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, action := range actions {
		state, err = store.Apply(context.Background(), action, func(proxy.CandidateLifecycleStateV1) (string, error) { return strings.Repeat("e", 64), nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	return state
}
func TestCLIV2CandidateStopThenRemove(t *testing.T) {
	for _, validated := range []bool{false, true} {
		t.Run(fmt.Sprint("validated=", validated), func(t *testing.T) {
			a, d := candidateV2Inputs(t)
			prepareCandidateV2Test(t, a, d)
			actions := []proxy.CandidateLifecycleAction{proxy.CandidateActionStart}
			if validated {
				actions = []proxy.CandidateLifecycleAction{proxy.CandidateActionRefreshBarrier, proxy.CandidateActionStart, proxy.CandidateActionValidateRelease}
			}
			candidateStateTransition(t, a.InstanceStateRoot, actions...)
			prepares, runs, closes := 0, 0, 0
			d.PrepareStop = func(ctx context.Context, s proxy.CandidateLifecycleStateV1, token []byte) (*candidateStopOperation, error) {
				prepares++
				if len(token) != 32 {
					t.Fatal("missing control authority")
				}
				return &candidateStopOperation{Close: func() error { closes++; return nil }, Run: func(work, cleanup context.Context, current proxy.CandidateLifecycleStateV1) ([]byte, error) {
					runs++
					wd, _ := work.Deadline()
					cd, _ := cleanup.Deadline()
					if cd.Sub(wd) != 15*time.Second {
						t.Fatalf("cleanup reserve %v", cd.Sub(wd))
					}
					return candidateNativeStopMaterial(current), nil
				}}, nil
			}
			args := []string{"proxy", "candidate", "stop", "--state-dir", a.InstanceStateRoot, "--confirm-client-stopped", "--json"}
			exit, out := runCandidateV2(t, context.Background(), args, &d)
			if exit != 0 || decodeCandidateV2(t, out).Phase != proxy.CandidatePhaseStopped {
				t.Fatalf("stop=%d %+v", exit, out)
			}
			if prepares != 1 || runs != 1 || closes != 1 {
				t.Fatalf("calls=%d/%d/%d", prepares, runs, closes)
			}
			stopped, err := proxy.InspectCandidateLifecycle(context.Background(), d.FS, a.InstanceStateRoot)
			if err != nil || !candidateRemovableState(stopped) {
				t.Fatalf("stop receipt: %v %+v", err, stopped)
			}
			exit, _ = runCandidateV2(t, context.Background(), args, &d)
			if exit != 6 || runs != 1 {
				t.Fatalf("double stop=%d runs=%d", exit, runs)
			}
			exit, out = runCandidateV2(t, context.Background(), []string{"proxy", "candidate", "remove", "--state-dir", a.InstanceStateRoot, "--confirm-candidate-state-loss", "--json"}, &d)
			if exit != 0 || decodeCandidateV2(t, out).Phase != proxy.CandidatePhaseRemoved {
				t.Fatalf("separate remove=%d %+v", exit, out)
			}
		})
	}
}
func TestCLIV2CandidatePreconditionsAndFailedStop(t *testing.T) {
	for _, name := range []string{"prepared stop", "missing consent", "wrong runtime", "legacy stopped", "validated live", "occupied remove", "failed stop", "cancelled stop"} {
		t.Run(name, func(t *testing.T) {
			a, d := candidateV2Inputs(t)
			prepareCandidateV2Test(t, a, d)
			action := "stop"
			confirm := "--confirm-client-stopped"
			want := 6
			switch name {
			case "prepared stop", "missing consent":
			case "legacy stopped":
				candidateStateTransition(t, a.InstanceStateRoot, proxy.CandidateActionStart, proxy.CandidateActionStop)
				action = "remove"
				confirm = "--confirm-candidate-state-loss"
			case "validated live":
				candidateStateTransition(t, a.InstanceStateRoot, proxy.CandidateActionRefreshBarrier, proxy.CandidateActionStart, proxy.CandidateActionValidateRelease)
				action = "remove"
				confirm = "--confirm-candidate-state-loss"
			case "occupied remove":
				action = "remove"
				confirm = "--confirm-candidate-state-loss"
				d.Absent = func(context.Context, int) error { return proxy.ErrCandidateLifecycleInvalid }
			default:
				candidateStateTransition(t, a.InstanceStateRoot, proxy.CandidateActionStart)
			}
			calls := 0
			d.PrepareStop = func(context.Context, proxy.CandidateLifecycleStateV1, []byte) (*candidateStopOperation, error) {
				calls++
				if name == "wrong runtime" {
					return nil, proxy.ErrCandidateLifecycleInvalid
				}
				return &candidateStopOperation{Close: func() error { return nil }, Run: func(context.Context, context.Context, proxy.CandidateLifecycleStateV1) ([]byte, error) {
					if name == "cancelled stop" {
						return nil, context.Canceled
					}
					return nil, errors.New("synthetic cleanup failure")
				}}, nil
			}
			before := candidateTreeBytes(t, a.InstanceStateRoot)
			args := []string{"proxy", "candidate", action, "--state-dir", a.InstanceStateRoot, "--json"}
			if name != "missing consent" {
				args = append(args, confirm)
			}
			if name == "failed stop" {
				want = 1
			}
			if name == "cancelled stop" {
				want = 130
			}
			exit, _ := runCandidateV2(t, context.Background(), args, &d)
			if exit != want {
				t.Fatalf("exit=%d want=%d", exit, want)
			}
			if name == "failed stop" || name == "cancelled stop" {
				state, err := proxy.InspectCandidateLifecycle(context.Background(), d.FS, a.InstanceStateRoot)
				if err != nil || state.Phase != proxy.CandidatePhaseRunning || state.PendingAction != proxy.CandidateActionStop || !state.EffectStarted {
					t.Fatalf("untruthful failure: %+v %v", state, err)
				}
				exit, _ = runCandidateV2(t, context.Background(), args, &d)
				if exit != 6 || calls != 1 {
					t.Fatalf("replayed failed stop: %d calls=%d", exit, calls)
				}
			} else if !reflect.DeepEqual(before, candidateTreeBytes(t, a.InstanceStateRoot)) {
				t.Fatal("precondition failure mutated state")
			}
		})
	}
}
func TestCLIV2CandidateNativeStopMarkerBinding(t *testing.T) {
	s := proxy.CandidateLifecycleStateV1{OperationID: strings.Repeat("a", 32), ProxyInstanceID: strings.Repeat("b", 32), ValidationRunID: strings.Repeat("c", 64), Port: 29280, Generation: 6, Phase: proxy.CandidatePhaseStopped}
	s.EffectReceiptDigest = proxy.CandidateEffectReceiptDigest(proxy.CandidateActionStop, candidateNativeStopMaterial(s))
	s.Generation++
	if !candidateRemovableState(s) {
		t.Fatal("valid marker rejected")
	}
	for _, mutate := range []func(*proxy.CandidateLifecycleStateV1){func(s *proxy.CandidateLifecycleStateV1) { s.Generation++ }, func(s *proxy.CandidateLifecycleStateV1) { s.Generation = 0 }, func(s *proxy.CandidateLifecycleStateV1) { s.Port++ }, func(s *proxy.CandidateLifecycleStateV1) { s.OperationID = strings.Repeat("d", 32) }, func(s *proxy.CandidateLifecycleStateV1) { s.ProxyInstanceID = strings.Repeat("d", 32) }, func(s *proxy.CandidateLifecycleStateV1) { s.ValidationRunID = strings.Repeat("d", 64) }, func(s *proxy.CandidateLifecycleStateV1) { s.PendingAction = proxy.CandidateActionStop }, func(s *proxy.CandidateLifecycleStateV1) { s.EffectStarted = true }, func(s *proxy.CandidateLifecycleStateV1) { s.EffectReceiptDigest = strings.Repeat("e", 64) }} {
		copy := s
		mutate(&copy)
		if candidateRemovableState(copy) {
			t.Fatalf("tampered marker accepted: %+v", copy)
		}
	}
}
func TestCLIV2CandidateReceiptReadOnly(t *testing.T) {
	for _, outcome := range []string{"published", "conflicted"} {
		t.Run(outcome, func(t *testing.T) {
			a, d := candidateV2Inputs(t)
			prepareCandidateV2Test(t, a, d)
			attempt := strings.Repeat("a", 32)
			key := bytes.Repeat([]byte{1}, 32)
			root := filepath.Join(a.InstanceStateRoot, "receipt-export")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			receipt := proxy.CandidateReceiptInspectionV1{Found: true, AttemptID: attempt, Outcome: outcome, ReceiptDigest: strings.Repeat("b", 64), PromotionDigest: strings.Repeat("c", 64)}
			body, err := proxy.CandidateReceiptStoredBytesV1(receipt, key)
			if err != nil {
				t.Fatal(err)
			}
			for name, value := range map[string][]byte{"key": key, attempt + ".json": body} {
				if err = os.WriteFile(filepath.Join(root, name), value, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// A contradictory different retained attempt must not select latest or replace
			// the exact addressed receipt. Its result has independent authenticated bytes.
			other := receipt
			other.AttemptID = strings.Repeat("d", 32)
			other.Outcome = "published"
			other.ReceiptDigest = strings.Repeat("e", 64)
			otherBody, _ := proxy.CandidateReceiptStoredBytesV1(other, key)
			if err = os.WriteFile(filepath.Join(root, other.AttemptID+".json"), otherBody, 0o600); err != nil {
				t.Fatal(err)
			}
			before := candidateTreeBytes(t, a.InstanceStateRoot)
			args := []string{"proxy", "candidate", "receipt", "show", "--state-dir", a.InstanceStateRoot, "--attempt-id", attempt, "--json"}
			for i := 0; i < 2; i++ {
				exit, out := runCandidateV2(t, context.Background(), args, &d)
				want := 0
				if outcome == "conflicted" {
					want = 6
				}
				if exit != want {
					t.Fatalf("receipt=%d %+v", exit, out)
				}
				var data struct {
					Receipt v2CandidateReceipt `json:"receipt"`
				}
				if err = json.Unmarshal(out.Data, &data); err != nil || data.Receipt.Outcome != outcome || data.Receipt.AttemptID != attempt {
					t.Fatalf("wrong receipt: %+v %v", data, err)
				}
			}
			if !reflect.DeepEqual(before, candidateTreeBytes(t, a.InstanceStateRoot)) {
				t.Fatal("receipt lookup wrote state")
			}
			args[len(args)-2] = strings.Repeat("f", 32)
			exit, _ := runCandidateV2(t, context.Background(), args, &d)
			if exit != 3 {
				t.Fatalf("missing receipt=%d", exit)
			}
		})
	}
}
func TestCLIV2CandidateBudgetBeginsBeforePreparation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			if !cancelled {
				var deadlineCancel context.CancelFunc
				parent, deadlineCancel = context.WithTimeout(parent, 10*time.Millisecond)
				defer deadlineCancel()
			}
			inv, err := cli.Parse([]string{"proxy", "candidate", "status", "--state-dir", "/missing/candidate"})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			out := handleV2CandidateWithPreparation(parent, inv, nil, func(ctx context.Context) (v2CandidateDependencies, error) {
				calls++
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("missing preparation budget")
				}
				if cancelled {
					cancel()
				}
				<-ctx.Done()
				return v2CandidateDependencies{}, errors.New("late preparation failure")
			})
			want := 7
			if cancelled {
				want = 130
			}
			if out.ExitCode != want || calls != 1 {
				t.Fatalf("preparation outcome=%+v calls=%d", out, calls)
			}
		})
	}
}
func TestCLIV2CandidateWrongRootRemoval(t *testing.T) {
	a, d := candidateV2Inputs(t)
	prepareCandidateV2Test(t, a, d)
	b, e := candidateV2Inputs(t)
	prepareCandidateV2Test(t, b, e)
	state := candidateStateTransition(t, a.InstanceStateRoot, proxy.CandidateActionRemove)
	before := candidateTreeBytes(t, b.InstanceStateRoot)
	if err := removeCandidateStateRoot(context.Background(), d.FS, b.InstanceStateRoot, state); !errors.Is(err, proxy.ErrCandidateLifecycleInvalid) {
		t.Fatalf("wrong root: %v", err)
	}
	if !reflect.DeepEqual(before, candidateTreeBytes(t, b.InstanceStateRoot)) {
		t.Fatal("wrong root mutated")
	}
}

type candidateRootOwnerFS struct {
	fsutil.OSFileSystem
	executable, parent string
	all                bool
}

func (f candidateRootOwnerFS) FileOwnerUID(info os.FileInfo) (uint64, bool) {
	if f.all || info.Name() == filepath.Base(f.executable) || info.Name() == filepath.Base(f.parent) {
		return 0, true
	}
	return f.OSFileSystem.FileOwnerUID(info)
}

type candidateSwapFS struct {
	fsutil.OSFileSystem
	path    string
	swapped bool
}

func (f *candidateSwapFS) OpenNoFollow(path string) (fsutil.SecureReadFile, error) {
	if path == f.path && !f.swapped {
		f.swapped = true
		if err := os.Rename(path, path+".old"); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte("replacement executable"), 0o700); err != nil {
			return nil, err
		}
	}
	return f.OSFileSystem.OpenNoFollow(path)
}
func TestCLIV2CandidateTrustedExecutableAndRootOwnership(t *testing.T) {
	a, d := candidateV2Inputs(t)
	d.FS = candidateRootOwnerFS{executable: a.ClientExecutable, parent: filepath.Dir(a.ClientExecutable)}
	if _, err := digestCandidateExecutableContext(context.Background(), d.FS, a.ClientExecutable, candidateExecutableMaxBytes); err != nil {
		t.Fatalf("trusted root executable rejected: %v", err)
	}
	d.FS = candidateRootOwnerFS{all: true}
	exit, _ := runCandidateV2(t, context.Background(), candidatePrepareArgs(a), &d)
	if exit != 6 {
		t.Fatalf("root-owned state parent accepted: %d", exit)
	}
	if _, err := os.Lstat(a.InstanceStateRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state created: %v", err)
	}
}
func TestCLIV2CandidateExecutableIdentityRace(t *testing.T) {
	a, d := candidateV2Inputs(t)
	d.FS = &candidateSwapFS{path: a.ClientExecutable}
	exit, _ := runCandidateV2(t, context.Background(), candidatePrepareArgs(a), &d)
	if exit != 6 {
		t.Fatalf("inode race=%d", exit)
	}
	if _, err := os.Lstat(a.InstanceStateRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("race created state")
	}
}

func TestCLIV2CandidateHelpAndSyntaxNoAccess(t *testing.T) {
	paths := []string{"proxy candidate", "proxy candidate client-safety", "proxy candidate release", "proxy candidate receipt", "proxy candidate prepare", "proxy candidate status", "proxy candidate stop", "proxy candidate remove", "proxy candidate receipt show", "proxy candidate start", "proxy candidate client-safety refresh", "proxy candidate release activate", "proxy candidate release validate"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			args := strings.Fields(path)
			args = append(args, "--help", "--json")
			var out bytes.Buffer
			exit := cli.Run(context.Background(), args, &cli.Session{Out: &out, Err: &bytes.Buffer{}}, func(string) (cli.Handler, bool) { panic("help accessed handler") })
			want, err := os.ReadFile(filepath.Join("..", "..", "specs", "cli-v2", "help", strings.ReplaceAll(path, " ", "-")+".txt"))
			if err != nil {
				t.Fatal(err)
			}
			if exit != 0 || !bytes.Equal(out.Bytes(), want) {
				t.Fatalf("help mismatch exit=%d", exit)
			}
		})
	}
	for _, args := range [][]string{
		{"proxy", "candidate", "start", "--state-dir", "relative"},
		{"proxy", "candidate", "start", "--state-dir", "/"},
		{"proxy", "candidate", "start", "--state-dir", "/tmp/a/../b"},
		{"proxy", "candidate", "start", "--state-dir", "/tmp/a", "--timeout", "29s"},
		{"proxy", "candidate", "start", "--state-dir", "/tmp/a", "--timeout", "91s"},
		{"proxy", "candidate", "stop", "--state-dir", "/tmp/a", "--timeout", "30s"},
		{"proxy", "candidate", "remove", "--state-dir", "/tmp/a", "--timeout", "30s"},
		{"proxy", "candidate", "receipt", "show", "--state-dir", "/tmp/a", "--attempt-id", "bad"},
		{"proxy", "candidate", "artifact", "switch", "--state-dir", "/tmp/a", "--role", "worker", "--release-set", strings.Repeat("a", 64), "--validation-run", strings.Repeat("a", 64)},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out bytes.Buffer
			exit := cli.Run(context.Background(), append(args, "--json"), &cli.Session{Out: &out, Err: &bytes.Buffer{}}, func(string) (cli.Handler, bool) { panic("invalid syntax reached lookup") })
			if exit != 2 {
				t.Fatalf("syntax exit=%d %s", exit, out.String())
			}
		})
	}
}

func TestCLIV2CandidateRegistryCredentialDomainsNullability(t *testing.T) {
	for _, tc := range []struct {
		name    string
		domains []string
		exit    int
	}{{"null", nil, 6}, {"empty_array", []string{}, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			a, d := candidateV2Inputs(t)
			var registry proxy.ClientSenderRegistryV1
			if err := json.Unmarshal(candidateTestRegistry(t), &registry); err != nil {
				t.Fatal(err)
			}
			registry.Senders = append(registry.Senders, proxy.ClientRequestSenderV1{SenderID: "additional", AdapterID: "synthetic", CredentialDomains: tc.domains, Transports: []string{"http"}, HookSupported: true})
			body, err := proxy.CanonicalJSONV1(registry)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(a.LocalTokenClientRegistry, body, 0o600); err != nil {
				t.Fatal(err)
			}
			exit, out := runCandidateV2(t, context.Background(), candidatePrepareArgs(a), &d)
			if exit != tc.exit {
				t.Fatalf("prepare exit=%d want=%d %+v", exit, tc.exit, out)
			}
			if tc.exit != 0 {
				if _, err = os.Stat(a.InstanceStateRoot); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid registry created state: %v", err)
				}
			}
		})
	}
}
