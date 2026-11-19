package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	"github.com/jacobcxdev/cq/internal/provider/opencodego"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/quota"
)

type sharedBudgetLogin struct {
	proxy.SharedBudgetCredential
	Email string
}

type sharedBudgetAuthorization struct {
	List func(context.Context, bool) ([]sharedBudgetLogin, error)
	For  func(context.Context, modelregistry.SharedBudgetCredentialScope) (sharedBudgetLogin, error)
}

func (a sharedBudgetAuthorization) first(ctx context.Context, refresh bool) (sharedBudgetLogin, error) {
	logins, err := a.List(ctx, refresh)
	if err != nil || len(logins) == 0 {
		return sharedBudgetLogin{}, err
	}
	return logins[0], nil
}

func (l sharedBudgetLogin) scope() modelregistry.SharedBudgetCredentialScope {
	credential := l.routeCredential()
	return modelregistry.FingerprintSharedBudgetCredential(credential.Bearer, credential.Account, credential.WorkspaceID)
}

func (l sharedBudgetLogin) routeCredential() proxy.SharedBudgetCredential {
	credential := l.SharedBudgetCredential
	if credential.Account == "" {
		credential.WorkspaceID = ""
	}
	return credential
}

type sharedBudgetAdapter struct {
	ID                     modelregistry.Provider
	Name                   string
	Plan                   string
	Icon                   string
	KeyEnv                 string
	ResponsesURL           string
	SessionHeader          string
	WorkspaceHeader        string
	OAuth                  func(fsutil.FileSystem, string, httputil.Doer) sharedBudgetAuthorization
	Catalogue              func(httputil.Doer, sharedBudgetAuthorization) modelregistry.NativeSource
	CatalogueRequiresFresh bool
	OmitStreamOptions      bool
	OmitEmptyCallIDOutputs bool
	PlaintextCollaboration bool
	Usage                  func(context.Context, httputil.Doer, string, string) []quota.SharedBudgetWindow
}

// sharedBudgetAdapters is ordered after the first-party registry sources.
var sharedBudgetAdapters = []sharedBudgetAdapter{
	{
		ID: modelregistry.ProviderOpenCodeGo, Name: "OpenCode", Plan: "Go", Icon: "\U000F0C15",
		KeyEnv: "OPENCODE_API_KEY", ResponsesURL: modelregistry.OpenCodeGoBaseURL + "/openai/v1/responses",
		SessionHeader: "X-OpenCode-Session", WorkspaceHeader: "X-OpenCode-Org-Id",
		OAuth: openCodeAuthorization, Usage: opencodego.Usage,
		CatalogueRequiresFresh: true,
		OmitStreamOptions:      true,
		OmitEmptyCallIDOutputs: true,
		PlaintextCollaboration: true,
		Catalogue: func(client httputil.Doer, authorize sharedBudgetAuthorization) modelregistry.NativeSource {
			return &modelregistry.OpenCodeGoSource{
				Client: client,
				Credentials: func(ctx context.Context) ([]modelregistry.OpenCodeGoCredential, error) {
					logins, err := authorize.List(ctx, true)
					if err != nil {
						return nil, err
					}
					credentials := make([]modelregistry.OpenCodeGoCredential, 0, len(logins))
					for _, login := range logins {
						credential := login.routeCredential()
						credentials = append(credentials, modelregistry.OpenCodeGoCredential{Bearer: credential.Bearer, Account: credential.Account, WorkspaceID: credential.WorkspaceID})
					}
					return credentials, nil
				},
			}
		},
	},
}

func sharedBudgetKey(fsys fsutil.FileSystem, env func(string) string, configDir string, id modelregistry.Provider, envName string) (string, error) {
	key := strings.TrimSpace(env(envName))
	if key == "" && configDir != "" {
		data, err := fsutil.ReadSecureFile(fsys, filepath.Join(configDir, string(id)+".key"), 4096)
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", errors.New("subscription key file must be private and readable")
		}
		key = string(bytes.TrimSpace(data))
		clear(data)
	}
	if len(key) > 4096 || strings.IndexFunc(key, func(r rune) bool { return r <= ' ' || r >= 127 }) >= 0 {
		return "", errors.New("invalid subscription API key")
	}
	return key, nil
}

// resolve uses the login record for discovery without rotating credentials.
// A failed or expired OAuth login still owns the selected route.
func (a sharedBudgetAdapter) resolve(fsys fsutil.FileSystem, env func(string) string, configDir string, client httputil.Doer, settings sharedBudgetSettings) sharedBudgetAuthorization {
	if settings.invalid {
		invalid := func() error { return fmt.Errorf("%s subscription settings unavailable", a.Name) }
		return sharedBudgetAuthorization{
			List: func(context.Context, bool) ([]sharedBudgetLogin, error) { return nil, invalid() },
			For: func(context.Context, modelregistry.SharedBudgetCredentialScope) (sharedBudgetLogin, error) {
				return sharedBudgetLogin{}, invalid()
			},
		}
	}
	var oauth sharedBudgetAuthorization
	if a.OAuth != nil && settings.Auth != "api_key" {
		oauth = a.OAuth(fsys, configDir, client)
	}
	keyLogin := func() (sharedBudgetLogin, error) {
		key, err := sharedBudgetKey(fsys, env, configDir, a.ID, a.KeyEnv)
		if err != nil {
			return sharedBudgetLogin{}, err
		}
		return sharedBudgetLogin{SharedBudgetCredential: proxy.SharedBudgetCredential{Bearer: key, WorkspaceID: settings.WorkspaceID}, Email: settings.Email}, nil
	}
	return sharedBudgetAuthorization{List: func(ctx context.Context, refresh bool) ([]sharedBudgetLogin, error) {
		if oauth.List != nil {
			logins, err := oauth.List(ctx, refresh)
			if !errors.Is(err, os.ErrNotExist) {
				return logins, err
			}
		}
		login, err := keyLogin()
		if err != nil {
			return nil, err
		}
		return []sharedBudgetLogin{login}, nil
	}, For: func(ctx context.Context, scope modelregistry.SharedBudgetCredentialScope) (sharedBudgetLogin, error) {
		if oauth.For != nil {
			login, err := oauth.For(ctx, scope)
			if !errors.Is(err, os.ErrNotExist) {
				return login, err
			}
		}
		login, err := keyLogin()
		if err != nil {
			return sharedBudgetLogin{}, err
		}
		if login.Bearer == "" || login.scope() != scope {
			return sharedBudgetLogin{}, os.ErrNotExist
		}
		return login, nil
	}}
}

func sharedBudgetProxyRoutes(fsys fsutil.FileSystem, env func(string) string, configDir string) map[modelregistry.Provider]proxy.SharedBudgetRoute {
	routes := make(map[modelregistry.Provider]proxy.SharedBudgetRoute, len(sharedBudgetAdapters))
	transport := httputil.NewClient(0, version).Transport
	client := httputil.NewClient(15*time.Second, version)
	for _, adapter := range sharedBudgetAdapters {
		settings := loadSharedBudgetSettings(fsys, configDir, adapter.ID)
		resolve := adapter.resolve(fsys, env, configDir, client, settings)
		routes[adapter.ID] = proxy.SharedBudgetRoute{
			Name: adapter.Name + " " + adapter.Plan, URL: adapter.ResponsesURL, SessionHeader: adapter.SessionHeader,
			WorkspaceHeader: adapter.WorkspaceHeader,
			Transport:       transport, AllowFallback: settings.AllowFallback,
			RequireCatalogueScope:  adapter.CatalogueRequiresFresh,
			OmitStreamOptions:      adapter.OmitStreamOptions,
			OmitEmptyCallIDOutputs: adapter.OmitEmptyCallIDOutputs,
			PlaintextCollaboration: adapter.PlaintextCollaboration,
			Credential: func(ctx context.Context) (proxy.SharedBudgetCredential, error) {
				login, err := resolve.first(ctx, true)
				return login.routeCredential(), err
			},
			Credentials: func(ctx context.Context) ([]proxy.SharedBudgetCredential, error) {
				logins, err := resolve.List(ctx, true)
				if err != nil {
					return nil, err
				}
				credentials := make([]proxy.SharedBudgetCredential, 0, len(logins))
				for _, login := range logins {
					credentials = append(credentials, login.routeCredential())
				}
				return credentials, nil
			},
			CredentialFor: func(ctx context.Context, scope modelregistry.SharedBudgetCredentialScope) (proxy.SharedBudgetCredential, error) {
				login, err := resolve.For(ctx, scope)
				return login.routeCredential(), err
			},
		}
	}
	return routes
}

type sharedBudgetSettings struct {
	Auth          string `json:"auth"`
	Email         string `json:"email"`
	WorkspaceID   string `json:"workspace_id"`
	ExtraUsage    *bool  `json:"extra_usage"`
	AllowFallback bool   `json:"allow_fallback"`
	invalid       bool
}

func loadSharedBudgetSettings(fsys fsutil.FileSystem, configDir string, id modelregistry.Provider) sharedBudgetSettings {
	var settings sharedBudgetSettings
	path := filepath.Join(configDir, string(id)+".json")
	// The config root may still have inherited permissions on a fresh install.
	// Check for absence before requiring a private directory to read settings.
	if inspector, ok := fsys.(fsutil.SecurePathInspector); ok {
		if _, err := inspector.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return settings
		}
	}
	data, err := fsutil.ReadSecureFile(fsys, path, 4096)
	if errors.Is(err, os.ErrNotExist) {
		return settings
	}
	if err == nil {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if json.Valid(data) {
			err = decoder.Decode(&settings)
		} else {
			err = errors.New("invalid JSON")
		}
		clear(data)
	}
	if settings.Auth != "" && settings.Auth != "api_key" {
		err = errors.New("invalid authentication method")
	}
	if settings.Email != "" {
		address, parseErr := mail.ParseAddress(settings.Email)
		if parseErr != nil || address.Address != settings.Email || strings.IndexFunc(settings.Email, unicode.IsControl) >= 0 {
			err = errors.New("invalid account email")
		}
	}
	if strings.IndexFunc(settings.WorkspaceID, func(r rune) bool { return r <= ' ' || r >= 127 }) >= 0 {
		err = errors.New("invalid workspace ID")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "cq: %s settings unavailable; check JSON fields and file permissions\n", id)
		return sharedBudgetSettings{invalid: true}
	}
	return settings
}

func sharedBudgetChecks(fsys fsutil.FileSystem, env func(string) string, configDir string, selected []string) ([]string, []app.SharedBudgetCheck) {
	providers := make([]string, 0, len(selected))
	sharedIDs := make(map[string]bool, len(sharedBudgetAdapters))
	var checks []app.SharedBudgetCheck
	client := httputil.NewClient(10*time.Second, version)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, adapter := range sharedBudgetAdapters {
		id := string(adapter.ID)
		sharedIDs[id] = true
		if len(selected) > 0 && !slices.Contains(selected, id) {
			continue
		}
		settings := loadSharedBudgetSettings(fsys, configDir, adapter.ID)
		resolve := adapter.resolve(fsys, env, configDir, client, settings)
		logins, err := resolve.List(context.Background(), false)
		if len(selected) == 0 && (err != nil || len(logins) == 0) {
			continue
		}
		if len(logins) == 0 {
			logins = []sharedBudgetLogin{{}}
		}
		for _, login := range logins {
			if len(selected) == 0 && login.Bearer == "" {
				continue
			}
			email := login.Email
			if login.Account == "" {
				email = settings.Email
			}
			report := quota.SharedBudgetReport{
				ProviderID: id, ProviderName: adapter.Name, Plan: adapter.Plan, Icon: adapter.Icon, Email: email,
			}
			if settings.ExtraUsage != nil {
				report.ExtraUsage = &quota.ExtraCredits{Enabled: *settings.ExtraUsage}
			}
			checks = append(checks, app.SharedBudgetCheck{
				Report: report,
				Fetch: func(ctx context.Context) []quota.SharedBudgetWindow {
					if adapter.Usage == nil {
						return nil
					}
					current, err := resolve.For(ctx, login.scope())
					if err != nil || current.Bearer == "" || current.Account != login.Account || current.WorkspaceID != login.WorkspaceID ||
						current.Account == "" && current.Bearer != login.Bearer {
						return nil
					}
					return adapter.Usage(ctx, client, current.Bearer, current.WorkspaceID)
				},
			})
		}
	}
	for _, id := range selected {
		if !sharedIDs[id] {
			providers = append(providers, id)
		}
	}
	return providers, checks
}
