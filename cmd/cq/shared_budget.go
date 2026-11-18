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

type sharedBudgetAuthorization func(context.Context, bool) (sharedBudgetLogin, error)

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
		Catalogue: func(client httputil.Doer, authorize sharedBudgetAuthorization) modelregistry.NativeSource {
			return &modelregistry.OpenCodeGoSource{
				Client: client,
				Credential: func(ctx context.Context) (string, string, string, error) {
					login, err := authorize(ctx, true)
					if login.Account == "" {
						login.SharedBudgetCredential.WorkspaceID = ""
					}
					return login.Bearer, login.Account, login.WorkspaceID, err
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
		return func(context.Context, bool) (sharedBudgetLogin, error) {
			return sharedBudgetLogin{}, fmt.Errorf("%s subscription settings unavailable", a.Name)
		}
	}
	var oauth sharedBudgetAuthorization
	if a.OAuth != nil && settings.Auth != "api_key" {
		oauth = a.OAuth(fsys, configDir, client)
	}
	return func(ctx context.Context, refresh bool) (sharedBudgetLogin, error) {
		if oauth != nil {
			login, err := oauth(ctx, refresh)
			if !errors.Is(err, os.ErrNotExist) {
				return login, err
			}
		}
		key, err := sharedBudgetKey(fsys, env, configDir, a.ID, a.KeyEnv)
		if err != nil {
			return sharedBudgetLogin{}, err
		}
		return sharedBudgetLogin{
			SharedBudgetCredential: proxy.SharedBudgetCredential{Bearer: key, WorkspaceID: settings.WorkspaceID},
			Email:                  settings.Email,
		}, nil
	}
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
			Credential: func(ctx context.Context) (proxy.SharedBudgetCredential, error) {
				login, err := resolve(ctx, true)
				if login.Account == "" {
					login.SharedBudgetCredential.WorkspaceID = ""
				}
				return login.SharedBudgetCredential, err
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
		login, err := resolve(context.Background(), false)
		if len(selected) == 0 && (err != nil || login.Bearer == "") {
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
				current, err := resolve(ctx, true)
				if err != nil || current.Bearer == "" || current.Account != login.Account || current.WorkspaceID != login.WorkspaceID ||
					current.Account == "" && current.Bearer != login.Bearer {
					return nil
				}
				return adapter.Usage(ctx, client, current.Bearer, current.WorkspaceID)
			},
		})
	}
	for _, id := range selected {
		if !sharedIDs[id] {
			providers = append(providers, id)
		}
	}
	return providers, checks
}
