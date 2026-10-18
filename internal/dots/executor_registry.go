package dots

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const executorRegistry = "codex-cloud-environments.chatgpt.com"

type executorRequest func(context.Context, Account, string, string, string, []byte) (int, string, []byte, error)

func executorOwnerHeaders(s *Settings, owner Account, alias string, now time.Time) (http.Header, error) {
	permitted := false
	for _, dot := range s.Dots {
		permitted = permitted || dot.OwnerAlias == alias
	}
	claims, err := Claims(owner.Token)
	expires, ok := claims["exp"].(float64)
	if err != nil || !ok || expires <= float64(now.Unix())+60 || owner.Alias != alias || alias == s.SystemAlias || !permitted {
		return nil, errors.New("secondary credential unavailable; refresh forbidden")
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+owner.Token)
	h.Set("ChatGPT-Account-Id", owner.AccountID)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("originator", "codex_work_desktop")
	h.Set("User-Agent", "Codex Desktop/"+s.AppVersion+" (Mac OS; arm64)")
	return h, nil
}

func executorDirectRequest(s *Settings, doer httputil.Doer, now func() time.Time, alias string) executorRequest {
	if doer == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		doer = &http.Client{Timeout: 25 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return func(ctx context.Context, owner Account, host, method, path string, body []byte) (int, string, []byte, error) {
		if host != "chatgpt.com" && host != executorRegistry {
			return 0, "", nil, errors.New("host outside executor transport")
		}
		target, err := url.Parse("https://" + host + path)
		if err != nil || target.Host != host || target.User != nil || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
			return 0, "", nil, errors.New("invalid executor request target")
		}
		headers, err := executorOwnerHeaders(s, owner, alias, now())
		if err != nil {
			return 0, "", nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
		if err != nil {
			return 0, "", nil, err
		}
		req.Header = headers
		response, err := doer.Do(req)
		if err != nil {
			return 0, "", nil, errors.New("executor owner request unavailable")
		}
		defer response.Body.Close()
		data, err := httputil.ReadBody(response.Body)
		if err != nil {
			return 0, "", nil, err
		}
		return response.StatusCode, response.Header.Get("Content-Type"), data, nil
	}
}

func validateExecutorRegistryResponse(data []byte, environment string) error {
	var value map[string]any
	if json.Unmarshal(data, &value) != nil || value["environment_id"] != environment {
		return errors.New("registry environment identity mismatch")
	}
	raw, _ := value["url"].(string)
	target, err := url.Parse(raw)
	if err != nil || target.Scheme != "wss" || target.User != nil || target.Hostname() == "" || (target.Port() != "" && target.Port() != "443") {
		return errors.New("registry rendezvous authority invalid")
	}
	host := strings.ToLower(target.Hostname())
	for _, domain := range []string{"chatgpt.com", "openai.com", "openai.org"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return nil
		}
	}
	return errors.New("registry rendezvous authority invalid")
}

type executorRegistryBroker struct {
	owner                   Account
	environment, capability string
	request                 executorRequest
	mu                      sync.Mutex
	events                  []map[string]any
	stopped                 bool
	server                  *http.Server
	listener                net.Listener
	done                    chan struct{}
}

func newExecutorRegistryBroker(s *Settings, owner Account, environment, alias string, request executorRequest) (*executorRegistryBroker, error) {
	allowed := false
	for _, dot := range s.Dots {
		allowed = allowed || dot.OwnerAlias == alias
	}
	if owner.Alias != alias || alias == s.SystemAlias || !allowed || !validExecutorEnvironment(environment) {
		return nil, errors.New("executor principal outside secondary proof")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := &executorRegistryBroker{owner: owner, environment: environment, capability: base64.RawURLEncoding.EncodeToString(key), request: request, listener: listener, done: make(chan struct{})}
	b.server = &http.Server{Handler: http.HandlerFunc(b.serve), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	return b, nil
}
func (b *executorRegistryBroker) start() {
	go func() {
		defer func() {
			_ = recover()
			b.stop()
			close(b.done)
		}()
		_ = b.server.Serve(b.listener)
	}()
}
func (b *executorRegistryBroker) stop() { b.mu.Lock(); b.stopped = true; b.mu.Unlock() }
func (b *executorRegistryBroker) snapshot() (bool, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	registered := false
	for _, event := range b.events {
		registered = registered || event["operation"] == "register" && event["status"] == 200
	}
	return b.stopped, registered
}
func (b *executorRegistryBroker) close() {
	b.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if b.server.Shutdown(ctx) != nil {
		_ = b.server.Close()
	}
	_ = b.listener.Close()
	select {
	case <-b.done:
	case <-ctx.Done():
	}
}
func (b *executorRegistryBroker) port() int { return b.listener.Addr().(*net.TCPAddr).Port }
func (b *executorRegistryBroker) fail(w http.ResponseWriter) {
	b.stop()
	b.mu.Lock()
	b.events = append(b.events, map[string]any{"failure_type": "ValueError"})
	b.mu.Unlock()
	http.Error(w, "Executor registry unavailable", http.StatusBadGateway)
}
func (b *executorRegistryBroker) serve(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if recover() != nil {
			b.fail(w)
		}
	}()
	prefix := "/api/cloud/environment/" + url.PathEscape(b.environment)
	stopped, _ := b.snapshot()
	if r.Method != "POST" || (r.URL.RequestURI() != prefix+"/register" && r.URL.RequestURI() != prefix+"/validate") || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.capability)) != 1 || stopped {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if r.ContentLength <= 0 || r.ContentLength > httputil.MaxResponseBody || len(r.TransferEncoding) != 0 {
		b.fail(w)
		return
	}
	body, err := httputil.ReadBody(r.Body)
	if err != nil || int64(len(body)) != r.ContentLength {
		b.fail(w)
		return
	}
	var decoded any
	if json.Unmarshal(body, &decoded) != nil {
		b.fail(w)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/register") {
		value, object := decoded.(map[string]any)
		_, key := value["executor_public_key"].(map[string]any)
		if !object || len(value) != 2 || value["security_profile"] != "noise_hybrid_ik_v1" || !key {
			b.fail(w)
			return
		}
	}
	status, contentType, data, err := b.request(r.Context(), b.owner, executorRegistry, "POST", r.URL.RequestURI(), body)
	if err != nil || status < 100 || status > 599 || len(data) > httputil.MaxResponseBody {
		b.fail(w)
		return
	}
	if status == 200 && strings.HasSuffix(r.URL.Path, "/register") && validateExecutorRegistryResponse(data, b.environment) != nil {
		b.fail(w)
		return
	}
	if status == 401 || status == 403 {
		b.stop()
	}
	b.mu.Lock()
	b.events = append(b.events, map[string]any{"operation": strings.TrimPrefix(r.URL.Path, prefix+"/"), "status": status})
	b.mu.Unlock()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func registerExecutor(ctx context.Context, request executorRequest, owner Account, installation, display string) (string, error) {
	if !nativeValidUUID(installation) {
		return "", errors.New("invalid executor installation identity")
	}
	body, _ := json.Marshal(map[string]any{"displayName": display, "installation_id": installation, "selected_capability_roots": []any{}})
	status, contentType, data, err := request(ctx, owner, "chatgpt.com", "POST", "/backend-api/flora/cca/executor", body)
	if err != nil {
		return "", err
	}
	var value map[string]any
	if status != 200 || !strings.HasPrefix(contentType, "application/json") || json.Unmarshal(data, &value) != nil {
		return "", fmt.Errorf("secondary executor registration HTTP %d", status)
	}
	environment, _ := value["executor_id"].(string)
	if !validExecutorEnvironment(environment) {
		return "", errors.New("secondary executor registration identity invalid")
	}
	return environment, nil
}
