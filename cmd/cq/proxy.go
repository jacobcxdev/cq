package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/proxy"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

func runProxy(args []string) error {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: cq proxy <start|install|uninstall|status>\n")
		return fmt.Errorf("missing subcommand")
	}
	switch args[0] {
	case "start":
		return runProxyStart()
	case "install":
		return installProxyAgent()
	case "uninstall":
		return uninstallProxyAgent()
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
	selector := proxy.NewAccountSelector(discover)
	refreshClient := httputil.NewClient(30*time.Second, version)

	transport := &proxy.TokenTransport{
		Selector:    selector,
		Refresher:   claudeprov.RefreshToken,
		Persister:   proxy.DefaultPersister,
		RefreshHTTP: refreshClient,
		Inner:       http.DefaultTransport,
	}

	// Codex account discovery (no refresh — tokens shared with Codex CLI).
	codexDiscover := proxy.CodexDiscoverer(func() []codexprov.CodexAccount {
		return codexprov.DiscoverAccounts(fsutil.OSFileSystem{})
	})
	codexSelector := proxy.NewCodexSelector(codexDiscover)

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

	srv := &proxy.Server{
		Config:        cfg,
		Selector:      selector,
		Discover:      discover,
		Transport:     transport,
		CodexSelector: codexSelector,
		CodexDiscover: codexDiscover,
	}

	return srv.ListenAndServe(context.Background())
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
