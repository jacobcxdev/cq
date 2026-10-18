package dots

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// RuntimeProcess is an owned process belonging to this installation.
type RuntimeProcess struct {
	PID  int
	Role string
}

var legacyRoles = map[string]string{
	"probe.py": "probe", "realtime_probe.py": "realtime",
	"room_coordination.py": "relay", "native_debug.py": "native-broker",
	"account_watch.py": "account-watch", "secondary_executors.py": "executors",
	"local_executor.py": "local-executor", "native_null_executor.py": "native-executor",
}

func helperRole(role string) bool {
	return slices.Contains([]string{"probe", "realtime", "relay", "native-broker", "account-watch", "executors", "local-executor", "native-executor", "desktop-recover"}, role)
}

type processOperations struct {
	list      func(context.Context) ([]int, error)
	arguments func(context.Context, int) (int, []string, error)
	cwd       func(context.Context, int) (string, error)
	signal    func(int) error
}

func defaultProcessOperations() processOperations {
	return processOperations{dotsProcessIDs, dotsProcessArguments, dotsProcessCWD, dotsTerminate}
}

func runtimeExecutableMatches(arguments []string, executable string) bool {
	if len(arguments) == 0 {
		return false
	}
	name := filepath.Base(arguments[0])
	if name != "cq-dots-runtime" && name != "ChatGPT" {
		return true
	}
	// The staged pathname can be replaced while the old mapped inode is alive.
	// Linux reports that old inode with this suffix; its original path is retained.
	executable = strings.TrimSuffix(executable, " (deleted)")
	expected, err := CanonicalPath(arguments[0])
	if err != nil {
		return false
	}
	actual, err := CanonicalPath(executable)
	return err == nil && expected == actual
}

func normalAppArguments(s *Settings, uid int, args []string) bool {
	if uid != os.Getuid() || len(args) == 0 || args[0] != filepath.Join(s.AppPath, "Contents", "MacOS", "ChatGPT") {
		return false
	}
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "--user-data-dir") {
			return false
		}
	}
	return true
}

// NormalAppRunning identifies the ordinary profile without attaching to it.
func NormalAppRunning(ctx context.Context, s *Settings) (bool, error) {
	return normalAppRunning(ctx, s, defaultProcessOperations())
}

func normalAppRunning(ctx context.Context, s *Settings, ops processOperations) (bool, error) {
	if s == nil {
		return false, errors.New("Dots settings unavailable")
	}
	pids, err := ops.list(ctx)
	if err != nil {
		return false, err
	}
	for _, pid := range pids {
		uid, args, err := ops.arguments(ctx, pid)
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if err == nil && normalAppArguments(s, uid, args) {
			return true, nil
		}
	}
	return false, nil
}

func pythonExecutables(s *Settings) map[string]bool {
	configured := filepath.Join(s.RuntimeRoot, "venv", "bin", "python")
	values := map[string]bool{configured: true}
	resolved, err := filepath.EvalSymlinks(configured)
	if err != nil {
		return values
	}
	version := filepath.Dir(filepath.Dir(resolved))
	if filepath.Base(resolved) == "python3.14" && filepath.Base(filepath.Dir(resolved)) == "bin" && filepath.Base(version) == "3.14" && filepath.Base(filepath.Dir(version)) == "Versions" && filepath.Base(filepath.Dir(filepath.Dir(version))) == "Python.framework" {
		framework := filepath.Join(version, "Resources", "Python.app", "Contents", "MacOS", "Python")
		if info, err := os.Stat(framework); err == nil && info.Mode().IsRegular() {
			values[framework] = true
		}
	}
	return values
}

func runtimeRole(s *Settings, pid, uid int, args []string, cwd string, python map[string]bool) string {
	if uid != os.Getuid() || len(args) == 0 {
		return ""
	}
	root := s.RuntimeRoot
	if args[0] == filepath.Join(s.AppPath, "Contents", "MacOS", "ChatGPT") {
		if s.IntegrationMode == "native" {
			data, err := readPrivate(filepath.Join(root, "native-debug.json"), root, true)
			var state struct {
				AppPID int `json:"app_pid"`
			}
			if err != nil || json.Unmarshal(data, &state) != nil || state.AppPID != pid || !normalAppArguments(s, uid, args) {
				return ""
			}
			var runtimeFlags, debugFlags []string
			for _, arg := range args[1:] {
				if strings.HasPrefix(arg, "--cq-dots-runtime=") {
					runtimeFlags = append(runtimeFlags, arg)
				}
				if strings.HasPrefix(arg, "--remote-debugging-") {
					debugFlags = append(debugFlags, arg)
				}
			}
			if slices.Equal(runtimeFlags, []string{"--cq-dots-runtime=" + root}) && slices.Equal(debugFlags, []string{"--remote-debugging-pipe"}) {
				return "app"
			}
			return ""
		}
		var directories []string
		for _, arg := range args[1:] {
			if strings.HasPrefix(arg, "--user-data-dir=") {
				directories = append(directories, strings.TrimPrefix(arg, "--user-data-dir="))
			}
		}
		if len(directories) == 1 && directories[0] == filepath.Join(root, "user-data") {
			return "app"
		}
		return ""
	}
	if cwd != root {
		return ""
	}
	if args[0] == filepath.Join(root, "cq-dots-runtime") && len(args) >= 4 && args[1] == "__dots" && helperRole(args[2]) && args[2] != "desktop-recover" && args[3] == filepath.Dir(root) {
		return args[2]
	}
	if !python[args[0]] {
		return ""
	}
	rest := args[1:]
	if len(rest) > 0 && rest[0] == "-u" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return ""
	}
	name := filepath.Base(rest[0])
	role := legacyRoles[name]
	if role == "" || (rest[0] != name && rest[0] != filepath.Join(root, name)) {
		return ""
	}
	info, err := os.Lstat(filepath.Join(root, name))
	if err != nil || !info.Mode().IsRegular() || !owned(info) {
		return ""
	}
	return role
}

// RuntimeProcesses lists exact owned app and helper identities, including legacy Python workers.
func RuntimeProcesses(ctx context.Context, s *Settings) ([]RuntimeProcess, error) {
	return runtimeProcesses(ctx, s, defaultProcessOperations())
}

func runtimeProcesses(ctx context.Context, s *Settings, ops processOperations) ([]RuntimeProcess, error) {
	if s == nil || !cleanAbsolute(s.RuntimeRoot) {
		return nil, errors.New("Dots runtime unavailable")
	}
	if err := ValidateRuntimePaths(s.RuntimeRoot); err != nil {
		return nil, err
	}
	pids, err := ops.list(ctx)
	if err != nil {
		return nil, err
	}
	python := pythonExecutables(s)
	rows := make([]RuntimeProcess, 0)
	app := filepath.Join(s.AppPath, "Contents", "MacOS", "ChatGPT")
	for _, pid := range pids {
		uid, args, err := ops.arguments(ctx, pid)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil || uid != os.Getuid() || len(args) == 0 {
			continue
		}
		if args[0] != app && args[0] != filepath.Join(s.RuntimeRoot, "cq-dots-runtime") && !python[args[0]] {
			continue
		}
		cwd := ""
		if args[0] != app {
			cwd, err = ops.cwd(ctx, pid)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err != nil {
				continue
			}
		}
		if role := runtimeRole(s, pid, uid, args, cwd, python); role != "" {
			rows = append(rows, RuntimeProcess{pid, role})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].PID < rows[j].PID })
	return rows, nil
}

// StopRuntime refuses to interrupt any ordinary app and never escalates beyond SIGTERM.
func StopRuntime(ctx context.Context, s *Settings, preserveWatcher bool) error {
	return stopRuntime(ctx, s, preserveWatcher, defaultProcessOperations())
}

func stopRuntime(ctx context.Context, s *Settings, preserveWatcher bool, ops processOperations) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	checkApp := func() error {
		running, err := normalAppRunning(ctx, s, ops)
		if err != nil {
			return err
		}
		if running {
			return errors.New("Quit the normal desktop app before stopping Dots; CQ will not interrupt its tasks")
		}
		return nil
	}
	if err := checkApp(); err != nil {
		return err
	}
	rows, err := runtimeProcesses(ctx, s, ops)
	if err != nil {
		return err
	}
	// An isolated app must finish exiting before its pipe broker can be stopped.
	for _, appPhase := range []bool{true, false} {
		for _, row := range rows {
			if (row.Role == "app") != appPhase || preserveWatcher && row.Role == "account-watch" {
				continue
			}
			if err := checkApp(); err != nil {
				return err
			}
			current, err := runtimeProcesses(ctx, s, ops)
			if err != nil {
				return err
			}
			if !slices.Contains(current, row) {
				continue
			}
			if err := ops.signal(row.PID); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return fmt.Errorf("Could not stop owned Dots helper: %w", err)
			}
		}
		for {
			current, err := runtimeProcesses(ctx, s, ops)
			if err != nil {
				return err
			}
			remaining := false
			for _, row := range current {
				if (row.Role == "app") == appPhase && !(preserveWatcher && row.Role == "account-watch") {
					remaining = true
				}
			}
			if !remaining {
				break
			}
			if err := waitContext(ctx, 100*time.Millisecond); err != nil {
				return errors.New("Dots processes have not stopped; no forced termination attempted")
			}
		}
	}
	return nil
}

// StageExecutable atomically replaces the staged path without changing running mapped inodes.
func StageExecutable(source, runtimeRoot string) (string, error) {
	if !cleanAbsolute(source) || !cleanAbsolute(runtimeRoot) {
		return "", errors.New("Dots executable paths must be absolute")
	}
	if err := privateDirectory(runtimeRoot); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", err
	}
	fs := fsutil.OSFileSystem{}
	input, err := fs.OpenNoFollow(resolved)
	if err != nil {
		return "", err
	}
	defer input.Close()
	before, err := input.Stat()
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0o111 == 0 || before.Mode().Perm()&0o022 != 0 {
		return "", errors.New("Dots executable source must be a regular executable without shared writes")
	}
	directory, err := fs.OpenSecureDirectory(runtimeRoot)
	if err != nil {
		return "", err
	}
	defer directory.Close()
	parent, err := directory.Stat()
	if err != nil {
		return "", err
	}
	name := "cq-dots-runtime"
	destination := filepath.Join(runtimeRoot, name)
	if existing, err := os.Lstat(destination); err == nil {
		if !existing.Mode().IsRegular() || !owned(existing) || existing.Mode().Perm() != 0o700 {
			return "", errors.New("Staged Dots executable must be owned and mode 0700")
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	temporary := ".cq-dots-runtime-" + hex.EncodeToString(random[:])
	output, err := directory.CreateExclusive(temporary, 0o700)
	if err != nil {
		return "", err
	}
	defer directory.Remove(temporary)
	if _, err = io.Copy(output, input); err != nil {
		output.Close()
		return "", err
	}
	after, err := input.Stat()
	if err != nil {
		output.Close()
		return "", err
	}
	if revision(before) != revision(after) {
		output.Close()
		return "", errors.New("Dots executable changed during staging")
	}
	if err = output.Sync(); err != nil {
		output.Close()
		return "", err
	}
	info, err := output.(fsutil.DurableFileInspector).Stat()
	if err != nil {
		output.Close()
		return "", err
	}
	identity, ok := fs.FileIdentity(info)
	if !ok {
		output.Close()
		return "", fsutil.ErrSecureCapabilityUnavailable
	}
	if err = output.Close(); err != nil {
		return "", err
	}
	current, err := os.Lstat(runtimeRoot)
	if err != nil || !os.SameFile(parent, current) {
		return "", errors.New("Dots runtime directory changed during staging")
	}
	if err = directory.(fsutil.IdentityBoundRenamer).RenameChecked(temporary, name, identity); err != nil {
		return "", err
	}
	if err = directory.Sync(); err != nil {
		return "", err
	}
	return destination, nil
}
