package main

import (
	"context"
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
	Login struct{} `cmd:"" help:"Sign in to OpenCode Go through the browser"`
}

func openCodeSessionAuthority(fsys fsutil.FileSystem, configDir string, client httputil.Doer) *opencodego.SessionAuthority {
	oauth := opencodego.OAuthClient{Client: client}
	return &opencodego.SessionAuthority{FS: fsys, ConfigDir: configDir, Exchange: oauth.Refresh}
}

func openCodeAuthorization(fsys fsutil.FileSystem, configDir string, client httputil.Doer) sharedBudgetAuthorization {
	authority := openCodeSessionAuthority(fsys, configDir, client)
	return func(ctx context.Context, refresh bool) (sharedBudgetLogin, error) {
		if err := ctx.Err(); err != nil {
			return sharedBudgetLogin{}, err
		}
		read := authority.Read
		if refresh {
			read = func() (opencodego.OAuthSession, error) { return authority.Fresh(ctx) }
		}
		session, err := read()
		if err != nil {
			return sharedBudgetLogin{}, err
		}
		return sharedBudgetLogin{
			SharedBudgetCredential: proxy.SharedBudgetCredential{
				Bearer: session.AccessToken, Account: session.MemberID, WorkspaceID: session.WorkspaceID,
			},
			Email: session.Email,
		}, nil
	}
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

func openCodeOAuthPreference(settings sharedBudgetSettings) error {
	if settings.invalid {
		return errors.New("OpenCode Go settings unavailable")
	}
	if settings.Auth == "api_key" {
		return errors.New("OpenCode Go is set to API-key auth; remove the auth override in opencode-go.json to sign in")
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
	_, err = openCodeSessionAuthority(fsys, roots.Config, client).Fresh(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
