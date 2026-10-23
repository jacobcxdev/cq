package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/jacobcxdev/cq/internal/cli"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

var terminalEndpointFixtures = []terminalFixtureRegistration{
	{"cq codex proxy credential-endpoint legacy inspect", "Success", "stdout", "text/cq-codex-proxy-credential-endpoint-legacy-inspect/02-success.txt"},
	{"cq codex proxy credential-endpoint legacy inspect", "Failure", "stderr", "text/cq-codex-proxy-credential-endpoint-legacy-inspect/03-failure.txt"},
	{"cq codex proxy credential-endpoint legacy prepare", "Success", "stdout", "text/cq-codex-proxy-credential-endpoint-legacy-prepare/02-success.txt"},
	{"cq codex proxy credential-endpoint legacy prepare", "Failure", "stderr", "text/cq-codex-proxy-credential-endpoint-legacy-prepare/03-failure.txt"},
	{"cq codex proxy credential-endpoint legacy activate", "Success", "stdout", "text/cq-codex-proxy-credential-endpoint-legacy-activate/02-success.txt"},
	{"cq codex proxy credential-endpoint legacy activate", "Failure", "stderr", "text/cq-codex-proxy-credential-endpoint-legacy-activate/03-failure.txt"},
	{"cq codex proxy credential-endpoint legacy activate", "Timeout · inspect before continuing", "stderr", "text/cq-codex-proxy-credential-endpoint-legacy-activate/04-timeout-inspect-before-continuing.txt"},
	{"cq codex proxy credential-endpoint legacy resume", "Success", "stdout", "text/cq-codex-proxy-credential-endpoint-legacy-resume/02-success.txt"},
	{"cq codex proxy credential-endpoint legacy resume", "Failure", "stderr", "text/cq-codex-proxy-credential-endpoint-legacy-resume/03-failure.txt"},
	{"cq codex proxy credential-endpoint legacy finalise", "Success", "stdout", "text/cq-codex-proxy-credential-endpoint-legacy-finalise/02-success.txt"},
	{"cq codex proxy credential-endpoint legacy finalise", "Failure", "stderr", "text/cq-codex-proxy-credential-endpoint-legacy-finalise/03-failure.txt"},
	{"cq codex proxy credential-endpoint legacy finalise", "Confirmation · pending", "stderr", "text/cq-codex-proxy-credential-endpoint-legacy-finalise/04-confirmation-pending.txt"},
	{"cq codex proxy credential-endpoint legacy rollback", "Success", "stdout", "text/cq-codex-proxy-credential-endpoint-legacy-rollback/02-success.txt"},
	{"cq codex proxy credential-endpoint legacy rollback", "Failure", "stderr", "text/cq-codex-proxy-credential-endpoint-legacy-rollback/03-failure.txt"},
	{"cq codex proxy credential-endpoint legacy rollback", "Timeout · inspect before continuing", "stderr", "text/cq-codex-proxy-credential-endpoint-legacy-rollback/04-timeout-inspect-before-continuing.txt"},
}

func TestTerminalEndpointRegistration(t *testing.T) {
	data, err := os.ReadFile("../../specs/cli-v2/presentation/examples/index.json")
	if err != nil {
		t.Fatal(err)
	}
	var index struct{ Fixtures []terminalFixtureRegistration }
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	registered := map[string]terminalFixtureRegistration{}
	for _, f := range terminalEndpointFixtures {
		registered[f.File] = f
	}
	for _, f := range index.Fixtures {
		if f.Scenario == "Help" || !strings.Contains(f.Command, "credential-endpoint legacy ") {
			continue
		}
		r, ok := registered[f.File]
		if !ok || r != f {
			t.Fatalf("unregistered endpoint fixture: %+v", f)
		}
		delete(registered, f.File)
	}
	if len(registered) != 0 {
		t.Fatalf("extra endpoint registrations: %+v", registered)
	}
	manifestBytes, err := os.ReadFile("../../docs/superpowers/plans/terminal-presentation-coverage.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Fixtures []struct{ Task, Command, Scenario, Stream, File string }
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, f := range terminalEndpointFixtures {
		covered[f.File] = true
	}
	for _, f := range manifest.Fixtures {
		if f.Task != "T09" {
			continue
		}
		if !covered[f.File] {
			t.Fatalf("T09 fixture absent: %s", f.File)
		}
		delete(covered, f.File)
	}
	if len(covered) != 0 {
		t.Fatalf("T09 fixtures missing from manifest: %+v", covered)
	}
}

func TestTerminalEndpointSuccessFixturesAndRoles(t *testing.T) {
	const path = "/Users/alex/Library/Application Support/cq/codex/credential.sock"
	const ticketID = "0123456789abcdef0123456789abcdef"
	states := map[string]codexprov.CredentialEndpointMaintenanceState{
		"prepare":  codexprov.CredentialEndpointMaintenancePrepared,
		"activate": codexprov.CredentialEndpointMaintenanceActivated,
		"resume":   codexprov.CredentialEndpointMaintenanceActivated,
		"finalise": codexprov.CredentialEndpointMaintenanceCommitted,
		"rollback": codexprov.CredentialEndpointMaintenanceRolledBack,
	}
	for _, f := range terminalEndpointFixtures {
		if f.Scenario != "Success" {
			continue
		}
		action := strings.TrimPrefix(f.Command, "cq codex proxy credential-endpoint legacy ")
		t.Run(action, func(t *testing.T) {
			calls := 0
			deps := v2EndpointDependencies{Path: path,
				Inspect: func(context.Context, string) (legacyEndpointInspection, error) {
					calls++
					return legacyEndpointInspection{Snapshot: &codexprov.LegacyCredentialEndpointSnapshot{Path: path, State: codexprov.LegacyCredentialEndpointRefused}}, nil
				},
				Transition: func(_ context.Context, gotPath string, opts legacyEndpointTransitionOptions) (codexprov.LegacyCredentialEndpointTransitionStatus, error) {
					calls++
					if gotPath != path || opts.action != action {
						t.Fatalf("transition args: %q %+v", gotPath, opts)
					}
					return codexprov.LegacyCredentialEndpointTransitionStatus{State: states[action], Ticket: codexprov.LegacyCredentialEndpointTransitionTicket{Path: path, ID: ticketID}}, nil
				},
			}
			options := map[string][]string{"timeout": {"30s"}, "non-interactive": {"true"}, "confirm-stopped-and-drained": {"true"}, "confirm-candidate-healthy": {"true"}}
			out := handleV2EndpointWithPreparation(context.Background(), cli.Invocation{Path: strings.TrimPrefix(f.Command, "cq "), Options: options}, nil, func(context.Context) (v2EndpointDependencies, error) { return deps, nil })
			if out.ExitCode != 0 || calls != 1 || out.Terminal == nil {
				t.Fatalf("outcome exit=%d calls=%d terminal=%v", out.ExitCode, calls, out.Terminal != nil)
			}
			for _, width := range []int{20, 40, 80, 120} {
				o := cli.TerminalOptions{Width: width, Colour: true}
				got := out.Terminal(o)
				if width == 120 {
					assertTerminalFixture(t, f.File, got)
				}
				for _, line := range strings.Split(got, "\n") {
					if ansi.StringWidth(line) > min(width, 100) {
						t.Fatalf("width %d: %q", width, line)
					}
				}
				if width >= 100 && (!strings.Contains(got, "\x1b[36m"+path) || !strings.Contains(got, "\x1b[36m"+func() string {
					if action == "inspect" {
						return "none"
					}
					return ticketID
				}())) {
					t.Fatalf("missing identity role: %q", got)
				}
				if plain := out.Terminal(cli.TerminalOptions{Width: width}); plain != ansi.Strip(got) {
					t.Fatal("NO_COLOR changed text")
				}
			}
		})
	}
}

func TestTerminalEndpointFailureDoesNotClaimTransition(t *testing.T) {
	for _, err := range []error{codexprov.ErrCredentialEndpointMaintenanceSnapshotChanged, context.Canceled, context.DeadlineExceeded, errors.New("failed persistence: secret-token")} {
		out := handleV2EndpointWithPreparation(context.Background(), cli.Invocation{Path: "codex proxy credential-endpoint legacy rollback", Options: map[string][]string{"timeout": {"30s"}, "non-interactive": {"true"}, "confirm-stopped-and-drained": {"true"}}}, nil, func(context.Context) (v2EndpointDependencies, error) {
			return v2EndpointDependencies{Path: "/tmp/credential.sock", Transition: func(context.Context, string, legacyEndpointTransitionOptions) (codexprov.LegacyCredentialEndpointTransitionStatus, error) {
				return codexprov.LegacyCredentialEndpointTransitionStatus{}, err
			}}, nil
		})
		if out.ExitCode == 0 || out.Terminal != nil || strings.Contains(out.Human, "rolled_back") || strings.Contains(out.Human, "secret-token") {
			t.Fatalf("failure leaked success or secret: %+v", out)
		}
	}
}

func TestTerminalEndpointFailureFixtures(t *testing.T) {
	for _, f := range terminalEndpointFixtures {
		if f.Stream != "stderr" || f.Scenario == "Confirmation · pending" {
			continue
		}
		t.Run(f.File, func(t *testing.T) {
			var cause error = codexprov.ErrCredentialEndpointMaintenanceConflict
			switch f.Scenario {
			case "Timeout · inspect before continuing":
				cause = context.DeadlineExceeded
			case "Failure":
				if strings.HasSuffix(f.Command, " inspect") {
					cause = os.ErrNotExist
				}
			}
			action := strings.TrimPrefix(f.Command, "cq codex proxy credential-endpoint legacy ")
			args := []string{"codex", "proxy", "credential-endpoint", "legacy", action}
			if action != "inspect" {
				flag := "--ticket-file"
				if action == "prepare" {
					flag = "--snapshot-file"
				}
				confirm := "--confirm-stopped-and-drained"
				if action == "finalise" {
					confirm = "--confirm-candidate-healthy"
				}
				args = append(args, flag, "/tmp/proof.json", confirm, "--non-interactive")
			}
			for _, width := range []int{20, 40, 80, 120} {
				var stdout, stderr bytes.Buffer
				o := terminalFixtureOptions(width)
				exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, endpointV2Lookup(func(context.Context) (v2EndpointDependencies, error) {
					return v2EndpointDependencies{}, cause
				}))
				if exit == 0 || stdout.Len() != 0 {
					t.Fatalf("exit=%d stdout=%q", exit, stdout.String())
				}
				if width == 120 {
					assertTerminalFixture(t, f.File, stderr.String())
				}
				if !strings.Contains(stderr.String(), "\x1b[31mError") || strings.Contains(stderr.String(), "secret-token") {
					t.Fatalf("failure roles: %q", stderr.String())
				}
				for _, line := range strings.Split(stderr.String(), "\n") {
					if ansi.StringWidth(line) > min(width, 100) {
						t.Fatalf("width %d: %q", width, line)
					}
				}
			}
		})
	}
}

func TestTerminalEndpointFinaliseConfirmationFixture(t *testing.T) {
	got := terminalEndpointPrompt("finalise", "/Users/alex/cq-inputs/endpoint-ticket.json", "/Users/alex/Library/Application Support/cq/codex/credential.sock", "0123456789abcdef0123456789abcdef", terminalFixtureOptions(100))
	got += "Type candidate-healthy to confirm the exact live candidate passed health verification: \n"
	assertTerminalFixture(t, "text/cq-codex-proxy-credential-endpoint-legacy-finalise/04-confirmation-pending.txt", got)
}

func TestTerminalEndpointFinalisePromptDoesNotTransitionBeforeConsent(t *testing.T) {
	path, proof, ticket := endpointV2State(t, "activated")
	before := endpointV2Inventory(t, path)
	args := endpointV2Args("finalise", proof, false)
	args = args[:len(args)-1]
	var stdout, stderr bytes.Buffer
	session := &cli.Session{In: strings.NewReader("no\n"), Out: &stdout, Err: &stderr, Interactive: true, ErrTerminal: terminalFixtureOptions(100)}
	exit := cli.Run(context.Background(), args, session, endpointV2Lookup(func(context.Context) (v2EndpointDependencies, error) {
		panic("finalise reached preparation before consent")
	}))
	if exit != 6 || stdout.Len() != 0 || !strings.Contains(stderr.String(), path) || !strings.Contains(stderr.String(), ticket.ID) || !strings.Contains(stderr.String(), "Permanently retire the rollback path") || strings.Contains(stderr.String(), "\x1b[32m") {
		t.Fatalf("unsafe confirmation: exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if after := endpointV2Inventory(t, path); !reflect.DeepEqual(before, after) {
		t.Fatal("confirmation mutated endpoint files")
	}
}

func TestTerminalEndpointPhaseRolesAndPrivateProof(t *testing.T) {
	for _, tc := range []struct {
		state string
		role  string
	}{
		{"prepared", "\x1b[33m"}, {"activating", "\x1b[33m"}, {"finalising", "\x1b[33m"},
		{"rolling_back", "\x1b[33m"}, {"activated", "\x1b[32m"}, {"committed", "\x1b[32m"}, {"rolled_back", "\x1b[32m"},
	} {
		result := v2EndpointResult{Kind: "transition", Path: "/tmp/credential.sock", State: tc.state, TicketID: "safe-ticket", Ticket: &codexprov.LegacyCredentialEndpointTransitionTicket{QuarantineName: "private-proof-marker"}}
		got := terminalEndpoint(result, terminalFixtureOptions(100))
		if !strings.Contains(got, tc.role+tc.state) || strings.Contains(got, "private-proof-marker") || strings.Contains(got, "healthy") {
			t.Fatalf("phase %q misleading or exposed proof: %q", tc.state, got)
		}
	}
	result := v2EndpointResult{Kind: "snapshot", Path: "/tmp/credential.sock", State: "legacy_refused", TicketID: "none"}
	if got := terminalEndpoint(result, terminalFixtureOptions(100)); strings.Contains(got, "\x1b[32m") || !strings.Contains(got, "Identity observed.") {
		t.Fatalf("snapshot claims completion: %q", got)
	}
}
