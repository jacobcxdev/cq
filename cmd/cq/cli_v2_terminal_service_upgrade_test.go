package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/proxy"
)

var terminalServiceUpgradeFixtures = []terminalFixtureRegistration{
	{"cq service upgrade", "Success", "stdout", "text/cq-service-upgrade/02-success.txt"},
	{"cq service upgrade", "Success · native JSON receipt", "stdout", "text/cq-service-upgrade/03-success-native-json-receipt.txt"},
	{"cq service upgrade", "Failure", "stderr", "text/cq-service-upgrade/04-failure.txt"},
	{"cq service upgrade", "Deferred · active turns", "stdout", "text/cq-service-upgrade/05-deferred-active-turns.txt"},
	{"cq service upgrade", "Deferred · active turns", "stderr", "text/cq-service-upgrade/06-deferred-active-turns.txt"},
	{"cq service upgrade", "Deferred · native JSON receipt", "stdout", "text/cq-service-upgrade/07-deferred-native-json-receipt.txt"},
	{"cq service upgrade", "Deferred · native JSON receipt", "stderr", "text/cq-service-upgrade/08-deferred-native-json-receipt.txt"},
}

func TestTerminalServiceUpgradeFixtures(t *testing.T) {
	for _, f := range terminalServiceUpgradeFixtures {
		for _, width := range []int{20, 40, 80, 100, 120} {
			t.Run(fmt.Sprintf("%s/%d", f.File, width), func(t *testing.T) {
				lifecycle, original, _ := newServiceHarness(t)
				platform := &retainedServicePlatform{fakeServicePlatform: original}
				lifecycle.Platform = platform
				previous := installer.RuntimeArtifact{Path: "/Users/river/.local/state/cq/runtime-artifacts/" + strings.Repeat("a", 64) + "/cq", Version: "0.34.0", SHA256: strings.Repeat("a", 64), ProtocolVersion: 1}
				candidate := installer.RuntimeArtifact{Path: "/Users/river/.local/state/cq/runtime-artifacts/" + strings.Repeat("b", 64) + "/cq", Version: "0.34.1", SHA256: strings.Repeat("b", 64), ProtocolVersion: 1}
				candidateInput := filepath.Join(t.TempDir(), "candidate-cq")
				lifecycle.RuntimeArtifacts = serviceUpgradeArtifacts{lifecycle.Executable: previous, previous.Path: previous, candidateInput: candidate, candidate.Path: candidate}
				lifecycle.RuntimePreflight = func(context.Context, installstate.Record) error { return nil }
				lifecycle.RuntimeRestoreRefresh = func(context.Context, servicePlatformSnapshot) error { return nil }
				lifecycle.RuntimeApply = func(context.Context, installer.RuntimeArtifact) (proxy.RuntimeUpgradeReceiptV1, error) {
					receipt := proxy.RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "example-upgrade", Generation: 1, Phase: "committed", Previous: previous, Candidate: candidate, ListenerIdentity: "tcp|127.0.0.1:19280", SupervisorPID: 2142, AdmissionPauseNanos: 18000000}
					if strings.HasPrefix(f.Scenario, "Deferred") {
						receipt.Phase, receipt.ErrorCode = "deferred", "active_turns"
					} else {
						platform.live = candidate.Path
					}
					return receipt, nil
				}
				if err := lifecycle.Install(context.Background(), installstate.OwnerHomebrew); err != nil {
					t.Fatal(err)
				}
				factory := serviceLifecycleFactory
				t.Cleanup(func() { serviceLifecycleFactory = factory })
				serviceLifecycleFactory = func(string) (*serviceLifecycle, error) {
					if f.Scenario == "Failure" {
						return nil, errors.New("service platform adapter is unavailable")
					}
					return lifecycle, nil
				}
				args := []string{"service", "upgrade", "--owner", "homebrew", "--candidate-executable", candidateInput, "--service-executable", lifecycle.Executable}
				if strings.Contains(f.Scenario, "JSON") {
					args = append(args, "--json")
				}
				var stdout, stderr bytes.Buffer
				o := terminalFixtureOptions(width)
				exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, lookupCLIV2)
				wantExit := 0
				if f.Scenario == "Failure" || strings.HasPrefix(f.Scenario, "Deferred") {
					wantExit = 1
				}
				if exit != wantExit {
					t.Fatalf("exit %d want %d: stdout=%s stderr=%s", exit, wantExit, stdout.String(), stderr.String())
				}
				got := stdout.String()
				if f.Stream == "stderr" {
					got = stderr.String()
				}
				assertTerminalFixture(t, f.File, got)
				if strings.Contains(stdout.String()+stderr.String(), "\x1b[") {
					t.Fatal("native service-upgrade output gained ANSI styling")
				}
				if wantExit == 0 && stderr.Len() != 0 || f.Scenario == "Failure" && stdout.Len() != 0 {
					t.Fatalf("unexpected companion stream: stdout=%q stderr=%q", stdout.String(), stderr.String())
				}
			})
		}
	}
}
