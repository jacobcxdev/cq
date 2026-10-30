package dots

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const NativeUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/155.0.0.0 Safari/537.36"

// CatalogueClient reads owner credentials and native profiles without refreshing them.
type CatalogueClient struct {
	Settings *Settings
	HTTP     httputil.Doer
	Origin   string
	Now      func() time.Time
}

func (c *CatalogueClient) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

type CatalogueHTTPError struct{ Status int }

func (e *CatalogueHTTPError) Error() string { return fmt.Sprintf("catalog HTTP status %d", e.Status) }

type DiscoveredDot struct {
	Dot
	Handle string `json:"handle"`
}

type Discovery struct {
	Dots     []DiscoveredDot  `json:"dots"`
	Accounts []AccountOutcome `json:"accounts"`
}

type RootOwner struct {
	OwnerAlias string
	ProfileID  string
}

type Catalogue struct {
	System   Account
	Profiles []map[string]any
	Owners   map[string]Account
	Summary  []map[string]any
}

type CatalogueFixture = Catalogue

// Get substitutes only the selected owner's identity and bounds every response.
func (c *CatalogueClient) Get(ctx context.Context, account Account, path string, headers http.Header) (map[string]any, error) {
	origin := c.Origin
	if origin == "" {
		origin = "https://chatgpt.com"
	}
	base, err := url.Parse(origin)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Scheme != "https" && base.Scheme != "http") || base.Path != "" && base.Path != "/" {
		return nil, fmt.Errorf("invalid catalogue origin")
	}
	relative, err := url.Parse(path)
	if err != nil || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || relative.IsAbs() || relative.Host != "" || relative.Fragment != "" {
		return nil, fmt.Errorf("invalid catalogue path")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base.ResolveReference(relative).String(), nil)
	if err != nil {
		return nil, fmt.Errorf("invalid catalogue request")
	}
	for key, values := range headers {
		switch strings.ToLower(key) {
		case "authorization", "chatgpt-account-id", "x-openai-account-routing-override", "x-openai-expected-account-id", "cookie", "x-openai-orbit-selection", "host", "content-length", "connection":
			continue
		}
		request.Header[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
	}
	if request.Header.Get("User-Agent") == "" {
		request.Header.Set("User-Agent", NativeUserAgent)
	}
	request.Header.Set("Authorization", "Bearer "+account.Token)
	request.Header.Set("ChatGPT-Account-Id", account.AccountID)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	client := c.HTTP
	if client == nil {
		bounded := probeHTTPClient()
		bounded.Timeout = 20 * time.Second
		client = bounded
	}
	response, err := client.Do(request)
	if err != nil {
		// Transport errors can contain URLs or headers; do not expose credential-bearing details.
		return nil, fmt.Errorf("catalog request failed")
	}
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("catalog response unavailable")
	}
	defer response.Body.Close()
	body, err := httputil.ReadBody(response.Body)
	if err != nil {
		return nil, fmt.Errorf("catalog response unavailable: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, &CatalogueHTTPError{response.StatusCode}
	}
	var value map[string]any
	if json.Unmarshal(body, &value) != nil || value == nil {
		return nil, fmt.Errorf("invalid catalog response")
	}
	return value, nil
}

// Discover returns safe availability outcomes while preserving ownership collisions as errors.
func (c *CatalogueClient) Discover(ctx context.Context) (Discovery, error) {
	inventory, err := c.Inventory()
	if err != nil {
		return Discovery{}, err
	}
	result := Discovery{Dots: []DiscoveredDot{}, Accounts: []AccountOutcome{}}
	profiles, roots, healthy := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, account := range inventory.Accounts {
		healthy[account.AccountID] = true
		outcome := AccountOutcome{OwnerAlias: account.Alias, OwnerAccountID: account.AccountID, System: account.AccountID == inventory.System.AccountID}
		value, fetchErr := c.Get(ctx, account, "/backend-api/tbo/primary", nil)
		if fetchErr == nil {
			selection, selectionOK := value["selection"].(map[string]any)
			profile, profileOK := value["profile"].(map[string]any)
			if value["selection"] == nil || selectionOK && selection["available"] == false {
				outcome.Status = "no_dot"
			} else if !selectionOK || selection["available"] != true || !profileOK {
				fetchErr = fmt.Errorf("invalid owner primary response")
			} else {
				id, idOK := profile["id"].(string)
				root, rootOK := profile["active_root_thread_id"].(string)
				name, nameOK := profile["display_name"].(string)
				if !idOK || !validText(id, 128) || !rootOK || !validText(root, 128) || selection["thread_id"] != root || !nameOK || !validText(name, 80) {
					fetchErr = fmt.Errorf("invalid owner primary identity")
				} else if profile["status"] == "deleting" {
					outcome.Status = "no_dot"
				} else {
					if profiles[id] || roots[root] {
						return Discovery{}, &IdentityCollision{"native profile ownership collision"}
					}
					profiles[id], roots[root] = true, true
					handle := StableHandle(account.AccountID, id)
					result.Dots = append(result.Dots, DiscoveredDot{Dot: Dot{OwnerAlias: account.Alias, OwnerAccountID: account.AccountID, ProfileID: id, RootThreadID: root, DisplayName: name}, Handle: handle})
					outcome.Status, outcome.Handle = "ready", handle
				}
			}
		}
		if fetchErr != nil {
			outcome.Status, outcome.FailureType = "unavailable", "ValueError"
			var httpErr *CatalogueHTTPError
			if errors.As(fetchErr, &httpErr) {
				outcome.FailureType, outcome.HTTPStatus = "CatalogHTTPError", httpErr.Status
			}
		}
		result.Accounts = append(result.Accounts, outcome)
	}
	for _, failure := range inventory.Failures {
		if !healthy[failure.OwnerAccountID] {
			result.Accounts = append(result.Accounts, failure)
		}
	}
	return result, nil
}

func (c *CatalogueClient) Catalogue(ctx context.Context, account Account, headers http.Header) ([]map[string]any, error) {
	items := []map[string]any{}
	cursor, seen := "", map[string]bool{}
	for page := 0; page < 20; page++ {
		query := url.Values{"limit": {"25"}, "include_room_preview": {"false"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		value, err := c.Get(ctx, account, "/backend-api/tbo?"+query.Encode(), headers)
		if err != nil {
			return nil, err
		}
		rows, ok := value["items"].([]any)
		if !ok {
			return nil, fmt.Errorf("invalid catalog items")
		}
		for _, row := range rows {
			item, ok := row.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid catalog item")
			}
			items = append(items, item)
		}
		if value["cursor"] == nil {
			return items, nil
		}
		cursor, ok = value["cursor"].(string)
		if !ok || cursor == "" {
			return nil, fmt.Errorf("invalid catalog cursor")
		}
		if seen[cursor] {
			return nil, fmt.Errorf("catalog cursor repeated")
		}
		seen[cursor] = true
	}
	return nil, fmt.Errorf("catalog pagination exceeds proof scope")
}

func (c *CatalogueClient) PrimaryRoots(ctx context.Context, accounts map[string]Account, headers http.Header) (map[string]RootOwner, error) {
	if c.Settings == nil {
		return nil, fmt.Errorf("Dots settings unavailable")
	}
	profiles := c.Settings.ProfileIDs()
	aliases := make([]string, 0, len(profiles))
	for alias := range profiles {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	roots := map[string]RootOwner{}
	for _, alias := range aliases {
		account, exists := accounts[alias]
		if !exists || account.Alias != alias {
			return nil, fmt.Errorf("primary principal mismatch")
		}
		value, err := c.Get(ctx, account, "/backend-api/tbo/primary", headers)
		if err != nil {
			return nil, err
		}
		selection, _ := value["selection"].(map[string]any)
		profile, _ := value["profile"].(map[string]any)
		if selection["available"] != true || profile["id"] != profiles[alias] {
			return nil, fmt.Errorf("primary profile outside proof scope")
		}
		root, ok := profile["active_root_thread_id"].(string)
		if !ok || !validText(root, 128) || selection["thread_id"] != root {
			return nil, fmt.Errorf("primary root mismatch")
		}
		if _, exists := roots[root]; exists {
			return nil, fmt.Errorf("primary root ownership collision")
		}
		roots[root] = RootOwner{OwnerAlias: alias, ProfileID: profiles[alias]}
	}
	return roots, nil
}

func (c *CatalogueClient) Fixture(ctx context.Context, headers http.Header, primaryItems []map[string]any) (*Catalogue, error) {
	inventory, err := c.Inventory()
	if err != nil {
		return nil, err
	}
	result := &Catalogue{System: inventory.System, Profiles: []map[string]any{}, Owners: map[string]Account{}, Summary: []map[string]any{}}
	profiles := c.Settings.ProfileIDs()
	for _, account := range inventory.Accounts {
		profileID, exists := profiles[account.Alias]
		if !exists {
			continue
		}
		items := primaryItems
		isSystem := account.AccountID == inventory.System.AccountID
		if !isSystem {
			items, err = c.Catalogue(ctx, account, headers)
			if err != nil {
				return nil, err
			}
		}
		summary := []map[string]any{}
		for _, profile := range items {
			if profile["id"] != profileID {
				continue
			}
			name, ok := profile["display_name"].(string)
			if !ok {
				return nil, fmt.Errorf("invalid catalog profile name")
			}
			summary = append(summary, map[string]any{"name": name, "root_present": profile["active_root_thread_id"] != nil, "room_present": profile["messaging_room_id"] != nil, "status": profile["status"]})
			if profile["status"] == "deleting" {
				continue
			}
			for _, field := range []string{"id", "active_root_thread_id", "messaging_room_id"} {
				if profile[field] == nil {
					continue
				}
				key, ok := profile[field].(string)
				if !ok || key == "" {
					return nil, fmt.Errorf("invalid catalog resource identity")
				}
				if previous, exists := result.Owners[key]; exists && previous.AccountID != account.AccountID {
					return nil, fmt.Errorf("resource ownership collision")
				}
				result.Owners[key] = account
			}
			result.Profiles = append(result.Profiles, profile)
		}
		result.Summary = append(result.Summary, map[string]any{"owner_alias": account.Alias, "system": isSystem, "requested_secondary": !isSystem, "profiles": summary})
	}
	return result, nil
}
