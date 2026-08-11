package proxy

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/modelregistry"
)

func TestCodexInstalledTaskAffinityUsesHardLimitOnlyFailover(t *testing.T) {
	if os.Getenv("CQ_RUN_CODEX_TASK_AFFINITY_ACCEPTANCE") != "1" {
		t.Skip("installed Codex task-affinity acceptance requires explicit opt-in")
	}
	clientPath, err := resolveCodexInstalledClientExecutable()
	if err != nil {
		t.Skip("installed Codex client unavailable")
	}
	clientProof, err := captureCodexInstalledExecutable(clientPath)
	if err != nil {
		t.Fatalf("capture installed Codex client: %v", err)
	}
	core, err := newCodexInstalledHTTPValidationRuntimeCore(context.Background())
	if err != nil {
		t.Fatalf("open authoritative validation runtime: %v", err)
	}
	t.Cleanup(func() {
		if err := core.close(); err != nil {
			t.Errorf("close authoritative validation runtime: %v", err)
		}
	})
	localToken, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		Config: &Config{
			LocalToken:     localToken,
			ClaudeUpstream: "http://" + core.upstream.address,
			CodexUpstream:  "http://" + core.upstream.address,
		},
		CodexNativeHTTP: core.nativeHTTPHandler(),
		Catalog:         modelregistry.NewCatalog(modelregistry.Snapshot{}),
	}
	handler, err := server.handler()
	if err != nil {
		t.Fatalf("construct candidate handler: %v", err)
	}
	listener := httptest.NewServer(handler)
	t.Cleanup(listener.Close)

	isolation := newCodexTaskAffinityAcceptanceIsolation(t, localToken)
	runner := codexTaskAffinityAcceptanceRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := runCodexTaskAffinityAcceptanceTurn(ctx, runner, clientProof, listener.URL, isolation, false, "PONG"); err != nil {
		t.Fatalf("first Codex task turn: %v", err)
	}
	if err := core.upstream.armNextNewTurnHardReplay(); err != nil {
		t.Fatalf("arm hard-limit replay: %v", err)
	}
	if err := runCodexTaskAffinityAcceptanceTurn(ctx, runner, clientProof, listener.URL, isolation, true, "PONG"); err != nil {
		t.Fatalf("second Codex task turn: %v", err)
	}
	if err := runCodexTaskAffinityAcceptanceTurn(ctx, runner, clientProof, listener.URL, isolation, true, "PONG"); err != nil {
		t.Fatalf("third Codex task turn: %v", err)
	}

	routes := core.upstream.routeHistory()
	if len(routes) != 4 {
		t.Fatalf("upstream route count = %d, want 4", len(routes))
	}
	if routes[0].AccountID != "validation-upstream-a" || routes[0].Status != 200 ||
		routes[1].AccountID != "validation-upstream-a" || routes[1].Status != 429 ||
		routes[2].AccountID != "validation-upstream-b" || routes[2].Status != 200 ||
		routes[3].AccountID != "validation-upstream-b" || routes[3].Status != 200 {
		t.Fatal("upstream route sequence did not match A/200 A/429 B/200 B/200")
	}
	sessions := []string{routes[0].Metadata.SessionID, routes[1].Metadata.SessionID, routes[2].Metadata.SessionID, routes[3].Metadata.SessionID}
	threads := []string{routes[0].Metadata.ThreadID, routes[1].Metadata.ThreadID, routes[2].Metadata.ThreadID, routes[3].Metadata.ThreadID}
	if !allCodexTaskAffinityValuesEqual(sessions) || !allCodexTaskAffinityValuesEqual(threads) {
		t.Fatal("resumed task session or thread identity changed")
	}
	turns := []string{routes[0].Metadata.TurnID, routes[1].Metadata.TurnID, routes[2].Metadata.TurnID, routes[3].Metadata.TurnID}
	if turns[0] == turns[1] || turns[1] != turns[2] || turns[2] == turns[3] {
		t.Fatal("turn identity sequence did not preserve failover retry identity")
	}
}

type codexTaskAffinityAcceptanceRunner struct{}

func (runner codexTaskAffinityAcceptanceRunner) Run(ctx context.Context, command codexAcceptanceCommand) ([]byte, error) {
	if runtime.GOOS != "darwin" || command.executable == "" || !command.expectedExecutable.valid() || !command.loopbackOnly {
		return nil, errors.New("Codex task-affinity runner unavailable")
	}
	before, err := captureCodexInstalledExecutable(command.executable)
	if err != nil || before != command.expectedExecutable {
		return nil, errors.New("Codex task-affinity executable changed")
	}
	profile, err := codexAcceptanceSandboxProfile(command)
	if err != nil {
		return nil, errors.New("Codex task-affinity sandbox unavailable")
	}
	arguments := append([]string{"-p", profile, command.executable}, command.args...)
	child := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", arguments...)
	child.Env = append([]string(nil), command.env...)
	child.Dir = command.dir
	child.Stdin = strings.NewReader("")
	child.Stdout = io.Discard
	child.Stderr = io.Discard
	child.WaitDelay = 2 * time.Second
	if err := child.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("Codex task-affinity command timed out")
		}
		return nil, errors.New("Codex task-affinity command failed")
	}
	after, err := captureCodexInstalledExecutable(command.executable)
	if err != nil || after != command.expectedExecutable {
		return nil, errors.New("Codex task-affinity executable changed")
	}
	return nil, nil
}

type codexTaskAffinityAcceptanceIsolation struct {
	root      string
	home      string
	codexHome string
	work      string
	tmp       string
	cache     string
	config    string
	data      string
}

func newCodexTaskAffinityAcceptanceIsolation(t *testing.T, localToken string) codexTaskAffinityAcceptanceIsolation {
	t.Helper()
	shortTempRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(shortTempRoot, "cq-codex-task-affinity-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	isolation := codexTaskAffinityAcceptanceIsolation{
		root:      root,
		home:      filepath.Join(root, "home"),
		codexHome: filepath.Join(root, "codex-home"),
		work:      filepath.Join(root, "work"),
		tmp:       filepath.Join(root, "tmp"),
		cache:     filepath.Join(root, "cache"),
		config:    filepath.Join(root, "config"),
		data:      filepath.Join(root, "data"),
	}
	for _, directory := range []string{isolation.home, isolation.codexHome, isolation.work, isolation.tmp, isolation.cache, isolation.config, isolation.data} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeCodexAcceptanceAuthWithToken(filepath.Join(isolation.codexHome, "auth.json"), localToken); err != nil {
		t.Fatal(err)
	}
	return isolation
}

func runCodexTaskAffinityAcceptanceTurn(
	ctx context.Context,
	runner codexAcceptanceRunner,
	client codexInstalledExecutableProof,
	baseURL string,
	isolation codexTaskAffinityAcceptanceIsolation,
	resume bool,
	output string,
) error {
	outputPath := filepath.Join(isolation.root, strings.ToLower(output)+".txt")
	args := codexAcceptanceExecArguments(baseURL, isolation.work, outputPath)
	args = slices.DeleteFunc(args, func(value string) bool { return value == "--ephemeral" })
	args[len(args)-1] = "Reply with exactly " + output + " and no other text."
	if resume {
		args = slices.Insert(args, 1, "resume", "--last")
		args = removeCodexTaskAffinityUnsupportedResumeArguments(args)
	}
	environment := append(codexAcceptanceBaseEnvironment(isolation.home, isolation.codexHome, isolation.tmp, isolation.cache, isolation.config),
		"XDG_DATA_HOME="+isolation.data,
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
	)
	_, err := runner.Run(ctx, codexAcceptanceCommand{
		executable:         client.path,
		expectedExecutable: client,
		args:               args,
		env:                environment,
		dir:                isolation.work,
		endpoint:           baseURL + legacyCodexResponsesPath,
		outputPath:         outputPath,
		sandboxWriteRoot:   isolation.root,
		loopbackOnly:       true,
	})
	if err != nil {
		return err
	}
	result, err := os.ReadFile(outputPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(result)) != output {
		return errors.New("Codex task-affinity output mismatch")
	}
	return nil
}

func removeCodexTaskAffinityUnsupportedResumeArguments(args []string) []string {
	result := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] == "--color" || args[index] == "-s" || args[index] == "-C" {
			index++
			continue
		}
		result = append(result, args[index])
	}
	return result
}

func allCodexTaskAffinityValuesEqual(values []string) bool {
	return len(values) > 0 && values[0] != "" && slices.Equal(values, slices.Repeat([]string{values[0]}, len(values)))
}
