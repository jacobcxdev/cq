package dots

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func probeChallengeValue(data []byte) (string, time.Duration, error) {
	v, err := probeObject(data)
	if err != nil {
		return "", 0, err
	}
	challenge, ok := v["attestation_challenge"].(string)
	if !ok || challenge == "" || len(challenge) > 65536 {
		return "", 0, fmt.Errorf("invalid native challenge")
	}
	expiry := 540.0
	if raw, exists := v["expire_after"]; exists && raw != nil {
		var ok bool
		expiry, ok = raw.(float64)
		if !ok || expiry <= 0 {
			return "", 0, fmt.Errorf("invalid native challenge expiry")
		}
	}
	if expiry > 540 {
		expiry = 540
	}
	return challenge, time.Duration(expiry * float64(time.Second)), nil
}

func (p *Probe) rememberChallenge(primary, secondary []byte, alias string, now time.Time) error {
	first, a, err := probeChallengeValue(primary)
	if err != nil {
		return err
	}
	second, b, err := probeChallengeValue(secondary)
	if err != nil {
		return err
	}
	if b < a {
		a = b
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, value := range p.challenges {
		if !value.expires.After(now) {
			delete(p.challenges, key)
		}
	}
	key := first + "\x00" + alias
	if _, exists := p.challenges[key]; exists {
		return fmt.Errorf("duplicate native challenge")
	}
	aliases := map[string]bool{}
	for _, dot := range p.settings.Dots {
		aliases[dot.OwnerAlias] = true
	}
	limit := 16 * (len(aliases) - 1)
	if limit < 16 {
		limit = 16
	}
	if len(p.challenges) >= limit {
		oldest := ""
		var expires time.Time
		for key, value := range p.challenges {
			if oldest == "" || value.expires.Before(expires) {
				oldest, expires = key, value.expires
			}
		}
		delete(p.challenges, oldest)
	}
	p.challenges[key] = probeChallenge{second, now.Add(a)}
	return nil
}

func (p *Probe) ownerMessage(body []byte, alias string) ([]byte, error) {
	v, err := probeObject(body)
	if err != nil {
		return nil, err
	}
	challenge := probeString(v["app_attest_challenge"])
	if challenge == "" {
		return nil, fmt.Errorf("native verification challenge missing")
	}
	p.mu.Lock()
	key := challenge + "\x00" + alias
	replacement, ok := p.challenges[key]
	delete(p.challenges, key)
	p.mu.Unlock()
	if !ok || !replacement.expires.After(p.options.Now()) {
		return nil, fmt.Errorf("owner verification challenge unavailable")
	}
	v["app_attest_challenge"] = replacement.value
	return json.Marshal(v)
}

func (p *Probe) rememberDownload(raw string, owner Account) error {
	if len(raw) > 8192 {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "chatgpt.com" || parsed.Fragment != "" || parsed.User != nil || !probeDownload.MatchString(parsed.EscapedPath()) {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.options.Now()
	for key, grant := range p.files {
		if !grant.expires.After(now) {
			delete(p.files, key)
		}
	}
	if old, ok := p.files[parsed.Path]; ok && (old.alias != owner.Alias || old.account != owner.AccountID) {
		return fmt.Errorf("file download owner collision")
	}
	if len(p.files) >= 2048 {
		oldest := ""
		order := ^uint64(0)
		for key, grant := range p.files {
			if grant.order < order {
				oldest, order = key, grant.order
			}
		}
		delete(p.files, oldest)
	}
	p.fileOrder++
	p.files[parsed.Path] = probeFile{owner.Alias, owner.AccountID, now.Add(1500 * time.Second), p.fileOrder}
	return nil
}

// downloadOwner is called with the snapshot lock held. URLs never provide credentials.
func (p *Probe) downloadOwner(path string, f *CatalogueFixture) *Account {
	grant, ok := p.files[path]
	if !ok {
		return nil
	}
	if !grant.expires.After(p.options.Now()) {
		delete(p.files, path)
		return nil
	}
	if f == nil {
		return nil
	}
	for _, profile := range f.Profiles {
		owner, ok := f.Owners[probeString(profile["id"])]
		if ok && owner.Alias == grant.alias && owner.AccountID == grant.account {
			return &owner
		}
	}
	return nil
}

func (p *Probe) observePrimary(ctx context.Context, headers http.Header, data []byte) error {
	v, err := probeObject(data)
	if err != nil {
		return err
	}
	selection, _ := v["selection"].(map[string]any)
	if selection["available"] != true {
		return nil
	}
	if err := p.options.RequireNative(ctx, headers); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.fixture
	if f == nil {
		return nil
	}
	if _, err := p.validateFixture(f); err != nil {
		return err
	}
	var expected map[string]any
	count := 0
	for _, profile := range f.Profiles {
		if f.Owners[probeString(profile["id"])].Alias == p.settings.SystemAlias {
			count++
			expected = profile
		}
	}
	returned, _ := v["profile"].(map[string]any)
	root := probeString(selection["thread_id"])
	if count != 1 || returned == nil || returned["id"] != expected["id"] || root == "" || len(root) > 128 || returned["active_root_thread_id"] != root {
		return fmt.Errorf("native primary differs from configured owner profile")
	}
	updated := map[string]any{}
	for key, value := range expected {
		updated[key] = value
	}
	for key, value := range returned {
		updated[key] = value
	}
	owners := map[string]Account{}
	for key, owner := range f.Owners {
		if owner.Alias != p.settings.SystemAlias {
			owners[key] = owner
		}
	}
	for _, field := range []string{"id", "active_root_thread_id", "messaging_room_id"} {
		if raw := updated[field]; raw != nil && raw != "" {
			id, ok := raw.(string)
			if !ok {
				return fmt.Errorf("native resource identity invalid")
			}
			if _, exists := owners[id]; exists {
				return fmt.Errorf("native resource ownership collision")
			}
			owners[id] = f.System
		}
	}
	profiles := make([]map[string]any, len(f.Profiles))
	for index, profile := range f.Profiles {
		profiles[index] = profile
		if profile["id"] == updated["id"] {
			profiles[index] = updated
		}
	}
	next := &CatalogueFixture{System: f.System, Profiles: profiles, Owners: owners, Summary: f.Summary}
	if err := p.publishRoots(next); err != nil {
		return err
	}
	p.fixture = next
	p.generation++
	return nil
}

type probeOwnerResult struct {
	alias string
	value any
	err   error
}

func (p *Probe) ownerResults(ctx context.Context, accounts []Account, operation func(context.Context, Account) (any, error)) map[string]probeOwnerResult {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	results := make(chan probeOwnerResult, len(accounts))
	pending := map[string]bool{}
	completed := map[string]probeOwnerResult{}
	for _, account := range accounts {
		select {
		case p.workers <- struct{}{}:
			pending[account.Alias] = true
			go func(account Account) {
				defer func() { <-p.workers }()
				result := probeOwnerResult{alias: account.Alias}
				defer func() {
					if recover() != nil {
						result.err = fmt.Errorf("owner worker panic")
					}
					results <- result
				}()
				result.value, result.err = operation(ctx, account)
			}(account)
		default:
			completed[account.Alias] = probeOwnerResult{alias: account.Alias, err: fmt.Errorf("owner workers unavailable")}
		}
	}
	for len(pending) > 0 {
		select {
		case result := <-results:
			if ctx.Err() == nil {
				delete(pending, result.alias)
				completed[result.alias] = result
			}
		case <-ctx.Done():
			for alias := range pending {
				completed[alias] = probeOwnerResult{alias: alias, err: fmt.Errorf("owner deadline exceeded")}
			}
			return completed
		}
	}
	return completed
}

func (p *Probe) transform(r *http.Request, response *http.Response, data []byte, owner *Account, f *CatalogueFixture, generation uint64, message, inspected bool) ([]byte, error) {
	path := r.URL.EscapedPath()
	native := p.settings.IntegrationMode == "native"
	secondary := owner != nil && owner.Alias != p.settings.SystemAlias
	if !native && response.StatusCode == 200 && ((r.Method == "GET" && path == "/backend-api/tbo/primary") || path == "/backend-api/celsius/ws/user") {
		p.mu.Lock()
		present := p.fixture != nil
		p.mu.Unlock()
		if !present {
			inventory, err := p.options.Catalogue.Inventory()
			if err != nil {
				return nil, err
			}
			items, err := p.options.Catalogue.Catalogue(r.Context(), inventory.System, r.Header)
			if err != nil {
				return nil, err
			}
			next, err := p.options.Catalogue.Fixture(r.Context(), r.Header, items)
			if err != nil {
				return nil, err
			}
			if _, err := p.validateFixture(next); err != nil {
				return nil, err
			}
			p.mu.Lock()
			if p.fixture == nil {
				if err := p.publishRoots(next); err != nil {
					p.mu.Unlock()
					return nil, err
				}
				p.fixture = next
				p.generation++
			}
			p.mu.Unlock()
		}
	}
	if secondary && r.Method == "GET" && response.StatusCode == 200 && probeFileInfo.MatchString(path) {
		v, err := probeObject(data)
		if err != nil {
			return nil, err
		}
		if err := p.rememberDownload(probeString(v["download_url"]), *owner); err != nil {
			return nil, err
		}
		asset := probeString(v["asset_pointer"])
		if strings.HasPrefix(asset, "sediment://") {
			id := strings.TrimPrefix(asset, "sediment://")
			if (strings.HasPrefix(id, "file_") || strings.HasPrefix(id, "file-")) && len(id) > 5 && len(id) <= 261 && probeFileID.MatchString(id) {
				if err := p.rememberDownload("https://chatgpt.com/backend-api/files/download/"+id, *owner); err != nil {
					return nil, err
				}
			}
		}
	}
	if secondary && r.Method == "GET" && response.StatusCode == 200 && p.options.ProjectComputers != nil {
		if match := probeComputers.FindStringSubmatch(path); match != nil {
			id, _ := url.PathUnescape(match[1])
			var err error
			data, err = p.options.ProjectComputers(owner.Alias, id, data)
			if err != nil {
				return nil, err
			}
		}
	}
	if r.Method == "GET" && path == "/backend-api/tbo" && response.StatusCode == 200 {
		envelope, err := probeObject(data)
		if err != nil {
			return nil, err
		}
		if envelope["cursor"] != nil || strings.Contains(r.URL.RawQuery, "cursor=") {
			return nil, fmt.Errorf("catalogue pagination exceeds proof scope")
		}
		items, err := probeProfiles(envelope["items"])
		if err != nil {
			return nil, err
		}
		var refreshed *CatalogueFixture
		results := p.ownerResults(r.Context(), []Account{{Alias: p.settings.SystemAlias}}, func(ctx context.Context, _ Account) (any, error) {
			return p.options.Catalogue.Fixture(ctx, r.Header, items)
		})
		result := results[p.settings.SystemAlias]
		if result.err == nil {
			refreshed, _ = result.value.(*CatalogueFixture)
			if _, err := p.validateFixture(refreshed); err != nil {
				refreshed = nil
			}
		}
		p.mu.Lock()
		if refreshed != nil && generation == p.generation {
			if err := p.publishRoots(refreshed); err != nil {
				p.mu.Unlock()
				return nil, err
			}
			p.fixture = refreshed
			p.generation++
		}
		f = p.fixture
		p.mu.Unlock()
		if refreshed == nil && f != nil {
			if _, err := p.validateFixture(f); err != nil {
				f = nil
			} else {
				var primary map[string]any
				for _, profile := range f.Profiles {
					if f.Owners[probeString(profile["id"])].Alias == p.settings.SystemAlias {
						primary = profile
					}
				}
				current := map[string]any(nil)
				for _, profile := range items {
					if profile["id"] == primary["id"] {
						if current != nil {
							current = nil
							break
						}
						current = profile
					}
				}
				if current == nil || current["active_root_thread_id"] != primary["active_root_thread_id"] || current["messaging_room_id"] != primary["messaging_room_id"] {
					f = nil
				}
			}
		}
		if f != nil {
			envelope["items"] = f.Profiles
			envelope["cursor"] = nil
			data, err = json.Marshal(envelope)
			if err != nil {
				return nil, err
			}
		}
	}
	if secondary && response.StatusCode == 200 && (r.Method == "GET" || message && strings.HasSuffix(path, "/messages")) && strings.HasPrefix(path, "/backend-api/messaging/rooms/") {
		v, err := probeObject(data)
		if err != nil {
			return nil, err
		}
		if ProjectNativeResponse(v, owner.ViewerIDs, r.Method == "POST") > 0 {
			data, err = json.Marshal(v)
			if err != nil {
				return nil, err
			}
		}
	}
	if strings.HasSuffix(path, "/wham/statsig/bootstrap") && response.StatusCode == 200 && (!native || inspected) {
		envelope, err := probeObject(data)
		if err != nil {
			return nil, err
		}
		payload, err := probeObject([]byte(probeString(envelope["statsigPayload"])))
		if err != nil {
			return nil, err
		}
		if payload["response_format"] == "init-v2" {
			return nil, fmt.Errorf("unexpected compact format")
		}
		gates, _ := payload["feature_gates"].(map[string]any)
		if gates == nil {
			gates = map[string]any{}
			payload["feature_gates"] = gates
		}
		gate, _ := gates["2929219922"].(map[string]any)
		if gate == nil {
			gate = map[string]any{"name": "2929219922", "rule_id": "cq-local-catalog-proof", "secondary_exposures": []any{}, "id_type": "userID"}
			gates["2929219922"] = gate
		}
		gate["value"] = true
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		envelope["statsigPayload"] = string(encoded)
		data, err = json.Marshal(envelope)
		if err != nil {
			return nil, err
		}
	}
	if path == "/backend-api/celsius/ws/user" && response.StatusCode == 200 && p.options.Celsius != nil && (!native || r.Method == "GET") {
		p.mu.Lock()
		f = p.fixture
		p.mu.Unlock()
		accounts, err := p.validateFixture(f)
		if err != nil {
			return nil, err
		}
		v, err := probeObject(data)
		if err != nil {
			return nil, err
		}
		urls := map[string]string{p.settings.SystemAlias: probeString(v["websocket_url"])}
		resolve := func(ctx context.Context, alias string) (string, error) {
			account, ok := accounts[alias]
			if !ok {
				return "", fmt.Errorf("unknown Celsius owner")
			}
			value, err := p.options.Catalogue.Get(ctx, account, r.URL.RequestURI(), OwnerHeaders(r.Header, account))
			if err != nil {
				return "", err
			}
			return probeString(value["websocket_url"]), nil
		}
		if !native {
			for alias := range accounts {
				if alias != p.settings.SystemAlias {
					urls[alias], err = resolve(r.Context(), alias)
					if err != nil {
						return nil, err
					}
				}
			}
		}
		var resolver func(context.Context, string) (string, error)
		if native {
			resolver = resolve
		}
		v["websocket_url"], err = p.options.Celsius.Issue(r.Context(), urls, f.Owners, resolver)
		if err != nil {
			return nil, err
		}
		data, err = json.Marshal(v)
		if err != nil {
			return nil, err
		}
	}
	if path == "/backend-api/ios/attestation_challenge" && r.Method == "GET" && response.StatusCode == 200 && r.Header.Get("X-Sentinel-Dc") != "" {
		p.mu.Lock()
		f = p.fixture
		p.mu.Unlock()
		if f == nil {
			return data, nil
		}
		accounts, err := p.validateFixture(f)
		if err != nil {
			return nil, err
		}
		candidates := []Account{}
		for alias, account := range accounts {
			if alias != p.settings.SystemAlias {
				candidates = append(candidates, account)
			}
		}
		started := p.options.Now()
		results := p.ownerResults(r.Context(), candidates, func(ctx context.Context, account Account) (any, error) {
			req, err := http.NewRequestWithContext(ctx, "GET", p.options.Origin+r.URL.RequestURI(), nil)
			if err != nil {
				return nil, err
			}
			req.Header = OwnerHeaders(probeHeaders(r.Header), account)
			res, err := p.options.HTTP.Do(req)
			if err != nil {
				return nil, err
			}
			defer res.Body.Close()
			body, err := probeRead(res.Body, 1<<20)
			if err != nil {
				return nil, err
			}
			return struct {
				Status int
				Data   []byte
			}{res.StatusCode, body}, nil
		})
		for alias, result := range results {
			issued := false
			status := 0
			if result.err == nil {
				value := result.value.(struct {
					Status int
					Data   []byte
				})
				status = value.Status
				if status == 200 {
					issued = p.rememberChallenge(data, value.Data, alias, started) == nil
				}
			}
			p.event(map[string]any{"operation": "owner_native_challenge", "routed_owner_alias": alias, "status": status, "issued": issued, "native_device_proof_preserved": true})
		}
	}
	return data, nil
}

func probeProfiles(value any) ([]map[string]any, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("catalogue items unavailable")
	}
	profiles := make([]map[string]any, 0, len(items))
	for _, item := range items {
		profile, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("catalogue item invalid")
		}
		profiles = append(profiles, profile)
	}
	return profiles, nil
}
