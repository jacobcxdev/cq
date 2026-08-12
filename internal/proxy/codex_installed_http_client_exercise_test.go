package proxy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

type testCodexInstalledHTTPExerciseFunc func(context.Context) error

func (exercise testCodexInstalledHTTPExerciseFunc) Run(ctx context.Context) error {
	return exercise(ctx)
}

func TestCodexInstalledHTTPClientExerciseUsesIsolatedExactClient(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(executable, []byte("exact client"), 0o500); err != nil {
		t.Fatalf("write exact client: %v", err)
	}
	proof, err := captureCodexInstalledExecutable(executable)
	if err != nil {
		t.Fatalf("capture exact client: %v", err)
	}
	var command codexAcceptanceCommand
	localToken := "Qx9m0c9Yx6L-1wH2fBzE3pV8uN5kT7rS4aD6jG0lM2o"
	runner := testCodexAcceptanceRunner(func(_ context.Context, got codexAcceptanceCommand) ([]byte, error) {
		command = got
		auth, err := os.ReadFile(filepath.Join(commandEnv(got.env, "CODEX_HOME"), "auth.json"))
		if err != nil {
			return nil, err
		}
		if !bytes.Contains(auth, []byte(localToken)) || bytes.Contains(auth, []byte(codexAcceptanceLocalToken)) {
			return nil, errors.New("installed client did not receive exact per-run token")
		}
		if err := os.WriteFile(got.outputPath, []byte("PONG\n"), 0o600); err != nil {
			return nil, err
		}
		return nil, nil
	})
	outcome := &codexInstalledHTTPClientOutcome{}
	exercise, err := newCodexInstalledHTTPClientExercise("127.0.0.1:43123", proof, localToken, runner, outcome)
	if err != nil {
		t.Fatalf("new installed client exercise: %v", err)
	}
	if err := exercise.Run(context.Background()); err != nil {
		t.Fatalf("run installed client exercise: %v", err)
	}
	if !outcome.exactPong.Load() || outcome.egressAttempts.Load() != 0 {
		t.Fatalf("client outcome pong/egress = %v/%d, want true/0", outcome.exactPong.Load(), outcome.egressAttempts.Load())
	}
	if command.executable != proof.path || command.expectedExecutable != proof || !command.loopbackOnly {
		t.Fatalf("command executable proof = %q/%#v, want exact attested client", command.executable, command.expectedExecutable)
	}
	if command.sandboxWriteRoot == "" || filepath.Dir(command.dir) != command.sandboxWriteRoot {
		t.Fatalf("sandbox write root/dir = %q/%q", command.sandboxWriteRoot, command.dir)
	}
	joinedEnv := strings.Join(command.env, "\n")
	for _, key := range []string{"HOME=", "CODEX_HOME=", "TMPDIR=", "XDG_CACHE_HOME=", "XDG_CONFIG_HOME=", "XDG_DATA_HOME="} {
		if !strings.Contains(joinedEnv, key) {
			t.Fatalf("command environment missing %q: %s", key, joinedEnv)
		}
	}
	if _, err := os.Stat(command.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolation root survived exercise: %v", err)
	}
}

func TestCodexInstalledWebSocketClientExerciseUsesWebSocketProvider(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(executable, []byte("exact client"), 0o500); err != nil {
		t.Fatal(err)
	}
	proof, err := captureCodexInstalledExecutable(executable)
	if err != nil {
		t.Fatal(err)
	}
	localToken := "Qx9m0c9Yx6L-1wH2fBzE3pV8uN5kT7rS4aD6jG0lM2o"
	runner := testCodexAcceptanceRunner(func(_ context.Context, command codexAcceptanceCommand) ([]byte, error) {
		joined := strings.Join(command.args, "\n")
		if !strings.Contains(joined, `supports_websockets = true`) || strings.Contains(joined, `supports_websockets = false`) {
			return nil, errors.New("installed client did not use WebSocket provider")
		}
		return nil, os.WriteFile(command.outputPath, []byte("PONG\n"), 0o600)
	})
	exercise, err := newCodexInstalledWebSocketClientExercise(
		"127.0.0.1:43123", proof, localToken, runner, &codexInstalledHTTPClientOutcome{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := exercise.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCodexInstalledHTTPClientExerciseRequiresPerRunToken(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(executable, []byte("exact client"), 0o500); err != nil {
		t.Fatal(err)
	}
	proof, err := captureCodexInstalledExecutable(executable)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", codexAcceptanceLocalToken, "short"} {
		if exercise, err := newCodexInstalledHTTPClientExercise("127.0.0.1:43123", proof, token, testCodexAcceptanceRunner(func(context.Context, codexAcceptanceCommand) ([]byte, error) { return nil, nil }), &codexInstalledHTTPClientOutcome{}); err == nil || exercise != nil {
			t.Fatalf("token %q accepted", token)
		}
	}
}

func TestCodexAcceptanceSandboxProfilePermitsOnlyExactRunAuthority(t *testing.T) {
	root := filepath.Join(t.TempDir(), "isolated")
	profile, err := codexAcceptanceSandboxProfile(codexAcceptanceCommand{
		endpoint:         "http://127.0.0.1:43123/responses",
		egressProxyURL:   "http://127.0.0.1:43124",
		sandboxWriteRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, exact := range []string{`(deny network*)`, `(deny file-write*)`, `localhost:43123`, `localhost:43124`, root} {
		if !strings.Contains(profile, exact) {
			t.Fatalf("sandbox profile missing %q: %s", exact, profile)
		}
	}
	for _, broad := range []string{"localhost:*", "127.0.0.1:", filepath.Join(filepath.Dir(root), "other")} {
		if strings.Contains(profile, broad) {
			t.Fatalf("sandbox profile contains broad authority %q: %s", broad, profile)
		}
	}

	versionProfile, err := codexAcceptanceSandboxProfile(codexAcceptanceCommand{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(versionProfile, "allow network") || strings.Contains(versionProfile, "allow file-write") {
		t.Fatalf("version sandbox grants authority: %s", versionProfile)
	}
}

func TestCodexAcceptanceSandboxEnforcesExactWriteRoot(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec unavailable")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "run")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "inside.txt")
	outside := filepath.Join(parent, "outside.txt")
	runner := osCodexAcceptanceRunner{}
	base := codexAcceptanceCommand{
		executable:       "/bin/sh",
		env:              []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"},
		dir:              root,
		sandboxWriteRoot: root,
		loopbackOnly:     true,
	}
	insideCommand := base
	insideCommand.args = []string{"-c", `printf inside > "$1"`, "sh", inside}
	if _, err := runner.Run(context.Background(), insideCommand); err != nil {
		t.Fatalf("inside-root write failed: %v", err)
	}
	if data, err := os.ReadFile(inside); err != nil || string(data) != "inside" {
		t.Fatalf("inside-root result = %q, %v", data, err)
	}
	outsideCommand := base
	outsideCommand.args = []string{"-c", `printf outside > "$1"`, "sh", outside}
	if _, err := runner.Run(context.Background(), outsideCommand); err == nil {
		t.Fatal("outside-root write succeeded")
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside-root file exists: %v", err)
	}
}

func TestRunCodexInstalledVersionCommandUsesSandboxedRetainedExecutable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	systemExecutable, err := os.ReadFile(filepath.Join(runtime.GOROOT(), "bin", "go"))
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(executable, systemExecutable, 0o500); err != nil {
		t.Fatal(err)
	}
	clearBytes(systemExecutable)
	proof, err := captureCodexInstalledExecutable(executable)
	if err != nil {
		t.Fatal(err)
	}
	shortTempRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	writeRoot, err := os.MkdirTemp(shortTempRoot, codexInstalledHTTPClientTempPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := removeCodexInstalledHTTPClientTempRoot(writeRoot); err != nil {
			t.Error(err)
		}
	})
	directOutput, directErr := (osCodexAcceptanceRunner{}).Run(context.Background(), codexAcceptanceCommand{
		executable:         proof.path,
		expectedExecutable: proof,
		args:               []string{"version"},
		env:                append(codexAcceptanceBaseEnvironment("", "", "", "", ""), "GOROOT="+runtime.GOROOT()),
		sandboxWriteRoot:   writeRoot,
		captureOutput:      true,
		loopbackOnly:       true,
	})
	if directErr != nil {
		t.Fatalf("sandboxed retained Mach-O: %v", directErr)
	}
	if !strings.HasPrefix(string(directOutput), "go version ") {
		t.Fatalf("sandboxed retained Mach-O output = %q", directOutput)
	}

	script := filepath.Join(t.TempDir(), "codex-version")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\"\n"), 0o500); err != nil {
		t.Fatal(err)
	}
	scriptProof, err := captureCodexInstalledExecutable(script)
	if err != nil {
		t.Fatal(err)
	}
	output, err := runCodexInstalledVersionCommand(context.Background(), scriptProof.path, scriptProof)
	if err != nil {
		t.Fatalf("sandboxed version probe: %v", err)
	}
	if string(output) != "--version\n" {
		t.Fatalf("version output = %q", output)
	}
}

func TestRunCodexInstalledVersionCommandUsesInstalledCodexExecutable(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("CQ_RUN_CODEX_INSTALLED_ACCEPTANCE") != "1" {
		t.Skip("set CQ_RUN_CODEX_INSTALLED_ACCEPTANCE=1 on Darwin to exercise installed Codex")
	}
	const path = "/Applications/ChatGPT.app/Contents/Resources/codex"
	proof, err := captureCodexInstalledExecutable(path)
	if err != nil {
		t.Fatalf("capture installed Codex executable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexInstalledProcessProofTimeout)
	defer cancel()
	output, err := runCodexInstalledVersionCommand(ctx, path, proof)
	if err != nil {
		t.Fatalf("run retained installed Codex executable: %v", err)
	}
	if got, ok := parseCodexInstalledVersionOutput(output); !ok || got != "0.147.0-alpha.6.5" {
		t.Fatalf("installed Codex version = %q/%v, want exact installed build", got, ok)
	}
}

func TestCodexInstalledHTTPClientExerciseRejectsNonExactPong(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(executable, []byte("exact client"), 0o500); err != nil {
		t.Fatalf("write exact client: %v", err)
	}
	proof, err := captureCodexInstalledExecutable(executable)
	if err != nil {
		t.Fatalf("capture exact client: %v", err)
	}
	runner := testCodexAcceptanceRunner(func(_ context.Context, command codexAcceptanceCommand) ([]byte, error) {
		return nil, os.WriteFile(command.outputPath, []byte("PONG plus explanation\n"), 0o600)
	})
	outcome := &codexInstalledHTTPClientOutcome{}
	exercise, err := newCodexInstalledHTTPClientExercise("127.0.0.1:43123", proof, "Qx9m0c9Yx6L-1wH2fBzE3pV8uN5kT7rS4aD6jG0lM2o", runner, outcome)
	if err != nil {
		t.Fatalf("new installed client exercise: %v", err)
	}
	if err := exercise.Run(context.Background()); err == nil {
		t.Fatal("non-exact PONG exercise succeeded")
	}
	if outcome.exactPong.Load() {
		t.Fatal("non-exact PONG recorded as exact")
	}
}

func TestCodexInstalledHTTPCompositeExerciseRunsTrafficOnly(t *testing.T) {
	var first, second atomic.Uint64
	exercise := &codexInstalledHTTPCompositeExercise{
		first:  testCodexInstalledHTTPExerciseFunc(func(context.Context) error { first.Add(1); return nil }),
		second: testCodexInstalledHTTPExerciseFunc(func(context.Context) error { second.Add(1); return nil }),
	}
	if err := exercise.Run(context.Background()); err != nil {
		t.Fatalf("run composite exercise: %v", err)
	}
	if first.Load() != 1 || second.Load() != 1 {
		t.Fatalf("exercise calls = %d/%d, want 1/1", first.Load(), second.Load())
	}
}
