package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/userdirs"
)

const agentLabel = "dev.jacobcx.cq.refresh"

func agentPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist"), nil
}

func agentLogPath(logsDir string) string {
	return filepath.Join(logsDir, "refresh.log")
}

func resolveExecutable() (string, error) {
	return resolveServiceExecutable("")
}

func installAgent(interval int) error {
	if interval <= 0 {
		interval = 1800
	}
	roots, err := userdirs.Default()
	if err != nil {
		return err
	}

	exe, err := resolveExecutable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("resolve absolute executable: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	exe = filepath.Clean(exe)
	platform := newDarwinCommandServicePlatform(home, roots, exe)
	if err := platform.installRefresh(context.Background(), exe, interval); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "cq: installed LaunchAgent (every %ds)\n", interval)
	fmt.Fprintf(os.Stderr, "cq: plist: %s\n", platform.plistPath(agentLabel))
	fmt.Fprintf(os.Stderr, "cq: log:   %s\n", agentLogPath(roots.Logs))
	return nil
}

// ensureAgent auto-installs the LaunchAgent on first run if not present.
func ensureAgent() {
	path, err := agentPlistPath()
	if err != nil {
		return
	}
	if _, err := os.Stat(path); err == nil {
		return // already installed
	}
	if err := installAgent(1800); err != nil {
		fmt.Fprintf(os.Stderr, "cq: auto-install refresh agent failed: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "cq: to disable: cq agent uninstall\n")
}

func uninstallAgent() error {
	plistPath, err := agentPlistPath()
	if err != nil {
		return err
	}

	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "cq: no LaunchAgent installed\n")
		return nil
	}
	roots, err := userdirs.Default()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	platform := newDarwinCommandServicePlatform(home, roots, "")
	if err := platform.RemoveRefresh(context.Background()); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "cq: uninstalled LaunchAgent\n")
	return nil
}

const darwinRefreshCompletionName = "refresh-completion.json"

type darwinRefreshCompletion struct {
	SchemaVersion int            `json:"schema_version"`
	StartedAt     time.Time      `json:"started_at"`
	CompletedAt   time.Time      `json:"completed_at"`
	ExitCode      int            `json:"exit_code"`
	Executable    string         `json:"executable"`
	Roots         userdirs.Roots `json:"roots"`
}

func init() { serviceRefreshRunner = runDarwinServiceRefresh }
func nativeDarwinHome() (string, error) {
	current, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(current.HomeDir) {
		return "", errServiceUnavailable
	}
	return filepath.Clean(current.HomeDir), nil
}
func darwinServiceEnvironment(home string, roots userdirs.Roots, refresh bool) (map[string]string, error) {
	env := map[string]string{"HOME": home, "XDG_CONFIG_HOME": filepath.Dir(roots.Config), "XDG_CACHE_HOME": filepath.Dir(roots.Cache), "PATH": "/usr/bin:/bin:/usr/sbin:/sbin"}
	resolved, err := (userdirs.Resolver{Getenv: func(key string) string { return env[key] }, UserHomeDir: func() (string, error) { return home, nil }}).Resolve()
	if err != nil || resolved != roots {
		return nil, fmt.Errorf("installed service roots cannot be represented by its environment")
	}
	if refresh {
		env["CQ_SERVICE_REFRESH"] = "1"
	}
	return env, nil
}
func darwinDefinitionRoots(d darwinLaunchAgentDefinition) (userdirs.Roots, error) {
	env := d.EnvironmentVariables
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		if !filepath.IsAbs(env[key]) || filepath.Clean(env[key]) != env[key] {
			return userdirs.Roots{}, errServiceUnavailable
		}
	}
	roots, err := (userdirs.Resolver{Getenv: func(key string) string { return env[key] }, UserHomeDir: func() (string, error) { return env["HOME"], nil }}).Resolve()
	log := "proxy.log"
	if d.Label == agentLabel {
		log = "refresh.log"
	}
	if err != nil || d.StandardErrorPath != filepath.Join(roots.Logs, log) {
		return userdirs.Roots{}, errServiceUnavailable
	}
	return roots, nil
}
func runDarwinServiceRefresh(run func() error) error {
	if os.Getenv("CQ_SERVICE_REFRESH") != "1" {
		return run()
	}
	home, err := nativeDarwinHome()
	if err != nil {
		return errServiceUnavailable
	}
	p := darwinServicePlatform{home: home}
	d, exists, err := p.ownedDefinition(agentLabel)
	if err != nil || !exists {
		return errServiceUnavailable
	}
	exe, err := os.Executable()
	if err != nil || !sameServiceExecutable(exe, d.ProgramArguments[0]) {
		return errServiceUnavailable
	}
	roots, err := darwinDefinitionRoots(d)
	if err != nil {
		return err
	}
	processRoots, err := userdirs.Default()
	if err != nil || roots != processRoots {
		return errServiceUnavailable
	}
	for key, value := range d.EnvironmentVariables {
		if os.Getenv(key) != value {
			return errServiceUnavailable
		}
	}
	return recordDarwinServiceRefresh(d, run, time.Now)
}
func recordDarwinServiceRefresh(d darwinLaunchAgentDefinition, run func() error, now func() time.Time) error {
	roots, err := darwinDefinitionRoots(d)
	if err != nil {
		return err
	}
	// Open the authority before execution and retain it through receipt publication.
	fs := fsutil.OSFileSystem{}
	if err := fsutil.EnsureSecureDirectory(fs, roots.State); err != nil {
		return err
	}
	directory, err := fs.OpenSecureDirectory(roots.State)
	if err != nil {
		return err
	}
	defer directory.Close()
	started := now().UTC()
	runErr := run()
	completed := now().UTC()
	exit := 0
	if runErr != nil {
		exit = 1
	}
	receipt := darwinRefreshCompletion{SchemaVersion: 1, StartedAt: started, CompletedAt: completed, ExitCode: exit, Executable: d.ProgramArguments[0], Roots: roots}
	data, err := json.Marshal(receipt)
	if err != nil {
		return errors.Join(runErr, err)
	}
	if err := fsutil.ValidateSecureDirectoryHandle(fs, directory, roots.State); err != nil {
		return errors.Join(runErr, err)
	}
	err = fsutil.SecureAtomicWriteInDirectoryChecked(fs, directory, darwinRefreshCompletionName, append(data, '\n'), func() error { return fsutil.ValidateSecureDirectoryHandle(fs, directory, roots.State) })
	return errors.Join(runErr, err)
}
func readDarwinRefreshCompletion(d darwinLaunchAgentDefinition, now time.Time) (darwinRefreshCompletion, error) {
	var receipt darwinRefreshCompletion
	roots, err := darwinDefinitionRoots(d)
	if err != nil {
		return receipt, err
	}
	fs := fsutil.OSFileSystem{}
	data, err := fsutil.ReadSecureFile(fs, filepath.Join(roots.State, darwinRefreshCompletionName), 4096)
	if err != nil {
		return receipt, err
	}
	if err := rejectLegacyMaintenanceDuplicateJSONKeys(json.NewDecoder(strings.NewReader(string(data)))); err != nil {
		return receipt, errServiceUnavailable
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, errServiceUnavailable
	}
	// Exact canonical encoding rejects nulls/omissions, case aliases and unknown or
	// duplicate fields, including nested root fields. Whitespace remains harmless.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || len(raw) != 6 {
		return receipt, errServiceUnavailable
	}
	for _, key := range []string{"schema_version", "started_at", "completed_at", "exit_code", "executable", "roots"} {
		value, ok := raw[key]
		if !ok || strings.TrimSpace(string(value)) == "null" {
			return receipt, errServiceUnavailable
		}
	}
	var rootFields map[string]json.RawMessage
	if err := json.Unmarshal(raw["roots"], &rootFields); err != nil || len(rootFields) != 5 {
		return receipt, errServiceUnavailable
	}
	for _, key := range []string{"Config", "State", "Cache", "Runtime", "Logs"} {
		value, ok := rootFields[key]
		if !ok || strings.TrimSpace(string(value)) == "null" {
			return receipt, errServiceUnavailable
		}
	}
	if receipt.SchemaVersion != 1 || receipt.StartedAt.IsZero() || receipt.CompletedAt.IsZero() || receipt.CompletedAt.Before(receipt.StartedAt) || receipt.CompletedAt.After(now) || receipt.ExitCode < 0 || receipt.Executable != d.ProgramArguments[0] || receipt.Roots != roots {
		return receipt, errServiceUnavailable
	}
	for _, key := range []string{"started_at", "completed_at"} {
		var value string
		_ = json.Unmarshal(raw[key], &value)
		if !strings.HasSuffix(value, "Z") {
			return receipt, errServiceUnavailable
		}
	}
	return receipt, nil
}
func runDarwinRefreshOnce(ctx context.Context, d darwinLaunchAgentDefinition) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := darwinDefinitionRoots(d); err != nil {
		return err
	}
	if d.Label != agentLabel || len(d.ProgramArguments) != 2 || d.ProgramArguments[1] != "refresh" || d.EnvironmentVariables["CQ_SERVICE_REFRESH"] != "1" {
		return errServiceUnavailable
	}
	if err := validateDarwinOwnedExecutable(d.ProgramArguments[0]); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, d.ProgramArguments[0], d.ProgramArguments[1:]...)
	// The child's environment is solely the installed definition, never the shell.
	keys := make([]string, 0, len(d.EnvironmentVariables))
	for key := range d.EnvironmentVariables {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		command.Env = append(command.Env, key+"="+d.EnvironmentVariables[key])
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	// Preserve the installed log binding without mixing diagnostics into CLI JSON.
	log, err := openDarwinRefreshLog(d.StandardErrorPath)
	if err != nil {
		return err
	}
	defer log.Close()
	command.Stderr = log
	err = command.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func openDarwinRefreshLog(path string) (*os.File, error) {
	fs := fsutil.OSFileSystem{}
	if err := fsutil.EnsureSecureDirectory(fs, filepath.Dir(path)); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	owner, ok := fs.FileOwnerUID(info)
	identity, identityOK := fs.FileIdentity(info)
	if !ok || owner != fs.EffectiveUID() || !identityOK || identity.Links != 1 || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		file.Close()
		return nil, errServiceUnavailable
	}
	return file, nil
}
