package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/cache"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
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

	accounts := keyring.DiscoverClaudeAccounts()
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

	discover := proxy.ClaudeDiscoverer(keyring.DiscoverClaudeAccounts)
	activeEmail := proxy.ActiveEmailFunc(keyring.ActiveClaudeEmail)
	refreshClient := httputil.NewClient(30*time.Second, version)

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
	codexSelector := proxy.NewCodexSelector(codexDiscover, quotaCache)

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
		Inner:    http.DefaultTransport,
	}

	if err := proxy.WriteClaudeCodeModelCapabilitiesCache(); err != nil {
		fmt.Fprintf(os.Stderr, "cq: model capabilities cache: %v (continuing without cache write)\n", err)
	}

	// Start headroom compression bridge if configured.
	var headroom *proxy.HeadroomBridge
	if cfg.Headroom {
		var err error
		headroom, err = proxy.StartHeadroomBridge()
		if err != nil {
			fmt.Fprintf(os.Stderr, "cq: headroom: %v (continuing without compression)\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "cq: headroom compression enabled\n")
		}
	}

	srv := &proxy.Server{
		Config:         cfg,
		Selector:       selector,
		Discover:       discover,
		Transport:      transport,
		CodexDiscover:  codexDiscover,
		CodexTransport: codexTransport,
		Headroom:       headroom,
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
