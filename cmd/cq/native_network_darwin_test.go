//go:build darwin

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type nativeNetworkTestExit int

func (code nativeNetworkTestExit) Error() string { return "test launchctl failure" }
func (code nativeNetworkTestExit) ExitCode() int { return int(code) }

func nativeNetworkTestAgent(t *testing.T) nativeNetworkAgent {
	t.Helper()
	home := t.TempDir()
	executable := filepath.Join(home, "cq with & spaces")
	if err := os.WriteFile(executable, []byte("test executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	return nativeNetworkAgent{home: home, uid: os.Getuid(), executable: executable}
}

func TestNativeNetworkAgentInstallUsesPrivateOwnLaunchAgent(t *testing.T) {
	agent := nativeNetworkTestAgent(t)
	var commands [][]string
	agent.run = func(_ context.Context, executable string, args ...string) ([]byte, error) {
		commands = append(commands, append([]string{executable}, args...))
		if args[0] == "print" {
			return nil, nativeNetworkTestExit(113)
		}
		return nil, nil
	}
	if err := agent.install(context.Background()); err != nil {
		t.Fatal(err)
	}
	plist, _, domain, target := agent.paths()
	want := [][]string{{"/bin/launchctl", "print", target}, {"/bin/launchctl", "bootstrap", domain, plist}}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands %v", commands)
	}
	info, err := os.Stat(plist)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", info, err)
	}
	data, err := readNativeNetworkAgent(plist)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := parseDarwinLaunchAgent(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(definition.ProgramArguments, []string{nativeNetworkExecutable, "run", "--cq", agent.executable}) || definition.Label != nativeNetworkIdentifier || !definition.KeepAlive || !definition.RunAtLoad {
		t.Fatalf("definition %+v", definition)
	}
	if strings.Contains(string(data), "& spaces") {
		t.Fatal("plist arguments were not XML escaped")
	}
}

func TestNativeNetworkAgentInstallDoesNotRestartExistingBroker(t *testing.T) {
	agent := nativeNetworkTestAgent(t)
	definition, err := agent.definition()
	if err != nil {
		t.Fatal(err)
	}
	plist, _, _, _ := agent.paths()
	if err := atomicWriteDarwinLaunchAgent(plist, definition); err != nil {
		t.Fatal(err)
	}
	calls := 0
	agent.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls++
		if args[0] != "print" {
			t.Fatalf("unexpected launchctl %v", args)
		}
		return nil, nil
	}
	if err := agent.install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls %d", calls)
	}
	agent.executable = filepath.Join(agent.home, "other-cq")
	if err := os.WriteFile(agent.executable, []byte("other"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := agent.install(context.Background()); err == nil {
		t.Fatal("changed live broker silently replaced")
	}
}

func TestNativeNetworkAgentRejectsUnsafeExistingFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "public", "unrelated"} {
		t.Run(kind, func(t *testing.T) {
			agent := nativeNetworkTestAgent(t)
			plist, _, _, _ := agent.paths()
			if err := os.MkdirAll(filepath.Dir(plist), 0o700); err != nil {
				t.Fatal(err)
			}
			definition, err := agent.definition()
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				if err := os.Symlink(agent.executable, plist); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.WriteFile(plist, definition, 0o644); err != nil {
					t.Fatal(err)
				}
			case "unrelated":
				definition = bytes.ReplaceAll(definition, []byte(nativeNetworkIdentifier), []byte("unrelated.agent"))
				if err := os.WriteFile(plist, definition, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			agent.run = func(context.Context, string, ...string) ([]byte, error) {
				t.Fatal("unsafe file reached launchctl")
				return nil, nil
			}
			if err := agent.install(context.Background()); err == nil {
				t.Fatal("unsafe file accepted")
			}
			if err := agent.remove(context.Background()); err == nil {
				t.Fatal("unsafe removal accepted")
			}
		})
	}
}

func TestNativeNetworkAgentDoesNotBootstrapAfterInspectionFailure(t *testing.T) {
	agent := nativeNetworkTestAgent(t)
	failure := errors.New("launchd unavailable")
	agent.run = func(context.Context, string, ...string) ([]byte, error) { return nil, failure }
	if err := agent.install(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("error %v", err)
	}
	plist, _, _, _ := agent.paths()
	if _, err := os.Stat(plist); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected plist: %v", err)
	}
}

func TestNativeNetworkAgentRemoveIsScopedAndIdempotent(t *testing.T) {
	agent := nativeNetworkTestAgent(t)
	definition, err := agent.definition()
	if err != nil {
		t.Fatal(err)
	}
	plist, _, _, target := agent.paths()
	if err := atomicWriteDarwinLaunchAgent(plist, definition); err != nil {
		t.Fatal(err)
	}
	var commands [][]string
	agent.run = func(_ context.Context, exe string, args ...string) ([]byte, error) {
		commands = append(commands, append([]string{exe}, args...))
		return nil, nativeNetworkTestExit(113)
	}
	for i := 0; i < 2; i++ {
		if err := agent.remove(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"/bin/launchctl", "bootout", target}
	if len(commands) != 2 || !reflect.DeepEqual(commands[0], want) || !reflect.DeepEqual(commands[1], want) {
		t.Fatalf("commands %v", commands)
	}
	if target != "gui/"+strconv.Itoa(os.Getuid())+"/"+nativeNetworkIdentifier {
		t.Fatal(target)
	}
	if _, err := os.Stat(plist); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist remained: %v", err)
	}
}

func TestNativeNetworkCommandOutputIsBounded(t *testing.T) {
	var output nativeNetworkOutput
	data := bytes.Repeat([]byte("x"), 32<<10)
	if count, err := output.Write(data); count != len(data) || err != nil {
		t.Fatalf("write %d,%v", count, err)
	}
	if output.Len() != 16<<10 || !output.truncated {
		t.Fatalf("output %d truncated=%t", output.Len(), output.truncated)
	}
	if _, err := output.Write(data); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 16<<10 {
		t.Fatal("buffer grew after limit")
	}
}

func TestNativeNetworkSigningRequirementHasStableIdentity(t *testing.T) {
	for _, value := range []string{"anchor apple generic", `identifier "dev.jacobcx.cq.network"`, `certificate leaf[subject.OU] = "K2648T24P4"`} {
		if !strings.Contains(nativeNetworkSigningRequirement, value) {
			t.Fatalf("requirement missing %s", value)
		}
	}
	if strings.Contains(nativeNetworkSigningRequirement, "cdhash") || strings.Contains(nativeNetworkSigningRequirement, "version") {
		t.Fatal("requirement pins a changing build")
	}
}

func TestNativeNetworkRejectsUnsupportedMacOS(t *testing.T) {
	for _, release := range []string{"", "bad", "21.6.0", "20.0.0"} {
		if err := validateNativeNetworkKernel(release); err == nil || !strings.Contains(err.Error(), "requires macOS 13") {
			t.Fatalf("kernel %q accepted: %v", release, err)
		}
	}
	for _, release := range []string{"22.0.0", "23.6.0", "26.1.0"} {
		if err := validateNativeNetworkKernel(release); err != nil {
			t.Fatalf("kernel %q rejected: %v", release, err)
		}
	}
}
