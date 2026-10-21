package dots

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	if os.Getenv("CQ_NATIVE_PROCESS_TEST") != "bridge" || len(os.Args) < 2 {
		return
	}
	var cfg Settings
	if json.Unmarshal([]byte(os.Getenv("CQ_NATIVE_PROCESS_CONFIG")), &cfg) != nil {
		os.Exit(2)
	}
	if os.Args[1] == "__dots" {
		if len(os.Args) < 4 || os.Args[2] != "cloud-stdio-bridge" {
			os.Exit(3)
		}
		local, err := cloudBridgeLocal()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(4)
		}
		deps := cloudStdioDefaults(&cfg)
		deps.launch = func(context.Context, string, []string, []string, io.Writer) (*cloudLocal, error) { return local, nil }
		if err := runCloudStdio(context.Background(), &cfg, nil, os.Args[4:], os.Stdin, os.Stdout, os.Stderr, deps); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(5)
		}
		os.Exit(0)
	}
	if os.Args[1] == "app-server" {
		reader := bufio.NewScanner(os.Stdin)
		for reader.Scan() {
			var request cloudMessage
			if json.Unmarshal(reader.Bytes(), &request) != nil {
				os.Exit(6)
			}
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"id": request["id"], "result": map[string]any{
				"pid": os.Getpid(), "parent": os.Getppid(), "request": json.RawMessage(reader.Bytes()),
			}})
		}
		os.Exit(7)
	}
}

func TestCloudBridgePreservesNativeAncestryAndRPCs(t *testing.T) {
	root := t.TempDir()
	cfg := Settings{AppPath: filepath.Join(root, "App.app"), RuntimeRoot: filepath.Join(root, "runtime")}
	path, _, _, _, err := cloudInvocation(&cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{filepath.Dir(path), cfg.RuntimeRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(os.Args[0], path); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCloudNativeProcessHelper$")
	command.Env = append(os.Environ(), "CQ_NATIVE_PROCESS_TEST=bridge-wrapper", "CQ_NATIVE_PROCESS_CONFIG="+string(data))
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	reader := bufio.NewReader(output)
	for _, request := range []string{
		`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"native-test","version":"1"}}}`,
		`{"id":2,"method":"thread/start","params":{"cwd":"/synthetic/workspace"}}`,
		`{"id":3,"method":"turn/start","params":{"threadId":"ordinary-task","_meta":{"x-codex-turn-metadata":{"session_id":"synthetic-session","turn_id":"synthetic-turn"}}}}`,
	} {
		if _, err := fmt.Fprintln(input, request); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("bridge response failed: %v", err)
		}
		var response struct {
			Result struct {
				PID     int             `json:"pid"`
				Parent  int             `json:"parent"`
				Request json.RawMessage `json:"request"`
			} `json:"result"`
		}
		if err := json.Unmarshal(line, &response); err != nil {
			t.Fatal(err)
		}
		if response.Result.PID != command.Process.Pid || response.Result.Parent != os.Getpid() {
			t.Fatalf("bridge changed native ancestry: %+v", response.Result)
		}
		if string(response.Result.Request) != request {
			t.Fatalf("bridge changed workspace or turn metadata: %s", response.Result.Request)
		}
	}
	_ = input.Close()
	if err := command.Wait(); err == nil {
		t.Fatal("bridge lost native exit status")
	} else if exited, ok := err.(*exec.ExitError); !ok || exited.ExitCode() != 7 {
		t.Fatalf("bridge native exit status: %v", err)
	}
}

func TestCloudStockPreservesNativeProcessIdentity(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestCloudNativeProcessHelper$")
	command.Env = append(os.Environ(), "CQ_NATIVE_PROCESS_TEST=wrapper")
	command.Stdin = strings.NewReader("synthetic input")
	output, err := command.Output()
	if exited, ok := err.(*exec.ExitError); !ok || exited.ExitCode() != 7 {
		t.Fatalf("native exit status changed: %v", err)
	}
	var result struct {
		PID   int    `json:"pid"`
		Input string `json:"input"`
		Pipe  string `json:"pipe"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	if result.PID != command.Process.Pid {
		t.Fatalf("unsigned wrapper remained in native ancestry: native PID %d, wrapper PID %d", result.PID, command.Process.Pid)
	}
	if result.Input != "synthetic input" || result.Pipe != "synthetic-native-pipe" {
		t.Fatalf("native input or environment changed: %+v", result)
	}
}

func TestCloudBridgeRejectedInvalidDescriptors(t *testing.T) {
	input, toNative, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer toNative.Close()
	fromNative, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer fromNative.Close()
	defer output.Close()
	file, err := os.CreateTemp(t.TempDir(), "not-a-pipe")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, test := range []struct {
		name  string
		files []*os.File
	}{
		{"regular-file", []*os.File{file, fromNative}},
		{"read-only-input", []*os.File{input, fromNative}},
		{"write-only-output", []*os.File{toNative, output}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCloudNativeProcessHelper$")
			command.Env = append(os.Environ(), "CQ_NATIVE_PROCESS_TEST=invalid-descriptors")
			command.ExtraFiles = test.files
			if result, err := command.CombinedOutput(); err != nil {
				t.Fatalf("invalid descriptors accepted: %v: %s", err, result)
			}
		})
	}
}

func TestCloudStockReplacementFailure(t *testing.T) {
	handled, err := cloudStockExec(filepath.Join(t.TempDir(), "absent"), nil, os.Environ(), os.Stdin, os.Stdout, os.Stderr)
	if !handled || err == nil || err.Error() != "stock Codex CLI replacement failed" {
		t.Fatalf("replacement failed without a terminal error: %v, %v", handled, err)
	}
}

func TestCloudNativeProcessHelper(t *testing.T) {
	switch os.Getenv("CQ_NATIVE_PROCESS_TEST") {
	case "invalid-descriptors":
		local, err := cloudBridgeLocal()
		if err != nil && err.Error() == "native bridge descriptors invalid" {
			os.Exit(0)
		}
		if local != nil {
			local.stop()
		}
		os.Exit(1)
	case "bridge-wrapper":
		var cfg Settings
		if json.Unmarshal([]byte(os.Getenv("CQ_NATIVE_PROCESS_CONFIG")), &cfg) != nil {
			os.Exit(2)
		}
		_ = os.Setenv("CQ_NATIVE_PROCESS_TEST", "bridge")
		err := RunCloudStdio(context.Background(), &cfg, nil, []string{"app-server", "-c", cloudAppMarker + "true"}, os.Stdin, os.Stdout, os.Stderr, nil)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	case "wrapper":
		environment := setEnvironment(os.Environ(), "CQ_NATIVE_PROCESS_TEST", "native")
		environment = setEnvironment(environment, "CODEX_APP_TOOLS_PIPE_PATH", "synthetic-native-pipe")
		err := cloudStock(context.Background(), os.Args[0], []string{"-test.run=^TestCloudNativeProcessHelper$"}, environment, os.Stdin, os.Stdout, os.Stderr)
		if exited, ok := err.(*CloudStdioExitError); ok {
			os.Exit(exited.Code)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	case "native":
		input, err := io.ReadAll(io.LimitReader(os.Stdin, 1024))
		if err != nil {
			os.Exit(2)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"pid": os.Getpid(), "input": string(input), "pipe": os.Getenv("CODEX_APP_TOOLS_PIPE_PATH")})
		os.Exit(7)
	}
}
