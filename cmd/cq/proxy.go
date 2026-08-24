package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/cache"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func normalCallerCredentials(ctx context.Context, cfg *proxy.Config, claudeAccounts []keyring.ClaudeOAuth, codexInventory codexprov.Inventory, codexSecrets codexprov.ExactSecretResolver) ([]proxy.NormalCallerCredentialV1, error) {
	if cfg == nil || cfg.LocalToken == "" {
		return nil, proxy.ErrNormalCallerAuthUnavailable
	}
	credentials := make([]proxy.NormalCallerCredentialV1, 0, 1+len(claudeAccounts)+len(codexInventory.Accounts))
	appendCredential := func(domain proxy.NormalCallerDomain, bearer, identity string, expires time.Time) error {
		if bearer == "" || identity == "" {
			return nil
		}
		credentials = append(credentials, proxy.NormalCallerCredentialV1{Domain: domain, Bearer: bearer, SubjectID: identity, ValidUntil: expires})
		return nil
	}
	if err := appendCredential(proxy.NormalCallerLocal, cfg.LocalToken, "local-owner", time.Time{}); err != nil {
		return nil, err
	}
	for index, account := range claudeAccounts {
		identity := account.AccountUUID
		if identity == "" && account.TokenAccount != nil {
			identity = account.TokenAccount.UUID
		}
		if identity == "" {
			identity = account.Email
		}
		if identity == "" {
			identity = fmt.Sprintf("anonymous-%d", index)
		}
		var expires time.Time
		if account.ExpiresAt > 0 {
			expires = time.UnixMilli(account.ExpiresAt)
		}
		if err := appendCredential(proxy.NormalCallerClaude, account.AccessToken, identity, expires); err != nil {
			return nil, err
		}
	}
	for _, account := range codexInventory.Accounts {
		for _, candidate := range account.Candidates {
			accessToken := candidate.Credential.AccessToken
			if accessToken == "" && candidate.Routable {
				if codexSecrets == nil {
					return nil, errors.New("Codex caller credential resolver unavailable")
				}
				material, resolveErr := codexSecrets.ResolveExact(ctx, codexprov.PlanCandidate(account, candidate))
				if resolveErr != nil {
					return nil, fmt.Errorf("resolve Codex caller credential: %w", resolveErr)
				}
				accessToken = material.AccessToken
			}
			identity := string(account.Key) + "\x00" + string(candidate.Ref.CandidateID) + "\x00" + string(candidate.Revision)
			if err := appendCredential(proxy.NormalCallerCodex, accessToken, identity, codexCallerBearerExpiry(accessToken)); err != nil {
				return nil, err
			}
		}
	}
	return credentials, nil
}

func codexCallerBearerExpiry(accessToken string) time.Time {
	expiresAt := auth.DecodeCodexClaims(accessToken).ExpiresAt
	if expiresAt <= 0 {
		return time.Time{}
	}
	return time.Unix(expiresAt, 0)
}

var runProxyRuntimeRoleFn = runProxyRuntimeRole
var newProxyRuntimeWorkerLauncherFn = func(proxy.RuntimeRoleManifestV1, proxy.LifecycleHolderProof) (proxy.RuntimeWorkerLauncher, error) {
	return nil, proxy.ErrRuntimeRoleUnavailable
}
var runProxyAdoptedRuntimeFn = func(context.Context, net.Listener, func(context.Context, net.Listener, http.Handler) error) error {
	return proxy.ErrRuntimeRoleUnavailable
}
var runProxyOwnedRuntimeFn = func(context.Context, int, func(context.Context, net.Listener, http.Handler) error) (bool, error) {
	return false, nil
}

var adoptProxyListenerFn = func() (net.Listener, error) { return nil, nil }

func runProxyRuntimeRole(ctx context.Context, manifest proxy.RuntimeRoleManifestV1) error {
	files := proxy.RuntimeRoleFiles{
		Lifecycle: os.NewFile(uintptr(manifest.LifecycleFD), "runtime-lifecycle"),
		Control:   os.NewFile(uintptr(manifest.ControlFD), "runtime-control"),
		Secret:    os.NewFile(uintptr(manifest.SecretFD), "runtime-secret"),
	}
	if manifest.Role == proxy.RuntimeRoleSupervisor {
		_ = files.Close()
		return proxy.ErrRuntimeRoleUnavailable
	}
	if manifest.Role != proxy.RuntimeRoleWorker {
		_ = files.Close()
		return proxy.ErrRuntimeRoleManifest
	}
	// ExtraFiles is sequential; the worker's reserved descriptor 3 is a sealed
	// non-socket placeholder and is closed before role-local validation.
	if reserved := os.NewFile(uintptr(proxy.RuntimeListenerFD), "runtime-reserved"); reserved != nil {
		_ = reserved.Close()
	}
	return proxy.RunRuntimeWorkerRole(ctx, manifest, files)
}

func runProxy(args []string) error {
	if len(args) > 0 && isHelpToken(args[0]) {
		path := []string{"proxy"}
		if len(args) > 1 && args[0] == "help" {
			path = append(path, args[1:]...)
		}
		return writeManualHelp(os.Stdout, path)
	}
	if len(args) == 0 {
		_ = writeManualHelp(os.Stderr, []string{"proxy"})
		return fmt.Errorf("missing subcommand")
	}
	switch args[0] {
	case "start":
		if helpRequested(args[1:]) {
			return writeManualHelp(os.Stdout, []string{"proxy", "start"})
		}
		opts, err := parseProxyCommandOptions(args[1:])
		if err != nil {
			return err
		}
		return runProxyStart(opts)
	case "install":
		if helpRequested(args[1:]) {
			return writeManualHelp(os.Stdout, []string{"proxy", "install"})
		}
		return installProxyAgent()
	case "uninstall":
		if helpRequested(args[1:]) {
			return writeManualHelp(os.Stdout, []string{"proxy", "uninstall"})
		}
		return uninstallProxyAgent()
	case "restart":
		if helpRequested(args[1:]) {
			return writeManualHelp(os.Stdout, []string{"proxy", "restart"})
		}
		return restartProxyAgent()
	case "validate-http":
		if helpRequested(args[1:]) {
			return writeManualHelp(os.Stdout, []string{"proxy", "validate-http"})
		}
		return runDefaultProxyValidateHTTP(args[1:], version)
	case "status":
		if helpRequested(args[1:]) {
			return writeManualHelp(os.Stdout, []string{"proxy", "status"})
		}
		opts, err := parseProxyCommandOptionsFor("proxy status", args[1:])
		if err != nil {
			return err
		}
		return runProxyStatus(opts)
	case "pin":
		return runProxyPin(args[1:])
	case "default":
		return runProxyDefault(args[1:])
	case "prime":
		return runProxyPrime(args[1:])
	case "endpoint":
		return runProxyEndpoint(args[1:])
	case "policy":
		if helpRequested(args[1:]) {
			return writeManualHelp(os.Stdout, []string{"proxy", "policy"})
		}
		return runProxyPolicy(args[1:], os.Stdout)
	case "rescue":
		if helpRequested(args[1:]) {
			return writeManualHelp(os.Stdout, []string{"proxy", "rescue"})
		}
		return runProxyRescue(args[1:], os.Stdout)
	case "hook":
		return runProxyHook(args[1:], os.Stdin, os.Stdout)
	case "candidate":
		authority, err := ClassifyProxyCommand(append([]string{"proxy"}, args...))
		if err != nil {
			return err
		}
		if authority.Terminating {
			if authority.Row == "ordinary_help" {
				path, ok := proxyHelpInspectionPath(args)
				if ok {
					return writeManualHelp(os.Stdout, path)
				}
			}
			return errors.New("proxy candidate usage")
		}
		ctx, cancel := context.WithTimeout(context.Background(), authority.Deadline.Total)
		defer cancel()
		_, err = runProxyCandidateCommand(ctx, os.Stdout, authority, defaultCandidateCommandDependencies())
		return err
	default:
		return fmt.Errorf("unknown proxy command: %s", args[0])
	}
}

func runProxyDefault(args []string) error {
	if len(args) > 0 && isHelpToken(args[0]) {
		path := []string{"proxy", "default"}
		if len(args) > 1 && args[0] == "help" {
			path = append(path, args[1:]...)
		}
		return writeManualHelp(os.Stdout, path)
	}
	if len(args) == 2 && args[0] == "codex" && helpRequested(args[1:]) {
		return writeManualHelp(os.Stdout, []string{"proxy", "default", "codex"})
	}
	if len(args) == 0 || args[0] != "codex" {
		return fmt.Errorf("usage: cq proxy default <provider>")
	}
	return runProxyCodexDefault(args[1:])
}

func runProxyPrime(args []string) error {
	if len(args) > 0 && isHelpToken(args[0]) {
		path := []string{"proxy", "prime"}
		if len(args) > 1 && args[0] == "help" {
			path = append(path, args[1:]...)
		}
		return writeManualHelp(os.Stdout, path)
	}
	if len(args) == 2 && helpRequested(args[1:]) {
		return writeManualHelp(os.Stdout, []string{"proxy", "prime", args[0]})
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: cq proxy prime <status|enable|disable>")
	}
	cfg, err := proxy.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	switch args[0] {
	case "status":
		state := "disabled"
		if cfg.CodexWindowPriming.Enabled {
			state = "enabled"
		}
		fmt.Printf("Codex window priming: %s\n", state)
		fmt.Printf("Model overrides: %d\n", len(cfg.CodexWindowPriming.ModelOverrides))
		return nil
	case "enable":
		cfg.CodexWindowPriming.Enabled = true
	case "disable":
		cfg.CodexWindowPriming.Enabled = false
	default:
		return fmt.Errorf("unknown proxy prime command: %s", args[0])
	}
	if err := proxy.SaveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Printf("Codex window priming %s.\n", args[0]+"d")
	fmt.Println("Restart proxy to apply change.")
	return nil
}

func runProxyPin(args []string) error {
	fsys := fsutil.OSFileSystem{}
	var home string
	return runProxyPinWithDependencies(context.Background(), args, proxyCodexDefaultDependencies{
		ListInventory: func(ctx context.Context) (codexprov.Inventory, error) {
			inventory, resolvedHome, err := listProxyCodexDefaultInventory(ctx, fsys)
			home = resolvedHome
			return inventory, err
		},
		LoadAliasIndex: func() (codexprov.AccountAliasIndex, error) {
			return (codexprov.Registry{FS: fsys, Home: home}).AccountAliasIndex()
		},
		LoadConfig: proxy.LoadConfig,
		SaveConfig: proxy.SaveConfig,
		Stdout:     os.Stdout,
	})
}

func runProxyPinWithDependencies(ctx context.Context, args []string, deps proxyCodexDefaultDependencies) error {
	if helpRequested(args) {
		return writeManualHelp(os.Stdout, []string{"proxy", "pin"})
	}
	if len(args) == 0 {
		cfg, err := deps.LoadConfig()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		if cfg.PinnedClaudeAccount == "" {
			fmt.Fprintln(deps.Stdout, "Claude proxy pin: not configured.")
		} else {
			fmt.Fprintf(deps.Stdout, "Claude proxy pin: %q\n", cfg.PinnedClaudeAccount)
		}
		if cfg.CodexRoutingPinnedAccountKey == "" {
			fmt.Fprintln(deps.Stdout, "Codex proxy pin: not configured.")
		} else {
			fmt.Fprintf(deps.Stdout, "Codex proxy pin: %q\n", cfg.CodexRoutingPinnedAccountKey)
		}
		return nil
	}

	switch args[0] {
	case "claude":
		return runProxyClaudePin(args[1:], deps)
	case "codex":
		return runProxyCodexPin(ctx, args[1:], deps)
	default:
		return errors.New("proxy pin provider required: use claude or codex")
	}
}

func runProxyClaudePin(args []string, deps proxyCodexDefaultDependencies) error {
	cfg, err := deps.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// cq proxy pin claude — show current pin
	if len(args) == 0 {
		if cfg.PinnedClaudeAccount == "" {
			fmt.Fprintln(deps.Stdout, "Claude proxy pin: not configured.")
		} else {
			fmt.Fprintf(deps.Stdout, "Claude proxy pin: %q\n", cfg.PinnedClaudeAccount)
		}
		return nil
	}

	// cq proxy pin claude --clear
	if len(args) == 1 && args[0] == "--clear" {
		cfg.PinnedClaudeAccount = ""
		if err := deps.SaveConfig(cfg); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		fmt.Fprintln(deps.Stdout, "Claude proxy pin cleared.")
		fmt.Fprintln(deps.Stdout, "A running proxy will pick up the change shortly.")
		return nil
	}

	// cq proxy pin claude <email-or-uuid>
	if len(args) == 1 {
		arg := args[0]
		lower := strings.ToLower(arg)

		// Reject reserved words that look like commands but aren't flags.
		if lower == "clear" || lower == "remove" {
			fmt.Fprintf(os.Stderr, "Usage: cq proxy pin claude [--clear | <email-or-account-uuid>]\n")
			return fmt.Errorf("reserved word %q is not valid; did you mean --clear?", arg)
		}

		// Reject any argument that looks like an unknown flag.
		if strings.HasPrefix(arg, "-") {
			fmt.Fprintf(os.Stderr, "Usage: cq proxy pin claude [--clear | <email-or-account-uuid>]\n")
			return fmt.Errorf("unknown flag %q", arg)
		}

		cfg.PinnedClaudeAccount = arg
		if err := deps.SaveConfig(cfg); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		fmt.Fprintf(deps.Stdout, "Claude proxy pin: %q\n", arg)
		fmt.Fprintln(deps.Stdout, "A running proxy will pick up the change shortly.")
		return nil
	}

	fmt.Fprintf(os.Stderr, "Usage: cq proxy pin claude [--clear | <email-or-account-uuid>]\n")
	return fmt.Errorf("unexpected arguments")
}

func runProxyCodexPin(ctx context.Context, args []string, deps proxyCodexDefaultDependencies) error {
	if len(args) > 1 || (len(args) == 1 && args[0] != "--clear" && strings.HasPrefix(args[0], "-")) {
		return errors.New("usage: cq proxy pin codex [--clear | <account-reference>]")
	}
	if len(args) == 0 || args[0] == "--clear" {
		cfg, err := deps.LoadConfig()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		if len(args) == 0 {
			if cfg.CodexRoutingPinnedAccountKey == "" {
				fmt.Fprintln(deps.Stdout, "Codex proxy pin: not configured.")
			} else {
				fmt.Fprintf(deps.Stdout, "Codex proxy pin: %q\n", cfg.CodexRoutingPinnedAccountKey)
			}
			return nil
		}
		cfg.CodexRoutingPinnedAccountKey = ""
		if err := deps.SaveConfig(cfg); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		fmt.Fprintln(deps.Stdout, "Codex proxy pin cleared.")
		fmt.Fprintln(deps.Stdout, "Restart proxy to apply change.")
		return nil
	}

	inventory, err := deps.ListInventory(ctx)
	if err != nil || proxyCodexDefaultInventoryIncomplete(inventory) {
		return errors.New("list Codex account inventory: unavailable")
	}
	aliases, err := deps.LoadAliasIndex()
	if err != nil {
		return errors.New("load Codex account aliases: unavailable")
	}
	accountKey, err := codexprov.ResolveAccountReference(inventory, aliases, args[0])
	if err != nil {
		return err
	}
	cfg, err := deps.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg.CodexRoutingPinnedAccountKey = accountKey
	if err := deps.SaveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintf(deps.Stdout, "Codex proxy pin: %q\n", string(accountKey))
	fmt.Fprintln(deps.Stdout, "Restart proxy to apply change.")
	return nil
}

type proxyCommandOptions struct {
	Port                 int
	MigrateLegacyManaged bool
	JSON                 bool
	RuntimeRole          *proxy.RuntimeRoleManifestV1
}

type proxyRegistryDependencies struct {
	FS                  fsutil.FileSystem
	HomeDir             string
	HTTPClient          httputil.Doer
	CodexClientVersion  string
	ClaudeToken         func() (string, error)
	CredentialAuthority codexRegistryCredentialAuthority
	Env                 func(string) string
	Stderr              io.Writer
}

func newProxyRegistryPipeline(cfg *proxy.Config, deps proxyRegistryDependencies) (*registryPipeline, error) {
	if cfg == nil {
		return nil, fmt.Errorf("registry pipeline: missing proxy config")
	}
	return newRegistryPipelineWithCodexAuthority(registryPipelineOptions{
		FS:                 deps.FS,
		HomeDir:            deps.HomeDir,
		ClaudeUpstream:     cfg.ClaudeUpstream,
		CodexUpstream:      cfg.CodexUpstream,
		HTTPClient:         deps.HTTPClient,
		CodexClientVersion: deps.CodexClientVersion,
		ClaudeToken:        deps.ClaudeToken,
		Env:                deps.Env,
		Stderr:             deps.Stderr,
	}, deps.CredentialAuthority)
}

func codexHealthFromInventory(inventory codexprov.Inventory) proxy.CodexHealth {
	health := proxy.CodexHealth{
		AccountCount:      len(inventory.Accounts),
		AccountCountKnown: true,
		HealthCode:        "ok",
		ExternalSources:   make([]proxy.CodexSourceHealth, len(inventory.ExternalSources)),
	}
	if len(inventory.Accounts) > 0 && codexDispatchableAccountCount(inventory) == 0 {
		health.HealthCode = "unroutable"
	}
	for i, source := range inventory.ExternalSources {
		health.ExternalSources[i] = proxy.CodexSourceHealth{
			Name: source.Name, CandidateCount: source.CandidateCount,
			HealthCode: codexSourceHealthCode(source.ErrorCode, source.OptionalAbsent),
		}
	}
	return health
}

func codexDispatchableAccountCount(inventory codexprov.Inventory) int {
	count := 0
	for _, account := range inventory.Accounts {
		if !account.Routable || account.Unstable || account.Identity.AccountID == "" || account.Identity.UserID == "" {
			continue
		}
		for _, candidate := range account.Candidates {
			if candidate.Routable && !candidate.DispatchBlocked && candidate.Ref.AccountKey == account.Key && candidate.Ref.CandidateID != "" && candidate.Revision != "" {
				count++
				break
			}
		}
	}
	return count
}

func codexSourceHealthCode(code string, optionalAbsent bool) string {
	if optionalAbsent {
		return "ok"
	}
	switch code {
	case "":
		return "ok"
	case "unavailable", "invalid", "invalid_manifest", "unsafe_path", "stale_revision", "identity_mismatch", "fingerprint_mismatch", "fetch_error":
		return code
	default:
		return "unknown"
	}
}

func writeCodexHealthDiagnostics(w io.Writer, health proxy.CodexHealth) {
	if health.AccountCountKnown {
		fmt.Fprintf(w, "cq: codex accounts: %d\n", health.AccountCount)
	} else {
		fmt.Fprintf(w, "cq: codex accounts: unknown health=%s\n", health.HealthCode)
	}
	for _, source := range health.ExternalSources {
		fmt.Fprintf(w, "cq: codex source: name=%s candidates=%d health=%s\n", source.Name, source.CandidateCount, source.HealthCode)
	}
}

func parseProxyCommandOptions(args []string) (proxyCommandOptions, error) {
	return parseProxyCommandOptionsFor("proxy start", args)
}

func parseProxyCommandOptionsFor(command string, args []string) (proxyCommandOptions, error) {
	var opts proxyCommandOptions
	if command == "proxy start" && len(args) > 0 && args[0] == "--runtime-role" {
		manifest, err := proxy.ParseRuntimeRoleArguments(args)
		if err != nil {
			return opts, err
		}
		opts.RuntimeRole = &manifest
		return opts, nil
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("%s: --port requires a value", command)
			}
			port, err := strconv.Atoi(args[i+1])
			if err != nil || port <= 0 || port > 65535 {
				return opts, fmt.Errorf("%s: invalid port %q", command, args[i+1])
			}
			opts.Port = port
			i++
		case "--migrate-legacy-managed":
			if command != "proxy start" {
				return opts, fmt.Errorf("%s: unknown argument %s", command, args[i])
			}
			opts.MigrateLegacyManaged = true
		case "--json":
			if command != "proxy status" {
				return opts, fmt.Errorf("%s: unknown argument", command)
			}
			opts.JSON = true
		default:
			return opts, fmt.Errorf("%s: unknown argument %s", command, args[i])
		}
	}
	return opts, nil
}

func listProxyCodexStartupInventory(ctx context.Context, inventory codexprov.CredentialInventory) (codexprov.Inventory, error) {
	return inventory.List(ctx)
}

func runProxyStart(opts proxyCommandOptions) (returnErr error) {
	var supervisorRole *proxy.RuntimeRoleManifestV1
	var workerRole *proxy.RuntimeRoleManifestV1
	var workerFiles proxy.RuntimeRoleFiles
	var supervisorFiles proxy.RuntimeRoleFiles
	var supervisorHolder proxy.LifecycleHolderProof
	var supervisorLauncher proxy.RuntimeWorkerLauncher
	if opts.RuntimeRole != nil {
		if opts.RuntimeRole.Role == proxy.RuntimeRoleWorker {
			workerRole = opts.RuntimeRole
			if reserved := os.NewFile(uintptr(proxy.RuntimeListenerFD), "runtime-reserved"); reserved != nil {
				_ = reserved.Close()
			}
			workerFiles = proxy.RuntimeRoleFiles{
				Lifecycle: os.NewFile(uintptr(workerRole.LifecycleFD), "runtime-lifecycle"),
				Control:   os.NewFile(uintptr(workerRole.ControlFD), "runtime-control"),
				Secret:    os.NewFile(uintptr(workerRole.SecretFD), "runtime-secret"),
				Work:      os.NewFile(uintptr(workerRole.WorkFD), "runtime-work"),
			}
			if err := proxy.ValidateRuntimeRoleFiles(*workerRole, workerFiles); err != nil {
				_ = workerFiles.Close()
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, workerFiles.Close()) }()
		}
		if opts.RuntimeRole.Role != proxy.RuntimeRoleSupervisor && opts.RuntimeRole.Role != proxy.RuntimeRoleWorker {
			return proxy.ErrRuntimeRoleManifest
		}
		if opts.RuntimeRole.Role == proxy.RuntimeRoleSupervisor {
			supervisorRole = opts.RuntimeRole
			supervisorFiles = proxy.RuntimeRoleFiles{
				Listener:  os.NewFile(uintptr(supervisorRole.ListenerFD), "runtime-listener"),
				Lifecycle: os.NewFile(uintptr(supervisorRole.LifecycleFD), "runtime-lifecycle"),
				Control:   os.NewFile(uintptr(supervisorRole.ControlFD), "runtime-control"),
				Secret:    os.NewFile(uintptr(supervisorRole.SecretFD), "runtime-secret"),
			}
			if err := proxy.ValidateRuntimeRoleFiles(*supervisorRole, supervisorFiles); err != nil {
				_ = supervisorFiles.Close()
				return err
			}
			secret, err := proxy.ReadRuntimeSecret(supervisorFiles.Secret)
			supervisorFiles.Secret = nil
			if err != nil {
				_ = supervisorFiles.Close()
				return err
			}
			secret.Destroy()
			var holderErr error
			supervisorHolder, holderErr = proxy.RuntimeLifecycleHolder(supervisorFiles.Lifecycle, hex.EncodeToString(supervisorRole.LifecycleHolderIdentityDigest[:]))
			if holderErr != nil {
				_ = supervisorFiles.Close()
				return holderErr
			}
			var launcherErr error
			supervisorLauncher, launcherErr = newProxyRuntimeWorkerLauncherFn(*supervisorRole, supervisorHolder)
			if launcherErr != nil {
				_ = supervisorFiles.Close()
				return launcherErr
			}
			defer func() { returnErr = errors.Join(returnErr, supervisorFiles.Close()) }()
		}
	}
	var adoptedListener net.Listener
	var err error
	if supervisorRole == nil && workerRole == nil {
		adoptedListener, err = adoptProxyListenerFn()
	}
	if err != nil {
		return err
	}
	if adoptedListener != nil {
		defer func() {
			if adoptedListener != nil {
				returnErr = errors.Join(returnErr, adoptedListener.Close())
			}
		}()
		err = runProxyAdoptedRuntimeFn(context.Background(), adoptedListener, serveRuntimeSupervisor)
		adoptedListener = nil
		return err
	}
	if supervisorRole != nil {
		store, err := proxy.OpenNormalCallerAdmissionStore(fsutil.OSFileSystem{}, proxy.DefaultNormalCallerAdmissionPath())
		if err != nil {
			return err
		}
		defer store.Close()
		err = proxy.RunValidatedRuntimeSupervisorRole(context.Background(), *supervisorRole, proxy.RuntimeSupervisorRoleDependencies{
			Files: supervisorFiles, SupervisorHolder: supervisorHolder, Launcher: supervisorLauncher,
			Checkpoints: &proxy.RuntimeHashCheckpointStore{}, CallerAdmissions: store,
			WorkerManifest: proxy.WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: hex.EncodeToString(supervisorRole.ManifestDigest[:])},
			Serve:          serveRuntimeSupervisor,
		})
		supervisorFiles = proxy.RuntimeRoleFiles{}
		return err
	}
	intent, err := claimInstalledHTTPValidationStartupRequest(
		version,
		consumeInstalledHTTPValidationStartupRequestFn,
		invalidateInstalledHTTPValidationMarkerFn,
	)
	if err != nil {
		return err
	}
	cfg, err := loadProxyStartConfigFn()
	if err != nil {
		return err
	}
	if opts.Port != 0 {
		cfg.Port = opts.Port
	}
	if intent != nil {
		return runInstalledHTTPValidationStartupFn(context.Background(), cfg, version, intent)
	}
	if workerRole == nil {
		handled, err := runProxyOwnedRuntimeFn(context.Background(), cfg.Port, serveRuntimeSupervisor)
		if handled {
			return err
		}
	}
	codexClientBuild := defaultCodexRoutingClientBuild()
	fsys := fsutil.OSFileSystem{}
	var resilienceState *proxy.ProxyResilienceState
	if stateDir := cfg.ResolvedProxyResilienceStateDir(); stateDir != "" {
		resilienceState, err = proxy.OpenProxyResilienceState(context.Background(), proxy.ProxyResilienceStateOptions{
			FS: fsys, Root: stateDir, Random: rand.Reader, Now: time.Now, SkipRuntimeMode: workerRole != nil,
		})
		if err != nil {
			return fmt.Errorf("proxy resilience state: %w", err)
		}
		defer func() { returnErr = errors.Join(returnErr, resilienceState.Close()) }()
	}
	refreshClient := newHTTPClientFn(30*time.Second, version)
	servingAttestor := proxy.NewServingAttestor()
	httpRequirements, _ := proxy.DefaultCodexRoutingRequirements(version, codexClientBuild)
	activeCanary, err := openProxyCodexCanary(fsys, proxy.DefaultCodexCanaryPath(), httpRequirements)
	if err != nil {
		return fmt.Errorf("Codex canary startup: %w", err)
	}
	if activeCanary != nil {
		if err := validateCodexCanaryStartConfig(cfg); err != nil {
			_ = activeCanary.Close()
			return err
		}
		defer func() {
			returnErr = errors.Join(returnErr, activeCanary.Close())
		}()
	}
	legacyFinaliseVerifier := newProxyLegacyMaintenanceFinaliseVerifier(version, codexClientBuild, cfg.Port, servingAttestor)
	credentialControl, err := codexprov.OpenDefaultRecoveringCredentialRefreshControlWithLegacyMaintenanceVerifierAndRecoveryRecorder(
		context.Background(), fsys, refreshClient, legacyFinaliseVerifier,
		codexprov.CredentialEndpointRecoveryRecorderFunc(codexCanaryEndpointRecoveryRecorder(activeCanary)),
	)
	if err != nil {
		return fmt.Errorf("Codex credential coordinator: %w", err)
	}
	defer func() {
		if closeErr := credentialControl.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close Codex credential coordinator: %w", closeErr))
		}
	}()
	if opts.MigrateLegacyManaged {
		migration, migrateErr := credentialControl.MigrateLegacyManaged(context.Background())
		if migrateErr != nil {
			return fmt.Errorf("migrate legacy Codex managed records: %w", migrateErr)
		}
		fmt.Fprintf(os.Stderr, "cq: migrated legacy Codex managed records: %d\n", migration.Migrated)
	}
	codexRouting, err := proxy.OpenCodexRoutingRuntime(cfg, version, codexClientBuild)
	if err != nil {
		return fmt.Errorf("Codex routing modes: %w", err)
	}
	if err := validateProxyCodexCanaryRuntime(activeCanary, codexRouting); err != nil {
		return err
	}
	codexContinuity, err := openProxyCodexContinuity(proxyCodexContinuityDependencies{
		FS:        fsys,
		StateDir:  cfg.ResolvedCodexContinuityStateDir(),
		Routing:   codexRouting,
		Retention: time.Duration(cfg.CodexLeaseRetentionDays) * 24 * time.Hour,
		Now:       time.Now,
		Authority: credentialControl,
		Canary:    activeCanary,
	})
	if err != nil {
		return fmt.Errorf("Codex continuity: %w", err)
	}
	if codexContinuity != nil {
		defer func() {
			returnErr = errors.Join(returnErr, proxyCodexContinuityCloseError(codexContinuity.Close()))
		}()
	}
	accounts := discoverClaudeAccountsFn()
	var emails []string
	for _, a := range accounts {
		if a.Email != "" {
			emails = append(emails, a.Email)
		}
	}
	fmt.Fprintf(os.Stderr, "cq: claude accounts: %d", len(accounts))
	if len(emails) > 0 {
		fmt.Fprintf(os.Stderr, " (%s)", strings.Join(emails, ", "))
	}
	fmt.Fprintln(os.Stderr)

	discover := proxy.ClaudeDiscoverer(discoverClaudeAccountsFn)
	activeEmail := proxy.ActiveEmailFunc(activeClaudeEmailFn)
	claudeProvider := claudeprov.New(refreshClient)
	quotaCache := proxy.NewQuotaCache(claudeProvider.FetchAccountUsage, cache.DefaultDir())
	baseSelector := proxy.NewAccountSelector(discover, activeEmail, quotaCache)
	affinitySelector := proxy.NewSessionAffinitySelector(baseSelector, discover, quotaCache)
	selector := proxy.NewPinnedClaudeSelector(affinitySelector, discover, cfg.PinnedClaudeAccount, quotaCache)
	selector.SetPinExpireFunc(clearPersistedClaudePin)
	if cfg.PinnedClaudeAccount != "" {
		fmt.Fprintf(os.Stderr, "cq: pinned claude account: %s\n", cfg.PinnedClaudeAccount)
	}
	proxyCtx, proxyCancel := context.WithCancel(context.Background())
	defer proxyCancel()
	startProxyConfigReload(proxyCtx, selector, codexRouting)

	accountsMgr := &claudeprov.Accounts{HTTP: refreshClient}
	switcher := proxy.AccountSwitcher(func(ctx context.Context, email string) error {
		_, err := accountsMgr.Switch(ctx, email)
		return err
	})

	transport := &proxy.TokenTransport{
		Selector:    selector,
		Refresher:   claudeprov.RefreshToken,
		Persister:   proxy.DefaultPersister,
		Switcher:    switcher,
		RefreshHTTP: refreshClient,
		Quota:       quotaCache,
		Inner:       http.DefaultTransport,
	}

	// Codex account discovery is owned by the credential coordinator.
	codexInventory, err := listProxyCodexStartupInventory(context.Background(), credentialControl)
	if err != nil {
		return fmt.Errorf("Codex credential inventory: %w", err)
	}
	codexRoutingInventory, codexContinuityInventory := newProxyCodexRoutingInventories(
		credentialControl,
		cfg.CodexRoutingAccountKeys,
		cfg.CodexRoutingPinnedAccountKey,
	)
	codexInventory, err = codexRoutingInventory.List(context.Background())
	if err != nil {
		return fmt.Errorf("Codex routing inventory: %w", err)
	}
	if codexRouting.HTTP.Effective == proxy.CodexRoutingEnforce && codexDispatchableAccountCount(codexInventory) == 0 {
		return errors.New("Codex HTTP enforcement has no allowed dispatchable account")
	}
	runtimeCallerCredentials, err := normalCallerCredentials(context.Background(), cfg, accounts, codexInventory, credentialControl)
	if err != nil {
		return fmt.Errorf("normal caller index: %w", err)
	}
	codexQuotaCache := proxy.NewCodexQuotaCache(cache.DefaultDir())
	codexCapacity := codexQuotaCache.CodexCapacityLedger()
	codexObserver, codexWebSocketObserver, err := newProxyCodexV2Observers(proxyCodexV2ObserverDependencies{
		Routing:    codexRouting,
		Continuity: codexContinuity,
		Capacity:   codexCapacity,
	})
	if err != nil {
		return fmt.Errorf("Codex turn observer: %w", err)
	}
	codexObserver.SetCanary(activeCanary)

	writeCodexHealthDiagnostics(os.Stderr, codexHealthFromInventory(codexInventory))
	codexHealthTracker := newCodexHealthTrackerWithRoutingDefaultInventory(
		codexRoutingInventory,
		codexContinuityInventory,
		cfg.CodexRoutingDefaultAccountKey,
		codexHealthFromInventory(codexInventory),
	)

	codexRequestScope := &proxy.CodexRequestScope{
		Inventory: codexRoutingInventory,
	}
	codexAttemptExecutor := &proxy.CodexAttemptExecutor{
		Inventory: credentialControl,
		Secrets:   credentialControl,
		Transport: &proxy.CodexTokenTransport{
			Inner: http.DefaultTransport,
		},
	}
	codexRequestRouter := &proxy.CodexRequestRouter{
		Scope:     codexRequestScope,
		Executor:  codexAttemptExecutor,
		Refresher: credentialControl,
		Capacity:  codexCapacity,
	}
	codexCapacityRefresher, err := newProxyCodexRoutingCapacityRefresher(cfg.CodexUpstream, codexRequestRouter, codexCapacity)
	if err != nil {
		return err
	}
	codexSelector := proxy.NewCodexInventorySelector(codexRoutingInventory, codexQuotaCache, codexCapacityRefresher)
	codexRequestScope.Chooser = codexSelector
	codexWebSocketExecutor := proxy.NewCodexWebSocketAttemptExecutor(credentialControl, credentialControl)

	if err := proxy.WriteClaudeCodeModelCapabilitiesCache(); err != nil {
		fmt.Fprintf(os.Stderr, "cq: model capabilities cache: %v (continuing without cache write)\n", err)
	}

	homeDir, homeErr := fsys.UserHomeDir()
	if homeErr != nil {
		fmt.Fprintf(os.Stderr, "cq: registry: resolve home dir: %v (registry disabled)\n", homeErr)
	}
	var pipeline *registryPipeline
	if homeErr == nil {
		pipeline, err = newProxyRegistryPipeline(cfg, proxyRegistryDependencies{
			FS:                  fsys,
			HomeDir:             homeDir,
			HTTPClient:          refreshClient,
			CodexClientVersion:  codexClientBuild,
			ClaudeToken:         firstClaudeAccessToken,
			CredentialAuthority: newCodexRegistryControlAdapter(credentialControl),
			Env:                 os.Getenv,
			Stderr:              os.Stderr,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "cq: registry: configure: %v (registry disabled)\n", err)
			pipeline = nil
		}
	}

	var catalog *modelregistry.Catalog
	var registryRefresher *modelregistry.Refresher
	publishRegistry := func() {}
	if pipeline != nil {
		catalog = pipeline.Catalog
		registryRefresher = pipeline.Refresher
		publishRegistry = pipeline.Publish
	}

	var proxyRefresher proxy.RegistryRefresher
	if registryRefresher != nil {
		proxyRefresher = proxy.RegistryRefresherFunc(func(ctx context.Context) (modelregistry.RefreshDiagnostics, error) {
			diag, err := registryRefresher.Refresh(ctx)
			writeRegistrySourceDiagnostics(os.Stderr, diag)
			if err == nil {
				publishRegistry()
			}
			return diag, err
		})
		initialRefreshCtx, initialRefreshCancel := context.WithTimeout(context.Background(), 30*time.Second)
		if initDiag, err := registryRefresher.Refresh(initialRefreshCtx); err != nil {
			fmt.Fprintf(os.Stderr, "cq: registry: initial refresh failed: %v (continuing with empty registry)\n", err)
		} else {
			writeRegistrySourceDiagnostics(os.Stderr, initDiag)
			publishRegistry()
		}
		initialRefreshCancel()
		if pipeline.StartReconciler != nil {
			pipeline.StartReconciler(context.Background())
		}
	}

	codexPrimer, err := buildCodexPrimer(cfg, credentialControl.Owner(), codexRequestRouter, credentialControl, catalog, fsys)
	if err != nil {
		return err
	}
	var codexPrimerDone chan struct{}
	if cfg.CodexWindowPriming.Enabled && codexPrimer == nil {
		fmt.Fprintln(os.Stderr, "cq: Codex window priming configured; credential-coordinator delegate remains read-only")
	}
	if codexPrimer != nil {
		codexPrimer.OnError = func(err error) {
			fmt.Fprintf(os.Stderr, "cq: Codex window primer: %v\n", err)
		}
		codexPrimerDone = make(chan struct{})
		go func() {
			defer close(codexPrimerDone)
			defer func() {
				if recovered := recover(); recovered != nil {
					fmt.Fprintf(os.Stderr, "cq: Codex window primer panic: %v\n", recovered)
				}
			}()
			_ = codexPrimer.Run(proxyCtx)
		}()
		fmt.Fprintln(os.Stderr, "cq: Codex window priming enabled")
	}

	// Start headroom compression bridge if configured.
	// HeadroomEnabled() returns true when either the legacy headroom bool is set
	// OR when an explicit headroom_mode is configured (e.g. "cache" without "headroom: true").
	var headroom *proxy.HeadroomBridge
	resolvedMode := cfg.ResolvedHeadroomMode()
	if cfg.HeadroomEnabled() {
		var err error
		headroom, err = proxy.StartHeadroomBridge()
		if err != nil {
			fmt.Fprintf(os.Stderr, "cq: headroom: %v (continuing without compression)\n", err)
		} else {
			headroom.Catalog = catalog
			fmt.Fprintf(os.Stderr, "cq: headroom compression enabled (mode: %s)\n", resolvedMode)
		}
	}
	var codexRoutes proxy.CodexHTTPRequestRouteSnapshotter
	var codexPlanRuntime proxy.CodexHTTPRequestPlanRuntime
	var sessionPolicy *proxy.SessionPolicyResolver
	var dispatchPermits proxy.CallerDispatchPermitAuthority
	if codexContinuity != nil {
		codexRoutes = codexContinuity.Coordinator
		codexPlanRuntime = codexContinuity.Runtime
	}
	if resilienceState != nil {
		sessionPolicy = resilienceState.Routing.Resolver()
		dispatchPermits = resilienceState.DispatchPermits
	}
	codexTurnReceipts, err := proxy.NewCodexTurnReceiptStore(rand.Reader, time.Now)
	if err != nil {
		return fmt.Errorf("Codex turn receipts: %w", err)
	}
	codexNativeHTTP, err := newProxyCodexNativeHTTP(proxyCodexNativeHTTPDependencies{
		Status:            codexRouting.HTTP,
		Inventory:         codexContinuityInventory,
		Capacity:          codexCapacity,
		Routes:            codexRoutes,
		Runtime:           codexPlanRuntime,
		DefaultAccountKey: cfg.CodexRoutingDefaultAccountKey,
		PinnedAccountKey:  cfg.CodexRoutingPinnedAccountKey,
		Executor:          codexAttemptExecutor,
		Refresher:         credentialControl,
		SessionPolicy:     sessionPolicy,
		DispatchPermits:   dispatchPermits,
		TurnReceipts:      codexTurnReceipts,
		Headroom:          proxy.NewCodexRequestHeadroomAdapter(headroom),
		HeadroomMode:      resolvedMode,
		Upstream:          cfg.CodexUpstream,
		Now:               time.Now,
	})
	if err != nil {
		return fmt.Errorf("Codex native HTTP routing: %w", err)
	}
	codexWebSocketBroker, err := newProxyCodexWebSocket(proxyCodexWebSocketDependencies{
		Status:            codexRouting.WebSocket,
		Inventory:         codexContinuityInventory,
		Capacity:          codexCapacity,
		Routes:            codexRoutes,
		Runtime:           codexPlanRuntime,
		DefaultAccountKey: cfg.CodexRoutingDefaultAccountKey,
		PinnedAccountKey:  cfg.CodexRoutingPinnedAccountKey,
		Executor:          codexWebSocketExecutor,
		SessionPolicy:     sessionPolicy,
		DispatchPermits:   dispatchPermits,
		TurnReceipts:      codexTurnReceipts,
		Upstream:          cfg.CodexUpstream,
		Now:               time.Now,
	})
	if err != nil {
		return fmt.Errorf("Codex WebSocket routing: %w", err)
	}
	codexCanaryStop, err := newProxyCodexCanaryStop(activeCanary, codexContinuity, codexNativeHTTP)
	if err != nil {
		return err
	}

	var diagnostics *proxy.DiagnosticsWriter
	if cfg.DiagnosticsLog != "" {
		diagnostics, err = proxy.OpenDiagnosticsWriter(cfg.DiagnosticsLog)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cq: diagnostics: %v (continuing without diagnostics)\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "cq: diagnostics enabled\n")
			defer func() {
				if err := diagnostics.Close(); err != nil {
					fmt.Fprintf(os.Stderr, "cq: diagnostics: close: %v\n", err)
				}
			}()
		}
	}
	if diagnostics != nil {
		diagnostics.SetCodexCanary(activeCanary)
	}

	var payloadDiag *proxy.PayloadWriter
	if cfg.PayloadDiagnosticsLog != "" {
		payloadDiag, err = proxy.OpenPayloadWriter(cfg.PayloadDiagnosticsLog)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cq: payload diagnostics: %v (continuing without payload diagnostics)\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "cq: payload diagnostics enabled — WARNING: log contains raw request bodies including prompts and message content\n")
			defer func() {
				if err := payloadDiag.Close(); err != nil {
					fmt.Fprintf(os.Stderr, "cq: payload diagnostics: close: %v\n", err)
				}
			}()
		}
	}

	srv := &proxy.Server{
		Config:    cfg,
		Selector:  selector,
		Discover:  discover,
		Transport: transport,
		CodexHealth: func() proxy.CodexHealth {
			return codexHealthTracker.Health(context.Background())
		},
		CodexRequests:                    codexRequestRouter,
		CodexWebSocketExecutor:           codexWebSocketExecutor,
		CodexWebSocketBroker:             codexWebSocketBroker,
		Headroom:                         headroom,
		Diag:                             diagnostics,
		PayloadDiag:                      payloadDiag,
		ServingAttestor:                  servingAttestor,
		CodexRouting:                     codexRouting,
		CodexNativeHTTP:                  codexNativeHTTP,
		CodexObserver:                    codexObserver,
		CodexWebSocketObserver:           codexWebSocketObserver,
		CodexWebSocketObserverConfigured: true,
		CodexPrimer:                      codexPrimer,
		CodexCanary:                      activeCanary,
		CodexCanaryStop:                  codexCanaryStop,
		HeadroomMode:                     resolvedMode,
		Catalog:                          catalog,
		Refresher:                        proxyRefresher,
		RuntimeCallerCredentials:         runtimeCallerCredentials,
		SessionPolicy:                    sessionPolicy,
		CodexTurnReceipts:                codexTurnReceipts,
	}
	if resilienceState != nil {
		srv.RoutingPolicy = resilienceState.Routing
	}
	if err := legacyFinaliseVerifier.bind(codexRouting, headroom, resolvedMode); err != nil {
		return fmt.Errorf("legacy credential endpoint finalise verifier: %w", err)
	}

	if workerRole != nil {
		handler, handlerErr := srv.RuntimeHandler()
		if handlerErr != nil {
			return handlerErr
		}
		credentialSource := proxy.NormalCallerCredentialSource(func(ctx context.Context) ([]proxy.NormalCallerCredentialV1, error) {
			current, listErr := codexRoutingInventory.List(ctx)
			if listErr != nil {
				return nil, listErr
			}
			return normalCallerCredentials(ctx, cfg, accounts, current, credentialControl)
		})
		err = proxy.RunRuntimeWorkerRoleWithHandlerAndCallerCredentialSource(proxyCtx, *workerRole, workerFiles, handler, credentialSource)
		workerFiles = proxy.RuntimeRoleFiles{}
	} else {
		err = srv.ListenAndServe(proxyCtx)
	}
	proxyCancel()
	if codexPrimerDone != nil {
		<-codexPrimerDone
	}
	if headroom != nil {
		headroom.Stop()
	}
	return err
}

func serveRuntimeSupervisor(ctx context.Context, listener net.Listener, handler http.Handler) error {
	if ctx == nil || listener == nil || handler == nil {
		return proxy.ErrRuntimeSupervisorUnavailable
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		case <-done:
		}
	}()
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
		return nil
	}
	return err
}

func newProxyCodexRoutingCapacityRefresher(
	codexUpstream string,
	router *proxy.CodexRequestRouter,
	capacity *proxy.CodexCapacityLedger,
) (*proxy.CodexRoutingCapacityRefresher, error) {
	if router == nil || capacity == nil {
		return nil, errors.New("Codex routing capacity refresh unavailable")
	}
	usageURL, err := proxy.CodexPrimerUsageURL(codexUpstream)
	if err != nil {
		return nil, fmt.Errorf("Codex routing capacity refresh: %w", err)
	}
	return &proxy.CodexRoutingCapacityRefresher{
		Usage: &proxy.CodexPrimerUsageReader{
			Router:   router,
			UsageURL: usageURL,
			Timeout:  5 * time.Second,
		},
		Capacity: capacity,
	}, nil
}

func buildCodexPrimer(cfg *proxy.Config, owner bool, router *proxy.CodexRequestRouter, inventory codexprov.CredentialInventory, catalog *modelregistry.Catalog, fsys fsutil.DurableFileSystem) (*proxy.CodexPrimer, error) {
	if cfg == nil || !cfg.CodexWindowPriming.Enabled || !owner {
		return nil, nil
	}
	if router == nil || inventory == nil || catalog == nil {
		return nil, fmt.Errorf("Codex window priming dependencies unavailable")
	}
	entries := catalog.Snapshot().Entries
	if err := proxy.ValidateCodexPrimerRegistry(entries); err != nil {
		return nil, fmt.Errorf("Codex window priming registry: %w", err)
	}
	if err := proxy.ValidateCodexPrimerOverrides(cfg.CodexWindowPriming.ModelOverrides, entries); err != nil {
		return nil, err
	}
	usageURL, err := proxy.CodexPrimerUsageURL(cfg.CodexUpstream)
	if err != nil {
		return nil, err
	}
	store, err := proxy.OpenDefaultCodexPrimerStore(fsys)
	if err != nil {
		return nil, fmt.Errorf("Codex window primer journal: %w", err)
	}
	primerRouter := *router
	primerRouter.Scope = &proxy.CodexRequestScope{Inventory: inventory}
	return &proxy.CodexPrimer{
		Accounts: primerRouter.AccountKeys,
		Usage:    &proxy.CodexPrimerUsageReader{Router: &primerRouter, UsageURL: usageURL},
		Requester: &proxy.CodexPrimerRequester{
			Router: &primerRouter, ResponsesURL: strings.TrimRight(cfg.CodexUpstream, "/") + "/responses",
		},
		Store:          store,
		Models:         func() []modelregistry.Entry { return catalog.Snapshot().Entries },
		ModelOverrides: cfg.CodexWindowPriming.ModelOverrides,
	}, nil
}

func clearPersistedClaudePin(pin string) {
	cfg, err := proxy.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cq: clear expired claude pin %q: %v\n", pin, err)
		return
	}
	if cfg.PinnedClaudeAccount != pin {
		return
	}
	cfg.PinnedClaudeAccount = ""
	if err := proxy.SaveConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "cq: clear expired claude pin %q: %v\n", pin, err)
		return
	}
	fmt.Fprintf(os.Stderr, "cq: cleared expired claude pin: %s\n", pin)
}

func startProxyConfigReload(ctx context.Context, selector *proxy.PinnedClaudeSelector, routing *proxy.CodexRoutingRuntime) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reloadProxyConfig(selector, routing)
			}
		}
	}()
}

func reloadProxyConfig(selector *proxy.PinnedClaudeSelector, routing *proxy.CodexRoutingRuntime) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "cq: proxy config reload panic: %v\n", r)
		}
	}()

	cfg, err := proxy.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cq: proxy config reload: %v\n", err)
		return
	}
	selector.SetPin(cfg.PinnedClaudeAccount)
	if routing.ConfiguredModesDiffer(cfg) {
		fmt.Fprintln(os.Stderr, "cq: proxy config reload: Codex routing mode change requires restart")
	}
}

func runProxyStatus(opts proxyCommandOptions) error {
	port, err := loadProxyStatusPort(opts)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	addr := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	resp, err := proxyStatusGet(addr)
	if err != nil {
		return fmt.Errorf("proxy not running: %w", err)
	}
	defer resp.Body.Close()

	var health map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return fmt.Errorf("parse health response: %w", err)
	}

	data, _ := json.MarshalIndent(health, "", "  ")
	fmt.Println(string(data))
	return nil
}

var proxyStatusGet = func(addr string) (*http.Response, error) {
	return (&http.Client{Timeout: 5 * time.Second}).Get(addr)
}

func loadProxyStatusPort(opts proxyCommandOptions) (int, error) {
	if opts.Port != 0 {
		return opts.Port, nil
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" || !filepath.IsAbs(configHome) {
		home, err := os.UserHomeDir()
		if err != nil {
			configHome = filepath.Join(os.TempDir(), "cq-config")
		} else {
			configHome = filepath.Join(home, ".config")
		}
	}
	data, err := os.ReadFile(filepath.Join(configHome, "cq", "proxy.json"))
	if os.IsNotExist(err) {
		return proxy.DefaultPort, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read proxy config: %w", err)
	}
	var cfg proxy.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return 0, fmt.Errorf("parse proxy config: %w", err)
	}
	setProxyStatusConfigDefaults(&cfg)
	if err := validateProxyStatusConfig(&cfg); err != nil {
		return 0, err
	}
	return cfg.Port, nil
}

func setProxyStatusConfigDefaults(cfg *proxy.Config) {
	if cfg.Port == 0 {
		cfg.Port = proxy.DefaultPort
	}
	if cfg.ClaudeUpstream == "" {
		cfg.ClaudeUpstream = proxy.DefaultUpstream
	}
	if cfg.CodexUpstream == "" {
		cfg.CodexUpstream = proxy.DefaultCodexUpstream
	}
	if cfg.CodexTurnRouting == "" {
		cfg.CodexTurnRouting = proxy.CodexRoutingOff
	}
	if cfg.CodexWSTurnRouting == "" {
		cfg.CodexWSTurnRouting = proxy.CodexRoutingOff
	}
	if cfg.CodexLeaseRetentionDays == 0 {
		cfg.CodexLeaseRetentionDays = 7
	}
}

func validateProxyStatusConfig(cfg *proxy.Config) error {
	if cfg.LocalToken == "" {
		return errors.New("local_token is required")
	}
	if _, err := url.Parse(cfg.ClaudeUpstream); err != nil {
		return fmt.Errorf("invalid claude_upstream URL: %w", err)
	}
	if _, err := url.Parse(cfg.CodexUpstream); err != nil {
		return fmt.Errorf("invalid codex_upstream URL: %w", err)
	}
	if cfg.HeadroomMode != "" && cfg.HeadroomMode != "token" && cfg.HeadroomMode != "cache" {
		return fmt.Errorf("invalid headroom_mode %q: must be \"token\" or \"cache\"", cfg.HeadroomMode)
	}
	if err := validateProxyStatusRoutingMode("codex_turn_routing", cfg.CodexTurnRouting); err != nil {
		return err
	}
	if err := validateProxyStatusRoutingMode("codex_ws_turn_routing", cfg.CodexWSTurnRouting); err != nil {
		return err
	}
	if cfg.CodexLeaseRetentionDays < 1 || cfg.CodexLeaseRetentionDays > 365 {
		return fmt.Errorf("invalid codex_lease_retention_days %d: must be between 1 and 365", cfg.CodexLeaseRetentionDays)
	}
	if cfg.CodexContinuityStateDir != "" {
		clean := filepath.Clean(cfg.CodexContinuityStateDir)
		if !filepath.IsAbs(cfg.CodexContinuityStateDir) || clean != cfg.CodexContinuityStateDir || clean == string(filepath.Separator) {
			return fmt.Errorf("invalid codex_continuity_state_dir %q: must be a clean absolute non-root path", cfg.CodexContinuityStateDir)
		}
	}
	seenRoutingAccounts := make(map[string]bool, len(cfg.CodexRoutingAccountKeys))
	for _, accountKey := range cfg.CodexRoutingAccountKeys {
		key := string(accountKey)
		if key == "" || seenRoutingAccounts[key] {
			return errors.New("invalid codex_routing_account_keys: keys must be non-empty and unique")
		}
		seenRoutingAccounts[key] = true
	}
	if len(seenRoutingAccounts) != 0 && cfg.CodexRoutingDefaultAccountKey != "" && !seenRoutingAccounts[string(cfg.CodexRoutingDefaultAccountKey)] {
		return errors.New("invalid codex_routing_default_account_key: account is not allowed for routing")
	}
	for scope, modelID := range cfg.CodexWindowPriming.ModelOverrides {
		if strings.TrimSpace(scope) == "" || strings.TrimSpace(modelID) == "" {
			return fmt.Errorf("invalid Codex window priming model override %q", scope)
		}
	}
	return nil
}

func validateProxyStatusRoutingMode(field string, mode proxy.CodexRoutingMode) error {
	switch mode {
	case proxy.CodexRoutingOff, proxy.CodexRoutingObserve, proxy.CodexRoutingEnforce:
		return nil
	default:
		return fmt.Errorf("invalid %s %q: must be \"off\", \"observe\", or \"enforce\"", field, mode)
	}
}
