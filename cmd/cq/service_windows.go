//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
	"golang.org/x/sys/windows"
)

var (
	loadWindowsProxyPort = func() (int, error) {
		config, err := proxy.LoadExistingConfig()
		if err != nil {
			return 0, err
		}
		return config.Port, nil
	}
	runWindowsNetstat = func(ctx context.Context) ([]byte, error) {
		return exec.CommandContext(ctx, "netstat.exe", "-ano", "-p", "tcp").Output()
	}
	windowsProcessExecutable = queryWindowsProcessExecutable
	checkWindowsProxyHealth  = checkWindowsProxyHTTPHealth
)

func init() {
	serviceLifecycleFactory = defaultWindowsServiceLifecycle
}

func defaultWindowsServiceLifecycle() (*serviceLifecycle, error) {
	roots, err := userdirs.Default()
	if err != nil {
		return nil, err
	}
	executable, err := resolveWindowsExecutable()
	if err != nil {
		return nil, err
	}
	sid, err := currentWindowsServiceSID()
	if err != nil {
		return nil, err
	}
	return newWindowsServiceLifecycle(
		executable,
		sid,
		roots,
		runWindowsSchtasks,
		queryWindowsTaskState,
		inspectWindowsProxyRuntime,
	), nil
}

func newWindowsServiceLifecycle(
	executable string,
	sid string,
	roots userdirs.Roots,
	run func(context.Context, ...string) ([]byte, error),
	queryState func(context.Context, string) (windowsTaskRuntimeState, error),
	inspectProxy func(context.Context, string) componentStatus,
) *serviceLifecycle {
	platform := &windowsTaskServicePlatform{
		sid:             sid,
		executable:      executable,
		temporaryRoot:   filepath.Join(roots.State, "task-xml"),
		refreshInterval: 1800,
		run:             run,
		queryState:      queryState,
		inspectProxy:    inspectProxy,
	}
	return &serviceLifecycle{
		Platform:       platform,
		Store:          &installstate.Store{FS: fsutil.OSFileSystem{}, Roots: roots},
		Executable:     executable,
		Version:        version,
		StatusAttempts: 30,
		StatusInterval: time.Second,
	}
}

func currentWindowsServiceSID() (string, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "", fmt.Errorf("open current Windows process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("read current Windows token user: %w", err)
	}
	if user.User.Sid == nil || !user.User.Sid.IsValid() {
		return "", fmt.Errorf("current Windows token has invalid SID")
	}
	return user.User.Sid.String(), nil
}

func resolveWindowsExecutable() (string, error) {
	if executable, err := exec.LookPath("cq.exe"); err == nil {
		absolute, err := filepath.Abs(executable)
		if err == nil {
			return filepath.Clean(absolute), nil
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve current executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", fmt.Errorf("resolve absolute executable: %w", err)
	}
	return filepath.Clean(executable), nil
}

func runWindowsSchtasks(ctx context.Context, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, "schtasks.exe", args...).CombinedOutput()
	if err == nil {
		return output, nil
	}
	message := strings.TrimSpace(string(output))
	lowerMessage := strings.ToLower(message)
	code := uint32(1)
	if strings.Contains(lowerMessage, "cannot find") || strings.Contains(lowerMessage, "not exist") {
		code = windowsTaskNotFound
	} else if strings.Contains(lowerMessage, "already running") {
		code = windowsTaskAlreadyRunning
	} else {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			code = uint32(exitError.ExitCode())
		}
	}
	return output, windowsTaskCommandError{Code: code, Output: message}
}

func queryWindowsTaskState(ctx context.Context, taskPath string) (windowsTaskRuntimeState, error) {
	taskFolder, taskName, err := splitWindowsTaskPath(taskPath)
	if err != nil {
		return windowsTaskRuntimeState{}, err
	}
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; $task=Get-ScheduledTask -TaskPath '%s' -TaskName '%s'; $info=$task | Get-ScheduledTaskInfo; [pscustomobject]@{State=[string]$task.State;LastResult=[uint32]$info.LastTaskResult} | ConvertTo-Json -Compress`,
		taskFolder,
		taskName,
	)
	output, err := exec.CommandContext(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if strings.Contains(strings.ToLower(message), "no scheduledtask objects") || strings.Contains(strings.ToLower(message), "cannot find") {
			return windowsTaskRuntimeState{}, windowsTaskCommandError{Code: windowsTaskNotFound, Output: message}
		}
		return windowsTaskRuntimeState{}, fmt.Errorf("query Windows task runtime: %w: %s", err, message)
	}
	if len(output) > 64<<10 {
		return windowsTaskRuntimeState{}, fmt.Errorf("Windows task runtime output exceeds size limit")
	}
	var state struct {
		State      string `json:"State"`
		LastResult uint32 `json:"LastResult"`
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return windowsTaskRuntimeState{}, fmt.Errorf("decode Windows task runtime: %w", err)
	}
	return windowsTaskRuntimeState{
		Running:       strings.EqualFold(state.State, "Running"),
		LastResult:    state.LastResult,
		HasLastResult: true,
	}, nil
}

func splitWindowsTaskPath(taskPath string) (string, string, error) {
	if !strings.HasPrefix(taskPath, `\cq\`) {
		return "", "", fmt.Errorf("unexpected Windows task path %q", taskPath)
	}
	name := strings.TrimPrefix(taskPath, `\cq\`)
	if name == "" || strings.Contains(name, `\`) {
		return "", "", fmt.Errorf("unexpected Windows task path %q", taskPath)
	}
	return `\cq\`, name, nil
}

func inspectWindowsProxyRuntime(ctx context.Context, executable string) componentStatus {
	status := componentStatus{ID: windowsProxyTaskPath, Manager: "task-scheduler"}
	port, err := loadWindowsProxyPort()
	if err != nil || port <= 0 || port > 65535 {
		status.Error = "proxy configuration is unavailable"
		return status
	}
	output, err := runWindowsNetstat(ctx)
	if err != nil {
		status.Error = "proxy listener inspection failed"
		return status
	}
	pid, err := parseWindowsListeningPID(output, port)
	if err != nil {
		status.Error = err.Error()
		return status
	}
	liveExecutable, err := windowsProcessExecutable(pid)
	if err != nil {
		status.Error = "proxy process inspection failed"
		return status
	}
	status.Running = true
	status.PID = int(pid)
	status.Listener = fmt.Sprintf("127.0.0.1:%d", port)
	status.LiveExecutable = liveExecutable
	if !sameServiceExecutable(liveExecutable, executable) {
		status.Error = "proxy listener executable differs from configured task"
		return status
	}
	if err := checkWindowsProxyHealth(ctx, status.Listener); err != nil {
		status.Error = "proxy health check failed"
		return status
	}
	status.Healthy = true
	return status
}

func parseWindowsListeningPID(output []byte, port int) (uint32, error) {
	wantAddress := fmt.Sprintf("127.0.0.1:%d", port)
	var found uint32
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 5 || !strings.EqualFold(fields[0], "TCP") || !strings.EqualFold(fields[3], "LISTENING") || fields[1] != wantAddress {
			continue
		}
		pid, err := strconv.ParseUint(fields[4], 10, 32)
		if err != nil || pid == 0 {
			return 0, fmt.Errorf("proxy listener PID is invalid")
		}
		if found != 0 && found != uint32(pid) {
			return 0, fmt.Errorf("proxy listener ownership is ambiguous")
		}
		found = uint32(pid)
	}
	if found == 0 {
		return 0, fmt.Errorf("proxy listener is absent")
	}
	return found, nil
}

func queryWindowsProcessExecutable(pid uint32) (string, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(process)
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size); err != nil {
		return "", err
	}
	if size == 0 || int(size) > len(buffer) {
		return "", fmt.Errorf("invalid process executable length")
	}
	return string(utf16.Decode(buffer[:size])), nil
}

func checkWindowsProxyHTTPHealth(ctx context.Context, address string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/health", http.NoBody)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("proxy health redirect refused")
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy health returned %s", response.Status)
	}
	return nil
}

func ensureAgent() {}

func installAgent(interval int) error {
	lifecycle, err := defaultWindowsServiceLifecycle()
	if err != nil {
		return err
	}
	platform := lifecycle.Platform.(*windowsTaskServicePlatform)
	platform.refreshInterval = normaliseWindowsRefreshInterval(interval)
	ctx := context.Background()
	if err := platform.Preflight(ctx, lifecycle.Executable); err != nil {
		return err
	}
	return platform.InstallRefresh(ctx, lifecycle.Executable)
}

func uninstallAgent() error {
	lifecycle, err := defaultWindowsServiceLifecycle()
	if err != nil {
		return err
	}
	return lifecycle.Platform.RemoveRefresh(context.Background())
}

func installProxyAgent() error {
	lifecycle, err := defaultWindowsServiceLifecycle()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := lifecycle.Platform.Preflight(ctx, lifecycle.Executable); err != nil {
		return err
	}
	return lifecycle.Platform.InstallProxy(ctx, lifecycle.Executable)
}

func uninstallProxyAgent() error {
	lifecycle, err := defaultWindowsServiceLifecycle()
	if err != nil {
		return err
	}
	return lifecycle.Platform.RemoveProxy(context.Background())
}

func restartProxyAgent() error {
	lifecycle, err := defaultWindowsServiceLifecycle()
	if err != nil {
		return err
	}
	return lifecycle.Platform.RestartProxy(context.Background())
}

func normaliseWindowsRefreshInterval(interval int) int {
	if interval <= 0 {
		return 1800
	}
	return interval
}
