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
	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/provider/grok"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type GrokResetsCmd struct {
	List struct {
		Reference string `arg:"" optional:"" help:"Account ID or unique label; defaults to all accounts"`
	} `cmd:"" help:"List reset reward expiry dates"`
}

type GrokCmd struct {
	Login    GrokLoginCmd     `cmd:"" help:"Sign in to a Grok subscription through the browser"`
	Accounts AccountsCmd      `cmd:"" help:"List Grok subscription accounts"`
	Switch   GrokReferenceCmd `cmd:"" help:"Choose CQ's preferred Grok account"`
	Remove   GrokReferenceCmd `cmd:"" help:"Remove a CQ-owned Grok login"`
	Prime    GrokPrimeCmd     `cmd:"" help:"Prime confirmed dormant subscription windows"`
	Resets   GrokResetsCmd    `cmd:"" help:"Inspect included-quota reset rewards"`
}
type GrokLoginCmd struct {
	Label    string `help:"Custom account label"`
	Activate *bool  `negatable:"" help:"Make this account preferred (default); --no-activate preserves preference"`
}
type GrokReferenceCmd struct {
	Reference string `arg:"" help:"Account ID or unique label"`
}

type grokCommandDependencies struct {
	authority  *grok.SessionAuthority
	discovery  grok.Authority
	login      func(context.Context) (auth.GrokLogin, error)
	validate   func(context.Context, grok.Login) (grok.Login, error)
	output     io.Writer
	invalidate func()
}

func newGrokAuthority(fsys fsutil.FileSystem, configDir string, client httputil.Doer) *grok.SessionAuthority {
	oauth := auth.GrokOAuth{Client: client}
	return grok.NewAuthority(grok.Store{FS: fsys, ConfigDir: configDir}, client, func(ctx context.Context, refreshToken string) (grok.TokenSet, error) {
		tokens, err := oauth.Refresh(ctx, refreshToken)
		if err != nil {
			return grok.TokenSet{}, err
		}
		identity, err := grok.SubscriptionIdentity(ctx, client, tokens.AccessToken)
		if err != nil {
			return grok.TokenSet{}, err
		}
		// The authenticated profile may omit optional principal fields. Keep
		// the token endpoint's hints while checking every field it supplies.
		if identity.PrincipalType == "" {
			identity.PrincipalType = tokens.PrincipalType
		}
		if identity.PrincipalID == "" {
			identity.PrincipalID = tokens.PrincipalID
		}
		if identity.TeamID == "" {
			identity.TeamID = tokens.TeamID
		}
		if tokens.UserID != "" && tokens.UserID != identity.UserID || tokens.PrincipalType != "" && tokens.PrincipalType != identity.PrincipalType || tokens.PrincipalID != "" && tokens.PrincipalID != identity.PrincipalID || tokens.TeamID != "" && tokens.TeamID != identity.TeamID {
			return grok.TokenSet{}, errors.New("Grok refreshed subscription identity mismatch")
		}
		return grok.TokenSet{AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, IDToken: tokens.IDToken, ExpiresAt: tokens.ExpiresAt, UserID: identity.UserID, PrincipalType: identity.PrincipalType, PrincipalID: identity.PrincipalID, TeamID: identity.TeamID}, nil
	})
}

func grokReadAuthority(fsys fsutil.FileSystem, configDir string, client httputil.Doer, env func(string) string, home string) grok.Authority {
	return grok.WithCodexBar(newGrokAuthority(fsys, configDir, client), fsys, env, home, client)
}

func defaultGrokCommandDependencies() (grokCommandDependencies, error) {
	roots, err := userdirs.Default()
	if err != nil {
		return grokCommandDependencies{}, err
	}
	client := grokHTTPClient(30 * time.Second)
	home, err := os.UserHomeDir()
	if err != nil {
		return grokCommandDependencies{}, err
	}
	owned := newGrokAuthority(fsutil.OSFileSystem{}, roots.Config, client)
	oauth := auth.GrokOAuth{Client: client, OpenURL: func(ctx context.Context, raw string) error {
		fmt.Fprintf(os.Stderr, "Open this URL to sign in to Grok:\n%s\n", raw)
		return auth.OpenBrowserContextWithOutput(ctx, raw, os.Stderr)
	}}
	return grokCommandDependencies{authority: owned, discovery: grok.WithCodexBar(owned, fsutil.OSFileSystem{}, os.Getenv, home, client), login: oauth.Login, validate: func(ctx context.Context, login grok.Login) (grok.Login, error) {
		return grok.ValidateLogin(ctx, client, login)
	}, output: os.Stdout, invalidate: func() { invalidateProviderCache(provider.Grok) }}, nil
}

func grokHTTPClient(timeout time.Duration) *http.Client {
	client := httputil.NewClient(timeout, version)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func runGrokCommand(ctx context.Context, command string, options GrokCmd, jsonOutput bool, deps grokCommandDependencies) error {
	if deps.authority == nil {
		return errors.New("Grok credential authority unavailable")
	}
	if deps.output == nil {
		deps.output = io.Discard
	}
	if deps.discovery == nil {
		deps.discovery = deps.authority
	}
	changed := false
	defer func() {
		if changed && deps.invalidate != nil {
			deps.invalidate()
		}
	}()
	switch command {
	case "login":
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if deps.login == nil || deps.validate == nil {
			return errors.New("Grok login unavailable")
		}
		verified, err := deps.login(ctx)
		if err != nil {
			return err
		}
		login := grok.Login{Account: grok.Account{UserID: verified.UserID, Email: verified.Email, PrincipalType: verified.PrincipalType, PrincipalID: verified.PrincipalID, TeamID: verified.TeamID}, AccessToken: verified.Tokens.AccessToken, RefreshToken: verified.Tokens.RefreshToken, IDToken: verified.Tokens.IDToken, ExpiresAt: verified.Tokens.ExpiresAt}
		login, err = deps.validate(ctx, login)
		if err != nil {
			return err
		}
		activate := options.Login.Activate == nil || *options.Login.Activate
		account, err := deps.authority.SaveLogin(ctx, login, options.Login.Label, activate)
		if err != nil {
			return err
		}
		changed = true
		if jsonOutput {
			return json.NewEncoder(deps.output).Encode(account)
		}
		_, err = fmt.Fprintf(deps.output, "Saved Grok subscription account %s.\n", account.ID)
		return err
	case "accounts":
		accounts, err := deps.discovery.Accounts(ctx)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(deps.output).Encode(accounts)
		}
		for _, account := range accounts {
			marker := " "
			if account.Active {
				marker = "*"
			}
			label := account.Label
			if label == "" {
				label = account.Email
			}
			if _, err := fmt.Fprintf(deps.output, "%s %s  %s  %s  %s\n", marker, account.ID, label, account.Plan, account.Source); err != nil {
				return err
			}
		}
		return nil
	case "resets list", "resets list <reference>":
		return runGrokResetList(ctx, options.Resets.List.Reference, jsonOutput, deps.discovery, deps.output)
	case "switch <reference>":
		if err := deps.authority.SetPreferred(ctx, options.Switch.Reference); err != nil {
			return err
		}
		changed = true
	case "remove <reference>":
		if err := deps.authority.Remove(ctx, options.Remove.Reference); err != nil {
			return err
		}
		changed = true
	default:
		return errors.New("unknown Grok command")
	}
	if jsonOutput {
		return json.NewEncoder(deps.output).Encode(struct {
			OK bool `json:"ok"`
		}{true})
	}
	return nil
}

func classifyGrokAuthority(args []string) OrdinaryCommandAuthorityV1 {
	var options GrokCmd
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
	row := "grok_" + fields[0]
	return OrdinaryCommandAuthorityV1{Catalogue: "ordinary", Row: row, Deadline: deadlineForOrdinary(row)}
}
