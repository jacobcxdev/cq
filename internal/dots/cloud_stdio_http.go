package dots

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/httputil"
)

type cloudHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type cloudUnavailableHTTP struct{}

func (cloudUnavailableHTTP) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("public upstream CA roots unavailable")
}

func cloudPublicTLS() (*tls.Config, error) {
	// CQ's child environment may point SSL_CERT_FILE at its interception CA.
	// Owner sessions always use the system's public bundle directly.
	for _, path := range []string{"/etc/ssl/cert.pem", "/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt"} {
		data, err := publicCertificate(path)
		if err != nil {
			continue
		}
		roots := x509.NewCertPool()
		if roots.AppendCertsFromPEM(data) {
			return &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, nil
		}
	}
	return nil, errors.New("public upstream CA roots unavailable")
}

func cloudEventSink(root string) func(map[string]any) {
	var lock sync.Mutex
	return func(value map[string]any) {
		lock.Lock()
		defer lock.Unlock()
		if privateDirectory(root) != nil {
			return
		}
		path := filepath.Join(root, "cloud-stdio-events.jsonl")
		before, err := os.Lstat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return
		}
		if before != nil {
			mode := before.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
			if !before.Mode().IsRegular() || !owned(before) || (mode != 0o600 && mode != 0o644) {
				return
			}
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer file.Close()
		after, err := file.Stat()
		if err != nil || !after.Mode().IsRegular() || !owned(after) || (before != nil && !os.SameFile(before, after)) {
			return
		}
		mode := after.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
		if mode != 0o600 && mode != 0o644 {
			return
		}
		if mode == 0o644 && file.Chmod(0o600) != nil {
			return
		}
		row := make(map[string]any, len(value)+2)
		for key, field := range value {
			row[key] = field
		}
		row["at"], row["pid"] = float64(time.Now().UnixNano())/1e9, os.Getpid()
		_ = json.NewEncoder(file).Encode(row)
	}
}

func cloudStdioDefaults(cfg *Settings) cloudStdioDependencies {
	tlsConfig, tlsErr := cloudPublicTLS()
	event := cloudEventSink(cfg.RuntimeRoot)
	transport := &http.Transport{DialContext: dotsDialContext, TLSClientConfig: tlsConfig}
	client := &http.Client{Transport: transport, Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var httpClient cloudHTTPDoer = client
	if tlsErr != nil {
		httpClient = cloudUnavailableHTTP{}
	}
	return cloudStdioDependencies{
		environment: os.Environ(), launch: startCloudLocal, stock: cloudStock, readRoots: ReadOwned,
		http: httpClient, now: time.Now, statusInterval: 2 * time.Second, event: event,
		resolveOwner: func(alias string) (Account, error) {
			inventory, err := (&CatalogueClient{Settings: cfg}).Inventory()
			if err != nil {
				return Account{}, errors.New("owner credential inventory unavailable")
			}
			for _, account := range inventory.Accounts {
				if account.Alias == alias {
					return account, nil
				}
			}
			return Account{}, errors.New("owner credential unavailable")
		},
		dial: func(ctx context.Context, account Account) (cloudSocket, int, error) {
			if tlsErr != nil {
				event(map[string]any{"owner_tls_failure_type": "unavailable"})
				return nil, 0, tlsErr
			}
			dialer := websocket.Dialer{HandshakeTimeout: 20 * time.Second, NetDialContext: dotsDialContext, TLSClientConfig: tlsConfig,
				Subprotocols: []string{"codex-app-server", "codex-client.desktop", "openai-bearer." + account.Token}}
			headers := http.Header{"Chatgpt-Account-Id": []string{account.AccountID}, "X-Openai-Product-Sku": []string{"codex"}, "X-Openai-Allow-Environmentless-Fallback": []string{"true"}}
			connection, response, err := dialer.DialContext(ctx, "wss://codex-cloud-backend.chatgpt.com/", headers)
			status := 0
			if response != nil {
				status = response.StatusCode
				if err != nil && response.Body != nil {
					_ = response.Body.Close()
				}
			}
			if err != nil {
				return nil, status, errors.New("owner cloud connection failed")
			}
			connection.SetReadLimit(16 * 1024 * 1024)
			return connection, status, nil
		},
	}
}

// Owners retain the verified startup identities. Only their credential cache
// changes, and reading configured sources never refreshes OAuth or retries RPCs.
func (b *cloudBridge) ownerCredential(alias string) (Account, error) {
	b.credentialsMu.Lock()
	defer b.credentialsMu.Unlock()
	pinned, exists := b.owners[alias]
	if !exists {
		return Account{}, errors.New("owner credential unavailable")
	}
	owner, cached := b.credentials[alias]
	if !cached {
		owner = pinned
	}
	usable := func(account Account) bool {
		claims, err := Claims(account.Token)
		if err != nil {
			return false
		}
		expires, ok := claims["exp"].(float64)
		return ok && expires > float64(b.deps.now().Unix())+60
	}
	if usable(owner) {
		return owner, nil
	}
	if b.deps.resolveOwner == nil {
		return Account{}, errors.New("owner credential unavailable")
	}
	owner, err := b.deps.resolveOwner(alias)
	if err != nil || owner.Alias != pinned.Alias || owner.AccountID != pinned.AccountID || !usable(owner) {
		return Account{}, errors.New("owner credential verification unavailable")
	}
	if b.credentials == nil {
		b.credentials = make(map[string]Account)
	}
	b.credentials[alias] = owner
	return owner, nil
}

func (b *cloudBridge) httpHeaders(owner Account) http.Header {
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+owner.Token)
	headers.Set("ChatGPT-Account-Id", owner.AccountID)
	headers.Set("X-OpenAI-Product-Sku", "aeon")
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "Codex Desktop/"+b.cfg.AppVersion+" (Mac OS; arm64)")
	headers.Set("originator", "codex_desktop")
	return headers
}

func (b *cloudBridge) httpRead(alias string, request cloudMessage, resource string) {
	method := request.method()
	owner, err := b.ownerCredential(alias)
	if err != nil {
		b.fail(request, "Owner cloud HTTP read unavailable")
		return
	}
	endpoint := ""
	switch method {
	case "thread/read":
		endpoint = "/v1/threads/" + url.PathEscape(resource)
	case "thread/turns/list":
		endpoint = "/v2/threads/" + url.PathEscape(resource) + "/turns"
	case "thread/items/list":
		endpoint = "/v2/threads/" + url.PathEscape(resource) + "/items"
	}
	var params map[string]any
	decoder := json.NewDecoder(bytes.NewReader(request["params"]))
	decoder.UseNumber()
	_ = decoder.Decode(&params)
	query := make(url.Values)
	for key, value := range params {
		if key != "threadId" && key != "includeTurns" && value != nil {
			query.Set(key, cloudQueryValue(value))
		}
	}
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}
	ctx, cancel := context.WithTimeout(b.ctx, 25*time.Second)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://codex-cloud-backend.chatgpt.com"+endpoint, nil)
	if err != nil {
		b.fail(request, "Owner cloud HTTP read unavailable")
		return
	}
	httpRequest.Header = b.httpHeaders(owner)
	response, err := b.deps.http.Do(httpRequest)
	if err != nil {
		b.event(map[string]any{"owner_alias": alias, "method": method, "http_failure_type": "unavailable"})
		b.fail(request, "Owner cloud HTTP read unavailable")
		return
	}
	defer response.Body.Close()
	data, err := httputil.ReadBodyLimit(response.Body, 16*1024*1024)
	if err != nil {
		b.fail(request, "Owner cloud HTTP read unavailable")
		return
	}
	b.event(map[string]any{"owner_alias": alias, "method": method, "http_status": response.StatusCode, "json_content_type": strings.HasPrefix(response.Header.Get("Content-Type"), "application/json")})
	if response.StatusCode != http.StatusOK {
		b.fail(request, "Owner cloud HTTP read failed")
		return
	}
	var value cloudMessage
	if json.Unmarshal(data, &value) != nil || value == nil {
		b.fail(request, "Owner cloud HTTP read unavailable")
		return
	}
	if method == "thread/read" {
		if cloudThreadID(data) != resource {
			b.fail(request, "Owner cloud HTTP read unavailable")
			return
		}
		var thread cloudMessage
		if json.Unmarshal(value["thread"], &thread) != nil || thread == nil {
			b.fail(request, "Owner cloud HTTP read unavailable")
			return
		}
		thread["turns"] = json.RawMessage(`[]`)
		value["thread"], _ = json.Marshal(thread)
	}
	if id, ok := request["id"]; ok {
		b.emit(map[string]any{"id": id, "result": value})
	}
}

func cloudQueryValue(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case bool:
		if value {
			return "True"
		}
		return "False"
	default:
		return fmt.Sprint(value)
	}
}

func (b *cloudBridge) ownerThreadStatus(alias, resource string) (json.RawMessage, error) {
	b.rootsMu.Lock()
	ownerAlias := b.roots[resource]
	b.rootsMu.Unlock()
	if ownerAlias != alias {
		return nil, errors.New("status resource owner mismatch")
	}
	owner, err := b.ownerCredential(alias)
	if err != nil {
		return nil, errors.New("status owner credential unavailable")
	}
	ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://codex-cloud-backend.chatgpt.com/v1/threads/"+url.PathEscape(resource), nil)
	if err != nil {
		return nil, errors.New("status metadata request invalid")
	}
	request.Header = b.httpHeaders(owner)
	response, err := b.deps.http.Do(request)
	if err != nil {
		return nil, errors.New("owner metadata HTTP read failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		return nil, errors.New("owner metadata HTTP read failed")
	}
	data, err := httputil.ReadBodyLimit(response.Body, 1024*1024)
	if err != nil || cloudThreadID(data) != resource {
		return nil, errors.New("owner metadata resource mismatch")
	}
	var metadata struct {
		Thread struct {
			Status json.RawMessage `json:"status"`
		} `json:"thread"`
	}
	if json.Unmarshal(data, &metadata) != nil {
		return nil, errors.New("owner metadata invalid")
	}
	var status struct {
		Type        string          `json:"type"`
		ActiveFlags json.RawMessage `json:"activeFlags"`
	}
	if json.Unmarshal(metadata.Thread.Status, &status) != nil {
		return nil, errors.New("owner metadata status invalid")
	}
	switch status.Type {
	case "notLoaded", "idle", "systemError":
	case "active":
		var flags []any
		if !bytes.HasPrefix(bytes.TrimSpace(status.ActiveFlags), []byte{'['}) || json.Unmarshal(status.ActiveFlags, &flags) != nil {
			return nil, errors.New("owner metadata active flags invalid")
		}
		for _, flag := range flags {
			if _, ok := flag.(string); !ok {
				return nil, errors.New("owner metadata active flags invalid")
			}
		}
	default:
		return nil, errors.New("owner metadata status invalid")
	}
	return metadata.Thread.Status, nil
}

func sameCloudStatus(left, right json.RawMessage) bool {
	var a, b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && reflect.DeepEqual(a, b)
}

func (b *cloudBridge) watchStatus(alias, resource string) {
	b.statusMu.Lock()
	if b.statusWatches[resource] {
		b.statusMu.Unlock()
		return
	}
	b.statusWatches[resource] = true
	b.statusMu.Unlock()
	b.workers.Add(1)
	go func() {
		defer b.workers.Done()
		b.guard(alias, func() {
			for b.ctx.Err() == nil {
				status, err := b.ownerThreadStatus(alias, resource)
				if err != nil {
					b.event(map[string]any{"owner_alias": alias, "status_refresh_failure_type": "unavailable"})
				} else {
					b.statusMu.Lock()
					changed := !sameCloudStatus(b.statusSeen[resource], status)
					b.statusSeen[resource] = status
					b.statusMu.Unlock()
					if changed && b.ctx.Err() == nil {
						b.emit(map[string]any{"method": "thread/status/changed", "params": map[string]any{"threadId": resource, "status": status}})
					}
				}
				timer := time.NewTimer(b.deps.statusInterval)
				select {
				case <-b.ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		})
	}()
}
