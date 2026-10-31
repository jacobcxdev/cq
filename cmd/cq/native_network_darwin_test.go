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
	"time"
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
	if !bytes.Contains(data, []byte("<key>ProcessType</key>\n\t<string>Interactive</string>")) {
		t.Fatal("user-facing native network broker must use Interactive scheduling")
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

func TestNativeNetworkAgentMigratesOnlyLegacyScheduling(t *testing.T) {
	for _, mode := range []string{"success", "bootout failure", "bootstrap failure", "rollback failure"} {
		t.Run(mode, func(t *testing.T) {
			agent := nativeNetworkTestAgent(t)
			desired, err := agent.definition()
			if err != nil {
				t.Fatal(err)
			}
			legacy := bytes.Replace(desired, []byte("<string>Interactive</string>"), []byte("<string>Background</string>"), 1)
			plist, _, domain, target := agent.paths()
			if err := atomicWriteDarwinLaunchAgent(plist, legacy); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("migration failed")
			rollbackFailure := errors.New("rollback failed")
			var commands [][]string
			bootstraps := 0
			agent.run = func(_ context.Context, executable string, args ...string) ([]byte, error) {
				commands = append(commands, append([]string{executable}, args...))
				switch args[0] {
				case "bootout":
					if mode == "bootout failure" {
						return nil, failure
					}
				case "bootstrap":
					bootstraps++
					data, err := readNativeNetworkAgent(plist)
					if err != nil {
						t.Fatal(err)
					}
					want := desired
					if bootstraps == 2 {
						want = legacy
					}
					if !bytes.Equal(data, want) {
						t.Fatal("bootstrap did not use the expected atomic definition")
					}
					if mode == "bootstrap failure" || mode == "rollback failure" {
						if bootstraps == 1 {
							return nil, failure
						}
						if mode == "rollback failure" {
							return nil, rollbackFailure
						}
					}
				}
				return nil, nil
			}
			err = agent.install(context.Background())
			want := [][]string{{"/bin/launchctl", "print", target}, {"/bin/launchctl", "bootout", target}}
			wantData := legacy
			if mode != "bootout failure" {
				want = append(want, []string{"/bin/launchctl", "bootstrap", domain, plist})
			}
			if mode == "success" {
				wantData = desired
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, failure) {
				t.Fatalf("migration error %v", err)
			}
			if mode == "bootstrap failure" || mode == "rollback failure" {
				want = append(want, []string{"/bin/launchctl", "bootout", target}, []string{"/bin/launchctl", "bootstrap", domain, plist})
			}
			if mode == "rollback failure" && !errors.Is(err, rollbackFailure) {
				t.Fatalf("rollback error was lost: %v", err)
			}
			if !reflect.DeepEqual(commands, want) {
				t.Fatalf("commands %v, want %v", commands, want)
			}
			data, err := readNativeNetworkAgent(plist)
			if err != nil || !bytes.Equal(data, wantData) {
				t.Fatalf("unexpected final definition: %v", err)
			}
		})
	}
}

func TestNativeNetworkAgentMigrationRollbackSurvivesCancellation(t *testing.T) {
	for _, cancelAt := range []string{"bootout", "bootstrap"} {
		t.Run(cancelAt, func(t *testing.T) {
			agent := nativeNetworkTestAgent(t)
			definition, err := agent.definition()
			if err != nil {
				t.Fatal(err)
			}
			legacy := bytes.Replace(definition, []byte("<string>Interactive</string>"), []byte("<string>Background</string>"), 1)
			plist, _, _, _ := agent.paths()
			if err := atomicWriteDarwinLaunchAgent(plist, legacy); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			bootouts, bootstraps := 0, 0
			restored := false
			agent.run = func(commandCtx context.Context, _ string, args ...string) ([]byte, error) {
				switch args[0] {
				case "bootout":
					bootouts++
					if bootouts == 1 {
						if cancelAt == "bootout" {
							cancel()
						}
						return nil, nil
					}
				case "bootstrap":
					bootstraps++
					if bootstraps == 1 {
						cancel()
						return nil, commandCtx.Err()
					}
					restored = true
				default:
					return nil, nil
				}
				if commandCtx.Err() != nil {
					t.Errorf("rollback inherited cancelled context: %v", commandCtx.Err())
					return nil, commandCtx.Err()
				}
				deadline, ok := commandCtx.Deadline()
				if remaining := time.Until(deadline); !ok || remaining <= 0 || remaining > 12*time.Second {
					t.Errorf("rollback context is not bounded: %v, %t", deadline, ok)
				}
				return nil, nil
			}
			if err := agent.install(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("migration error %v", err)
			}
			if !restored || bootouts != 2 || bootstraps != 2 {
				t.Fatalf("rollback did not restart old broker: restored=%t, bootouts=%d, bootstraps=%d", restored, bootouts, bootstraps)
			}
			data, err := readNativeNetworkAgent(plist)
			if err != nil || !bytes.Equal(data, legacy) {
				t.Fatalf("old definition was not restored: %v", err)
			}
		})
	}
}

func TestNativeNetworkAgentMigrationWriteFailureRestartsUnchangedDefinition(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	agent := nativeNetworkTestAgent(t)
	definition, err := agent.definition()
	if err != nil {
		t.Fatal(err)
	}
	legacy := bytes.Replace(definition, []byte("<string>Interactive</string>"), []byte("<string>Background</string>"), 1)
	plist, _, _, _ := agent.paths()
	if err := atomicWriteDarwinLaunchAgent(plist, legacy); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(plist)
	t.Cleanup(func() {
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Error(err)
		}
	})
	bootstraps := 0
	agent.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "bootout":
			return nil, os.Chmod(directory, 0o500)
		case "bootstrap":
			bootstraps++
			data, err := readNativeNetworkAgent(plist)
			if err != nil || !bytes.Equal(data, legacy) {
				t.Fatalf("bootstrap must use unchanged old definition: %v", err)
			}
		}
		return nil, nil
	}
	if err := agent.install(context.Background()); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("write error %v", err)
	}
	if bootstraps != 1 {
		t.Fatalf("old readable definition was not bootstrapped: %d", bootstraps)
	}
}

func TestNativeNetworkAgentRejectsOtherLoadedDefinitionChanges(t *testing.T) {
	for _, change := range []string{"argument", "extra key", "other scheduling"} {
		t.Run(change, func(t *testing.T) {
			agent := nativeNetworkTestAgent(t)
			definition, err := agent.definition()
			if err != nil {
				t.Fatal(err)
			}
			definition = bytes.Replace(definition, []byte("<string>Interactive</string>"), []byte("<string>Background</string>"), 1)
			switch change {
			case "argument":
				definition = bytes.Replace(definition, []byte("<string>run</string>"), []byte("<string>status</string>"), 1)
			case "extra key":
				definition = bytes.Replace(definition, []byte("</dict>"), []byte("<key>Disabled</key><true/>\n</dict>"), 1)
			case "other scheduling":
				definition = bytes.Replace(definition, []byte("<string>Background</string>"), []byte("<string>Standard</string>"), 1)
			}
			plist, _, _, target := agent.paths()
			if err := atomicWriteDarwinLaunchAgent(plist, definition); err != nil {
				t.Fatal(err)
			}
			agent.run = func(_ context.Context, executable string, args ...string) ([]byte, error) {
				if executable != "/bin/launchctl" || !reflect.DeepEqual(args, []string{"print", target}) {
					t.Fatalf("foreign definition reached mutation: %s %v", executable, args)
				}
				return nil, nil
			}
			if err := agent.install(context.Background()); err == nil {
				t.Fatal("changed definition was accepted")
			}
			data, err := readNativeNetworkAgent(plist)
			if err != nil || !bytes.Equal(data, definition) {
				t.Fatalf("foreign definition changed: %v", err)
			}
		})
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
