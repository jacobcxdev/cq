//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installer"
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

func defaultWindowsServiceLifecycle(stableExecutable string) (*serviceLifecycle, error) {
	roots, err := userdirs.Default()
	if err != nil {
		return nil, err
	}
	executable, err := resolveServiceExecutable(stableExecutable)
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
		queryFolder:     queryWindowsTaskFolderState,
		createFolder:    createWindowsTaskFolder,
		removeFolder:    removeWindowsTaskFolderIfEmpty,
		inspectProxy:    inspectProxy,
	}
	return &serviceLifecycle{
		Platform:       platform,
		Store:          &installstate.Store{FS: fsutil.OSFileSystem{}, Roots: roots},
		Executable:     executable,
		Version:        version,
		StatusAttempts: 30,
		StatusInterval: time.Second,
		MutationLocker: installer.FileInstallLocker{FS: fsutil.OSFileSystem{}, StateRoot: roots.State},
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
	return resolveServiceExecutable("")
}

func runWindowsSchtasks(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) >= 3 && strings.EqualFold(args[1], "/TN") {
		switch strings.ToLower(args[0]) {
		case "/run", "/end", "/delete":
			return runWindowsTaskMutation(ctx, strings.ToLower(args[0]), args[2])
		}
	}
	output, err := exec.CommandContext(ctx, "schtasks.exe", args...).CombinedOutput()
	if err == nil {
		return output, nil
	}
	message := strings.TrimSpace(string(output))
	code := uint32(1)
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		code = uint32(exitError.ExitCode())
	}
	return output, windowsTaskCommandError{Code: code, Output: message}
}

func runWindowsTaskMutation(ctx context.Context, action, taskPath string) ([]byte, error) {
	taskFolder, taskName, err := splitWindowsTaskPath(taskPath)
	if err != nil {
		return nil, err
	}
	comTaskFolder := strings.TrimSuffix(taskFolder, `\`)
	var operation string
	switch action {
	case "/run":
		operation = fmt.Sprintf(`$task=$service.GetFolder('%s').GetTask('%s'); $null=$task.Run($null)`, comTaskFolder, taskName)
	case "/end":
		operation = fmt.Sprintf(`$task=$service.GetFolder('%s').GetTask('%s'); $instances=$task.GetInstances(0); for($index=1;$index -le $instances.Count;$index++){ $instances.Item($index).Stop() }`, comTaskFolder, taskName)
	case "/delete":
		operation = fmt.Sprintf(`$service.GetFolder('%s').DeleteTask('%s',0)`, comTaskFolder, taskName)
	default:
		return nil, fmt.Errorf("unsupported Windows task mutation %q", action)
	}
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; $service=New-Object -ComObject 'Schedule.Service'; $service.Connect(); $code=[int64]0; try { %s } catch { $code=[int64]$_.Exception.HResult }; [pscustomobject]@{Code=$code} | ConvertTo-Json -Compress`,
		operation,
	)
	output, err := runWindowsTaskPowerShell(ctx, script)
	if err != nil {
		return nil, err
	}
	var result struct {
		Code int64 `json:"Code"`
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("decode Windows task mutation: %w", err)
	}
	code, err := normaliseWindowsHRESULT(result.Code)
	if err != nil {
		return nil, fmt.Errorf("decode Windows task mutation: %w", err)
	}
	if code != 0 {
		return nil, windowsTaskCommandError{Code: code}
	}
	return nil, nil
}

func queryWindowsTaskState(ctx context.Context, taskPath string) (windowsTaskRuntimeState, error) {
	taskFolder, taskName, err := splitWindowsTaskPath(taskPath)
	if err != nil {
		return windowsTaskRuntimeState{}, err
	}
	comTaskFolder := strings.TrimSuffix(taskFolder, `\`)
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; $service=New-Object -ComObject 'Schedule.Service'; $service.Connect(); try { $registered=$service.GetFolder('%s').GetTask('%s'); $instances=$registered.GetInstances(0); $pids=@(); for($index=1;$index -le $instances.Count;$index++){ $pids += [uint32]$instances.Item($index).EnginePID }; [pscustomobject]@{Exists=$true;Code=[int64]0;Running=([int]$registered.State -eq 4);LastResult=[int64]$registered.LastTaskResult;EnginePIDs=@($pids);SecurityDescriptor=[string]$registered.GetSecurityDescriptor(4)} | ConvertTo-Json -Compress } catch { $code=[int64]$_.Exception.HResult; if($code -eq -2147024894){ [pscustomobject]@{Exists=$false;Code=$code;Running=$false;LastResult=[int64]0;EnginePIDs=@();SecurityDescriptor=''} | ConvertTo-Json -Compress } else { throw } }`,
		comTaskFolder,
		taskName,
	)
	output, err := exec.CommandContext(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		return windowsTaskRuntimeState{}, fmt.Errorf("query Windows task runtime: %w: %s", err, message)
	}
	if len(output) > 64<<10 {
		return windowsTaskRuntimeState{}, fmt.Errorf("Windows task runtime output exceeds size limit")
	}
	var state struct {
		Exists             bool     `json:"Exists"`
		Code               int64    `json:"Code"`
		Running            bool     `json:"Running"`
		LastResult         int64    `json:"LastResult"`
		EnginePIDs         []uint32 `json:"EnginePIDs"`
		SecurityDescriptor string   `json:"SecurityDescriptor"`
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return windowsTaskRuntimeState{}, fmt.Errorf("decode Windows task runtime: %w", err)
	}
	code, err := normaliseWindowsHRESULT(state.Code)
	if err != nil {
		return windowsTaskRuntimeState{}, fmt.Errorf("decode Windows task runtime code: %w", err)
	}
	if !state.Exists {
		return windowsTaskRuntimeState{}, windowsTaskCommandError{Code: code}
	}
	lastResult, err := normaliseWindowsHRESULT(state.LastResult)
	if err != nil {
		return windowsTaskRuntimeState{}, fmt.Errorf("decode Windows task last result: %w", err)
	}
	return windowsTaskRuntimeState{
		Running:            state.Running,
		LastResult:         lastResult,
		HasLastResult:      true,
		EnginePIDs:         append([]uint32(nil), state.EnginePIDs...),
		SecurityDescriptor: state.SecurityDescriptor,
	}, nil
}

func normaliseWindowsHRESULT(value int64) (uint32, error) {
	if value < -1<<31 || value > 1<<32-1 {
		return 0, fmt.Errorf("Windows HRESULT %d is outside 32-bit range", value)
	}
	return uint32(value), nil
}

func queryWindowsTaskFolderState(ctx context.Context) (windowsTaskFolderState, error) {
	script := `$ErrorActionPreference='Stop'; $service=New-Object -ComObject 'Schedule.Service'; $service.Connect(); try { $folder=$service.GetFolder('\cq'); [pscustomobject]@{Exists=$true;SecurityDescriptor=[string]$folder.GetSecurityDescriptor(4)} | ConvertTo-Json -Compress } catch { if ($_.Exception.HResult -eq -2147024894) { [pscustomobject]@{Exists=$false;SecurityDescriptor=''} | ConvertTo-Json -Compress } else { throw } }`
	output, err := runWindowsTaskPowerShell(ctx, script)
	if err != nil {
		return windowsTaskFolderState{}, fmt.Errorf("query Windows task folder: %w", err)
	}
	var state windowsTaskFolderState
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return windowsTaskFolderState{}, fmt.Errorf("decode Windows task folder: %w", err)
	}
	return state, nil
}

func createWindowsTaskFolder(ctx context.Context, securityDescriptor string) error {
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; $service=New-Object -ComObject 'Schedule.Service'; $service.Connect(); $root=$service.GetFolder('\'); try { $null=$service.GetFolder('\cq') } catch { if ($_.Exception.HResult -ne -2147024894) { throw }; $null=$root.CreateFolder('cq','%s') }`,
		securityDescriptor,
	)
	_, err := runWindowsTaskPowerShell(ctx, script)
	if err != nil {
		return fmt.Errorf("create Windows task folder: %w", err)
	}
	return nil
}

func removeWindowsTaskFolderIfEmpty(ctx context.Context) error {
	script := `$ErrorActionPreference='Stop'; $service=New-Object -ComObject 'Schedule.Service'; $service.Connect(); $root=$service.GetFolder('\'); $folder=$null; try { $folder=$service.GetFolder('\cq') } catch { if ($_.Exception.HResult -ne -2147024894) { throw } }; if ($null -ne $folder -and $folder.GetTasks(0).Count -eq 0 -and $folder.GetFolders(0).Count -eq 0) { $root.DeleteFolder('cq',0) }; exit 0`
	_, err := runWindowsTaskPowerShell(ctx, script)
	if err != nil {
		return fmt.Errorf("remove empty Windows task folder: %w", err)
	}
	return nil
}

func runWindowsTaskPowerShell(ctx context.Context, script string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if len(output) > 64<<10 {
		return nil, fmt.Errorf("Windows Task Scheduler output exceeds size limit")
	}
	if err != nil {
		return nil, fmt.Errorf("PowerShell failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return output, nil
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
	lifecycle, err := defaultWindowsServiceLifecycle("")
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
	lifecycle, err := defaultWindowsServiceLifecycle("")
	if err != nil {
		return err
	}
	return lifecycle.Platform.RemoveRefresh(context.Background())
}

func installProxyAgent() error {
	lifecycle, err := defaultWindowsServiceLifecycle("")
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
	lifecycle, err := defaultWindowsServiceLifecycle("")
	if err != nil {
		return err
	}
	return lifecycle.Platform.RemoveProxy(context.Background())
}

func restartProxyAgent() error {
	lifecycle, err := defaultWindowsServiceLifecycle("")
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
