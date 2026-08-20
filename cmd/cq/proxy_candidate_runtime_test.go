package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestCandidateRuntimeProcessHelper(t *testing.T) {
	if os.Getenv("CQ_CANDIDATE_RUNTIME_HELPER") != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 {
		os.Exit(2)
	}
	if err := runCandidateRuntimeChild(context.Background(), os.Args[separator+3:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestCandidateRuntimeStartsAuthenticatesAndStopsRealChild(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	previousExecutable, previousCommand := candidateRuntimeExecutable, candidateRuntimeCommand
	candidateRuntimeExecutable = func() (string, error) { return os.Executable() }
	candidateRuntimeCommand = func(_ string, arguments ...string) *exec.Cmd {
		commandArguments := append([]string{"-test.run=^TestCandidateRuntimeProcessHelper$", "--"}, arguments...)
		command := exec.Command(os.Args[0], commandArguments...)
		command.Env = append(os.Environ(), "CQ_CANDIDATE_RUNTIME_HELPER=1")
		return command
	}
	t.Cleanup(func() { candidateRuntimeExecutable, candidateRuntimeCommand = previousExecutable, previousCommand })
	state := proxy.CandidateLifecycleStateV1{
		ProxyInstanceID: strings.Repeat("1", 32), ValidationRunID: strings.Repeat("2", 64),
		Port: port, Generation: 7,
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started, err := startCandidateRuntime(ctx, t.TempDir(), state, token)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(started), state.ProxyInstanceID) {
		t.Fatalf("start receipt = %q", started)
	}
	if _, err := inspectCandidateRuntime(ctx, port, make([]byte, 32)); err == nil {
		t.Fatal("wrong control token accepted")
	}
	stopped, err := stopCandidateRuntime(ctx, t.TempDir(), state, token)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stopped), strconv.Itoa(int(state.Generation))) {
		t.Fatalf("stop receipt = %q", stopped)
	}
}

func TestCandidateRuntimeArtifactSwitchWaitsForReplacement(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	previousExecutable, previousCommand := candidateRuntimeExecutable, candidateRuntimeCommand
	candidateRuntimeExecutable = func() (string, error) { return os.Executable() }
	candidateRuntimeCommand = func(_ string, arguments ...string) *exec.Cmd {
		command := exec.Command(os.Args[0], append([]string{"-test.run=^TestCandidateRuntimeProcessHelper$", "--"}, arguments...)...)
		command.Env = append(os.Environ(), "CQ_CANDIDATE_RUNTIME_HELPER=1")
		return command
	}
	t.Cleanup(func() { candidateRuntimeExecutable, candidateRuntimeCommand = previousExecutable, previousCommand })
	state := proxy.CandidateLifecycleStateV1{ProxyInstanceID: strings.Repeat("3", 32), ValidationRunID: strings.Repeat("4", 64), Port: port, Generation: 8}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := startCandidateRuntime(ctx, t.TempDir(), state, token); err != nil {
		t.Fatal(err)
	}
	if _, err := switchCandidateRuntimeArtifact(ctx, t.TempDir(), state, token); err != nil {
		t.Fatal(err)
	}
	if health, err := inspectCandidateRuntime(ctx, port, token); err != nil || health.ProxyInstanceID != state.ProxyInstanceID {
		t.Fatalf("replacement health = %#v, %v", health, err)
	}
	if _, err := stopCandidateRuntime(ctx, t.TempDir(), state, token); err != nil {
		t.Fatal(err)
	}
}
