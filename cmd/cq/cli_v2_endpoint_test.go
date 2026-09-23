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
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

func init() {
	registerV2Fixture("endpoint-finalise-drain-only", endpointDrainOnlyFixture)
	registerV2Fixture("endpoint-inspect", func(t *testing.T) *v2Fixture {
		path, _, _ := endpointV2State(t, "quarantined")
		deps := v2EndpointDependenciesForPath(path)
		deps.Transition = func(context.Context, string, legacyEndpointTransitionOptions) (codexprov.LegacyCredentialEndpointTransitionStatus, error) {
			panic("inspect reached mutation")
		}
		return &v2Fixture{In: noAccessV2Input{}, Lookup: endpointV2Lookup(func(context.Context) (v2EndpointDependencies, error) { return deps, nil })}
	})
}

func endpointDrainOnlyFixture(t *testing.T) *v2Fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix endpoint fixture")
	}
	_, path := createCLIRefusedLegacyEndpoint(t)
	ctx := context.Background()
	snapshot, err := codexprov.InspectLegacyCredentialEndpoint(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := codexprov.PrepareLegacyCredentialEndpointTransition(ctx, path, snapshot, codexprov.DrainAuthorityFunc(func(context.Context, string) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(ticket)
	if err != nil {
		t.Fatal(err)
	}
	ticketFile := filepath.Join(filepath.Dir(path), "ticket.json")
	if err := os.WriteFile(ticketFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &v2Fixture{In: noAccessV2Input{}}
	f.Lookup = func(command string) (cli.Handler, bool) {
		_, ok := lookupV2Endpoint(command)
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			inv.Options["ticket-file"] = []string{ticketFile}
			result := handleV2EndpointWithPreparation(ctx, inv, s, func(context.Context) (v2EndpointDependencies, error) { return v2EndpointDependenciesForPath(path), nil })
			status, err := codexprov.InspectLegacyCredentialEndpointTransition(context.Background(), path)
			if err != nil || status.State != codexprov.CredentialEndpointMaintenanceActivated || status.Ticket != ticket {
				t.Fatal("failed finalise lost activated rollback authority")
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(path), ticket.QuarantineName)); err != nil {
				t.Fatal("failed finalise lost quarantine")
			}
			return result
		}, ok
	}
	return f
}

func TestCLIV2EndpointContract(t *testing.T) {
	runV2Case(t, v2Case{Name: "finalise needs candidate health authority", Scenario: "endpoint-finalise-drain-only", Args: []string{"codex", "proxy", "credential-endpoint", "legacy", "finalise", "--ticket-file", "/tmp/cq-v2-ticket.json", "--confirm-candidate-healthy", "--non-interactive", "--json"}, Exit: 6, Command: "codex proxy credential-endpoint legacy finalise", Code: "endpoint_conflict", Forbid: []string{"credential-publish"}})
}

func endpointV2Lookup(prepare func(context.Context) (v2EndpointDependencies, error)) cli.Lookup {
	return func(path string) (cli.Handler, bool) {
		_, ok := lookupV2Endpoint(path)
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2EndpointWithPreparation(ctx, inv, s, prepare)
		}, ok
	}
}
func endpointV2Args(action, file string, alias bool) []string {
	args := []string{"codex", "proxy", "credential-endpoint", "legacy", action}
	if alias {
		args = []string{"proxy", "endpoint", "transition-legacy", action}
		if action == "inspect" {
			args = []string{"proxy", "endpoint", "inspect-legacy"}
		}
	}
	if action != "inspect" {
		flag := "--ticket-file"
		if action == "prepare" {
			flag = "--snapshot-file"
		}
		confirmation := "--confirm-stopped-and-drained"
		if action == "finalise" {
			confirmation = "--confirm-candidate-healthy"
		}
		args = append(args, flag, file, confirmation)
	}
	return append(args, "--json")
}
func runEndpointV2(t *testing.T, ctx context.Context, args []string, prepare func(context.Context) (v2EndpointDependencies, error), input io.Reader, interactive bool) (int, v2EndpointResult, candidateV2Envelope, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	exit := cli.Run(ctx, args, &cli.Session{In: input, Out: &stdout, Err: &stderr, Interactive: interactive}, endpointV2Lookup(prepare))
	var envelope candidateV2Envelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid output: %v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if err := validateV2Envelope(stdout.Bytes(), v2Case{Command: envelope.Command, Exit: exit, Code: func() string {
		if len(envelope.Errors) > 0 {
			return envelope.Errors[0].Code
		}
		return ""
	}()}); err != nil {
		t.Fatal(err)
	}
	var data struct {
		Endpoint v2EndpointResult `json:"endpoint"`
	}
	if string(envelope.Data) != "null" {
		if err := json.Unmarshal(envelope.Data, &data); err != nil {
			t.Fatal(err)
		}
	}
	return exit, data.Endpoint, envelope, stderr.String()
}
func endpointV2State(t *testing.T, phase string) (string, string, codexprov.LegacyCredentialEndpointTransitionTicket) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix endpoint fixture")
	}
	_, path := createCLIRefusedLegacyEndpoint(t)
	ctx := context.Background()
	snapshot, err := codexprov.InspectLegacyCredentialEndpoint(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(filepath.Dir(path), "proof.json")
	if phase == "snapshot" {
		endpointV2Write(t, file, snapshot)
		return path, file, codexprov.LegacyCredentialEndpointTransitionTicket{}
	}
	transition, err := codexprov.PrepareLegacyCredentialEndpointTransition(ctx, path, snapshot, codexprov.DrainAuthorityFunc(func(context.Context, string) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	if phase == "activated" {
		if err := transition.Activate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if phase == "rolled_back" {
		if err := transition.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	}
	ticket := transition.Ticket()
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	endpointV2Write(t, file, ticket)
	return path, file, ticket
}
func endpointV2Write(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

type endpointV2Entry struct {
	Identity fsutil.SecureFileIdentity
	Mode     os.FileMode
	Size     int64
	Modified time.Time
	Bytes    string
}

func endpointV2Inventory(t *testing.T, path string) map[string]endpointV2Entry {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]endpointV2Entry{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		id, ok := (fsutil.OSFileSystem{}).FileIdentity(info)
		if !ok {
			t.Fatal("no native identity")
		}
		record := endpointV2Entry{Identity: id, Mode: info.Mode(), Size: info.Size(), Modified: info.ModTime()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(filepath.Join(filepath.Dir(path), entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			record.Bytes = string(data)
		}
		result[entry.Name()] = record
	}
	return result
}
func TestCLIV2EndpointStagesAndAliases(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(fmt.Sprint(alias), func(t *testing.T) {
			path, snapshotFile, _ := endpointV2State(t, "snapshot")
			deps := v2EndpointDependenciesForPath(path)
			prepare := func(context.Context) (v2EndpointDependencies, error) { return deps, nil }
			exit, inspected, _, _ := runEndpointV2(t, context.Background(), endpointV2Args("inspect", "", alias), prepare, noAccessV2Input{}, false)
			if exit != 0 || inspected.Kind != "snapshot" || inspected.Snapshot == nil || inspected.Ticket != nil || inspected.TicketID != "none" {
				t.Fatal("invalid snapshot result")
			}
			exit, prepared, _, _ := runEndpointV2(t, context.Background(), endpointV2Args("prepare", snapshotFile, alias), prepare, noAccessV2Input{}, false)
			if exit != 0 || prepared.State != "quarantined" || prepared.Snapshot != nil || prepared.Ticket == nil || prepared.TicketID != prepared.Ticket.ID {
				t.Fatal("invalid prepared result")
			}
			ticketFile := filepath.Join(filepath.Dir(path), "ticket.json")
			endpointV2Write(t, ticketFile, prepared.Ticket)
			for _, stage := range []string{"quarantined", "activated", "rolled_back"} {
				if stage != "quarantined" {
					action := "activate"
					if stage == "rolled_back" {
						action = "rollback"
					}
					exit, result, _, _ := runEndpointV2(t, context.Background(), endpointV2Args(action, ticketFile, alias), prepare, noAccessV2Input{}, false)
					if exit != 0 || result.State != stage {
						t.Fatalf("%s exit=%d state=%s", action, exit, result.State)
					}
				}
				before := endpointV2Inventory(t, path)
				for repeat := 0; repeat < 2; repeat++ {
					exit, result, _, _ := runEndpointV2(t, context.Background(), endpointV2Args("resume", ticketFile, alias), prepare, noAccessV2Input{}, false)
					if exit != 0 || result.State != stage || !reflect.DeepEqual(result.Ticket, prepared.Ticket) {
						t.Fatalf("resume %s exit=%d state=%s", stage, exit, result.State)
					}
				}
				if after := endpointV2Inventory(t, path); !reflect.DeepEqual(before, after) {
					t.Fatalf("resume changed %s bytes or identities", stage)
				}
			}
			restored, err := codexprov.InspectLegacyCredentialEndpointTransition(context.Background(), path)
			if err != nil || restored.State != "rolled_back" {
				t.Fatal("rollback unavailable")
			}
		})
	}
}

func TestCLIV2EndpointInspectImmutable(t *testing.T) {
	runV2Case(t, v2Case{Name: "retained journal", Scenario: "endpoint-inspect", Args: endpointV2Args("inspect", "", false), Exit: 0, Command: "codex proxy credential-endpoint legacy inspect", WantJSON: `{"endpoint":{"kind":"transition","snapshot":null,"state":"quarantined"}}`, Forbid: []string{"filesystem-write", "credential-publish"}})
	for _, phase := range []string{"snapshot", "quarantined", "activated", "rolled_back"} {
		t.Run(phase, func(t *testing.T) {
			path, _, _ := endpointV2State(t, phase)
			deps := v2EndpointDependenciesForPath(path)
			deps.Transition = func(context.Context, string, legacyEndpointTransitionOptions) (codexprov.LegacyCredentialEndpointTransitionStatus, error) {
				panic("inspection reached write dependency")
			}
			before := endpointV2Inventory(t, path)
			for i := 0; i < 2; i++ {
				exit, _, _, _ := runEndpointV2(t, context.Background(), endpointV2Args("inspect", "", false), func(context.Context) (v2EndpointDependencies, error) { return deps, nil }, noAccessV2Input{}, false)
				if exit != 0 {
					t.Fatalf("inspect exit=%d", exit)
				}
			}
			if after := endpointV2Inventory(t, path); !reflect.DeepEqual(before, after) {
				t.Fatal("inspection mutated retained bytes or identities")
			}
		})
	}
}

func TestCLIV2EndpointInvalidProof(t *testing.T) {
	for _, phase := range []string{"snapshot", "quarantined"} {
		for _, kind := range []string{"empty", "oversize", "duplicate", "unknown", "missing", "null", "trailing", "utf8", "wrong-path", "wrong-inode", "symlink", "writable", "directory", "missing-file"} {
			t.Run(phase+"/"+kind, func(t *testing.T) {
				path, file, _ := endpointV2State(t, phase)
				raw, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				var proof map[string]any
				if err := json.Unmarshal(raw, &proof); err != nil {
					t.Fatal(err)
				}
				exitWant, code := 2, "endpoint_invalid_proof"
				switch kind {
				case "empty":
					raw = nil
				case "oversize":
					raw = bytes.Repeat([]byte(" "), legacyEndpointProofMaxBytes+1)
				case "duplicate":
					raw = append([]byte(`{"version":1,`), raw[1:]...)
				case "unknown":
					proof["secret-fixture-must-not-leak"] = "secret-fixture-must-not-leak"
					raw, _ = json.Marshal(proof)
				case "missing":
					delete(proof, "version")
					raw, _ = json.Marshal(proof)
				case "null":
					proof["directory"].(map[string]any)["device"] = nil
					raw, _ = json.Marshal(proof)
				case "trailing":
					raw = append(raw, []byte(` {}`)...)
				case "utf8":
					raw = append(raw, 255)
				case "wrong-path":
					proof["path"] = "/different/credential.sock"
					raw, _ = json.Marshal(proof)
					exitWant, code = 6, "endpoint_conflict"
				case "wrong-inode":
					proof["socket"].(map[string]any)["inode"] = float64(1)
					raw, _ = json.Marshal(proof)
					exitWant, code = 6, "endpoint_conflict"
				case "symlink":
					link := file + "-link"
					if err := os.Symlink(file, link); err != nil {
						t.Fatal(err)
					}
					file = link
					exitWant, code = 6, "endpoint_conflict"
				case "writable":
					if err := os.Chmod(file, 0o666); err != nil {
						t.Fatal(err)
					}
					exitWant, code = 6, "endpoint_conflict"
				case "directory":
					file = filepath.Dir(file)
					exitWant, code = 6, "endpoint_conflict"
				case "missing-file":
					file += "-missing"
					exitWant, code = 3, "endpoint_not_found"
				}
				if kind != "symlink" && kind != "directory" && kind != "missing-file" {
					if err := os.WriteFile(file, raw, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				before := endpointV2Inventory(t, path)
				deps := v2EndpointDependenciesForPath(path)
				action := "resume"
				if phase == "snapshot" {
					action = "prepare"
				}
				exit, _, out, stderr := runEndpointV2(t, context.Background(), endpointV2Args(action, file, false), func(context.Context) (v2EndpointDependencies, error) { return deps, nil }, noAccessV2Input{}, false)
				if exit != exitWant || out.Errors[0].Code != code {
					t.Fatalf("exit=%d errors=%v want=%d %s", exit, out.Errors, exitWant, code)
				}
				if strings.Contains(fmt.Sprint(out.Errors)+stderr, "secret-fixture") {
					t.Fatal("proof contents leaked")
				}
				if after := endpointV2Inventory(t, path); !reflect.DeepEqual(before, after) {
					t.Fatal("invalid proof mutated endpoint")
				}
			})
		}
	}
}

func TestCLIV2EndpointConfirmation(t *testing.T) {
	for _, action := range []string{"prepare", "resume", "activate", "finalise", "rollback"} {
		for _, mode := range []string{"missing", "false", "wrong-authority", "nonterminal", "noninteractive-only"} {
			t.Run(action+"/"+mode, func(t *testing.T) {
				args := endpointV2Args(action, "/tmp/proof.json", false)
				args = args[:len(args)-1]
				switch mode {
				case "missing":
					args = args[:len(args)-1]
					args = append(args, "--json")
				case "false":
					args[len(args)-1] += "=false"
					args = append(args, "--json")
				case "wrong-authority":
					if action == "finalise" {
						args[len(args)-1] = "--confirm-stopped-and-drained"
					} else {
						args[len(args)-1] = "--confirm-candidate-healthy"
					}
					args = append(args, "--json")
				case "noninteractive-only":
					args = args[:len(args)-1]
					args = append(args, "--non-interactive", "--json")
				}
				var stdout, stderr bytes.Buffer
				exit := cli.Run(context.Background(), args, &cli.Session{In: noAccessV2Input{}, Out: &stdout, Err: &stderr}, endpointV2Lookup(func(context.Context) (v2EndpointDependencies, error) {
					panic("unconfirmed operation prepared dependencies")
				}))
				want := 6
				if mode == "wrong-authority" {
					want = 2
				}
				if exit != want {
					t.Fatalf("exit=%d want=%d", exit, want)
				}
			})
		}
	}
	for _, answer := range []string{"stopped-and-drained\n", "stopped-and-drained", "yes\n", "candidate-healthy\n"} {
		t.Run(answer, func(t *testing.T) {
			path, file, _ := endpointV2State(t, "snapshot")
			deps := v2EndpointDependenciesForPath(path)
			args := endpointV2Args("prepare", file, false)
			args = args[:len(args)-1]
			var stdout, stderr bytes.Buffer
			exit := cli.Run(context.Background(), args, &cli.Session{In: strings.NewReader(answer), Out: &stdout, Err: &stderr, Interactive: true}, endpointV2Lookup(func(context.Context) (v2EndpointDependencies, error) { return deps, nil }))
			want := 6
			if answer == "stopped-and-drained\n" {
				want = 0
			}
			if exit != want {
				t.Fatalf("exit=%d want=%d", exit, want)
			}
			if !strings.Contains(stderr.String(), "Type stopped-and-drained") {
				t.Fatal("missing typed confirmation")
			}
		})
	}
}

func TestCLIV2EndpointBudget(t *testing.T) {
	for _, phase := range []string{"preparation", "inspection", "transition"} {
		for _, cancelled := range []bool{false, true} {
			t.Run(phase+fmt.Sprint(cancelled), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if !cancelled {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, 20*time.Millisecond)
					defer stop()
				}
				wait := func(work context.Context) error {
					if cancelled {
						cancel()
					}
					<-work.Done()
					return errors.New("secret-fixture-preparation-error")
				}
				deps := v2EndpointDependencies{Path: "/unused", Inspect: func(ctx context.Context, _ string) (legacyEndpointInspection, error) {
					if phase != "inspection" {
						panic("unexpected inspect")
					}
					return legacyEndpointInspection{}, wait(ctx)
				}, Transition: func(ctx context.Context, _ string, _ legacyEndpointTransitionOptions) (codexprov.LegacyCredentialEndpointTransitionStatus, error) {
					if phase != "transition" {
						panic("unexpected transition")
					}
					return codexprov.LegacyCredentialEndpointTransitionStatus{}, wait(ctx)
				}}
				prepare := func(ctx context.Context) (v2EndpointDependencies, error) {
					if _, ok := ctx.Deadline(); !ok {
						t.Error("preparation has no budget")
					}
					if phase == "preparation" {
						return v2EndpointDependencies{}, wait(ctx)
					}
					return deps, nil
				}
				action := "inspect"
				if phase == "transition" {
					action = "activate"
				}
				exit, _, out, _ := runEndpointV2(t, ctx, endpointV2Args(action, "/tmp/proof.json", false), prepare, noAccessV2Input{}, false)
				want, code := 7, "endpoint_timeout"
				if cancelled {
					want, code = 130, "interrupted"
				}
				if exit != want || out.Errors[0].Code != code {
					t.Fatalf("exit=%d errors=%v", exit, out.Errors)
				}
			})
		}
	}
}

func TestCLIV2EndpointHelpAndRetiredCommit(t *testing.T) {
	panicLookup := func(string) (cli.Handler, bool) { panic("presentation read state") }
	for _, action := range []string{"inspect", "prepare", "resume", "activate", "finalise", "rollback"} {
		for _, alias := range []bool{false, true} {
			args := endpointV2Args(action, "/tmp/proof.json", alias)
			args = append(args, "--help")
			var stdout, stderr bytes.Buffer
			exit := cli.Run(context.Background(), args, &cli.Session{In: noAccessV2Input{}, Out: &stdout, Err: &stderr}, panicLookup)
			want, err := os.ReadFile("../../specs/cli-v2/help/codex-proxy-credential-endpoint-legacy-" + action + ".txt")
			if err != nil {
				t.Fatal(err)
			}
			if exit != 0 || stdout.String() != string(want) {
				t.Fatalf("help %s alias=%t exit=%d differs", action, alias, exit)
			}
		}
	}
	var stdout, stderr bytes.Buffer
	exit := cli.Run(context.Background(), []string{"proxy", "endpoint", "transition-legacy", "commit", "--json"}, &cli.Session{In: noAccessV2Input{}, Out: &stdout, Err: &stderr}, panicLookup)
	if exit != 4 || !strings.Contains(stdout.String(), "endpoint_commit_retired") {
		t.Fatalf("retired commit exit=%d", exit)
	}
}

func TestCLIV2EndpointRetiredCommitSyntax(t *testing.T) {
	for _, tail := range [][]string{nil, {"--help"}, {"--ticket-file", "/tmp/ticket.json", "--confirm-stopped-and-drained", "--non-interactive"}, {"--unknown"}, {"extra"}, {"--ticket-file", "/tmp/a", "--ticket-file", "/tmp/b"}} {
		args := append([]string{"proxy", "endpoint", "transition-legacy", "commit", "--json"}, tail...)
		var stdout, stderr bytes.Buffer
		exit := cli.Run(context.Background(), args, &cli.Session{In: noAccessV2Input{}, Out: &stdout, Err: &stderr}, func(string) (cli.Handler, bool) { panic("retired command reached lookup") })
		want := 4
		if len(tail) > 0 && (tail[0] == "--unknown" || tail[0] == "extra" || (len(tail) == 4 && tail[2] == "--ticket-file")) {
			want = 2
		}
		if exit != want {
			t.Fatalf("args=%v exit=%d want=%d", tail, exit, want)
		}
		if want == 4 && (!strings.Contains(stdout.String(), "endpoint_commit_retired") || !strings.Contains(stdout.String(), "commit is unavailable; use activate, verify the exact live owner, then finalise.")) {
			t.Fatal("retirement message differs")
		}
	}
}

func TestCLIV2EndpointProofPermissionsAndHuman(t *testing.T) {
	for _, mode := range []os.FileMode{0o400, 0o600, 0o644} {
		t.Run(mode.String(), func(t *testing.T) {
			path, file, _ := endpointV2State(t, "snapshot")
			if err := os.Chmod(file, mode); err != nil {
				t.Fatal(err)
			}
			deps := v2EndpointDependenciesForPath(path)
			exit, result, _, _ := runEndpointV2(t, context.Background(), endpointV2Args("prepare", file, false), func(context.Context) (v2EndpointDependencies, error) { return deps, nil }, noAccessV2Input{}, false)
			if exit != 0 || result.State != "quarantined" {
				t.Fatalf("allowed proof mode %v exit=%d", mode, exit)
			}
			args := endpointV2Args("inspect", "", false)
			args = args[:len(args)-1]
			deps.Transition = func(context.Context, string, legacyEndpointTransitionOptions) (codexprov.LegacyCredentialEndpointTransitionStatus, error) {
				panic("human inspect wrote")
			}
			var stdout, stderr bytes.Buffer
			exit = cli.Run(context.Background(), args, &cli.Session{In: noAccessV2Input{}, Out: &stdout, Err: &stderr}, endpointV2Lookup(func(context.Context) (v2EndpointDependencies, error) { return deps, nil }))
			want := fmt.Sprintf("Credential endpoint: %s\nMigration state: quarantined\nTicket: %s\n", path, result.TicketID)
			if exit != 0 || stdout.String() != want || stderr.Len() != 0 {
				t.Fatal("human template differs")
			}
		})
	}
}

func TestCLIV2EndpointProductionBudgetAndCleanup(t *testing.T) {
	old := prepareV2EndpointDependencies
	defer func() { prepareV2EndpointDependencies = old }()
	calls := 0
	prepareV2EndpointDependencies = func(ctx context.Context) (v2EndpointDependencies, error) {
		calls++
		<-ctx.Done()
		return v2EndpointDependencies{}, errors.New("private preparation failure")
	}
	var stdout, stderr bytes.Buffer
	args := append(endpointV2Args("inspect", "", false), "--timeout", "1s")
	started := time.Now()
	exit := cli.Run(context.Background(), args, &cli.Session{In: noAccessV2Input{}, Out: &stdout, Err: &stderr}, lookupV2Endpoint)
	if exit != 7 || calls != 1 || time.Since(started) > 2*time.Second || !strings.Contains(stdout.String(), "endpoint_timeout") {
		t.Fatalf("production budget exit=%d calls=%d", exit, calls)
	}
	path, file, _ := endpointV2State(t, "snapshot")
	deps := v2EndpointDependenciesForPath(path)
	transition := deps.Transition
	deps.Transition = func(ctx context.Context, path string, opts legacyEndpointTransitionOptions) (codexprov.LegacyCredentialEndpointTransitionStatus, error) {
		status, err := transition(ctx, path, opts)
		if err != nil {
			return status, err
		}
		<-ctx.Done()
		return status, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	exit, _, out, _ := runEndpointV2(t, ctx, endpointV2Args("prepare", file, false), func(context.Context) (v2EndpointDependencies, error) { return deps, nil }, noAccessV2Input{}, false)
	if exit != 7 || out.Errors[0].Code != "endpoint_timeout" {
		t.Fatalf("cleanup budget exit=%d", exit)
	}
	status, err := codexprov.InspectLegacyCredentialEndpointTransition(context.Background(), path)
	if err != nil || status.State != "quarantined" {
		t.Fatal("timeout lost inspectable prepared journal")
	}
}

type endpointDelayedInput struct {
	wait   func()
	reader io.Reader
	once   bool
}

func (r *endpointDelayedInput) Read(p []byte) (int, error) {
	if !r.once {
		r.once = true
		r.wait()
	}
	return r.reader.Read(p)
}
func TestCLIV2EndpointConfirmationBudgetAndInterrupt(t *testing.T) {
	path, file, _ := endpointV2State(t, "snapshot")
	deps := v2EndpointDependenciesForPath(path)
	args := endpointV2Args("prepare", file, false)
	args = args[:len(args)-1]
	args = append(args, "--timeout", "1s", "--non-interactive=false")
	var stdout, stderr bytes.Buffer
	input := &endpointDelayedInput{wait: func() { time.Sleep(1100 * time.Millisecond) }, reader: strings.NewReader("stopped-and-drained\n")}
	exit := cli.Run(context.Background(), args, &cli.Session{In: input, Out: &stdout, Err: &stderr, Interactive: true}, endpointV2Lookup(func(context.Context) (v2EndpointDependencies, error) { return deps, nil }))
	if exit != 0 {
		t.Fatalf("confirmation consumed operation budget: exit=%d stderr=%s", exit, stderr.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	input = &endpointDelayedInput{wait: func() { cancel(); <-release }, reader: strings.NewReader("stopped-and-drained\n")}
	defer close(release)
	stdout.Reset()
	stderr.Reset()
	exit = cli.Run(ctx, args, &cli.Session{In: input, Out: &stdout, Err: &stderr, Interactive: true}, endpointV2Lookup(func(context.Context) (v2EndpointDependencies, error) { panic("cancelled consent prepared operation") }))
	if exit != 130 {
		t.Fatalf("consent interruption exit=%d", exit)
	}
}

// This fixture authority is local to the temporary live owner; it never reads
// credentials or substitutes an adapter outcome.
type endpointV2FinaliseAuthority struct{}

func (endpointV2FinaliseAuthority) AcquireLegacyMaintenanceFinalise(context.Context, codexprov.LegacyMaintenanceFinaliseVerification) (codexprov.LegacyMaintenanceFinaliseLease, error) {
	return endpointV2FinaliseAuthority{}, nil
}
func (endpointV2FinaliseAuthority) Release() {}

func TestCLIV2EndpointMissingTransition(t *testing.T) {
	for _, action := range []string{"resume", "activate", "rollback"} {
		for _, state := range []string{"absent", "wrong-directory", "wrong-lock", "missing-lock", "malformed-journal", "malformed-rollback", "unsafe-record", "mismatched-record"} {
			t.Run(action+"/"+state, func(t *testing.T) {
				path, file, ticket := endpointV2State(t, "activated")
				rollbackFile := path + ".maintenance.rollback.json"
				retained, err := os.ReadFile(rollbackFile)
				if err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				owner, err := codexprov.OpenCredentialControlPreparedWithLegacyMaintenanceVerifier(ctx, path, &codexprov.CredentialCoordinator{}, nil, endpointV2FinaliseAuthority{})
				if err != nil {
					t.Fatal(err)
				}
				defer owner.Close()
				deps := v2EndpointDependenciesForPath(path)
				prepare := func(context.Context) (v2EndpointDependencies, error) { return deps, nil }
				exit, result, _, _ := runEndpointV2(t, ctx, endpointV2Args("finalise", file, false), prepare, noAccessV2Input{}, false)
				if exit != 0 || result.State != "committed" {
					t.Fatalf("finalise exit=%d state=%s", exit, result.State)
				}
				if err := owner.Close(); err != nil {
					t.Fatal(err)
				}
				for _, absent := range []string{path + ".maintenance.json", rollbackFile} {
					if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("record remains: %v", err)
					}
				}
				wantExit, wantCode := 6, "endpoint_conflict"
				switch state {
				case "absent":
					wantExit, wantCode = 3, "endpoint_not_found"
				case "wrong-directory":
					ticket.Directory.Inode++
				case "wrong-lock":
					ticket.Lock.Inode++
				case "missing-lock":
					if err := os.Remove(path + ".lock"); err != nil {
						t.Fatal(err)
					}
				case "malformed-journal", "malformed-rollback", "unsafe-record":
					name := rollbackFile
					if state == "malformed-journal" {
						name = path + ".maintenance.json"
					}
					if err := os.WriteFile(name, []byte("{}"), 0o600); err != nil {
						t.Fatal(err)
					}
					if state == "unsafe-record" {
						if err := os.Chmod(name, 0o666); err != nil {
							t.Fatal(err)
						}
					}
				case "mismatched-record":
					if err := os.WriteFile(rollbackFile, retained, 0o600); err != nil {
						t.Fatal(err)
					}
					ticket.ID = strings.Repeat("b", 32)
					ticket.QuarantineName = "." + filepath.Base(path) + ".legacy-" + ticket.ID + ".quarantine"
				}
				endpointV2Write(t, file, ticket)
				before := endpointV2Inventory(t, path)
				for repeat := 0; repeat < 2; repeat++ {
					exit, _, envelope, _ := runEndpointV2(t, ctx, endpointV2Args(action, file, false), prepare, noAccessV2Input{}, false)
					if exit != wantExit || len(envelope.Errors) != 1 || envelope.Errors[0].Code != wantCode {
						t.Errorf("exit=%d errors=%v want %d/%s", exit, envelope.Errors, wantExit, wantCode)
					}
				}
				if state == "absent" {
					transition, err := codexprov.ResumeLegacyCredentialEndpointTransition(ctx, path, ticket, codexprov.DrainAuthorityFunc(func(context.Context, string) error { return nil }))
					if transition != nil {
						transition.Close()
						t.Error("legacy resume unexpectedly succeeded")
					}
					if !errors.Is(err, codexprov.ErrCredentialEndpointMaintenancePending) || !errors.Is(err, codexprov.ErrCredentialEndpointMaintenanceTicketMismatch) {
						t.Errorf("legacy absence changed: %v", err)
					}
				}
				if after := endpointV2Inventory(t, path); !reflect.DeepEqual(before, after) {
					t.Fatal("absent/conflicting transition mutated bytes or identities")
				}
			})
		}
	}
}
