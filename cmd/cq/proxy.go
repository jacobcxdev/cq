package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/cache"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

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
	case "codex-default":
		if helpRequested(args[1:]) {
			return writeManualHelp(os.Stdout, []string{"proxy", "codex-default"})
		}
		return runProxyCodexDefault(args[1:])
	case "prime":
		return runProxyPrime(args[1:])
	case "endpoint":
		return runProxyEndpoint(args[1:])
	default:
		return fmt.Errorf("unknown proxy command: %s", args[0])
	}
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
	if helpRequested(args) {
		return writeManualHelp(os.Stdout, []string{"proxy", "pin"})
	}
	cfg, err := proxy.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// cq proxy pin (no args) — show current pin
	if len(args) == 0 {
		if cfg.PinnedClaudeAccount == "" {
			fmt.Println("No pin is active. All Claude requests use automatic account selection.")
		} else {
			fmt.Printf("Pinned Claude account: %s\n", cfg.PinnedClaudeAccount)
		}
		return nil
	}

	// cq proxy pin --clear
	if len(args) == 1 && args[0] == "--clear" {
		cfg.PinnedClaudeAccount = ""
		if err := proxy.SaveConfig(cfg); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		fmt.Println("Pinned Claude account cleared.")
		fmt.Println("A running proxy will pick up the change shortly.")
		return nil
	}

	// cq proxy pin <email-or-uuid>
	if len(args) == 1 {
		arg := args[0]
		lower := strings.ToLower(arg)

		// Reject reserved words that look like commands but aren't flags.
		if lower == "clear" || lower == "remove" {
			fmt.Fprintf(os.Stderr, "Usage: cq proxy pin [--clear | <email-or-account-uuid>]\n")
			return fmt.Errorf("reserved word %q is not valid; did you mean --clear?", arg)
		}

		// Reject any argument that looks like an unknown flag.
		if strings.HasPrefix(arg, "-") {
			fmt.Fprintf(os.Stderr, "Usage: cq proxy pin [--clear | <email-or-account-uuid>]\n")
			return fmt.Errorf("unknown flag %q", arg)
		}

		cfg.PinnedClaudeAccount = arg
		if err := proxy.SaveConfig(cfg); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		fmt.Printf("Pinned Claude account set to %q.\n", arg)
		fmt.Println("A running proxy will pick up the change shortly.")
		return nil
	}

	fmt.Fprintf(os.Stderr, "Usage: cq proxy pin [--clear | <email-or-account-uuid>]\n")
	return fmt.Errorf("unexpected arguments")
}

type proxyCommandOptions struct {
	Port int
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
	for i, source := range inventory.ExternalSources {
		health.ExternalSources[i] = proxy.CodexSourceHealth{
			Name: source.Name, CandidateCount: source.CandidateCount, HealthCode: codexSourceHealthCode(source.ErrorCode),
		}
	}
	return health
}

func codexSourceHealthCode(code string) string {
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
		default:
			return opts, fmt.Errorf("%s: unknown argument %s", command, args[i])
		}
	}
	return opts, nil
}

func runProxyStart(opts proxyCommandOptions) error {
	cfg, err := proxy.LoadConfig()
	if err != nil {
		return err
	}
	if opts.Port != 0 {
		cfg.Port = opts.Port
	}
	codexClientBuild := defaultCodexRoutingClientBuild()
	fsys := fsutil.OSFileSystem{}
	refreshClient := newHTTPClientFn(30*time.Second, version)
	legacyFinaliseVerifier := newProxyLegacyMaintenanceFinaliseVerifier(version, codexClientBuild, cfg.Port)
	credentialControl, err := codexprov.OpenDefaultRecoveringCredentialRefreshControlWithLegacyMaintenanceVerifier(context.Background(), fsys, refreshClient, legacyFinaliseVerifier)
	if err != nil {
		return fmt.Errorf("Codex credential coordinator: %w", err)
	}
	defer credentialControl.Close()
	codexRouting, err := proxy.OpenCodexRoutingRuntime(cfg, version, codexClientBuild)
	if err != nil {
		return fmt.Errorf("Codex routing modes: %w", err)
	}
	codexObserver, err := proxy.OpenCodexRuntimeObserver(codexRouting, fsys)
	if err != nil {
		return fmt.Errorf("Codex turn observer: %w", err)
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
	codexInventory, err := credentialControl.List(context.Background())
	if err != nil {
		return fmt.Errorf("Codex credential inventory: %w", err)
	}
	for _, intent := range codexInventory.Intents {
		if intent.Kind != codexprov.IntentAdopt {
			continue
		}
		activator, err := codexprov.NewFileSystemActivator(fsys)
		if err != nil {
			return err
		}
		snapshot, err := activator.Active(context.Background())
		if err != nil {
			return fmt.Errorf("Codex system snapshot: %w", err)
		}
		if _, _, err := credentialControl.Adopt(context.Background(), snapshot); err != nil {
			return fmt.Errorf("Codex credential adoption: %w", err)
		}
		codexInventory, err = credentialControl.List(context.Background())
		if err != nil {
			return fmt.Errorf("Codex credential inventory after adoption: %w", err)
		}
		break
	}
	codexQuotaCache := proxy.NewCodexQuotaCache(cache.DefaultDir())
	codexCapacity := codexQuotaCache.CodexCapacityLedger()
	codexObserver.BindCapacity(codexCapacity)
	codexSelector := proxy.NewCodexInventorySelector(credentialControl, codexQuotaCache)

	writeCodexHealthDiagnostics(os.Stderr, codexHealthFromInventory(codexInventory))
	codexHealthTracker := newCodexHealthTracker(credentialControl, cfg.CodexRoutingDefaultAccountKey, codexHealthFromInventory(codexInventory))

	codexRequestScope := &proxy.CodexRequestScope{
		Chooser:   codexSelector,
		Inventory: credentialControl,
	}
	codexRequestRouter := &proxy.CodexRequestRouter{
		Scope: codexRequestScope,
		Executor: &proxy.CodexAttemptExecutor{
			Inventory: credentialControl,
			Secrets:   credentialControl,
			Transport: &proxy.CodexTokenTransport{
				Inner: http.DefaultTransport,
			},
		},
		Refresher: credentialControl,
		Capacity:  codexCapacity,
	}
	codexWebSocketExecutor := proxy.NewCodexWebSocketAttemptExecutor(credentialControl, credentialControl)
	var codexHTTPEnforcer *proxy.CodexHTTPEnforcer
	if codexRouting.HTTP.Effective == proxy.CodexRoutingEnforce || len(codexRouting.HTTP.RetainedAuthoritativeEpochs) != 0 {
		if codexObserver == nil || codexObserver.Store == nil {
			return fmt.Errorf("Codex HTTP enforcement: lease store unavailable")
		}
		codexHTTPEnforcer, err = proxy.NewCodexHTTPEnforcerWithSharedLeases(
			codexRequestRouter,
			codexRouting.HTTP.ModeEpoch,
			codexRouting.HTTP.Effective == proxy.CodexRoutingEnforce,
			codexRouting.HTTP.RetainedAuthoritativeEpochs,
			codexObserver.Store,
			codexObserver.Leases,
		)
		if err != nil {
			return fmt.Errorf("Codex HTTP enforcement: %w", err)
		}
	}

	if err := proxy.WriteClaudeCodeModelCapabilitiesCache(); err != nil {
		fmt.Fprintf(os.Stderr, "cq: model capabilities cache: %v (continuing without cache write)\n", err)
	}

	homeDir, homeErr := fsys.UserHomeDir()
	if homeErr != nil {
		fmt.Fprintf(os.Stderr, "cq: registry: resolve home dir: %v (registry disabled)\n", homeErr)
	}
	if homeErr == nil && codexHTTPEnforcer != nil {
		protected := []string{filepath.Join(homeDir, ".codex", "auth.json"), filepath.Join(homeDir, ".codex", "accounts", "registry.json")}
		canary, canaryErr := proxy.OpenCodexCanary(fsys, proxy.DefaultCodexCanaryPath(), protected)
		if canaryErr == nil {
			state := canary.State()
			if state.Active {
				if state.Tuple.CQBuild != version || state.Tuple.ClientBuild != codexClientBuild || state.Tuple.ParserSchema != proxy.CurrentCodexParserSchema || state.Tuple.LeaseSchema != proxy.CurrentCodexLeaseSchema || state.Tuple.FixtureHash != proxy.CodexHTTPFixtureHash {
					return fmt.Errorf("Codex canary tuple does not match running enforcement")
				}
				codexHTTPEnforcer.SetCanary(canary)
			}
		} else if !os.IsNotExist(canaryErr) {
			return fmt.Errorf("Codex canary: %w", canaryErr)
		}
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

	codexPrimer, err := buildCodexPrimer(cfg, credentialControl.Owner(), codexRequestRouter, catalog, fsys)
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
		CodexRequests:          codexRequestRouter,
		CodexWebSocketExecutor: codexWebSocketExecutor,
		Headroom:               headroom,
		Diag:                   diagnostics,
		PayloadDiag:            payloadDiag,
		CodexRouting:           codexRouting,
		CodexObserver:          codexObserver,
		CodexHTTPEnforcer:      codexHTTPEnforcer,
		CodexPrimer:            codexPrimer,
		HeadroomMode:           resolvedMode,
		Catalog:                catalog,
		Refresher:              proxyRefresher,
	}
	if err := legacyFinaliseVerifier.bind(codexRouting, headroom != nil, resolvedMode); err != nil {
		return fmt.Errorf("legacy credential endpoint finalise verifier: %w", err)
	}

	err = srv.ListenAndServe(proxyCtx)
	proxyCancel()
	if codexPrimerDone != nil {
		<-codexPrimerDone
	}
	if headroom != nil {
		headroom.Stop()
	}
	return err
}

func buildCodexPrimer(cfg *proxy.Config, owner bool, router *proxy.CodexRequestRouter, catalog *modelregistry.Catalog, fsys fsutil.DurableFileSystem) (*proxy.CodexPrimer, error) {
	if cfg == nil || !cfg.CodexWindowPriming.Enabled || !owner {
		return nil, nil
	}
	if router == nil || catalog == nil {
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
	return &proxy.CodexPrimer{
		Accounts: router.AccountKeys,
		Usage:    &proxy.CodexPrimerUsageReader{Router: router, UsageURL: usageURL},
		Requester: &proxy.CodexPrimerRequester{
			Router: router, ResponsesURL: strings.TrimRight(cfg.CodexUpstream, "/") + "/responses",
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
	cfg, err := proxy.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if opts.Port != 0 {
		cfg.Port = opts.Port
	}

	addr := fmt.Sprintf("http://127.0.0.1:%d/health", cfg.Port)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(addr)
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
