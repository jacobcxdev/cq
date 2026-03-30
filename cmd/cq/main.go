package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/alecthomas/kong"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/cache"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/output"
	"github.com/jacobcxdev/cq/internal/provider"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	geminiprov "github.com/jacobcxdev/cq/internal/provider/gemini"
)

// CLI defines the kong command structure.
type CLI struct {
	JSON    bool             `help:"Output JSON" short:"j"`
	Refresh bool             `help:"Bypass cache" short:"r"`
	Version kong.VersionFlag `help:"Print version" short:"v"`

	Check  CheckCmd  `cmd:"" default:"withargs" help:"Check quota usage"`
	Claude ClaudeCmd `cmd:"" help:"Claude account management"`
	Codex  CodexCmd  `cmd:"" help:"Codex account management"`
	Gemini GeminiCmd `cmd:"" help:"Gemini account management"`
}

// CheckCmd is the default command that checks provider quota usage.
type CheckCmd struct {
	Providers []string `arg:"" optional:"" enum:"claude,codex,gemini" help:"Providers to check"`
}

// ClaudeCmd groups Claude-specific subcommands.
type ClaudeCmd struct {
	Login    LoginCmd    `cmd:"" help:"Add Claude account"`
	Accounts AccountsCmd `cmd:"" help:"List Claude accounts"`
	Switch   SwitchCmd   `cmd:"" help:"Switch active Claude account"`
}

// CodexCmd groups Codex-specific subcommands.
type CodexCmd struct {
	Login    LoginCmd    `cmd:"" help:"Add Codex account"`
	Accounts AccountsCmd `cmd:"" help:"List Codex accounts"`
	Switch   SwitchCmd   `cmd:"" help:"Switch active Codex account"`
}

// GeminiCmd groups Gemini-specific subcommands.
type GeminiCmd struct {
	Accounts AccountsCmd `cmd:"" help:"Show Gemini account"`
}

// LoginCmd adds a new account via OAuth.
type LoginCmd struct {
	Activate bool `help:"Set as active account after login"`
}

// AccountsCmd lists known accounts.
type AccountsCmd struct{}

// SwitchCmd switches the active account.
type SwitchCmd struct {
	Email string `arg:"" help:"Email of account to activate"`
}

// version is set at build time via -ldflags. Falls back to "dev".
var version = "dev"

func main() {
	// Handle commands that conflict with kong's default:"withargs" on CheckCmd.
	// Kong validates the enum constraint on providers before trying command
	// matching, so "refresh" and "agent" must be intercepted first.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "refresh":
			if err := runRefresh(); err != nil {
				fmt.Fprintf(os.Stderr, "cq: %v\n", err)
				os.Exit(1)
			}
			return
		case "agent":
			if err := runAgent(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "cq: %v\n", err)
				os.Exit(1)
			}
			return
		}
	}

	var cli CLI
	ctx := kong.Parse(&cli,
		kong.Name("cq"),
		kong.Description("Check AI provider usage limits"),
		kong.UsageOnError(),
		kong.Vars{"version": version},
	)
	if err := dispatch(ctx, &cli); err != nil {
		fmt.Fprintf(os.Stderr, "cq: %v\n", err)
		os.Exit(1)
	}
	ensureAgent()
}

func runAgent(args []string) error {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: cq agent <install|uninstall>\n")
		return fmt.Errorf("missing subcommand")
	}
	switch args[0] {
	case "install":
		return installAgent(1800)
	case "uninstall":
		return uninstallAgent()
	default:
		return fmt.Errorf("unknown agent command: %s", args[0])
	}
}

func dispatch(ctx *kong.Context, cli *CLI) error {
	switch ctx.Command() {
	case "check", "check <providers>":
		return runCheck(cli)
	case "claude login":
		return app.RunLogin(context.Background(), httputil.NewClient(10*time.Second, version), cli.Claude.Login.Activate)
	case "claude accounts":
		return app.RunAccounts(provider.Claude)
	case "claude switch <email>":
		return app.RunSwitch(provider.Claude, cli.Claude.Switch.Email, httputil.NewClient(10*time.Second, version))
	case "codex login":
		return app.RunCodexLogin(context.Background(), httputil.NewClient(10*time.Second, version), cli.Codex.Login.Activate)
	case "codex accounts":
		return app.RunAccounts(provider.Codex)
	case "codex switch <email>":
		return app.RunSwitch(provider.Codex, cli.Codex.Switch.Email, httputil.NewClient(10*time.Second, version))
	case "gemini accounts":
		return app.RunAccounts(provider.Gemini)
	default:
		return fmt.Errorf("unknown command: %s", ctx.Command())
	}
}

func runCheck(cli *CLI) error {
	httpClient := httputil.NewClient(10*time.Second, version)

	services := map[provider.ID]provider.Services{
		provider.Claude: {Usage: claudeprov.New(httpClient)},
		provider.Codex:  {Usage: codexprov.New(httpClient)},
		provider.Gemini: {Usage: geminiprov.New(httpClient)},
	}

	providerIDs := []provider.ID{provider.Claude, provider.Codex, provider.Gemini}
	if len(cli.Check.Providers) > 0 {
		providerIDs = make([]provider.ID, len(cli.Check.Providers))
		for i, p := range cli.Check.Providers {
			providerIDs[i] = provider.ID(p)
		}
	}

	tty := isTerminal()
	ttyRenderer := &output.TTYRenderer{
		W: os.Stdout,
	}
	var renderer app.Renderer
	if cli.JSON {
		renderer = &output.JSONRenderer{
			W:        os.Stdout,
			Pretty:   tty,
			Colorise: tty,
		}
	} else {
		renderer = ttyRenderer
	}

	c, err := cache.New(cache.OSFileSystem{}, cacheDir(), cacheTTL())
	if err != nil {
		fmt.Fprintf(os.Stderr, "cq: cache unavailable, running uncached: %v\n", err)
		c = nil
	}

	runner := &app.Runner{
		Clock:    systemClock{},
		Cache:    c,
		Services: services,
		Renderer: renderer,
	}

	ctx := context.Background()
	req := app.RunRequest{
		Providers: providerIDs,
		Refresh:   cli.Refresh,
	}
	report, err := runner.BuildReport(ctx, req)
	if err != nil {
		return err
	}
	if !cli.JSON {
		ttyRenderer.Now = report.GeneratedAt
	}
	return renderer.Render(ctx, report)
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func cacheDir() string {
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "cq")
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "cq")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "cq-cache")
	}
	return filepath.Join(home, ".cache", "cq")
}

func cacheTTL() time.Duration {
	if v := os.Getenv("CQ_TTL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 0 {
				n = 0
			}
			if n > 3600 {
				n = 3600
			}
			return time.Duration(n) * time.Second
		}
	}
	return 30 * time.Second
}
