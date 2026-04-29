package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/cache"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func runProxy(args []string) error {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: cq proxy <start|install|uninstall|restart|status|pin>\n")
		return fmt.Errorf("missing subcommand")
	}
	switch args[0] {
	case "start":
		opts, err := parseProxyCommandOptions(args[1:])
		if err != nil {
			return err
		}
		return runProxyStart(opts)
	case "install":
		return installProxyAgent()
	case "uninstall":
		return uninstallProxyAgent()
	case "restart":
		return restartProxyAgent()
	case "status":
		opts, err := parseProxyCommandOptions(args[1:])
		if err != nil {
			return err
		}
		return runProxyStatus(opts)
	case "pin":
		return runProxyPin(args[1:])
	default:
		return fmt.Errorf("unknown proxy command: %s", args[0])
	}
}

func runProxyPin(args []string) error {
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

func parseProxyCommandOptions(args []string) (proxyCommandOptions, error) {
	var opts proxyCommandOptions
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("proxy start: --port requires a value")
			}
			port, err := strconv.Atoi(args[i+1])
			if err != nil || port <= 0 || port > 65535 {
				return opts, fmt.Errorf("proxy start: invalid port %q", args[i+1])
			}
			opts.Port = port
			i++
		default:
			return opts, fmt.Errorf("proxy start: unknown argument %s", args[i])
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

	fmt.Fprintf(os.Stderr, "cq: proxy token: %s\n", cfg.LocalToken)

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
	refreshClient := newHTTPClientFn(30*time.Second, version)

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
	startProxyConfigReload(proxyCtx, selector)

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

	// Codex account discovery (no refresh — tokens shared with Codex CLI).
	codexDiscover := proxy.CodexDiscoverer(func() []codexprov.CodexAccount {
		return codexprov.DiscoverAccounts(fsutil.OSFileSystem{})
	})
	codexQuotaCache := proxy.NewCodexQuotaCache(cache.DefaultDir())
	codexSelector := proxy.NewCodexSelector(codexDiscover, codexQuotaCache)

	codexAccounts := codexDiscover()
	var codexEmails []string
	for _, a := range codexAccounts {
		if a.Email != "" {
			codexEmails = append(codexEmails, a.Email)
		}
	}
	fmt.Fprintf(os.Stderr, "cq: codex accounts: %d", len(codexAccounts))
	if len(codexEmails) > 0 {
		fmt.Fprintf(os.Stderr, " (%s)", strings.Join(codexEmails, ", "))
	}
	fmt.Fprintln(os.Stderr)

	// Codex account switcher (best-effort, persists switch to disk).
	codexAccountsMgr := &codexprov.Accounts{FS: fsutil.OSFileSystem{}}
	codexSwitcher := proxy.CodexAccountSwitcher(func(ctx context.Context, email string) error {
		_, err := codexAccountsMgr.Switch(ctx, email)
		return err
	})

	codexTransport := &proxy.CodexTokenTransport{
		Selector: codexSelector,
		Switcher: codexSwitcher,
		Quota:    codexQuotaCache,
		Inner:    http.DefaultTransport,
	}

	// WebSocket upgrades require HTTP/1.1. http.DefaultTransport may negotiate
	// HTTP/2 via ALPN TLS, which does not support the 101 Switching Protocols
	// response needed for WebSocket. Use a dedicated HTTP/1.1-only transport.
	http11Transport := http.DefaultTransport.(*http.Transport).Clone()
	http11Transport.TLSNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)
	http11Transport.ForceAttemptHTTP2 = false
	codexUpgradeTransport := &proxy.CodexTokenTransport{
		Selector: codexSelector,
		Switcher: codexSwitcher,
		Quota:    codexQuotaCache,
		Inner:    http11Transport,
	}

	if err := proxy.WriteClaudeCodeModelCapabilitiesCache(); err != nil {
		fmt.Fprintf(os.Stderr, "cq: model capabilities cache: %v (continuing without cache write)\n", err)
	}

	fsys := fsutil.OSFileSystem{}
	homeDir, homeErr := fsys.UserHomeDir()
	if homeErr != nil {
		fmt.Fprintf(os.Stderr, "cq: registry: resolve home dir: %v (registry disabled)\n", homeErr)
	}

	var pipeline *registryPipeline
	if homeErr == nil {
		pipeline, err = newRegistryPipeline(registryPipelineOptions{
			FS:                 fsys,
			HomeDir:            homeDir,
			ClaudeUpstream:     cfg.ClaudeUpstream,
			CodexUpstream:      cfg.CodexUpstream,
			HTTPClient:         refreshClient,
			CodexClientVersion: defaultCodexClientVersion(),
			ClaudeToken:        firstClaudeAccessToken,
			CodexToken: func() (string, error) {
				return firstCodexAccessTokenWithRefresh(
					context.Background(),
					codexDiscover(),
					func(ctx context.Context, refreshToken string) (*auth.CodexTokenResponse, error) {
						return auth.RefreshCodexToken(ctx, refreshClient, refreshToken)
					},
					fsys,
					homeDir,
					codexprov.PersistCodexAccount,
				)
			},
			Env:    os.Getenv,
			Stderr: os.Stderr,
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
		Config:                cfg,
		Selector:              selector,
		Discover:              discover,
		Transport:             transport,
		CodexDiscover:         codexDiscover,
		CodexTransport:        codexTransport,
		CodexUpgradeTransport: codexUpgradeTransport,
		Headroom:              headroom,
		Diag:                  diagnostics,
		PayloadDiag:           payloadDiag,
		HeadroomMode:          resolvedMode,
		Catalog:               catalog,
		Refresher:             proxyRefresher,
	}

	err = srv.ListenAndServe(proxyCtx)
	if headroom != nil {
		headroom.Stop()
	}
	return err
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

func startProxyConfigReload(ctx context.Context, selector *proxy.PinnedClaudeSelector) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reloadProxyConfig(selector)
			}
		}
	}()
}

func reloadProxyConfig(selector *proxy.PinnedClaudeSelector) {
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
