package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/cache"
	"github.com/jacobcxdev/cq/internal/fsutil"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func runProxy(args []string) error {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: cq proxy <start|install|uninstall|restart|status>\n")
		return fmt.Errorf("missing subcommand")
	}
	switch args[0] {
	case "start":
		return runProxyStart()
	case "install":
		return installProxyAgent()
	case "uninstall":
		return uninstallProxyAgent()
	case "restart":
		return restartProxyAgent()
	case "status":
		return runProxyStatus()
	default:
		return fmt.Errorf("unknown proxy command: %s", args[0])
	}
}

func runProxyStart() error {
	cfg, err := proxy.LoadConfig()
	if err != nil {
		return err
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
	selector := proxy.NewAccountSelector(discover, activeEmail, quotaCache)

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
			fmt.Fprintf(os.Stderr, "cq: headroom compression enabled (mode: %s)\n", resolvedMode)
		}
	}

	srv := &proxy.Server{
		Config:         cfg,
		Selector:       selector,
		Discover:       discover,
		Transport:      transport,
		CodexDiscover:         codexDiscover,
		CodexTransport:        codexTransport,
		CodexUpgradeTransport: codexUpgradeTransport,
		Headroom:       headroom,
		HeadroomMode:   resolvedMode,
	}

	err = srv.ListenAndServe(context.Background())
	if headroom != nil {
		headroom.Stop()
	}
	return err
}

func runProxyStatus() error {
	cfg, err := proxy.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
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
