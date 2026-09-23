package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

var terminalCandidateFixtures = []terminalFixtureRegistration{
	{"cq proxy candidate prepare", "Success", "stdout", "text/cq-proxy-candidate-prepare/02-success.txt"},
	{"cq proxy candidate prepare", "Failure", "stderr", "text/cq-proxy-candidate-prepare/03-failure.txt"},
	{"cq proxy candidate prepare", "Timeout · inspect retained candidate", "stderr", "text/cq-proxy-candidate-prepare/04-timeout-inspect-retained-candidate.txt"},
	{"cq proxy candidate receipt show", "Success", "stdout", "text/cq-proxy-candidate-receipt-show/02-success.txt"},
	{"cq proxy candidate receipt show", "Failure", "stderr", "text/cq-proxy-candidate-receipt-show/03-failure.txt"},
	{"cq proxy candidate receipt show", "Failure · retained conflicted receipt", "stdout", "text/cq-proxy-candidate-receipt-show/04-failure-retained-conflicted-receipt.txt"},
	{"cq proxy candidate receipt show", "Failure · retained conflicted receipt", "stderr", "text/cq-proxy-candidate-receipt-show/05-failure-retained-conflicted-receipt.txt"},
	{"cq proxy candidate client-safety refresh", "Failure", "stderr", "text/cq-proxy-candidate-client-safety-refresh/02-failure.txt"},
	{"cq proxy candidate start", "Failure", "stderr", "text/cq-proxy-candidate-start/02-failure.txt"},
	{"cq proxy candidate release activate", "Failure", "stderr", "text/cq-proxy-candidate-release-activate/02-failure.txt"},
	{"cq proxy candidate release validate", "Failure", "stderr", "text/cq-proxy-candidate-release-validate/02-failure.txt"},
	{"cq proxy candidate status", "Success", "stdout", "text/cq-proxy-candidate-status/02-success.txt"},
	{"cq proxy candidate status", "Failure", "stderr", "text/cq-proxy-candidate-status/03-failure.txt"},
	{"cq proxy candidate status", "Success · pending external effect", "stdout", "text/cq-proxy-candidate-status/04-success-pending-external-effect.txt"},
	{"cq proxy candidate stop", "Success", "stdout", "text/cq-proxy-candidate-stop/02-success.txt"},
	{"cq proxy candidate stop", "Failure", "stderr", "text/cq-proxy-candidate-stop/03-failure.txt"},
	{"cq proxy candidate remove", "Success", "stdout", "text/cq-proxy-candidate-remove/02-success.txt"},
	{"cq proxy candidate remove", "Failure", "stderr", "text/cq-proxy-candidate-remove/03-failure.txt"},
}

func candidateFixtureState() v2CandidateStatus {
	return v2CandidateStatus{
		StateDir: "/Users/alex/cq-candidate-29280", InstanceID: "11112222333344445555666677778888", Port: 29280,
		OperationID: "88887777666655554444333322221111", ValidationRunID: strings.Repeat("12345678", 8),
		Phase: proxy.CandidatePhasePrepared, Generation: 1, UpdatedAt: "2026-09-23T15:30:00Z",
		ClientBuild: "0.114.0", CredentialMode: "none", SourceConfigDigest: strings.Repeat("1", 64),
		TargetBundleFileDigest: strings.Repeat("2", 64), TargetReleaseDigest: "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd",
		ClientExecutableDigest: strings.Repeat("3", 64), ClientRegistryDigest: strings.Repeat("4", 64),
	}
}

func TestTerminalCandidateRegistration(t *testing.T) {
	data, err := os.ReadFile("../../specs/cli-v2/presentation/examples/index.json")
	if err != nil {
		t.Fatal(err)
	}
	var index struct{ Fixtures []terminalFixtureRegistration }
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	leaves := map[string]bool{}
	for _, path := range []string{"prepare", "receipt show", "client-safety refresh", "start", "release activate", "release validate", "status", "stop", "remove"} {
		leaves["cq proxy candidate "+path] = true
	}
	var want []terminalFixtureRegistration
	for _, f := range index.Fixtures {
		if leaves[f.Command] && f.Scenario != "Help" {
			want = append(want, f)
		}
	}
	if !reflect.DeepEqual(terminalCandidateFixtures, want) {
		t.Fatalf("candidate fixture registration differs from index\nwant: %+v\ngot: %+v", want, terminalCandidateFixtures)
	}
}

func TestTerminalCandidateFixtures(t *testing.T) {
	state := candidateFixtureState()
	receipt := v2CandidateReceipt{AttemptID: "0123456789abcdef0123456789abcdef", Outcome: "published", ReceiptDigest: strings.Repeat("5", 64), PromotionDigest: strings.Repeat("6", 64)}
	for _, f := range terminalCandidateFixtures {
		if f.Stream != "stdout" {
			continue
		}
		t.Run(f.File, func(t *testing.T) {
			var got string
			switch f.File {
			case "text/cq-proxy-candidate-receipt-show/02-success.txt", "text/cq-proxy-candidate-receipt-show/04-failure-retained-conflicted-receipt.txt":
				r := receipt
				if strings.Contains(f.File, "conflicted") {
					r.Outcome = "conflicted"
				}
				got = terminalCandidateReceipt(r, terminalFixtureOptions(100))
			default:
				s := state
				switch f.File {
				case "text/cq-proxy-candidate-status/04-success-pending-external-effect.txt":
					s.Phase, s.Generation, s.PendingAction, s.EffectStarted = proxy.CandidatePhaseRunning, 6, v2AccountString("stop"), true
				case "text/cq-proxy-candidate-stop/02-success.txt":
					s.Phase, s.Generation, s.EffectReceiptDigest = proxy.CandidatePhaseStopped, 7, v2AccountString(strings.Repeat("9", 64))
				case "text/cq-proxy-candidate-remove/02-success.txt":
					s.Phase, s.Generation, s.EffectReceiptDigest = proxy.CandidatePhaseRemoved, 10, v2AccountString(strings.Repeat("9", 64))
				}
				got = terminalCandidateStatus(s, terminalFixtureOptions(100))
			}
			assertTerminalFixture(t, f.File, got)
		})
	}
}

func TestTerminalCandidateFailureFixtures(t *testing.T) {
	a, _ := candidateV2Inputs(t)
	receipt := v2CandidateReceipt{AttemptID: "0123456789abcdef0123456789abcdef", Outcome: "conflicted", ReceiptDigest: strings.Repeat("5", 64), PromotionDigest: strings.Repeat("6", 64)}
	for _, f := range terminalCandidateFixtures {
		if f.Stream != "stderr" {
			continue
		}
		t.Run(f.File, func(t *testing.T) {
			path := strings.TrimPrefix(f.Command, "cq ")
			var args []string
			if path == "proxy candidate prepare" {
				args = candidatePrepareArgs(a)
				args = args[:len(args)-1]
			} else {
				for _, reserved := range candidateReservedArgs() {
					if strings.HasPrefix(strings.Join(reserved, " "), path+" ") {
						args = reserved
						break
					}
				}
				if args == nil {
					args = strings.Fields(path)
					args = append(args, "--state-dir", a.InstanceStateRoot)
					if path == "proxy candidate receipt show" {
						args = append(args, "--attempt-id", receipt.AttemptID)
					} else if path == "proxy candidate stop" {
						args = append(args, "--confirm-client-stopped")
					} else if path == "proxy candidate remove" {
						args = append(args, "--confirm-candidate-state-loss")
					}
				}
			}
			var stdout, stderr bytes.Buffer
			o := terminalFixtureOptions(100)
			lookup := func(requested string) (cli.Handler, bool) {
				if requested != path {
					return nil, false
				}
				return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
					if strings.Contains(path, "client-safety") || path == "proxy candidate start" || strings.Contains(path, "release ") {
						return handleV2CandidateWithPreparation(ctx, inv, s, func(context.Context) (v2CandidateDependencies, error) {
							panic("unavailable command accessed dependencies")
						})
					}
					var out cli.Outcome
					switch {
					case strings.Contains(f.File, "timeout"):
						out = v2SelectionFailure(7, "candidate_timeout", "Candidate operation timed out; inspect candidate status before retrying.")
					case strings.Contains(f.File, "conflicted-receipt"):
						out = v2SelectionFailure(6, "candidate_receipt_conflicted", "Candidate receipt is conflicted.")
						out.Terminal = func(o cli.TerminalOptions) string { return terminalCandidateReceipt(receipt, o) }
					case path == "proxy candidate receipt show" || path == "proxy candidate status":
						out = v2SelectionFailure(3, "candidate_not_found", "Candidate state or receipt does not exist.")
					default:
						out = v2SelectionFailure(6, "candidate_conflict", "Candidate state conflicts with this operation.")
					}
					return out
				}, true
			}
			exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, lookup)
			if exit == 0 {
				t.Fatal("failure exited successfully")
			}
			assertTerminalFixture(t, f.File, stderr.String())
			if strings.Contains(f.File, "conflicted-receipt") {
				assertTerminalFixture(t, "text/cq-proxy-candidate-receipt-show/04-failure-retained-conflicted-receipt.txt", stdout.String())
			} else if stdout.Len() != 0 {
				t.Fatalf("failure wrote stdout: %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), "\x1b[31m") {
				t.Fatal("failure role missing")
			}
		})
	}
}

func TestTerminalCandidateWidthsAndRoles(t *testing.T) {
	s := candidateFixtureState()
	s.Generation = math.MaxUint64
	s.PendingAction = v2AccountString("stop")
	s.EffectStarted = true
	s.EffectReceiptDigest = v2AccountString(strings.Repeat("9", 64))
	receipt := v2CandidateReceipt{AttemptID: strings.Repeat("a", 32), Outcome: "published", ReceiptDigest: strings.Repeat("b", 64), PromotionDigest: strings.Repeat("c", 64)}
	for _, width := range []int{20, 40, 80, 120} {
		o := terminalFixtureOptions(width)
		got := terminalCandidateStatus(s, o)
		for _, output := range []string{got, terminalCandidateReceipt(receipt, o)} {
			for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
				if ansi.StringWidth(line) > min(width, 100) {
					t.Fatalf("width %d: %q", width, line)
				}
			}
		}
		plain := ansi.Strip(got)
		compact := strings.Join(strings.Fields(plain), "")
		for _, value := range []string{"18,446,744,073,709,551,615", strings.Repeat("9", 64), "stop", "Active release"} {
			if !strings.Contains(compact, strings.ReplaceAll(value, " ", "")) {
				t.Fatalf("width %d lost %q", width, value)
			}
		}
		if !strings.Contains(got, "\x1b[33m") || !strings.Contains(got, "\x1b[1;34m") || !strings.Contains(got, "\x1b[35m") {
			t.Fatalf("width %d lost semantic roles", width)
		}
		if receiptOut := terminalCandidateReceipt(receipt, o); !strings.Contains(receiptOut, "\x1b[32m") || !strings.Contains(strings.Join(strings.Fields(ansi.Strip(receiptOut)), ""), receipt.ReceiptDigest) {
			t.Fatalf("width %d lost published receipt identity", width)
		}
		receipt.Outcome = "conflicted"
		if !strings.Contains(terminalCandidateReceipt(receipt, o), "\x1b[31m") {
			t.Fatalf("width %d lost conflicted receipt role", width)
		}
		receipt.Outcome = "published"
		o.Colour = false
		if strings.Contains(terminalCandidateStatus(s, o), "\x1b[") || strings.Contains(terminalCandidateReceipt(receipt, o), "\x1b[") {
			t.Fatalf("width %d ignored NO_COLOR", width)
		}
	}
}

func TestTerminalCandidateAdapterAndCompatibility(t *testing.T) {
	a, d := candidateV2Inputs(t)
	args := candidatePrepareArgs(a)
	args = args[:len(args)-1]
	var terminalOut, terminalErr bytes.Buffer
	o := terminalFixtureOptions(100)
	lookup := func(path string) (cli.Handler, bool) {
		_, ok := lookupV2Candidate(path)
		return func(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
			return handleV2CandidateWithPreparation(ctx, inv, session, func(context.Context) (v2CandidateDependencies, error) { return d, nil })
		}, ok
	}
	exit := cli.Run(context.Background(), args, &cli.Session{Out: &terminalOut, Err: &terminalErr, OutTerminal: o, ErrTerminal: o}, lookup)
	if exit != 0 || !strings.Contains(ansi.Strip(terminalOut.String()), "Candidate · prepared\n") {
		t.Fatalf("terminal prepare exit=%d out=%q err=%q", exit, terminalOut.String(), terminalErr.String())
	}
	if strings.Contains(terminalOut.String(), "Active release         "+a.TargetReleaseSet) {
		t.Fatal("fabricated active release")
	}
	var plain bytes.Buffer
	exit = cli.Run(context.Background(), []string{"proxy", "candidate", "status", "--state-dir", a.InstanceStateRoot}, &cli.Session{Out: &plain, Err: &terminalErr}, lookup)
	if exit != 0 || !strings.HasPrefix(plain.String(), "Candidate: ") {
		t.Fatalf("plain changed: %d %q", exit, plain.String())
	}
	exit, envelope := runCandidateV2(t, context.Background(), []string{"proxy", "candidate", "status", "--state-dir", a.InstanceStateRoot, "--json"}, &d)
	if exit != 0 || decodeCandidateV2(t, envelope).ActiveReleaseDigest != nil {
		t.Fatalf("JSON changed: %d %+v", exit, envelope)
	}
}

func TestTerminalCandidateStopAndRemoveEffects(t *testing.T) {
	a, d := candidateV2Inputs(t)
	prepareCandidateV2Test(t, a, d)
	candidateStateTransition(t, a.InstanceStateRoot, proxy.CandidateActionStart)
	d.PrepareStop = func(context.Context, proxy.CandidateLifecycleStateV1, []byte) (*candidateStopOperation, error) {
		return &candidateStopOperation{Close: func() error { return nil }, Run: func(_, _ context.Context, current proxy.CandidateLifecycleStateV1) ([]byte, error) {
			return candidateNativeStopMaterial(current), nil
		}}, nil
	}
	lookup := func(path string) (cli.Handler, bool) {
		_, ok := lookupV2Candidate(path)
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2CandidateWithPreparation(ctx, inv, s, func(context.Context) (v2CandidateDependencies, error) { return d, nil })
		}, ok
	}
	run := func(args ...string) (int, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		o := terminalFixtureOptions(100)
		exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, lookup)
		return exit, ansi.Strip(stdout.String()), ansi.Strip(stderr.String())
	}
	exit, stdout, stderr := run("proxy", "candidate", "stop", "--state-dir", a.InstanceStateRoot, "--confirm-client-stopped")
	if exit != 0 || stderr != "" || !strings.Contains(stdout, "Candidate · stopped\n") {
		t.Fatalf("stop=%d out=%q err=%q", exit, stdout, stderr)
	}
	stopped, err := proxy.InspectCandidateLifecycle(context.Background(), d.FS, a.InstanceStateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "Effect receipt         "+stopped.EffectReceiptDigest) || stopped.EffectReceiptDigest == "" || !strings.Contains(stdout, "Active release         unknown") {
		t.Fatalf("lost known effect or invented release: %q", stdout)
	}
	stale := stopped
	stale.Generation--
	if candidateRemovableState(stale) {
		t.Fatal("stale generation accepted for removal")
	}
	exit, stdout, stderr = run("proxy", "candidate", "remove", "--state-dir", a.InstanceStateRoot, "--confirm-candidate-state-loss")
	if exit != 0 || stderr != "" || !strings.Contains(stdout, "Candidate · removed\n") {
		t.Fatalf("remove=%d out=%q err=%q", exit, stdout, stderr)
	}
	exit, stdout, stderr = run("proxy", "candidate", "receipt", "show", "--state-dir", a.InstanceStateRoot, "--attempt-id", strings.Repeat("a", 32))
	if exit != 3 || stdout != "" || !strings.Contains(stderr, "candidate_not_found") {
		t.Fatalf("missing receipt=%d out=%q err=%q", exit, stdout, stderr)
	}
}

func TestTerminalCandidateReceiptAdapter(t *testing.T) {
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
				if err := os.WriteFile(filepath.Join(root, name), value, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := candidateTreeBytes(t, a.InstanceStateRoot)
			lookup := func(path string) (cli.Handler, bool) {
				_, ok := lookupV2Candidate(path)
				return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
					return handleV2CandidateWithPreparation(ctx, inv, s, func(context.Context) (v2CandidateDependencies, error) { return d, nil })
				}, ok
			}
			var stdout, stderr bytes.Buffer
			o := terminalFixtureOptions(100)
			exit := cli.Run(context.Background(), []string{"proxy", "candidate", "receipt", "show", "--state-dir", a.InstanceStateRoot, "--attempt-id", attempt}, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, lookup)
			wantExit := 0
			if outcome == "conflicted" {
				wantExit = 6
			}
			if exit != wantExit || !strings.Contains(ansi.Strip(stdout.String()), "Candidate receipt · "+outcome) || !strings.Contains(ansi.Strip(stdout.String()), receipt.ReceiptDigest) {
				t.Fatalf("receipt=%d out=%q err=%q", exit, stdout.String(), stderr.String())
			}
			if outcome == "conflicted" && !strings.Contains(ansi.Strip(stderr.String()), "candidate_receipt_conflicted") {
				t.Fatalf("missing conflict diagnostic: %q", stderr.String())
			}
			if !reflect.DeepEqual(before, candidateTreeBytes(t, a.InstanceStateRoot)) {
				t.Fatal("receipt display wrote candidate state")
			}
		})
	}
}

func TestTerminalCandidatePendingStopAfterFailure(t *testing.T) {
	a, d := candidateV2Inputs(t)
	prepareCandidateV2Test(t, a, d)
	candidateStateTransition(t, a.InstanceStateRoot, proxy.CandidateActionStart)
	d.PrepareStop = func(context.Context, proxy.CandidateLifecycleStateV1, []byte) (*candidateStopOperation, error) {
		return &candidateStopOperation{Close: func() error { return nil }, Run: func(context.Context, context.Context, proxy.CandidateLifecycleStateV1) ([]byte, error) {
			return nil, errors.New("synthetic stop failure")
		}}, nil
	}
	lookup := func(path string) (cli.Handler, bool) {
		_, ok := lookupV2Candidate(path)
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2CandidateWithPreparation(ctx, inv, s, func(context.Context) (v2CandidateDependencies, error) { return d, nil })
		}, ok
	}
	run := func(args ...string) (int, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		o := terminalFixtureOptions(100)
		exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, lookup)
		return exit, ansi.Strip(stdout.String()), ansi.Strip(stderr.String())
	}
	exit, stdout, stderr := run("proxy", "candidate", "stop", "--state-dir", a.InstanceStateRoot, "--confirm-client-stopped")
	if exit != 1 || stdout != "" || !strings.Contains(stderr, "candidate_io_failed") {
		t.Fatalf("failed stop=%d out=%q err=%q", exit, stdout, stderr)
	}
	exit, stdout, stderr = run("proxy", "candidate", "status", "--state-dir", a.InstanceStateRoot)
	if exit != 0 || stderr != "" || !strings.Contains(stdout, "Pending action  stop") || !strings.Contains(stdout, "Effect started  yes") || !strings.Contains(stdout, "Effect receipt         —") {
		t.Fatalf("pending state=%d out=%q err=%q", exit, stdout, stderr)
	}
	exit, stdout, stderr = run("proxy", "candidate", "stop", "--state-dir", a.InstanceStateRoot, "--confirm-client-stopped")
	if exit != 6 || stdout != "" || !strings.Contains(stderr, "candidate_conflict") {
		t.Fatalf("stale retry=%d out=%q err=%q", exit, stdout, stderr)
	}
}

type candidatePersistenceFailureFS struct {
	fsutil.OSFileSystem
	root string
}

func (f candidatePersistenceFailureFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	directory, err := f.OSFileSystem.OpenSecureDirectory(path)
	if err != nil || filepath.Clean(path) != filepath.Clean(f.root) {
		return directory, err
	}
	return candidatePersistenceFailureDirectory{SecureDirectory: directory}, nil
}

type candidatePersistenceFailureDirectory struct{ fsutil.SecureDirectory }

func (candidatePersistenceFailureDirectory) CreateExclusive(string, os.FileMode) (fsutil.DurableFile, error) {
	return nil, errors.New("injected candidate state persistence failure")
}

func TestTerminalCandidateFailedPersistence(t *testing.T) {
	a, d := candidateV2Inputs(t)
	prepareCandidateV2Test(t, a, d)
	candidateStateTransition(t, a.InstanceStateRoot, proxy.CandidateActionStart)
	before := candidateTreeBytes(t, a.InstanceStateRoot)
	d.FS = candidatePersistenceFailureFS{root: a.InstanceStateRoot}
	d.PrepareStop = func(context.Context, proxy.CandidateLifecycleStateV1, []byte) (*candidateStopOperation, error) {
		return &candidateStopOperation{Close: func() error { return nil }, Run: func(context.Context, context.Context, proxy.CandidateLifecycleStateV1) ([]byte, error) {
			t.Fatal("external effect ran after failed intent persistence")
			return nil, nil
		}}, nil
	}
	lookup := func(path string) (cli.Handler, bool) {
		_, ok := lookupV2Candidate(path)
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2CandidateWithPreparation(ctx, inv, s, func(context.Context) (v2CandidateDependencies, error) { return d, nil })
		}, ok
	}
	var stdout, stderr bytes.Buffer
	o := terminalFixtureOptions(100)
	exit := cli.Run(context.Background(), []string{"proxy", "candidate", "stop", "--state-dir", a.InstanceStateRoot, "--confirm-client-stopped"}, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, lookup)
	if exit != 1 || stdout.Len() != 0 || !strings.Contains(ansi.Strip(stderr.String()), "candidate_io_failed") {
		t.Fatalf("failed persistence=%d out=%q err=%q", exit, stdout.String(), stderr.String())
	}
	if !reflect.DeepEqual(before, candidateTreeBytes(t, a.InstanceStateRoot)) {
		t.Fatal("failed persistence altered candidate files")
	}
}

func TestTerminalCandidateHistoricalTime(t *testing.T) {
	s := candidateFixtureState()
	s.UpdatedAt = "2026-09-22T15:30:00Z"
	o := terminalFixtureOptions(100)
	if !strings.Contains(ansi.Strip(terminalCandidateStatus(s, o)), "1d ago · 22 Sep 2026") {
		t.Fatal("lost historical timestamp")
	}
}
