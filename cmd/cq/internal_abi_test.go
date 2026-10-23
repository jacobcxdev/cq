package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type internalABICase struct {
	Snapshot   string   `json:"snapshot"`
	Name       string   `json:"name"`
	Executable string   `json:"executable"`
	Argv       []string `json:"argv"`
	Accepted   bool     `json:"accepted"`
	Platform   string   `json:"platform"`
	Scenario   string   `json:"scenario"`
	Stdout     string   `json:"stdout"`
	Stderr     string   `json:"stderr"`
	Exit       int      `json:"exit"`
	Effects    []string `json:"effects"`
}

func internalABICorpus(t *testing.T) []internalABICase {
	t.Helper()
	data, err := os.ReadFile("testdata/cli-v2/internal-abi.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []internalABICase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("empty machine ABI corpus")
	}
	return cases
}

func TestCLIV2InternalBoundary(t *testing.T) {
	if isMachineABI([]string{"proxy", "start", "--help"}) {
		t.Fatal("public help entered machine dispatch")
	}
	if isMachineABI([]string{"proxy", "serve", "--runtime-role=worker"}) {
		t.Fatal("canonical public path accepted internal flags")
	}
}

// A permissive prefix match would dispatch malformed child or package arguments;
// a blanket rejection would strand the package and process launchers.
func TestCLIV2InternalCorpus(t *testing.T) {
	for _, c := range internalABICorpus(t) {
		if c.Executable != "cq" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			want := c.Accepted && internalABIPlatformMatches(c.Platform)
			if got := isMachineABI(c.Argv); got != want {
				t.Fatalf("isMachineABI(%q) = %t, want %t", c.Argv, got, want)
			}
		})
	}
}

func TestCLIV2InternalDispatchCorpus(t *testing.T) {
	previousFactory, previousCandidate := serviceLifecycleFactory, runProxyValidationCandidateFn
	t.Cleanup(func() { serviceLifecycleFactory, runProxyValidationCandidateFn = previousFactory, previousCandidate })
	var effects []string
	serviceLifecycleFactory = func(string) (*serviceLifecycle, error) {
		effects = append(effects, "service-factory")
		return nil, errors.New("fixture service access blocked")
	}
	runProxyValidationCandidateFn = func(context.Context, proxyCommandOptions, string) (bool, error) {
		return true, errors.New("proxy validation candidate is unavailable")
	}
	for _, c := range internalABICorpus(t) {
		if c.Executable != "cq" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			effects = nil
			if !internalABIPlatformMatches(c.Platform) {
				c.Accepted, c.Exit, c.Stderr, c.Effects = false, 0, "", []string{}
			}
			var stdout, stderr string
			var handled bool
			var code int
			if c.Accepted && (c.Scenario == "role" || c.Scenario == "candidate" || c.Scenario == "namespace") {
				stdout, stderr, code = internalABIChild(t, c.Argv)
				handled = true // The helper exits 99 if dispatch did not recognise the command.
			} else {
				stdout, stderr = captureInternalABI(t, func() { handled, code = dispatchMachineABI(c.Argv) })
			}
			if handled != c.Accepted || code != c.Exit || stdout != c.Stdout || stderr != c.Stderr {
				t.Fatalf("dispatch = (%t, %d), stdout %q, stderr %q; want (%t, %d), %q, %q", handled, code, stdout, stderr, c.Accepted, c.Exit, c.Stdout, c.Stderr)
			}
			if !reflect.DeepEqual(append([]string{}, effects...), c.Effects) {
				t.Fatalf("effects = %v, want %v", effects, c.Effects)
			}
		})
	}
}

func captureInternalABI(t *testing.T, run func()) (string, string) {
	t.Helper()
	previousInput, previousOutput, previousError := os.Stdin, os.Stdout, os.Stderr
	files := make([]*os.File, 3)
	for i := range files {
		file, err := os.CreateTemp(t.TempDir(), "abi-stream")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		files[i] = file
	}
	os.Stdin, os.Stdout, os.Stderr = files[0], files[1], files[2]
	defer func() { os.Stdin, os.Stdout, os.Stderr = previousInput, previousOutput, previousError }()
	run()
	var result [2]string
	for i, file := range files[1:] {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		result[i] = string(data)
	}
	return result[0], result[1]
}

func internalABIChild(t *testing.T, argv []string) (string, string, int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("inherited ExtraFiles helper requires Unix")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestCLIV2InternalProcessHelper$", "--"}, argv...)...)
	command.Env = append(os.Environ(), "CQ_INTERNAL_ABI_HELPER=1")
	// Regular, empty files deliberately cannot confer socket/lock authority.
	for i := 0; i < 5; i++ {
		file, err := os.CreateTemp(t.TempDir(), "synthetic-descriptor")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		command.ExtraFiles = append(command.ExtraFiles, file)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

func TestCLIV2InternalProcessHelper(t *testing.T) {
	if os.Getenv("CQ_INTERNAL_ABI_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			handled, code := dispatchMachineABI(os.Args[i+1:])
			if !handled {
				os.Exit(99)
			}
			os.Exit(code)
		}
	}
	os.Exit(99)
}

func TestCLIV2InternalRuntimeGeneratorPairs(t *testing.T) {
	for _, c := range internalABICorpus(t) {
		if !c.Accepted {
			continue
		}
		switch c.Name {
		case "supervisor", "worker":
			manifest, err := proxy.ParseRuntimeRoleArguments(c.Argv[2:])
			if err != nil {
				t.Fatal(err)
			}
			if got := proxy.RuntimeRoleArguments(manifest); !reflect.DeepEqual(got, c.Argv[2:]) {
				t.Fatalf("runtime launcher argv = %q, want %q", got, c.Argv[2:])
			}
		case "candidate child":
			got := candidateRuntimeArguments(proxy.CandidateLifecycleStateV1{
				ProxyInstanceID: strings.Repeat("1", 32), ValidationRunID: strings.Repeat("2", 64), Generation: 7, Port: 23456,
			})
			if !reflect.DeepEqual(got, c.Argv) {
				t.Fatalf("candidate launcher argv = %q, want %q", got, c.Argv)
			}
		}
	}
}

func TestCLIV2InternalServiceTransaction(t *testing.T) {
	lifecycle, platform, store := newServiceHarness(t)
	previous := serviceLifecycleFactory
	serviceLifecycleFactory = func(string) (*serviceLifecycle, error) { return lifecycle, nil }
	t.Cleanup(func() { serviceLifecycleFactory = previous })
	lock, err := lifecycle.MutationLocker.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	inherited, err := installer.InheritedInstallLockFile(installer.ContextWithInstallLock(context.Background(), lock))
	if err != nil {
		t.Fatal(err)
	}
	defer inherited.Close()
	snapshotPath := filepath.Join(store.Roots.State, "snapshot.json")
	run := func(action string, tail ...string) {
		t.Helper()
		argv := append([]string{"service", action, "--owner=go", "--installer-lock-held"}, tail...)
		stdout, stderr := captureInternalABI(t, func() {
			os.Stdin = inherited
			handled, code := dispatchMachineABI(argv)
			if !handled || code != 0 {
				t.Errorf("%s = handled %t exit %d", action, handled, code)
			}
		})
		if stdout != "" || stderr != "" {
			t.Fatalf("%s stdout=%q stderr=%q", action, stdout, stderr)
		}
	}
	run("install")
	record, err := store.Load()
	if err != nil || record.Owner != installstate.OwnerGo {
		t.Fatalf("installed ownership=%v error=%v", record.Owner, err)
	}
	platform.proxyDefinition, platform.refreshDefinition = "proxy", "refresh"
	platform.proxyRunning = false
	run("snapshot", "--snapshot-file="+snapshotPath)
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := json.Marshal(lifecycle.Executable)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"owner":"go","executable":` + string(executable) + `,"platform":{"manager":"fake","components":[{"id":"proxy","definition":"cHJveHk=","exists":true},{"id":"refresh","definition":"cmVmcmVzaA==","exists":true}]}}`
	if string(data) != want {
		t.Fatalf("snapshot=%s, want %s", data, want)
	}
	info, err := os.Stat(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode=%o", info.Mode().Perm())
	}
	platform.proxyDefinition, platform.refreshDefinition, platform.proxyRunning = "changed", "changed", true
	run("restore", "--snapshot-file="+snapshotPath)
	if platform.proxyDefinition != "proxy" || platform.refreshDefinition != "refresh" || platform.proxyRunning {
		t.Fatal("restore did not reinstate exact definitions and stopped state")
	}
	run("uninstall")
	if _, err := store.Load(); !errors.Is(err, installstate.ErrNotInstalled) {
		t.Fatalf("uninstall state=%v", err)
	}
}

func TestCLIV2InternalServiceLockMarkerCannotGrantAuthority(t *testing.T) {
	for _, action := range []string{"install", "uninstall", "snapshot", "restore"} {
		t.Run(action, func(t *testing.T) {
			lifecycle, platform, store := newServiceHarness(t)
			previous := serviceLifecycleFactory
			serviceLifecycleFactory = func(string) (*serviceLifecycle, error) { return lifecycle, nil }
			defer func() { serviceLifecycleFactory = previous }()
			argv := []string{"service", action, "--owner=go", "--installer-lock-held"}
			if action == "snapshot" || action == "restore" {
				argv = append(argv, "--snapshot-file="+filepath.Join(store.Roots.State, "snapshot.json"))
			}
			stdout, stderr := captureInternalABI(t, func() {
				handled, code := dispatchMachineABI(argv)
				if !handled || code != 1 {
					t.Fatalf("handled=%t exit=%d", handled, code)
				}
			})
			if stdout != "" || !strings.HasPrefix(stderr, "cq: validate inherited installer lock: ") {
				t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
			}
			if len(platform.calls) != 0 {
				t.Fatalf("unverified marker touched service: %v", platform.calls)
			}
		})
	}
}

func TestCLIV2InternalServiceSnapshotRejectsInvalidDocument(t *testing.T) {
	for _, c := range internalABICorpus(t) {
		if c.Executable != "snapshot-payload" || !internalABIPlatformMatches(c.Platform) {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			lifecycle, platform, store := newServiceHarness(t)
			lifecycle.Executable = "/fixture/cq"
			path := filepath.Join(store.Roots.State, "snapshot.json")
			if err := fsutil.SecureAtomicWrite(fsutil.OSFileSystem{}, path, []byte(c.Snapshot)); err != nil {
				t.Fatal(err)
			}
			err := lifecycle.Restore(context.Background(), installstate.OwnerGo, path)
			if err == nil || "cq: "+err.Error()+"\n" != c.Stderr {
				t.Fatalf("restore error=%v, want %q", err, c.Stderr)
			}
			if len(platform.calls) != 0 {
				t.Fatalf("invalid snapshot touched platform: %v", platform.calls)
			}
		})
	}
}

func TestCLIV2InternalServiceOmittedOwner(t *testing.T) {
	for _, action := range []string{"install", "uninstall"} {
		command, err := parseServiceCommand([]string{action})
		if err != nil || command.Owner != installstate.OwnerManual || command.OwnerSet {
			t.Fatalf("omitted owner=%#v error=%v", command, err)
		}
		if _, err := parseServiceCommand([]string{action, "--owner=manual"}); err == nil {
			t.Fatal("explicit manual owner accepted")
		}
	}
}

func TestCLIV2InternalHookNativeOutput(t *testing.T) {
	var output bytes.Buffer
	err := runProxyCodexStopHook(context.Background(), strings.NewReader(`{"hook_event_name":"Stop","session_id":"s","turn_id":"t","future_field":true}`), &output, proxyCodexHookDependencies{
		LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "synthetic-local-token"}, nil },
		Doer: testDoer(func(request *http.Request) (*http.Response, error) {
			if request.URL.Host != "127.0.0.1:19280" {
				t.Fatalf("default lookup address=%s", request.URL.Host)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"schema_version":2,"found":false}`))}, nil
		}),
	})
	if err != nil || output.String() != "{}\n" {
		t.Fatalf("native output=%q error=%v", output.String(), err)
	}
}

// The literal Unix path corpus is complemented by the transaction test's
// native temporary paths. Namespace entrypoints exist only on Linux.
func internalABIPlatformMatches(platform string) bool {
	return platform == "" || platform == runtime.GOOS || (platform == "unix" && runtime.GOOS != "windows")
}
