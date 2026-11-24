//go:build windows

package dots

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func dotsProcessIDs(ctx context.Context) ([]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	var currentSession uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &currentSession); err != nil {
		return nil, err
	}
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, err
	}
	var pids []int
	for {
		name := windows.UTF16ToString(entry.ExeFile[:])
		if strings.EqualFold(name, "ChatGPT.exe") || strings.EqualFold(name, "cq-dots-runtime.exe") {
			var session uint32
			if windows.ProcessIdToSessionId(entry.ProcessID, &session) == nil && session == currentSession {
				pids = append(pids, int(entry.ProcessID))
			}
		}
		err = windows.Process32Next(snapshot, &entry)
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return pids, ctx.Err()
		}
		if err != nil {
			return nil, err
		}
	}
}

func windowsOwnedProcess(pid int, access uint32) (windows.Handle, error) {
	if pid <= 0 {
		return 0, errors.New("invalid Dots process identity")
	}
	var currentSession, session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &currentSession); err != nil {
		return 0, err
	}
	if err := windows.ProcessIdToSessionId(uint32(pid), &session); err != nil || session != currentSession {
		return 0, errors.New("Dots process session differs")
	}
	process, err := windows.OpenProcess(access|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		windows.CloseHandle(process)
		return 0, err
	}
	defer token.Close()
	owner, err := token.GetTokenUser()
	if err != nil {
		windows.CloseHandle(process)
		return 0, err
	}
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || !owner.User.Sid.Equals(current.User.Sid) {
		windows.CloseHandle(process)
		return 0, errors.New("Dots process owner differs")
	}
	return process, nil
}

func windowsProcessImage(process windows.Handle) (string, error) {
	buffer := make([]uint16, 32768)
	length := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &length); err != nil {
		return "", err
	}
	if length == 0 || int(length) > len(buffer) {
		return "", errors.New("Dots process image invalid")
	}
	return filepath.Clean(windows.UTF16ToString(buffer[:length])), nil
}

func windowsProcessCommandLine(process windows.Handle) ([]string, error) {
	words := make([]uint64, 4096/8)
	buffer := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)
	for {
		var needed uint32
		err := windows.NtQueryInformationProcess(process, windows.ProcessCommandLineInformation, unsafe.Pointer(&buffer[0]), uint32(len(buffer)), &needed)
		if errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) && needed > uint32(len(buffer)) && needed <= 1<<20 {
			words = make([]uint64, (needed+7)/8)
			buffer = unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)
			continue
		}
		if err != nil {
			return nil, err
		}
		break
	}
	value := (*windows.NTUnicodeString)(unsafe.Pointer(&buffer[0]))
	start := uintptr(unsafe.Pointer(&buffer[0]))
	address := uintptr(unsafe.Pointer(value.Buffer))
	length := uintptr(value.Length)
	if length == 0 || length&1 != 0 || length > 1<<20 || address < start || address > start+uintptr(len(buffer)) || length > start+uintptr(len(buffer))-address {
		return nil, errors.New("Dots process command line invalid")
	}
	args, err := windows.DecomposeCommandLine(windows.UTF16ToString(unsafe.Slice(value.Buffer, value.Length/2)))
	if err != nil || len(args) == 0 || len(args) > 4096 {
		return nil, errors.New("Dots process command line invalid")
	}
	return args, nil
}

func dotsProcessArguments(ctx context.Context, pid int) (int, []string, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	process, err := windowsOwnedProcess(pid, windows.PROCESS_VM_READ)
	if err != nil {
		return 0, nil, err
	}
	defer windows.CloseHandle(process)
	image, err := windowsProcessImage(process)
	if err != nil {
		return 0, nil, err
	}
	args, err := windowsProcessCommandLine(process)
	if err != nil || !strings.EqualFold(filepath.Base(args[0]), filepath.Base(image)) {
		return 0, nil, errors.New("Dots process command line differs from its image")
	}
	args[0] = image
	return os.Getuid(), args, nil
}

func dotsProcessCWD(context.Context, int) (string, error) {
	// Windows helper identity uses the retained executable path, current-user
	// token and exact role/root arguments; it does not infer a working directory.
	return "", nil
}

func dotsParentPID(ctx context.Context, pid int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return 0, err
	}
	for {
		if entry.ProcessID == uint32(pid) {
			return int(entry.ParentProcessID), nil
		}
		err = windows.Process32Next(snapshot, &entry)
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return 0, os.ErrProcessDone
		}
		if err != nil {
			return 0, err
		}
	}
}

func dotsTerminate(pid int) error {
	return errors.New("Windows Dots termination requires an installation boundary")
}

func windowsStoppableRole(s *Settings, pid int, image string, args []string) string {
	if s == nil || len(args) == 0 || !strings.EqualFold(filepath.Base(args[0]), filepath.Base(image)) {
		return ""
	}
	if dotsSamePath(image, helperExecutable(s)) && len(args) >= 4 && args[1] == "__dots" && helperRole(args[2]) && args[3] == filepath.Dir(s.RuntimeRoot) {
		return "helper"
	}
	if s.IntegrationMode != "native" || !dotsSamePath(image, nativeAppExecutable(s)) {
		return ""
	}
	verified := append([]string(nil), args...)
	verified[0] = image
	if runtimeRole(s, pid, os.Getuid(), verified, "", nil) == "app" {
		return "app"
	}
	return ""
}

func dotsTerminateForSettings(s *Settings, pid int) error {
	if s == nil {
		return errors.New("Dots settings unavailable")
	}
	process, err := windowsOwnedProcess(pid, windows.PROCESS_TERMINATE)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	image, err := windowsProcessImage(process)
	if err != nil {
		return err
	}
	args, err := windowsProcessCommandLine(process)
	if err != nil {
		return err
	}
	role := windowsStoppableRole(s, pid, image, args)
	if role == "" {
		return errors.New("refusing to terminate a process outside this Dots runtime")
	}
	if role == "app" {
		data, err := readPrivate(filepath.Join(s.RuntimeRoot, nativeStateName), s.RuntimeRoot, true)
		var state struct {
			BrokerPID int `json:"broker_pid"`
		}
		if err != nil || json.Unmarshal(data, &state) != nil || state.BrokerPID <= 0 {
			return errors.New("refusing to terminate a Dots app without its broker identity")
		}
		parent, err := dotsParentPID(context.Background(), pid)
		if err != nil || parent != state.BrokerPID {
			return errors.New("refusing to terminate a Dots app with changed parent")
		}
		uid, brokerArgs, err := dotsProcessArguments(context.Background(), state.BrokerPID)
		if err != nil || !nativeBrokerMatches(s, uid, brokerArgs, "") {
			return errors.New("refusing to terminate a Dots app with changed broker")
		}
	}
	if err := windows.TerminateProcess(process, 1); errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return os.ErrProcessDone
	} else {
		return err
	}
}

// SpawnHelper starts only this installation's staged executable. Each worker
// receives a new log created relative to the retained private directory.
func SpawnHelper(s *Settings, executable, role string, args, env []string) (*os.Process, error) {
	if s == nil || !helperRole(role) || executable != helperExecutable(s) {
		return nil, errors.New("invalid Windows Dots helper identity")
	}
	if err := privateDirectory(s.RuntimeRoot); err != nil {
		return nil, err
	}
	if err := ValidateRuntimePaths(s.RuntimeRoot); err != nil {
		return nil, err
	}
	if err := windowsValidatePrivateFile(executable); err != nil {
		return nil, err
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	logPath := filepath.Join(s.RuntimeRoot, "release-"+role+"-"+hex.EncodeToString(random[:])+".log")
	fsys, err := windowsStateFS(logPath)
	if err != nil {
		return nil, err
	}
	directory, err := fsys.OpenSecureDirectory(s.RuntimeRoot)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	log, err := directory.CreateExclusive(filepath.Base(logPath), 0o600)
	if err != nil {
		return nil, err
	}
	command := exec.Command(executable, append([]string{"__dots", role, filepath.Dir(s.RuntimeRoot)}, args...)...)
	command.Dir = s.RuntimeRoot
	command.Env = env
	command.Stdout, command.Stderr = log, log
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := command.Start(); err != nil {
		_ = log.Close()
		return nil, err
	}
	go func() {
		defer func() { _ = recover(); _ = log.Close() }()
		_ = command.Wait()
		_ = log.Sync()
	}()
	return command.Process, nil
}
