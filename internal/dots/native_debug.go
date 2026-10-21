package dots

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const nativeSocketName = "native-debug.sock"
const nativeStateName = "native-debug.json"

func nativeRoot(root string) error {
	if err := privateDirectory(root); err != nil {
		return err
	}
	if len(filepath.Join(root, nativeSocketName)) > 103 {
		return errors.New("native debugger socket path is too long")
	}
	return nil
}
func nativeReceive(c net.Conn, limit int) (map[string]any, error) {
	reader := bufio.NewReaderSize(c, limit+1)
	data, err := reader.ReadSlice('\n')
	if err != nil || len(data) > limit || reader.Buffered() != 0 {
		return nil, errors.New("native debugger frame invalid or too large")
	}
	var v map[string]any
	if json.Unmarshal(data, &v) != nil || v == nil {
		return nil, errors.New("native debugger frame invalid")
	}
	return v, nil
}
func nativeSend(c net.Conn, v any, limit int) error {
	data, err := json.Marshal(v)
	if err != nil || len(data)+1 > limit {
		return errors.New("native debugger frame too large")
	}
	data = append(data, '\n')
	for len(data) > 0 {
		n, e := c.Write(data)
		if e != nil {
			return e
		}
		data = data[n:]
	}
	return nil
}
func nativeRequest(ctx context.Context, s *Settings, request map[string]any) (map[string]any, error) {
	if err := nativeRoot(s.RuntimeRoot); err != nil {
		return nil, err
	}
	path := filepath.Join(s.RuntimeRoot, nativeSocketName)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || !owned(info) || info.Mode().Perm() != 0o600 {
		return nil, errors.New("native debugger socket must be owned and mode 0600")
	}
	ctx, cancel := context.WithTimeout(ctx, nativeTimeout+5*time.Second)
	defer cancel()
	raw, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, errors.New("native debugger unavailable")
	}
	defer raw.Close()
	c, ok := raw.(*net.UnixConn)
	if !ok {
		return nil, errors.New("native debugger unavailable")
	}
	deadline, _ := ctx.Deadline()
	_ = c.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Now()) })
	defer stop()
	uid, err := nativePeerUID(c)
	if err != nil || uid != uint32(os.Getuid()) {
		return nil, errors.New("native debugger peer mismatch")
	}
	if err = nativeSend(c, request, nativeMaxRequest); err != nil {
		return nil, err
	}
	response, err := nativeReceive(c, nativeMaxFrame)
	if err != nil {
		return nil, err
	}
	if response["ok"] != true {
		return nil, errors.New("native debugger request failed")
	}
	return response, nil
}

// NativeIdentity probes the private endpoint without evaluating renderer code.
func NativeIdentity(ctx context.Context, s *Settings) (map[string]int, error) {
	response, err := nativeRequest(ctx, s, map[string]any{"operation": "identity"})
	if err != nil {
		return nil, err
	}
	result := map[string]int{}
	for _, key := range []string{"app_pid", "broker_pid"} {
		value, ok := response[key].(float64)
		if !ok || value <= 0 || float64(int(value)) != value {
			return nil, errors.New("native debugger identity invalid")
		}
		result[key] = int(value)
	}
	return result, nil
}

type nativeBroker struct {
	listener *net.UnixListener
	path     string
	identity os.FileInfo
	appPID   int
	pipe     *nativePipe
}

func newNativeBroker(root string) (*nativeBroker, error) {
	if err := nativeRoot(root); err != nil {
		return nil, err
	}
	path := filepath.Join(root, nativeSocketName)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return nil, errors.New("native debugger socket already exists")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	b := &nativeBroker{listener: listener, path: path}
	b.identity, err = os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	if err = os.Chmod(path, 0o600); err != nil {
		b.close()
		return nil, err
	}
	return b, nil
}
func (b *nativeBroker) close() {
	_ = b.listener.Close()
	if info, err := os.Lstat(b.path); err == nil && os.SameFile(info, b.identity) {
		_ = os.Remove(b.path)
	}
}
func (b *nativeBroker) once(ctx context.Context) error {
	_ = b.listener.SetDeadline(time.Now().Add(250 * time.Millisecond))
	c, err := b.listener.AcceptUnix()
	if err != nil {
		if e, ok := err.(net.Error); ok && e.Timeout() {
			return nil
		}
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(nativeTimeout + 5*time.Second))
	response := map[string]any{"ok": false, "error": "Native debugger request failed"}
	uid, err := nativePeerUID(c)
	if err == nil && uid == uint32(os.Getuid()) {
		request, e := nativeReceive(c, nativeMaxRequest)
		if e == nil {
			if request["operation"] == "identity" && len(request) == 1 {
				response = map[string]any{"ok": true, "app_pid": b.appPID, "broker_pid": os.Getpid()}
			} else {
				operation, _ := request["operation"].(string)
				pid, valid := request["app_pid"].(float64)
				expression, _ := request["expression"].(string)
				if len(request) == 3 && valid && pid == float64(b.appPID) && expression != "" && (operation == "evaluate" || operation == "evaluate_all") {
					values, e := b.pipe.evaluate(ctx, expression, operation == "evaluate_all")
					if e == nil {
						var value any = values
						if operation == "evaluate" {
							value = values[0]
						}
						response = map[string]any{"ok": true, "app_pid": b.appPID, "value": value}
					}
				}
			}
		}
	}
	_ = nativeSend(c, response, nativeMaxFrame)
	return nil
}

// LaunchNativeBroker owns the app's inherited CDP pipe until the exact app exits.
// Cancellation or SIGTERM closes IPC, then waits without terminating the app.
// Run it in the detached native-broker helper, never in a short-lived CLI process.
func LaunchNativeBroker(ctx context.Context, s *Settings, executable string, args, env []string) error {
	if executable != filepath.Join(s.AppPath, "Contents", "MacOS", "ChatGPT") {
		return errors.New("native debugger requires the configured official executable")
	}
	if err := AppCheck(s); err != nil {
		return err
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--remote-debugging-") {
			return errors.New("native debugger flags are broker-owned")
		}
	}
	var markers []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "--user-data-dir") {
			return errors.New("native debugger requires the normal desktop profile")
		}
		if strings.HasPrefix(arg, "--cq-dots-runtime=") {
			markers = append(markers, arg)
		}
	}
	if !slices.Equal(markers, []string{"--cq-dots-runtime=" + s.RuntimeRoot}) {
		return errors.New("native debugger runtime marker mismatch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := newNativeBroker(s.RuntimeRoot)
	if err != nil {
		return err
	}
	defer b.close()
	childRead, parentWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	defer parentWrite.Close()
	parentRead, childWrite, err := os.Pipe()
	if err != nil {
		childRead.Close()
		return err
	}
	defer parentRead.Close()
	args = append(append([]string{}, args...), "--remote-debugging-pipe")
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	app, err := startNativeApp(executable, args, env, childRead, childWrite)
	_ = childRead.Close()
	_ = childWrite.Close()
	if err != nil {
		return err
	}
	b.appPID = app.Pid
	b.pipe = newNativePipe(parentRead, parentWrite)
	// Reap exactly this child. This goroutine never cancels or kills it.
	exited := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				exited <- errors.New("native app wait failed")
			}
		}()
		_, err := app.Wait()
		exited <- err
	}()
	statePath := filepath.Join(s.RuntimeRoot, nativeStateName)
	stateErr := SaveJSON(statePath, map[string]any{"app_pid": b.appPID, "broker_pid": os.Getpid()})
	stateInfo, _ := os.Lstat(statePath)
	defer func() {
		if stateInfo != nil {
			if info, e := os.Lstat(statePath); e == nil && os.SameFile(info, stateInfo) {
				_ = os.Remove(statePath)
			}
		}
	}()
	if stateErr != nil {
		b.close()
		<-exited
		return stateErr
	}
	for {
		select {
		case <-exited:
			return nil
		case <-ctx.Done():
			b.close()
			<-exited
			return nil
		case <-signals:
			b.close()
			<-exited
			return nil
		default:
		}
		if err = b.once(ctx); err != nil {
			b.close()
			<-exited
			return errors.New("native debugger IPC unavailable")
		}
	}
}

// nativeBrokerMatches also recognizes an already-running legacy broker. This
// permits read-only status during migration without launching Python or restarting
// the user's app. Both implementations retain the same exact process boundary.
func nativeBrokerMatches(s *Settings, uid int, args []string, cwd string) bool {
	if uid != os.Getuid() || cwd != s.RuntimeRoot || len(args) == 0 {
		return false
	}
	if args[0] == filepath.Join(s.RuntimeRoot, "cq-dots-runtime") {
		return len(args) >= 4 && slices.Equal(args[1:4], []string{"__dots", "native-broker", filepath.Dir(s.RuntimeRoot)})
	}
	return len(args) >= 3 && pythonExecutables(s)[args[0]] && slices.Equal(args[1:3], []string{"-u", filepath.Join(s.RuntimeRoot, "native_debug.py")})
}

func nativeProcessPID(ctx context.Context, s *Settings, port int) (int, error) {
	pids, err := dotsProcessIDs(ctx)
	if err != nil {
		return 0, err
	}
	var matches []int
	for _, pid := range pids {
		uid, args, e := dotsProcessArguments(ctx, pid)
		if e != nil || uid != os.Getuid() || len(args) == 0 || args[0] != filepath.Join(s.AppPath, "Contents", "MacOS", "ChatGPT") {
			continue
		}
		var data, debug, markers []string
		for _, arg := range args[1:] {
			if strings.HasPrefix(arg, "--user-data-dir") {
				data = append(data, arg)
			}
			if strings.HasPrefix(arg, "--remote-debugging-") {
				debug = append(debug, arg)
			}
			if strings.HasPrefix(arg, "--cq-dots-runtime=") {
				markers = append(markers, arg)
			}
		}
		if s.IntegrationMode == "native" {
			if len(data) == 0 && slices.Equal(debug, []string{"--remote-debugging-pipe"}) && slices.Equal(markers, []string{"--cq-dots-runtime=" + s.RuntimeRoot}) {
				matches = append(matches, pid)
			}
		} else if slices.Equal(data, []string{"--user-data-dir=" + filepath.Join(s.RuntimeRoot, "user-data")}) && slices.Contains(debug, "--remote-debugging-port="+strconv.Itoa(port)) {
			matches = append(matches, pid)
		}
	}
	if len(matches) != 1 {
		return 0, errors.New("exact isolated process unavailable")
	}
	pid := matches[0]
	if s.IntegrationMode == "native" {
		identity, e := NativeIdentity(ctx, s)
		if e != nil {
			return 0, e
		}
		if identity["app_pid"] != pid {
			return 0, errors.New("native private renderer app identity changed")
		}
		uid, args, e := dotsProcessArguments(ctx, identity["broker_pid"])
		cwd, cwdErr := dotsProcessCWD(ctx, identity["broker_pid"])
		if e != nil || cwdErr != nil || !nativeBrokerMatches(s, uid, args, cwd) {
			return 0, errors.New("native private renderer broker identity changed")
		}
		parent, e := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(pid), "-o", "ppid=").Output()
		if e != nil || strings.TrimSpace(string(parent)) != strconv.Itoa(identity["broker_pid"]) {
			return 0, errors.New("native private renderer broker identity changed")
		}
		return pid, nil
	}
	owners, e := exec.CommandContext(ctx, "/usr/sbin/lsof", "-t", "-a", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN").Output()
	if e != nil {
		return 0, errors.New("isolated debugger listener ownership changed")
	}
	found := false
	for _, owner := range strings.Fields(string(owners)) {
		other, e := strconv.Atoi(owner)
		if e != nil {
			return 0, errors.New("isolated debugger listener ownership changed")
		}
		if other == pid {
			found = true
			continue
		}
		uid, args, e := dotsProcessArguments(ctx, other)
		parent, pErr := exec.CommandContext(ctx, "/bin/ps", "-p", owner, "-o", "ppid=").Output()
		helper := filepath.Join(s.RuntimeRoot, "codex-home", "computer-use", "Codex Computer Use.app", "Contents", "MacOS", "SkyComputerUseService")
		if e != nil || pErr != nil || uid != os.Getuid() || !slices.Equal(args, []string{helper}) || strings.TrimSpace(string(parent)) != strconv.Itoa(pid) {
			return 0, errors.New("isolated debugger listener ownership changed")
		}
	}
	if !found {
		return 0, errors.New("isolated debugger listener ownership changed")
	}
	return pid, nil
}

// LaunchIsolatedApp runs the configured signed app with only its private profile.
// It deliberately does not tie the child's lifetime to ctx cancellation.
func LaunchIsolatedApp(ctx context.Context, s *Settings, args, env []string) error {
	if s.IntegrationMode != "isolated" {
		return errors.New("isolated app requires isolated integration")
	}
	if err := AppCheck(s); err != nil {
		return err
	}
	if err := ValidateRuntimePaths(s.RuntimeRoot); err != nil {
		return err
	}
	var profiles, debug []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "--user-data-dir") {
			profiles = append(profiles, arg)
		}
		if strings.HasPrefix(arg, "--remote-debugging-") {
			debug = append(debug, arg)
		}
	}
	if !slices.Equal(profiles, []string{"--user-data-dir=" + filepath.Join(s.RuntimeRoot, "user-data")}) {
		return errors.New("isolated app profile mismatch")
	}
	port := 0
	for _, arg := range debug {
		if strings.HasPrefix(arg, "--remote-debugging-port=") {
			if port != 0 {
				return errors.New("isolated debugger flags invalid")
			}
			value, err := strconv.Atoi(strings.TrimPrefix(arg, "--remote-debugging-port="))
			if err != nil || value <= 1024 || value >= 65536 {
				return errors.New("isolated debugger port invalid")
			}
			port = value
		} else if arg != "--remote-debugging-address=127.0.0.1" {
			return errors.New("isolated debugger flags invalid")
		}
	}
	if port == 0 || len(debug) != 2 {
		return errors.New("isolated debugger flags invalid")
	}
	if environmentValue(env, "CODEX_HOME") != filepath.Join(s.RuntimeRoot, "codex-home") || environmentValue(env, "CODEX_ELECTRON_USER_DATA_PATH") != filepath.Join(s.RuntimeRoot, "user-data") {
		return errors.New("isolated app environment mismatch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	command := exec.Command(filepath.Join(s.AppPath, "Contents", "MacOS", "ChatGPT"), args...)
	command.Env = env
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Stdin = os.Stdin
	if err := command.Start(); err != nil {
		return err
	}
	return command.Wait()
}
