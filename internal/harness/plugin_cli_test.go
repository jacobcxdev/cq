package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const emptyNativeInventory = `{"marketplaces":[],"marketplaceLoadErrors":[]}`

func pluginCLIResponse(data string) PluginCLI {
	return PluginCLI{Executable: "/native/codex", Run: func(_ context.Context, _, _ string, output io.Writer) error {
		_, err := io.WriteString(output, data)
		return err
	}}
}

func TestPluginCLIInstalledFiltersAndDeduplicatesExactIDs(t *testing.T) {
	client := pluginCLIResponse(`{"marketplaceLoadErrors":[],"marketplaces":[{"plugins":[
		{"id":"z@market","installed":true,"enabled":true},
		{"id":"disabled@market","installed":true,"enabled":false},
		{"id":"absent@market","installed":false,"enabled":true},
		{"id":"a@market","installed":true,"enabled":true},
		{"id":"z@market","installed":true,"enabled":true},
		{"installed":false,"enabled":false}
	]}]}`)
	got, err := client.Installed(context.Background(), "/work/project")
	if err != nil || !slices.Equal(got, []string{"a@market", "z@market"}) {
		t.Fatalf("installed = %v, %v", got, err)
	}
}

func TestPluginCLIRejectsInvalidEligibleIDs(t *testing.T) {
	for name, row := range map[string]string{
		"missing":              `{"installed":true,"enabled":true}`,
		"empty":                `{"id":"","installed":true,"enabled":true}`,
		"space":                `{"id":" plugin@market","installed":true,"enabled":true}`,
		"nul":                  `{"id":"plugin\u0000@market","installed":true,"enabled":true}`,
		"wrong type":           `{"id":123,"installed":true,"enabled":true}`,
		"missing installed":    `{"id":"one@market","enabled":true}`,
		"missing enabled":      `{"id":"one@market","installed":true}`,
		"null installed":       `{"id":"one@market","installed":null,"enabled":true}`,
		"null enabled":         `{"id":"one@market","installed":true,"enabled":null}`,
		"unknown disabled row": `{"id":"one@market","installed":false}`,
		"too long":             `{"id":"` + strings.Repeat("a", 1025) + `","installed":true,"enabled":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := pluginCLIResponse(`{"marketplaceLoadErrors":[],"marketplaces":[{"plugins":[`+row+`]}]}`).Installed(context.Background(), "/work")
			if err == nil || got != nil {
				t.Fatalf("invalid eligible plugin = %v, %v", got, err)
			}
		})
	}
}

func TestPluginCLIRejectsMissingMalformedAndTrailingInventory(t *testing.T) {
	for name, data := range map[string]string{
		"empty":           "",
		"null":            `null`,
		"missing list":    `{}`,
		"null list":       `{"marketplaces":null,"marketplaceLoadErrors":[]}`,
		"wrong list type": `{"marketplaces":{},"marketplaceLoadErrors":[]}`,
		"wrong row type":  `{"marketplaces":[1],"marketplaceLoadErrors":[]}`,
		"missing plugins": `{"marketplaces":[{}],"marketplaceLoadErrors":[]}`,
		"null plugins":    `{"marketplaces":[{"plugins":null}],"marketplaceLoadErrors":[]}`,
		"load error":      `{"marketplaces":[],"marketplaceLoadErrors":[{"message":"private diagnostic"}]}`,
		"missing errors":  `{"marketplaces":[]}`,
		"invalid JSON":    `{"marketplaces":[`,
		"trailing JSON":   emptyNativeInventory + " " + emptyNativeInventory,
		"trailing text":   emptyNativeInventory + ` diagnostic`,
		"leading message": "warning\n" + emptyNativeInventory,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := pluginCLIResponse(data).Installed(context.Background(), "/work")
			if err == nil || got != nil {
				t.Fatalf("invalid inventory = %v, %v", got, err)
			}
		})
	}
	for _, data := range []string{emptyNativeInventory, "  " + emptyNativeInventory + "\n"} {
		got, err := pluginCLIResponse(data).Installed(context.Background(), "/work")
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("valid empty inventory = %v, %v", got, err)
		}
	}
}

func TestPluginCLIPassesExecutableProjectAndBoundedContext(t *testing.T) {
	cwd := "/work/project with spaces/../exact-input"
	client := PluginCLI{Executable: "/native executable/codex", Run: func(ctx context.Context, executable, project string, output io.Writer) error {
		if executable != "/native executable/codex" || project != cwd {
			t.Fatalf("inventory execution = %q, %q", executable, project)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 20*time.Second {
			t.Fatalf("inventory context is unbounded: %v, %t", deadline, ok)
		}
		_, err := io.WriteString(output, emptyNativeInventory)
		return err
	}}
	if _, err := client.Installed(context.Background(), cwd); err != nil {
		t.Fatal(err)
	}
}

func TestPluginCLICommandFailureDoesNotExposePrivateOutput(t *testing.T) {
	private := "private-token-from-native-process"
	client := PluginCLI{Run: func(_ context.Context, _, _ string, output io.Writer) error {
		_, _ = io.WriteString(output, emptyNativeInventory)
		return errors.New(private)
	}}
	got, err := client.Installed(context.Background(), "/work")
	if err == nil || got != nil || strings.Contains(err.Error(), private) {
		t.Fatalf("command failure = %v, %v", got, err)
	}
}

func TestPluginCLIOutputLimitRejectsEvenIgnoredWriterError(t *testing.T) {
	valid := emptyNativeInventory
	exact := valid + strings.Repeat(" ", (1<<20)-len(valid))
	if got, err := pluginCLIResponse(exact).Installed(context.Background(), "/work"); err != nil || len(got) != 0 {
		t.Fatalf("exact bounded inventory = %v, %v", got, err)
	}
	for _, singleWrite := range []bool{false, true} {
		client := PluginCLI{Run: func(_ context.Context, _, _ string, output io.Writer) error {
			if singleWrite {
				if count, err := io.WriteString(output, exact+" "); err == nil || count != 0 {
					t.Fatalf("oversized write = %d, %v", count, err)
				}
			} else {
				if _, err := io.WriteString(output, exact); err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(output, " "); err == nil {
					t.Fatal("incremental overflow was accepted")
				}
			}
			return nil // A runner cannot turn its ignored write failure into success.
		}}
		if got, err := client.Installed(context.Background(), "/work"); err == nil || got != nil {
			t.Fatalf("overflow inventory = %v, %v", got, err)
		}
	}
}

func TestPluginCLIOutputLimitAlsoBoundsReaderFromCopy(t *testing.T) {
	valid := emptyNativeInventory
	oversized := valid + strings.Repeat(" ", (1<<20)+1-len(valid))
	client := PluginCLI{Run: func(_ context.Context, _, _ string, output io.Writer) error {
		// Hide the source's WriterTo so io.Copy exercises any ReaderFrom method
		// promoted by the destination rather than going through Write directly.
		_, _ = io.Copy(output, struct{ io.Reader }{strings.NewReader(oversized)})
		return nil
	}}
	if got, err := client.Installed(context.Background(), "/work"); err == nil || got != nil {
		t.Fatalf("io.Copy bypassed output bound: %v, %v", got, err)
	}
}

func TestPluginCLINativePeerProtocolAndTeardown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("helper shell executable")
	}
	for _, scenario := range []string{"success", "init-error", "query-error", "malformed", "truncated", "oversized", "message-limit", "wrong-id", "nonzero-exit", "cancel", "inherited-pipe"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			executable := filepath.Join(dir, "native codex")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nexec \"$CQ_TEST_PLUGIN_TEST_BINARY\" -test.run=^TestPluginCLIHelperProcess$ -- \"$@\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			testBinary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("CQ_TEST_PLUGIN_TEST_BINARY", testBinary)
			t.Setenv("CQ_TEST_PLUGIN_HELPER", scenario)
			t.Setenv("CQ_TEST_PLUGIN_CWD", dir)
			t.Setenv("CQ_TEST_PLUGIN_RECEIPT", filepath.Join(dir, "receipt"))
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if scenario == "cancel" || scenario == "inherited-pipe" {
				cancel()
				bound := 100 * time.Millisecond
				if scenario == "inherited-pipe" {
					bound = 750 * time.Millisecond
				}
				ctx, cancel = context.WithTimeout(context.Background(), bound)
			}
			defer cancel()
			defer func() {
				if data, err := os.ReadFile(filepath.Join(dir, "holder-pid")); err == nil {
					pid, _ := strconv.Atoi(string(data))
					if process, err := os.FindProcess(pid); err == nil {
						_ = process.Kill()
						_ = process.Release()
					}
				}
			}()
			started := time.Now()
			ids, err := (PluginCLI{Executable: executable}).Installed(ctx, dir)
			if scenario == "inherited-pipe" {
				if _, err := os.Stat(filepath.Join(dir, "holder-pid")); err != nil {
					t.Fatal("fixture did not start stdout holder")
				}
				if time.Since(started) > 2*time.Second {
					t.Fatal("inventory cancellation waited for inherited stdout")
				}
			}
			if scenario == "success" {
				if err != nil || !slices.Equal(ids, []string{"one@market"}) {
					t.Fatalf("native inventory = %v, %v", ids, err)
				}
				receipt, err := os.ReadFile(filepath.Join(dir, "receipt"))
				if err != nil || string(receipt) != "stdin-closed" {
					t.Fatalf("child was not reaped after stdin EOF: %q, %v", receipt, err)
				}
			} else if err == nil || ids != nil {
				t.Fatalf("invalid peer accepted: %v, %v", ids, err)
			}
			if (scenario == "cancel" || scenario == "inherited-pipe") && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancellation was lost: %v", err)
			}
			if pidData, readErr := os.ReadFile(filepath.Join(dir, "pid")); readErr == nil {
				pid, _ := strconv.Atoi(string(pidData))
				process, _ := os.FindProcess(pid)
				if process != nil {
					defer process.Release()
					if err := process.Signal(syscall.Signal(0)); err == nil {
						t.Fatalf("native child %d remained alive", pid)
					}
				}
			}
		})
	}
}

func TestPluginCLIHelperProcess(t *testing.T) {
	scenario := os.Getenv("CQ_TEST_PLUGIN_HELPER")
	if scenario == "" {
		return
	}
	if scenario == "hold-stdout" {
		time.Sleep(3 * time.Second)
		os.Exit(0)
	}
	args := os.Args[len(os.Args)-3:]
	if !slices.Equal(args, []string{"app-server", "--stdio", "--strict-config"}) {
		os.Exit(70)
	}
	_ = os.WriteFile(filepath.Join(os.Getenv("CQ_TEST_PLUGIN_CWD"), "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	reader := bufio.NewReader(os.Stdin)
	read := func() map[string]any {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			os.Exit(71)
		}
		var value map[string]any
		if json.Unmarshal(line, &value) != nil {
			os.Exit(72)
		}
		return value
	}
	write := func(value any) {
		if json.NewEncoder(os.Stdout).Encode(value) != nil {
			os.Exit(73)
		}
	}
	first := read()
	if first["method"] != "initialize" {
		os.Exit(74)
	}
	if scenario == "inherited-pipe" {
		child := exec.Command(os.Args[0], "-test.run=^TestPluginCLIHelperProcess$")
		child.Env = append(os.Environ(), "CQ_TEST_PLUGIN_HELPER=hold-stdout")
		child.Stdout = os.Stdout
		if child.Start() != nil {
			os.Exit(80)
		}
		_ = os.WriteFile(filepath.Join(os.Getenv("CQ_TEST_PLUGIN_CWD"), "holder-pid"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		_, _ = io.Copy(io.Discard, reader)
		os.Exit(0)
	}
	if scenario == "cancel" {
		_, _ = io.Copy(io.Discard, reader)
		os.Exit(75)
	}
	if scenario == "init-error" {
		write(map[string]any{"id": first["id"], "error": map[string]any{"code": -1, "message": "private diagnostic"}})
		_, _ = io.Copy(io.Discard, reader)
		os.Exit(0)
	}
	if scenario == "malformed" {
		_, _ = io.WriteString(os.Stdout, "not JSON\n")
		_, _ = io.Copy(io.Discard, reader)
		os.Exit(0)
	}
	if scenario == "oversized" {
		_, _ = io.WriteString(os.Stdout, strings.Repeat(" ", 1<<20)+"\n")
		_, _ = io.Copy(io.Discard, reader)
		os.Exit(0)
	}
	if scenario == "message-limit" {
		for range 129 {
			write(map[string]any{"id": 99, "result": map[string]any{}})
		}
		_, _ = io.Copy(io.Discard, reader)
		os.Exit(0)
	}
	write(map[string]any{"id": 99, "result": map[string]any{}})
	write(map[string]any{"id": first["id"], "result": map[string]any{}})
	if read()["method"] != "initialized" {
		os.Exit(76)
	}
	query := read()
	if query["method"] != "plugin/installed" {
		os.Exit(77)
	}
	params, ok := query["params"].(map[string]any)
	if !ok || !reflect.DeepEqual(params["cwds"], []any{os.Getenv("CQ_TEST_PLUGIN_CWD")}) {
		os.Exit(78)
	}
	if scenario == "truncated" {
		_, _ = io.WriteString(os.Stdout, `{"id":2,"result":`)
		os.Exit(0)
	}
	if scenario == "query-error" {
		write(map[string]any{"id": query["id"], "error": map[string]any{"code": -1}})
	} else {
		id := query["id"]
		if scenario == "wrong-id" {
			id = 999
		}
		write(map[string]any{"id": id, "result": json.RawMessage(`{"marketplaces":[{"plugins":[{"id":"one@market","installed":true,"enabled":true}]}],"marketplaceLoadErrors":[]}`)})
	}
	if scenario == "wrong-id" {
		os.Exit(0)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		os.Exit(79)
	}
	_ = os.WriteFile(os.Getenv("CQ_TEST_PLUGIN_RECEIPT"), []byte("stdin-closed"), 0o600)
	if scenario == "nonzero-exit" {
		os.Exit(7)
	}
	os.Exit(0)
}
