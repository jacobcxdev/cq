package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"github.com/charmbracelet/x/term"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/provider"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type ZAICmd struct {
	Login    ZAILoginCmd     `cmd:"" help:"Sign in through the browser; use --api-key for key login"`
	Accounts AccountsCmd     `cmd:"" help:"List Z.ai accounts without API keys"`
	Switch   ZAIReferenceCmd `cmd:"" help:"Choose preferred Z.ai account"`
	Remove   ZAIRemoveCmd    `cmd:"" help:"Remove a stored account or API keys after browser login"`
	Route    ZAIRouteCmd     `cmd:"" help:"Choose an account's native or ZCode gateway route"`
	ZCode    ZAIZCodeCmd     `cmd:"" name:"zcode" help:"Inspect ZCode login, token plans and reset cards"`
	Prime    ZAIPrimeCmd     `cmd:"" help:"Manage Z.ai quota-window priming"`
	Resets   ZAIResetsCmd    `cmd:"" help:"Inspect and use Z.ai reset cards"`
}

type ZAILoginCmd struct {
	ZCode       bool   `name:"zcode" help:"Sign in through ZCode browser OAuth (default)"`
	APIKey      bool   `name:"api-key" help:"Use a Personal Coding Plan API key through a hidden prompt"`
	APIKeyStdin bool   `help:"Read API key from standard input instead of hidden prompt"`
	Label       string `help:"Custom account label; defaults to the browser account email"`
	Activate    *bool  `negatable:"" help:"Make this account preferred (default for browser login); --no-activate keeps the current preference"`
}

func (c ZAILoginCmd) Validate() error {
	if c.ZCode && (c.APIKey || c.APIKeyStdin) {
		return errors.New("choose browser login or API-key input")
	}
	return nil
}

func (c ZAILoginCmd) shouldActivate() bool {
	if c.Activate != nil {
		return *c.Activate
	}
	return !c.APIKey && !c.APIKeyStdin
}

type ZAIReferenceCmd struct {
	Reference string `arg:"" help:"Account ID or unique label"`
}

type ZAIRemoveCmd struct {
	Reference string `arg:"" optional:"" help:"Account ID or unique label"`
	APIKeys   bool   `name:"api-keys" help:"Remove all stored API-key accounts after a saved browser login; keep browser logins"`
}

func (c ZAIRemoveCmd) Validate() error {
	if c.APIKeys && c.Reference != "" {
		return errors.New("choose one account or --api-keys")
	}
	if !c.APIKeys && c.Reference == "" {
		return errors.New("provide an account ID, unique label or --api-keys")
	}
	return nil
}

type ZAIRouteCmd struct {
	Reference string `arg:"" help:"Account ID or unique label"`
	Transport string `arg:"" enum:"responses,zcode,zcode-start" help:"Upstream route or Start Plan token pool"`
}

type zaiCommandDependencies struct {
	fs         fsutil.FileSystem
	configDir  func() (string, error)
	getenv     func(string) string
	client     httputil.Doer
	input      io.Reader
	output     io.Writer
	errOutput  io.Writer
	promptKey  func() ([]byte, error)
	deviceMid  func() (string, error)
	openURL    func(context.Context, string) error
	oauthLogin func(context.Context, zcode.OAuthClient) (zcode.LoginSession, error)
	invalidate func()
}

func defaultZAICommandDependencies() zaiCommandDependencies {
	return zaiCommandDependencies{
		fs: fsutil.OSFileSystem{},
		configDir: func() (string, error) {
			roots, err := userdirs.Default()
			return roots.Config, err
		},
		getenv:    os.Getenv,
		client:    zaiHTTPClient(10 * time.Second),
		input:     os.Stdin,
		output:    os.Stdout,
		errOutput: os.Stderr,
		deviceMid: zcodeDeviceMid,
		openURL:   openZCodeBrowser,
		oauthLogin: func(ctx context.Context, client zcode.OAuthClient) (zcode.LoginSession, error) {
			return client.Login(ctx)
		},
		promptKey: func() ([]byte, error) {
			if !term.IsTerminal(os.Stdin.Fd()) {
				return nil, errors.New("use --api-key-stdin when standard input is not a terminal")
			}
			fmt.Fprint(os.Stderr, "Z.ai Coding Plan API key: ")
			key, err := term.ReadPassword(os.Stdin.Fd())
			fmt.Fprintln(os.Stderr)
			if err != nil {
				return nil, errors.New("read Z.ai API key")
			}
			return key, nil
		},
		invalidate: func() { invalidateProviderCache(provider.ZAI) },
	}
}

func zaiHTTPClient(timeout time.Duration) *http.Client {
	client := httputil.NewClient(timeout, version)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func runZAICommand(ctx context.Context, command string, options ZAICmd, jsonOutput bool, deps zaiCommandDependencies) error {
	dir, err := deps.configDir()
	if err != nil {
		return errors.New("resolve Z.ai credential directory")
	}
	switch command {
	case "login":
		if err := options.Login.Validate(); err != nil {
			return err
		}
		if !options.Login.APIKey && !options.Login.APIKeyStdin {
			return runZAIOAuthLogin(ctx, options.Login, dir, jsonOutput, deps)
		}
		var raw []byte
		if options.Login.APIKeyStdin {
			raw, err = io.ReadAll(io.LimitReader(deps.input, 4097))
			if err != nil {
				return errors.New("read Z.ai API key from standard input")
			}
		} else {
			raw, err = deps.promptKey()
			if err != nil {
				return err
			}
		}
		if len(raw) > 4096 {
			clear(raw)
			return errors.New("invalid Z.ai API key")
		}
		key := strings.TrimSpace(string(raw))
		clear(raw)
		if key == "" {
			return errors.New("invalid Z.ai API key")
		}
		for _, r := range key {
			if r <= ' ' || r >= 127 {
				return errors.New("invalid Z.ai API key")
			}
		}
		// Validate before saving. Exhausted quota still identifies a valid account.
		results, fetchErr := zaiprov.New(deps.client, func() (string, error) { return key, nil }).Fetch(ctx, time.Now())
		if fetchErr != nil || len(results) != 1 {
			return errors.New("validate Z.ai API key")
		}
		if results[0].Error != nil {
			return fmt.Errorf("validate Z.ai API key: %s", results[0].Error.Code)
		}
		account, err := zaiprov.StoreAccount(deps.fs, dir, key, options.Login.Label, options.Login.shouldActivate())
		if err != nil {
			return err
		}
		deps.invalidate()
		if jsonOutput {
			return json.NewEncoder(deps.output).Encode(account)
		}
		_, err = fmt.Fprintf(deps.output, "Saved Z.ai account %s.\n", account.ID)
		if deps.getenv("ZAI_API_KEY") != "" {
			fmt.Fprintln(deps.output, "ZAI_API_KEY overrides stored accounts while set.")
		}
		return err
	case "accounts":
		accounts, err := zaiprov.ReadAccounts(deps.fs, deps.getenv, dir)
		if err != nil {
			return err
		}
		type accountInfo struct {
			ID        string `json:"id"`
			Label     string `json:"label,omitempty"`
			Preferred bool   `json:"preferred"`
			Transport string `json:"transport"`
		}
		info := make([]accountInfo, 0, len(accounts))
		for i, account := range accounts {
			transport := account.Transport
			if transport == "" {
				transport = "responses"
			}
			info = append(info, accountInfo{ID: account.ID, Label: account.Label, Preferred: i == 0, Transport: transport})
		}
		if jsonOutput {
			return json.NewEncoder(deps.output).Encode(info)
		}
		for _, account := range info {
			marker := " "
			if account.Preferred {
				marker = "*"
			}
			if _, err := fmt.Fprintf(deps.output, "%s %s  %s  [%s]\n", marker, account.ID, account.Label, account.Transport); err != nil {
				return err
			}
		}
		if len(info) == 0 {
			_, err = fmt.Fprintln(deps.output, "No Z.ai accounts. Run cq zai login.")
		}
		return err
	case "switch <reference>":
		err = zaiprov.SetActiveAccount(deps.fs, dir, options.Switch.Reference)
	case "remove", "remove <reference>":
		if err := options.Remove.Validate(); err != nil {
			return err
		}
		if options.Remove.APIKeys {
			err = zaiprov.RemoveAPIKeyAccounts(deps.fs, dir)
		} else {
			err = zaiprov.RemoveAccount(deps.fs, dir, options.Remove.Reference)
		}
	case "route <reference> <transport>":
		err = zaiprov.SetTransport(deps.fs, dir, options.Route.Reference, options.Route.Transport)
	default:
		return errors.New("unknown Z.ai command")
	}
	if err != nil {
		return err
	}
	deps.invalidate()
	if jsonOutput {
		return json.NewEncoder(deps.output).Encode(struct {
			OK bool `json:"ok"`
		}{OK: true})
	}
	_, err = fmt.Fprintln(deps.output, "Z.ai accounts updated.")
	return err
}

func classifyZAIAuthority(args []string) OrdinaryCommandAuthorityV1 {
	var options ZAICmd
	parser, err := kong.New(&options, kong.Writers(io.Discard, io.Discard))
	if err != nil {
		return terminatingOrdinary("ordinary_usage_error")
	}
	parsed, err := parser.Parse(args)
	if err != nil {
		return terminatingOrdinary("ordinary_usage_error")
	}
	fields := strings.Fields(parsed.Command())
	if len(fields) == 0 {
		return terminatingOrdinary("ordinary_usage_error")
	}
	command := fields[0]
	return OrdinaryCommandAuthorityV1{Catalogue: "ordinary", Row: "zai_" + command, Deadline: deadlineForOrdinary("zai_" + command)}
}
