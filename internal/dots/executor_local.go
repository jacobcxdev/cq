package dots

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

var executorEnvironmentPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func validExecutorEnvironment(value string) bool {
	return executorEnvironmentPattern.MatchString(value)
}

func executorDirectory(s *Settings, parts ...string) (string, error) {
	if s == nil {
		return "", errors.New("executor settings unavailable")
	}
	if err := privateDirectory(s.RuntimeRoot); err != nil {
		return "", err
	}
	current := s.RuntimeRoot
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\") {
			return "", errors.New("invalid executor directory")
		}
		current = filepath.Join(current, part)
		if err := os.Mkdir(current, 0o700); err != nil && !os.IsExist(err) {
			return "", err
		}
		if err := privateDirectory(current); err != nil {
			return "", err
		}
		canonical, err := CanonicalPath(current)
		if err != nil || canonical != current {
			return "", errors.New("executor directory outside runtime root")
		}
	}
	return current, nil
}
func executorRead(path, root string) (map[string]any, error) {
	data, err := readPrivate(path, root, true)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if json.Unmarshal(data, &value) != nil || value == nil {
		return nil, errors.New("invalid executor state")
	}
	return value, nil
}

// ExecutorCLIEnvironment contains only non-secret ambient values and a local broker capability.
func ExecutorCLIEnvironment(root, capability string) []string {
	env := []string{}
	for _, key := range []string{"PATH", "HOME", "USER", "LOGNAME", "LANG"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return append(env, "CODEX_HOME="+filepath.Join(root, "home"), "TMPDIR="+filepath.Join(root, "tmp"), "CODEX_API_KEY="+capability, "CODEX_EXEC_SERVER_EXIT_ON_STDIN_CLOSE=true", "CODEX_INTERNAL_ORIGINATOR_OVERRIDE=codex_work_desktop", "LOG_FORMAT=json", "RUST_LOG=codex_exec_server::remote=info")
}
func executorReal(s *Settings) string {
	return filepath.Join(s.AppPath, "Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex")
}
func prepareLocalExecutor(s *Settings) (string, error) {
	root, err := executorDirectory(s, "null-executor")
	if err != nil {
		return "", err
	}
	for _, name := range []string{"home", "work", "tmp"} {
		if _, err := executorDirectory(s, "null-executor", name); err != nil {
			return "", err
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	protected := []string{filepath.Join(home, ".codex"), s.AppPath, filepath.Dir(s.SystemAuth), s.CQAccounts}
	if s.AccountSource != "" {
		protected = append(protected, s.AccountSource)
	}
	for directory, names := range map[string][]string{"Application Support": {"ChatGPT", "Codex", "com.openai.codex", "com.openai.chat", "CodexBar", "OpenAI"}, "Preferences": {"com.openai.codex.plist"}, "Logs": {"com.openai.codex"}, "Caches": {"com.openai.codex"}} {
		for _, name := range names {
			protected = append(protected, filepath.Join(home, "Library", directory, name))
		}
	}
	policy := "(version 1)\n(allow default)\n(deny signal (target others))\n"
	for _, path := range protected {
		encoded, _ := json.Marshal(path)
		policy += "(deny file-write* (subpath " + string(encoded) + "))\n"
	}
	encoded, _ := json.Marshal(filepath.Join(home, ".codex"))
	policy += "(deny file-read* (subpath " + string(encoded) + "))\n"
	if err := launchWrite(filepath.Join(root, "isolation.sb"), []byte(policy), 0o600); err != nil {
		return "", err
	}
	return root, nil
}

type executorProcess interface {
	poll() (int, bool)
	closeStdin() error
	terminate() error
	kill() error
	wait(time.Duration) bool
	pid() int
}
type executorCommand struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	done    chan struct{}
	mu      sync.Mutex
	running bool
	code    int
}

func (p *executorCommand) poll() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code, p.running
}
func (p *executorCommand) closeStdin() error { return p.stdin.Close() }
func (p *executorCommand) terminate() error  { return dotsTerminate(p.cmd.Process.Pid) }
func (p *executorCommand) kill() error       { return p.cmd.Process.Kill() }
func (p *executorCommand) pid() int          { return p.cmd.Process.Pid }
func (p *executorCommand) wait(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}

type executorLaunch func(s *Settings, root string, port int, environment, capability string, sandbox, forward bool) (executorProcess, error)

func executorCommandFor(s *Settings, root string, port int, environment, capability string, sandbox, forward bool) (*exec.Cmd, error) {
	if !validExecutorEnvironment(environment) || port <= 0 || port > 65535 {
		return nil, errors.New("invalid executor environment")
	}
	real := executorReal(s)
	args := []string{"-c", `cli_auth_credentials_store="file"`, "exec-server", "--remote", fmt.Sprintf("http://127.0.0.1:%d/api", port), "--environment-id", environment, "--exit-on-stdin-close"}
	if sandbox {
		args = append([]string{"-f", filepath.Join(root, "isolation.sb"), real}, args...)
		real = "/usr/bin/sandbox-exec"
	}
	cmd := exec.Command(real, args...)
	cmd.Dir = filepath.Join(root, "work")
	cmd.Env = ExecutorCLIEnvironment(root, capability)
	if forward {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	return cmd, nil
}

func launchExecutor(s *Settings, root string, port int, environment, capability string, sandbox, forward bool) (executorProcess, error) {
	cmd, err := executorCommandFor(s, root, port, environment, capability, sandbox, forward)
	if err != nil {
		return nil, err
	}
	return startExecutorCommand(cmd)
}

func startExecutorCommand(cmd *exec.Cmd) (executorProcess, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}
	child := &executorCommand{cmd: cmd, stdin: stdin, done: make(chan struct{}), running: true, code: -1}
	go func() {
		defer func() {
			_ = recover()
			child.mu.Lock()
			child.running = false
			child.mu.Unlock()
			close(child.done)
		}()
		_ = cmd.Wait()
		child.mu.Lock()
		child.code = cmd.ProcessState.ExitCode()
		child.mu.Unlock()
	}()
	return child, nil
}
func closeExecutor(child executorProcess) error {
	_ = child.closeStdin()
	if !child.wait(10 * time.Second) {
		_ = child.terminate()
		if !child.wait(10 * time.Second) {
			_ = child.kill()
			if !child.wait(5 * time.Second) {
				return errors.New("executor process did not stop")
			}
		}
	}
	return nil
}

type executorDependencies struct {
	inventory func() (Inventory, error)
	get       func(context.Context, Account, string, http.Header) (map[string]any, error)
	request   executorRequest
	launch    executorLaunch
	now       func() time.Time
	stdin     io.Reader
}

func executorDefaults(s *Settings, c *CatalogueClient, alias string) executorDependencies {
	if c == nil {
		c = &CatalogueClient{Settings: s}
	}
	deps := executorDependencies{inventory: c.Inventory, get: c.Get, launch: launchExecutor, now: c.now}
	direct := executorDirectRequest(s, c.HTTP, c.now, alias)
	deps.request = func(ctx context.Context, bound Account, host, method, path string, body []byte) (int, string, []byte, error) {
		inventory, err := deps.inventory()
		if err != nil {
			return 0, "", nil, err
		}
		matches := []Account{}
		for _, owner := range inventory.Accounts {
			if owner.Alias == bound.Alias && owner.AccountID == bound.AccountID && owner.AccountID != inventory.System.AccountID {
				matches = append(matches, owner)
			}
		}
		if len(matches) != 1 {
			return 0, "", nil, errors.New("secondary executor owner unavailable")
		}
		return direct(ctx, matches[0], host, method, path, body)
	}
	return deps
}
func executorSecondary(s *Settings, inventory Inventory, alias string) (Account, error) {
	if alias == s.SystemAlias || !aliasPattern.MatchString(alias) {
		return Account{}, errors.New("executor owner outside secondary scope")
	}
	var matches []Account
	for _, owner := range inventory.Accounts {
		if owner.Alias == alias && owner.AccountID != inventory.System.AccountID {
			matches = append(matches, owner)
		}
	}
	profiles := map[string]bool{}
	for _, dot := range s.Dots {
		if dot.OwnerAlias == alias {
			profiles[dot.ProfileID] = true
			if dot.OwnerAccountID != "" && len(matches) == 1 && dot.OwnerAccountID != matches[0].AccountID {
				return Account{}, errors.New("secondary executor owner changed")
			}
		}
	}
	if len(matches) != 1 || len(profiles) != 1 {
		return Account{}, errors.New("secondary executor owner unavailable")
	}
	return matches[0], nil
}
func executorCurrentRoot(ctx context.Context, s *Settings, deps executorDependencies, owner Account) (string, error) {
	headers, err := executorOwnerHeaders(s, owner, owner.Alias, deps.now())
	if err != nil {
		return "", err
	}
	value, err := deps.get(ctx, owner, "/backend-api/tbo/primary", headers)
	if err != nil {
		return "", err
	}
	profile, _ := value["profile"].(map[string]any)
	selection, _ := value["selection"].(map[string]any)
	root, _ := profile["active_root_thread_id"].(string)
	if profile["id"] != s.ProfileIDs()[owner.Alias] || selection["available"] != true || selection["thread_id"] != root || root == "" || len(root) > 128 {
		return "", errors.New("secondary primary identity changed")
	}
	return root, nil
}

// RunNativeExecutor reuses the existing proof-bound singleton, never the system executor.
func RunNativeExecutor(ctx context.Context, s *Settings, c *CatalogueClient, args []string) error {
	return RunNativeExecutorIO(ctx, s, c, args, os.Stdin, os.Stdout, os.Stderr)
}

// RunNativeExecutorIO forwards the native CLI's streams without interpreting messages.
func RunNativeExecutorIO(ctx context.Context, s *Settings, c *CatalogueClient, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if s == nil {
		return errors.New("executor settings unavailable")
	}
	deps := executorDefaults(s, c, s.SecondaryAlias)
	deps.stdin = stdin
	deps.launch = func(s *Settings, root string, port int, environment, capability string, sandbox, forward bool) (executorProcess, error) {
		cmd, err := executorCommandFor(s, root, port, environment, capability, sandbox, forward)
		if err != nil {
			return nil, err
		}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		return startExecutorCommand(cmd)
	}
	return runProofExecutor(ctx, s, args, deps, true)
}

// RunLocalExecutor runs the isolated proof-bound secondary process.
func RunLocalExecutor(ctx context.Context, s *Settings, c *CatalogueClient, args []string) error {
	if s == nil {
		return errors.New("executor settings unavailable")
	}
	return runProofExecutor(ctx, s, args, executorDefaults(s, c, s.SecondaryAlias), false)
}
func runProofExecutor(ctx context.Context, s *Settings, args []string, deps executorDependencies, forward bool) (result error) {
	root := filepath.Join(s.RuntimeRoot, "null-executor")
	proof, err := executorRead(filepath.Join(root, "proof.json"), s.RuntimeRoot)
	if err != nil {
		return err
	}
	environment, _ := proof["environment_id"].(string)
	argumentEnvironment := ""
	execServer := false
	for index, arg := range args {
		if arg == "exec-server" {
			execServer = true
		}
		if arg == "--environment-id" && index+1 < len(args) {
			argumentEnvironment = args[index+1]
		}
	}
	if !execServer || argumentEnvironment != environment || !validExecutorEnvironment(environment) || proof["owner_alias"] != s.SecondaryAlias || proof["executor_stopped"] != true {
		return errors.New("native executor outside isolated reuse scope")
	}
	inventory, err := deps.inventory()
	if err != nil {
		return err
	}
	owner, err := executorSecondary(s, inventory, s.SecondaryAlias)
	if err != nil {
		return err
	}
	current, err := executorCurrentRoot(ctx, s, deps, owner)
	if err != nil {
		return err
	}
	if proof["root_thread_id"] != current {
		return errors.New("secondary root changed")
	}
	root, err = prepareLocalExecutor(s)
	if err != nil {
		return err
	}
	lock, err := fsutil.AcquireExclusiveLock(fsutil.OSFileSystem{}, filepath.Join(root, "process.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	// Reread after locking so an earlier stopped proof cannot race another wrapper.
	latest, err := executorRead(filepath.Join(root, "proof.json"), s.RuntimeRoot)
	if err != nil {
		return err
	}
	if latest["executor_stopped"] != true || latest["environment_id"] != environment || latest["owner_alias"] != owner.Alias || latest["root_thread_id"] != current {
		return errors.New("secondary executor already active")
	}
	proof = latest
	broker, err := newExecutorRegistryBroker(s, owner, environment, owner.Alias, deps.request)
	if err != nil {
		return err
	}
	broker.start()
	defer broker.close()
	child, err := deps.launch(s, root, broker.port(), environment, broker.capability, true, forward)
	if err != nil {
		return err
	}
	proof["executor_stopped"] = false
	proof["executor_pid"] = child.pid()
	proof["native_executor_wrapper_pid"] = os.Getpid()
	proof["native_singleton_managed"] = true
	proof["connected_at"] = float64(deps.now().UnixNano()) / 1e9
	if err := installSave(filepath.Join(root, "proof.json"), proof); err != nil {
		_ = closeExecutor(child)
		return err
	}
	defer func() {
		if err := closeExecutor(child); result == nil {
			result = err
		}
		code, running := child.poll()
		proof["executor_stopped"] = !running
		proof["executor_exit_code"] = code
		if err := installSave(filepath.Join(root, "proof.json"), proof); result == nil {
			result = err
		}
	}()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if forward {
		input := deps.stdin
		if input == nil {
			input = os.Stdin
		}
		go func() { defer func() { _ = recover(); cancel() }(); _, _ = io.Copy(io.Discard, input) }()
	}
	for {
		_, running := child.poll()
		stopped, _ := broker.snapshot()
		if !running || stopped {
			return nil
		}
		if waitContext(runCtx, time.Second) != nil {
			return nil
		}
	}
}

// ReuseNativeExecutor returns the genuine existing registration after checking both owners.
func ReuseNativeExecutor(ctx context.Context, s *Settings, c *CatalogueClient, headers http.Header, body []byte) (map[string]any, map[string]any, error) {
	if s == nil {
		return nil, nil, errors.New("executor settings unavailable")
	}
	deps := executorDefaults(s, c, s.SecondaryAlias)
	inventory, err := deps.inventory()
	if err != nil {
		return nil, nil, err
	}
	system := inventory.System
	authorization := headers.Get("Authorization")
	if headers.Get("ChatGPT-Account-Id") != system.AccountID || !strings.HasPrefix(authorization, "Bearer ") {
		return nil, nil, errors.New("isolated native executor identity mismatch")
	}
	system.Token = strings.TrimPrefix(authorization, "Bearer ")
	selection, err := deps.get(ctx, system, "/backend-api/tbo/primary", headers)
	if err != nil {
		return nil, nil, err
	}
	profile, _ := selection["profile"].(map[string]any)
	selected, _ := selection["selection"].(map[string]any)
	expected, err := executorSystemDot(s)
	if err != nil {
		return nil, nil, err
	}
	if profile["id"] != expected.ProfileID || selected["thread_id"] != expected.RootThreadID {
		return nil, nil, errors.New("isolated native executor identity mismatch")
	}
	owner, err := executorSecondary(s, inventory, s.SecondaryAlias)
	if err != nil {
		return nil, nil, err
	}
	root, err := executorCurrentRoot(ctx, s, deps, owner)
	if err != nil {
		return nil, nil, err
	}
	proof, err := executorRead(filepath.Join(s.RuntimeRoot, "null-executor", "proof.json"), s.RuntimeRoot)
	if err != nil {
		return nil, nil, err
	}
	request, err := ValidateNativeRegistration(s, headers, body, system, proof, root)
	if err != nil {
		return nil, nil, err
	}
	environment, _ := proof["environment_id"].(string)
	if !validExecutorEnvironment(environment) {
		return nil, nil, errors.New("research registration identity invalid")
	}
	status, contentType, data, err := deps.request(ctx, owner, executorRegistry, "GET", "/api/cloud/environment/"+url.PathEscape(environment)+"/status", nil)
	if err != nil {
		return nil, nil, err
	}
	var value map[string]any
	if status != 200 || !strings.HasPrefix(contentType, "application/json") || json.Unmarshal(data, &value) != nil || value["environment_id"] != environment {
		return nil, nil, errors.New("existing research registration unavailable")
	}
	roots, _ := request["selected_capability_roots"].([]any)
	return map[string]any{"executor_id": environment}, map[string]any{"native_executor_reused": true, "owner_alias": s.SecondaryAlias, "new_registration_sent": false, "capability_roots_granted": 0, "native_proposed_roots": len(roots)}, nil
}
