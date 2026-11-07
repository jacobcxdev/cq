package dots

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
)

var probeComputerOperation = regexp.MustCompile(`^/backend-api/tbo/([^/]+)/computers/([^/]+)/(connect|connect-and-replace|disconnect)$`)
var probeComputerRemoval = regexp.MustCompile(`^/backend-api/computers/([A-Za-z0-9_-]{1,128})$`)

// Computer removal has no profile ID in its URL. Resolve it from fresh, raw
// owner lists, not projected installation IDs or the selected sidebar row.
func (p *Probe) computerRemovalOwner(ctx context.Context, path string, headers http.Header, f *CatalogueFixture) (*Account, error) {
	match := probeComputerRemoval.FindStringSubmatch(path)
	if match == nil || f == nil {
		return nil, fmt.Errorf("computer ownership unavailable")
	}
	if _, err := p.validateFixture(f); err != nil {
		return nil, err
	}
	accounts := []Account{}
	profiles := map[string][]string{}
	for _, profile := range f.Profiles {
		id := probeString(profile["id"])
		owner, err := p.resourceOwner("/backend-api/tbo/"+url.PathEscape(id), f)
		if err != nil || owner == nil {
			return nil, fmt.Errorf("computer profile ownership unavailable")
		}
		if len(profiles[owner.Alias]) == 0 {
			accounts = append(accounts, *owner)
		}
		profiles[owner.Alias] = append(profiles[owner.Alias], id)
	}
	results := p.collectOwnerResults(ctx, accounts, func(ctx context.Context, owner Account) (any, error) {
		matches := 0
		for _, profile := range profiles[owner.Alias] {
			value, err := p.options.Catalogue.Get(ctx, owner, "/backend-api/tbo/"+url.PathEscape(profile)+"/computers", headers)
			if err != nil {
				return nil, err
			}
			computers, ok := value["computers"].([]any)
			if !ok {
				return nil, fmt.Errorf("computer list unavailable")
			}
			for _, item := range computers {
				computer, ok := item.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("computer list invalid")
				}
				if computer["environment_id"] != match[1] {
					continue
				}
				if computer["kind"] != "user" || computer["can_delete"] != true {
					return nil, fmt.Errorf("computer is not removable")
				}
				matches++
			}
		}
		return matches, nil
	}, true)
	var selected *Account
	for _, account := range accounts {
		result, ok := results[account.Alias]
		count, valid := result.value.(int)
		if !ok || result.err != nil || !valid || count > 1 {
			return nil, fmt.Errorf("computer ownership could not be verified")
		}
		if count == 0 {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("computer owner collision")
		}
		selected = &account
	}
	if selected == nil {
		return nil, fmt.Errorf("computer is not listed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fixture != f {
		return nil, fmt.Errorf("computer ownership changed")
	}
	return selected, nil
}

func probeComputerTarget(ctx context.Context, s *Settings, c *CatalogueClient, path string, body []byte, f *CatalogueFixture) (string, error) {
	parsed, err := url.Parse(path)
	if err != nil || len(body) > 0 || f == nil {
		return "", nil
	}
	match := probeComputerOperation.FindStringSubmatch(parsed.EscapedPath())
	if match == nil {
		return "", nil
	}
	profileID, err := url.PathUnescape(match[1])
	if err != nil {
		return "", nil
	}
	owner, ok := f.Owners[profileID]
	if !ok || owner.Alias == s.SystemAlias {
		return "", nil
	}
	var profile map[string]any
	count := 0
	for _, candidate := range f.Profiles {
		if candidate["id"] == profileID {
			profile = candidate
			count++
		}
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil || count != 1 || len(query) != 1 || len(query["expected_thread_id"]) != 1 || query.Get("expected_thread_id") != profile["active_root_thread_id"] {
		return "", nil
	}
	rootOwner, ok := f.Owners[probeString(profile["active_root_thread_id"])]
	if !ok || !probeEqual(owner, rootOwner) {
		return "", nil
	}
	row, err := SecondaryExecutorMapping(s, owner.Alias)
	if err != nil {
		return "", err
	}
	environment := ""
	if row != nil && row["profile_id"] == profileID && (match[2] == row["environment_id"] || match[2] == row["primary_environment_id"]) {
		environment = probeString(row["environment_id"])
	} else if match[3] == "disconnect" && validExecutorEnvironment(match[2]) {
		value, err := c.Get(ctx, owner, "/backend-api/tbo/"+match[1]+"/computers", nil)
		if err != nil {
			return "", nil
		}
		computers, ok := value["computers"].([]any)
		if !ok {
			return "", nil
		}
		matches := 0
		for _, item := range computers {
			computer, ok := item.(map[string]any)
			if !ok {
				return "", nil
			}
			if computer["kind"] == "user" && computer["environment_id"] == match[2] && computer["can_disconnect"] == true {
				matches++
			}
		}
		if matches != 1 {
			return "", nil
		}
		environment = match[2]
	}
	if environment == "" {
		return "", nil
	}
	return "/backend-api/tbo/" + match[1] + "/computers/" + environment + "/" + match[3] + "?" + parsed.RawQuery, nil
}
