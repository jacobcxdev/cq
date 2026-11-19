package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	"github.com/jacobcxdev/cq/internal/provider/opencodego"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type OpenCodeCmd struct {
	Login  struct{}  `cmd:"" help:"Sign in to OpenCode Go through the browser"`
	Switch SwitchCmd `cmd:"" help:"Choose the preferred OpenCode Go account"`
}

func openCodeSessionAuthority(fsys fsutil.FileSystem, configDir string, client httputil.Doer) *opencodego.SessionAuthority {
	oauth := opencodego.OAuthClient{Client: client}
	return &opencodego.SessionAuthority{FS: fsys, ConfigDir: configDir, Exchange: oauth.Refresh}
}

func openCodeAuthorization(fsys fsutil.FileSystem, configDir string, client httputil.Doer) sharedBudgetAuthorization {
	authority := openCodeSessionAuthority(fsys, configDir, client)
	convert := func(session opencodego.OAuthSession) sharedBudgetLogin {
		return sharedBudgetLogin{
			SharedBudgetCredential: proxy.SharedBudgetCredential{Bearer: session.AccessToken, Account: session.MemberID, WorkspaceID: session.WorkspaceID},
			Email:                  session.Email,
		}
	}
	return sharedBudgetAuthorization{List: func(ctx context.Context, refresh bool) ([]sharedBudgetLogin, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sessions, err := authority.ReadAll()
		if err != nil {
			return nil, err
		}
		logins := make([]sharedBudgetLogin, 0, len(sessions))
		for _, session := range sessions {
			if refresh {
				session, err = authority.FreshFor(ctx, session.MemberID, session.WorkspaceID)
				if err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					continue
				}
			}
			logins = append(logins, convert(session))
		}
		if len(logins) == 0 && refresh {
			return nil, errors.New("OpenCode Go credentials unavailable")
		}
		return logins, nil
	}, For: func(ctx context.Context, scope modelregistry.SharedBudgetCredentialScope) (sharedBudgetLogin, error) {
		sessions, err := authority.ReadAll()
		if err != nil {
			return sharedBudgetLogin{}, err
		}
		for _, session := range sessions {
			login := convert(session)
			if login.scope() != scope {
				continue
			}
			fresh, err := authority.FreshFor(ctx, session.MemberID, session.WorkspaceID)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return sharedBudgetLogin{}, errors.New("OpenCode Go account is no longer available")
				}
				return sharedBudgetLogin{}, err
			}
			return convert(fresh), nil
		}
		return sharedBudgetLogin{}, errors.New("OpenCode Go account is no longer available")
	}, Preferred: func(ctx context.Context) (sharedBudgetLogin, error) {
		if err := ctx.Err(); err != nil {
			return sharedBudgetLogin{}, err
		}
		session, err := authority.Preferred()
		if err != nil {
			return sharedBudgetLogin{}, err
		}
		return convert(session), nil
	}}
}

func runOpenCodeLogin(ctx context.Context) error {
	roots, err := userdirs.Default()
	if err != nil {
		return fmt.Errorf("resolve CQ directories: %w", err)
	}
	fsys := fsutil.OSFileSystem{}
	if err := openCodeOAuthPreference(loadSharedBudgetSettings(fsys, roots.Config, modelregistry.ProviderOpenCodeGo)); err != nil {
		return err
	}
	client := httputil.NewClient(15*time.Second, version)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	oauth := opencodego.OAuthClient{Client: client}
	authority := openCodeSessionAuthority(fsys, roots.Config, client)
	session, err := oauth.Login(ctx, func(verificationURL, userCode string) error {
		return openCodeBrowserPrompt(ctx, os.Stderr, verificationURL, userCode)
	})
	if err != nil {
		return err
	}
	if err := saveOpenCodeGoLogin(ctx, client, authority, session); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Signed in to OpenCode Go as %s.\n", session.Email)
	return nil
}

func runOpenCodeSwitch(ctx context.Context, email string, jsonOutput bool) error {
	roots, err := userdirs.Default()
	if err != nil {
		return fmt.Errorf("resolve CQ directories: %w", err)
	}
	fsys := fsutil.OSFileSystem{}
	if err := openCodeOAuthPreference(loadSharedBudgetSettings(fsys, roots.Config, modelregistry.ProviderOpenCodeGo)); err != nil {
		return err
	}
	authority := opencodego.SessionAuthority{FS: fsys, ConfigDir: roots.Config}
	selected, err := authority.SetPreferred(ctx, email)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("OpenCode Go account not found. Run cq opencode login to add an account")
	}
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(struct {
			Email string `json:"email"`
		}{Email: selected.Email})
	}
	_, err = fmt.Fprintf(os.Stdout, "Preferred OpenCode Go account is %s.\n", selected.Email)
	return err
}

func openCodeOAuthPreference(settings sharedBudgetSettings) error {
	if settings.invalid {
		return errors.New("OpenCode Go settings unavailable")
	}
	if settings.Auth == "api_key" {
		return errors.New("OpenCode Go is set to API-key auth; remove the auth override in opencode-go.json to use OAuth accounts")
	}
	return nil
}

func openCodeBrowserPrompt(ctx context.Context, w io.Writer, verificationURL, userCode string) error {
	if _, err := fmt.Fprintf(w, "Open %s and approve code %s for your OpenCode Go workspace.\n", verificationURL, userCode); err != nil {
		return err
	}
	if err := auth.OpenBrowserContextWithOutput(ctx, verificationURL, w); err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func saveOpenCodeGoLogin(ctx context.Context, client httputil.Doer, authority *opencodego.SessionAuthority, session opencodego.OAuthSession) error {
	if len(opencodego.Usage(ctx, client, session.AccessToken, session.WorkspaceID)) == 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("could not verify included OpenCode Go allowance for the selected workspace")
	}
	return authority.Save(ctx, session)
}

func refreshOpenCodeOAuth(ctx context.Context, client httputil.Doer) error {
	roots, err := resolveRefreshRootsFn()
	if err != nil {
		return err
	}
	fsys := fsutil.OSFileSystem{}
	settings := loadSharedBudgetSettings(fsys, roots.Config, modelregistry.ProviderOpenCodeGo)
	if settings.Auth == "api_key" {
		return nil
	}
	if err := openCodeOAuthPreference(settings); err != nil {
		return err
	}
	authority := openCodeSessionAuthority(fsys, roots.Config, client)
	sessions, err := authority.ReadAll()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures []error
	for _, session := range sessions {
		if _, err := authority.FreshFor(ctx, session.MemberID, session.WorkspaceID); err != nil {
			failures = append(failures, fmt.Errorf("OpenCode Go account %s: %w", session.Email, err))
		}
	}
	return errors.Join(failures...)
}
