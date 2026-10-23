package dots

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const nativeUploadLimit = 20*1024*1024 + 65536

// ProbeOptions supplies process-local seams. None of the callbacks may retry writes.
type ProbeOptions struct {
	Catalogue        *CatalogueClient
	HTTP             httputil.Doer
	Origin           string
	RequireNative    func(context.Context, http.Header) error
	Celsius          *CelsiusBroker
	Now              func() time.Time
	Event            func(map[string]any)
	ComputerTarget   func(context.Context, string, []byte, *CatalogueFixture) (string, error)
	ProjectComputers func(string, string, []byte) ([]byte, error)
}

type probeChallenge struct {
	value   string
	expires time.Time
}
type probeFile struct {
	alias, account string
	expires        time.Time
	order          uint64
}

// Probe owns catalogue snapshots, one-use attestation challenges and download grants.
type Probe struct {
	settings   Settings
	options    ProbeOptions
	mu         sync.Mutex
	fixture    *CatalogueFixture
	generation uint64
	challenges map[string]probeChallenge
	files      map[string]probeFile
	fileOrder  uint64
	workers    chan struct{}
	eventMu    sync.Mutex
}

func NewProbe(settings Settings, options ProbeOptions) (*Probe, error) {
	if options.HTTP == nil {
		options.HTTP = probeHTTPClient()
	}
	if options.Catalogue == nil {
		options.Catalogue = &CatalogueClient{Settings: &settings, HTTP: options.HTTP}
	}
	if options.Origin == "" {
		options.Origin = "https://chatgpt.com"
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.RequireNative == nil {
		options.RequireNative = NewNativeGate(&settings, options.Catalogue).Require
	}
	if options.ComputerTarget == nil {
		options.ComputerTarget = func(ctx context.Context, path string, body []byte, f *CatalogueFixture) (string, error) {
			return probeComputerTarget(ctx, &settings, options.Catalogue, path, body, f)
		}
	}
	if options.ProjectComputers == nil {
		options.ProjectComputers = func(alias, profile string, data []byte) ([]byte, error) {
			return ProjectExecutorComputers(&settings, alias, profile, data)
		}
	}
	p := &Probe{settings: settings, options: options, challenges: map[string]probeChallenge{}, files: map[string]probeFile{}, workers: make(chan struct{}, 8)}
	return p, nil
}

// RunProbe serves the qualified native origin; it never alters the signed app.
func RunProbe(ctx context.Context, settings Settings) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p, err := NewProbe(settings, ProbeOptions{})
	if err != nil {
		return err
	}
	p.options.Celsius = NewCelsiusBroker(&settings, p.event)
	celsiusListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer celsiusListener.Close()
	celsiusDone := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				celsiusDone <- fmt.Errorf("Celsius server panic")
			}
		}()
		celsiusDone <- p.options.Celsius.Serve(ctx, celsiusListener, nil)
	}()
	port := filepath.Join(settings.RuntimeRoot, "port")
	if info, err := os.Lstat(port); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("proxy port must be a regular file")
		}
		data, err := os.ReadFile(port)
		if err != nil {
			return err
		}
		if len(data) >= 32 || strings.TrimSpace(string(data)) != "8000" {
			return fmt.Errorf("qualified native authentication requires loopback proxy port 8000")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	certificate, err := tls.LoadX509KeyPair(filepath.Join(settings.RuntimeRoot, "leaf-cert.pem"), filepath.Join(settings.RuntimeRoot, "leaf-key.pem"))
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:8000")
	if err != nil {
		return fmt.Errorf("native Dots proxy requires free loopback port 8000: %w", err)
	}
	defer listener.Close()
	if err := launchWrite(port, []byte("8000"), 0o600); err != nil {
		return err
	}
	server := &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second, ReadTimeout: 30 * time.Second, MaxHeaderBytes: 1 << 20, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer func() { _ = recover() }()
		select {
		case <-ctx.Done():
			_ = server.Close()
		case err := <-celsiusDone:
			if err != nil {
				p.event(map[string]any{"operation": "celsius_server", "error_type": "server"})
			}
			_ = server.Close()
		case <-done:
		}
	}()
	p.event(map[string]any{"listening_port": 8000, "verified_room_text_sends_enabled": true})
	err = server.Serve(tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (p *Probe) event(value map[string]any) {
	p.eventMu.Lock()
	defer p.eventMu.Unlock()
	if p.options.Event != nil {
		p.options.Event(value)
		return
	}
	file, err := os.OpenFile(filepath.Join(p.settings.RuntimeRoot, "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	value["at"] = float64(p.options.Now().UnixNano()) / 1e9
	_ = json.NewEncoder(file).Encode(value)
}

func probeJSON(w http.ResponseWriter, status int, value any) {
	data, _ := json.Marshal(value)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
func probeError(w http.ResponseWriter, status int, message string) {
	probeJSON(w, status, map[string]any{"error": message})
}
func probeString(value any) string { text, _ := value.(string); return text }
func probeObject(data []byte) (map[string]any, error) {
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil || value == nil {
		return nil, fmt.Errorf("expected JSON object")
	}
	return value, nil
}
func probeEqual(a, b Account) bool { return a.Alias == b.Alias && a.AccountID == b.AccountID }

func OwnerHeaders(headers http.Header, owner Account) http.Header {
	result := headers.Clone()
	for _, key := range []string{"Authorization", "ChatGPT-Account-Id", "X-OpenAI-Account-Routing-Override", "X-OpenAI-Expected-Account-Id", "Cookie", "X-OpenAI-Orbit-Selection"} {
		result.Del(key)
	}
	result.Set("Authorization", "Bearer "+owner.Token)
	result.Set("ChatGPT-Account-Id", owner.AccountID)
	return result
}

func probeHeaders(headers http.Header) http.Header {
	result := headers.Clone()
	for _, value := range headers.Values("Connection") {
		for _, key := range strings.Split(value, ",") {
			result.Del(strings.TrimSpace(key))
		}
	}
	for _, key := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "TE", "Trailer", "Upgrade", "Host", "Transfer-Encoding"} {
		result.Del(key)
	}
	return result
}

func (p *Probe) validateFixture(f *CatalogueFixture) (map[string]Account, error) {
	if f == nil {
		return nil, fmt.Errorf("catalogue unavailable")
	}
	configured := map[string]Dot{}
	aliases := map[string]bool{}
	for _, dot := range p.settings.Dots {
		configured[dot.ProfileID] = dot
		aliases[dot.OwnerAlias] = true
	}
	if len(f.Profiles) != len(configured) {
		return nil, fmt.Errorf("catalogue profiles differ from configured Dot IDs")
	}
	accounts := map[string]Account{f.System.Alias: f.System}
	seen := map[string]bool{}
	for _, profile := range f.Profiles {
		id := probeString(profile["id"])
		dot, ok := configured[id]
		root := probeString(profile["active_root_thread_id"])
		owner, exists := f.Owners[id]
		rootOwner, rootExists := f.Owners[root]
		if !ok || seen[id] || root == "" || !exists || !rootExists || owner.Alias != dot.OwnerAlias || rootOwner.Alias != dot.OwnerAlias || !probeEqual(owner, rootOwner) {
			return nil, fmt.Errorf("catalogue profile ownership differs from configured Dot owner")
		}
		if old, ok := accounts[owner.Alias]; ok && !probeEqual(old, owner) {
			return nil, fmt.Errorf("catalogue alias collision")
		}
		seen[id] = true
		accounts[owner.Alias] = owner
	}
	if f.System.Alias != p.settings.SystemAlias || len(accounts) != len(aliases) {
		return nil, fmt.Errorf("catalogue principals differ from configured owners")
	}
	for alias := range accounts {
		if !aliases[alias] {
			return nil, fmt.Errorf("catalogue principals differ from configured owners")
		}
	}
	return accounts, nil
}

func (p *Probe) publishRoots(f *CatalogueFixture) error {
	if _, err := p.validateFixture(f); err != nil {
		return err
	}
	roots := []map[string]string{}
	seen := map[string]bool{}
	for _, profile := range f.Profiles {
		id, root := probeString(profile["id"]), probeString(profile["active_root_thread_id"])
		if seen[root] {
			return fmt.Errorf("catalogue root collision")
		}
		seen[root] = true
		roots = append(roots, map[string]string{"owner_alias": f.Owners[id].Alias, "profile_id": id, "root_thread_id": root})
	}
	path := filepath.Join(p.settings.RuntimeRoot, "native-roots.json")
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("native roots must not be a symlink")
	}
	data, _ := json.Marshal(map[string]any{"version": 1, "roots": roots})
	return launchWrite(path, data, 0o600)
}

var probeResource = regexp.MustCompile(`^/backend-api/(?:tbo/by-thread/([^/]+)|tbo/([^/]+)|messaging/rooms/([^/]+))(?:/|$)`)
var probeRoom = regexp.MustCompile(`^/backend-api/messaging/rooms/([A-Za-z0-9_-]+)/(messages|aeon/prepare|read|live|responding_heartbeat)$`)
var probeUpload = regexp.MustCompile(`^/backend-api/messaging/rooms/[A-Za-z0-9_-]+/files(?:/uploads(?:/[A-Za-z0-9_-]+/complete)?)?$`)
var probeFileID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
var probeDownload = regexp.MustCompile(`^/backend-api/files/download/[A-Za-z0-9_-]+$`)
var probeActivity = regexp.MustCompile(`^/backend-api/tbo/[^/]+/activity/stream$`)
var probeComputers = regexp.MustCompile(`^/backend-api/tbo/([^/]+)/computers$`)
var probeFileInfo = regexp.MustCompile(`^/backend-api/messaging/rooms/[^/]+/files/[^/]+$`)

func (p *Probe) resourceOwner(path string, f *CatalogueFixture) (*Account, error) {
	if f == nil {
		return nil, nil
	}
	match := probeResource.FindStringSubmatch(path)
	if match == nil {
		return nil, nil
	}
	field, id := "", ""
	for i, key := range []string{"active_root_thread_id", "id", "messaging_room_id"} {
		if match[i+1] != "" {
			field = key
			id, _ = url.PathUnescape(match[i+1])
			break
		}
	}
	count := 0
	var profile map[string]any
	for _, candidate := range f.Profiles {
		if candidate[field] == id {
			count++
			profile = candidate
		}
	}
	if count == 0 {
		return nil, nil
	}
	owner, ok := f.Owners[id]
	profileOwner, hasProfile := f.Owners[probeString(profile["id"])]
	rootOwner, hasRoot := f.Owners[probeString(profile["active_root_thread_id"])]
	if count != 1 || !ok || !hasProfile || !hasRoot || !probeEqual(owner, profileOwner) || !probeEqual(owner, rootOwner) {
		return nil, fmt.Errorf("resource ownership differs from current catalogue")
	}
	return &owner, nil
}

func (p *Probe) roomOwner(path string, f *CatalogueFixture) *Account {
	match := probeRoom.FindStringSubmatch(path)
	if match == nil || f == nil {
		return nil
	}
	count := 0
	for _, profile := range f.Profiles {
		if profile["messaging_room_id"] == match[1] {
			count++
		}
	}
	owner, ok := f.Owners[match[1]]
	if count != 1 || !ok {
		return nil
	}
	return &owner
}

func probeReceipt(body []byte) bool {
	v, err := probeObject(body)
	if err != nil || len(v) != 1 {
		return false
	}
	stamp, ok := v["last_read_at"].(string)
	if !ok || len(stamp) > 64 {
		return false
	}
	_, err = time.Parse(time.RFC3339Nano, stamp)
	return err == nil
}
func probeName(path string, body []byte) bool {
	if !regexp.MustCompile(`^/backend-api/tbo/[^/?]+$`).MatchString(path) {
		return false
	}
	v, err := probeObject(body)
	name, ok := v["display_name"].(string)
	return err == nil && len(v) == 1 && ok && validText(name, 24) && strings.TrimSpace(name) != "" && !strings.Contains(name, `\`)
}
func probeMessage(body []byte, attachmentsAllowed bool) bool {
	v, err := probeObject(body)
	if err != nil {
		return false
	}
	content, ok := v["content"].(map[string]any)
	if !ok {
		return false
	}
	text, ok := content["text"].(string)
	if !ok {
		return false
	}
	attachments := content["attachments"]
	files := false
	if list, ok := attachments.([]any); ok && len(list) > 0 && attachmentsAllowed {
		files = true
		for _, item := range list {
			file, ok := item.(map[string]any)
			if !ok || file["type"] != "file" || !probeFileID.MatchString(probeString(file["file_id"])) {
				files = false
				break
			}
		}
	}
	emptyAttachments := attachments == nil
	if list, ok := attachments.([]any); ok && len(list) == 0 {
		emptyAttachments = true
	}
	id, ok := v["request_id"].(string)
	return (strings.TrimSpace(text) != "" || files) && (emptyAttachments || files) && ok && id != "" && v["idempotency_token"] == id
}

func (p *Probe) runtimeOwner(r *http.Request, body []byte, f *CatalogueFixture) *Account {
	if r.Method != "POST" || len(body) > 0 || r.URL.RawQuery != "" || f == nil {
		return nil
	}
	parts := strings.Split(r.URL.EscapedPath(), "/")
	if len(parts) != 6 || parts[1] != "backend-api" || parts[2] != "tbo" || parts[4] != "runtime" || (parts[5] != "pause" && parts[5] != "resume") {
		return nil
	}
	owner, err := p.resourceOwner(r.URL.EscapedPath(), f)
	if err != nil || owner == nil || owner.Alias == p.settings.SystemAlias {
		return nil
	}
	return owner
}

func (p *Probe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if recover() != nil {
			p.event(map[string]any{"operation": "probe_request", "error_type": "panic"})
			probeError(w, 502, "Native request unavailable")
		}
	}()
	if r.URL.IsAbs() || r.URL.Host != "" || !strings.HasPrefix(r.URL.Path, "/") || strings.HasPrefix(r.URL.Path, "//") {
		w.WriteHeader(400)
		return
	}
	path := r.URL.EscapedPath()
	native := p.settings.IntegrationMode == "native"
	p.mu.Lock()
	fixture, generation := p.fixture, p.generation
	owner, err := p.resourceOwner(path, fixture)
	fileDownload := false
	if owner == nil && (r.Method == "GET" || r.Method == "HEAD") {
		owner = p.downloadOwner(path, fixture)
		fileDownload = owner != nil
	}
	p.mu.Unlock()
	if err != nil {
		w.WriteHeader(409)
		return
	}
	secondary := owner != nil && owner.Alias != p.settings.SystemAlias
	inspected := (r.Method == "GET" && (path == "/backend-api/tbo" || path == "/backend-api/celsius/ws/user" || path == "/backend-api/ios/attestation_challenge")) || ((r.Method == "GET" || r.Method == "POST") && strings.HasSuffix(path, "/wham/statsig/bootstrap"))
	passthrough := native && !secondary && !inspected && path != "/probe/state"
	upload := native && secondary && r.Method == "POST" && r.URL.RawQuery == "" && probeUpload.MatchString(path) && ((strings.HasSuffix(path, "/uploads") && strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]), "application/json")) || (!strings.HasSuffix(path, "/uploads") && strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data;")))
	if len(r.Header.Values("Content-Length")) > 1 || len(r.TransferEncoding) > 0 && (!passthrough || len(r.TransferEncoding) != 1 || r.TransferEncoding[0] != "chunked" || r.ContentLength >= 0) {
		w.WriteHeader(400)
		return
	}
	if passthrough {
		if r.Method == "GET" && path == "/backend-api/wham/remote/control/server" && r.URL.RawQuery == "" && r.URL.Fragment == "" && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
				w.WriteHeader(400)
				return
			}
			p.remoteControl(w, r)
			return
		}
		p.forward(w, r, nil)
		return
	}
	limit := int64(1 << 20)
	if upload && !strings.HasSuffix(path, "/uploads") {
		limit = nativeUploadLimit
	}
	if r.ContentLength > limit {
		w.WriteHeader(413)
		return
	}
	body, err := probeRead(r.Body, limit)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(413)
		return
	}
	if r.ContentLength >= 0 && int64(len(body)) != r.ContentLength {
		w.WriteHeader(400)
		return
	}
	if path == "/probe/state" {
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		probeJSON(w, 200, map[string]any{"rewrite": true, "messages": true, "secondary_messages": true, "secondary_challenges": true, "primary_probe_owner": nil, "reboot_armed": false})
		return
	}
	if !native || secondary || inspected {
		if err := p.options.RequireNative(r.Context(), r.Header); err != nil {
			p.event(map[string]any{"native_request_rejected": true, "method": r.Method})
			probeError(w, 401, "Native authentication required")
			return
		}
	}
	if fileDownload {
		r.Body = io.NopCloser(bytes.NewReader(body))
		p.forward(w, r, owner)
		return
	}
	if !native && r.Method == "POST" && (path == "/backend-api/cloud-aeons/primary/reboot" || path == "/backend-api/flora/cca/executor") {
		if path == "/backend-api/flora/cca/executor" {
			if _, err := os.Stat(filepath.Join(p.settings.RuntimeRoot, "native-secondary-executor.enabled")); err == nil && r.URL.RawQuery == "" {
				value, audit, err := ReuseNativeExecutor(r.Context(), &p.settings, p.options.Catalogue, r.Header, body)
				if err == nil {
					p.event(audit)
					probeJSON(w, 200, value)
					return
				}
				p.event(map[string]any{"operation": "native_executor_reuse", "new_registration_sent": false, "error_type": "validation"})
			}
		}
		probeError(w, 409, "Isolated research operation unavailable; no operation submitted")
		return
	}
	roomOwner := p.roomOwner(path, fixture)
	runtimeOwner := p.runtimeOwner(r, body, fixture)
	message := r.Method == "POST" && roomOwner != nil && ((strings.HasSuffix(path, "/aeon/prepare") && len(body) == 0) || (strings.HasSuffix(path, "/messages") && probeMessage(body, native)))
	receipt := r.Method == "POST" && roomOwner != nil && strings.HasSuffix(path, "/read") && probeReceipt(body)
	watch := r.Method == "POST" && roomOwner != nil && (strings.HasSuffix(path, "/live") || strings.HasSuffix(path, "/responding_heartbeat")) && len(body) == 0
	name := r.Method == "PATCH" && r.URL.RawQuery == "" && owner != nil && probeName(path, body)
	target := ""
	if native && r.Method == "POST" && p.options.ComputerTarget != nil {
		target, err = p.options.ComputerTarget(r.Context(), r.URL.RequestURI(), body, fixture)
		if err != nil {
			w.WriteHeader(409)
			return
		}
	}
	allowed := r.Method == "GET" || runtimeOwner != nil || message || receipt || watch || (r.Method == "POST" && (strings.HasSuffix(path, "/wham/statsig/bootstrap") || path == "/backend-api/devicecheck"))
	if native {
		allowed = !secondary || r.Method == "GET" || r.Method == "HEAD" || runtimeOwner != nil || message || receipt || watch || name || target != "" || upload
	}
	if !allowed {
		p.event(map[string]any{"method": r.Method, "path": path, "blocked": true})
		probeError(w, 503, "Read-only probe blocks this operation")
		return
	}
	if owner == nil && !native {
		owner, _ = p.resourceOwner(path, fixture)
	}
	if owner != nil && (message || receipt || watch) && roomOwner != nil && !probeEqual(*owner, *roomOwner) {
		w.WriteHeader(409)
		return
	}
	if owner != nil && runtimeOwner != nil && !probeEqual(*owner, *runtimeOwner) {
		w.WriteHeader(409)
		return
	}
	if message && strings.HasSuffix(path, "/messages") && owner != nil && owner.Alias != p.settings.SystemAlias {
		body, err = p.ownerMessage(body, owner.Alias)
		if err != nil {
			probeError(w, 409, "Owner native verification unavailable; no message submitted")
			return
		}
	}
	headers := probeHeaders(r.Header)
	headers.Del("Content-Length")
	headers.Del("Accept-Encoding")
	headers.Set("Accept-Encoding", "identity")
	if owner != nil && (!native || secondary) {
		headers = OwnerHeaders(headers, *owner)
	}
	requestPath := r.URL.RequestURI()
	if target != "" {
		requestPath = target
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.options.Origin+requestPath, bytes.NewReader(body))
	if err != nil {
		w.WriteHeader(502)
		return
	}
	req.Header = headers
	response, err := p.options.HTTP.Do(req)
	if err != nil {
		p.event(map[string]any{"method": r.Method, "path": path, "error_type": "upstream"})
		w.WriteHeader(502)
		return
	}
	defer response.Body.Close()
	if response.StatusCode == 200 && ((watch && strings.HasSuffix(path, "/live")) || (secondary && r.Method == "GET" && probeActivity.MatchString(path))) {
		if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
			w.WriteHeader(502)
			return
		}
		p.stream(w, r, response, true)
		return
	}
	data, err := probeRead(response.Body, 16<<20)
	if err != nil {
		w.WriteHeader(502)
		return
	}
	data, err = p.transform(r, response, data, owner, fixture, generation, message, inspected)
	if err != nil {
		p.event(map[string]any{"method": r.Method, "path": path, "error_type": "projection"})
		if path == "/backend-api/celsius/ws/user" {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(503)
		} else {
			w.WriteHeader(502)
		}
		return
	}
	p.event(map[string]any{"method": r.Method, "path": path, "status": response.StatusCode, "has_authorization": r.Header.Get("Authorization") != "", "routed_owner_alias": func() string {
		if owner != nil {
			return owner.Alias
		}
		return ""
	}()})
	probeResponseHeaders(w.Header(), response.Header, secondary, true)
	if r.Method == "HEAD" {
		if length := response.Header.Get("Content-Length"); length != "" {
			w.Header().Set("Content-Length", length)
		}
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	}
	w.WriteHeader(response.StatusCode)
	if r.Method != "HEAD" {
		_, _ = w.Write(data)
	}
}

func probeRead(reader io.Reader, limit int64) ([]byte, error) {
	var buffer bytes.Buffer
	n, err := buffer.ReadFrom(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, fmt.Errorf("body exceeds bound")
	}
	return buffer.Bytes(), nil
}

var probeCookieDomain = regexp.MustCompile(`(?i);\s*Domain=[^;]+`)

func probeResponseHeaders(dst, src http.Header, secondary, identity bool) {
	for key, values := range probeHeaders(src) {
		if strings.EqualFold(key, "Content-Length") || identity && strings.EqualFold(key, "Content-Encoding") {
			continue
		}
		for _, value := range values {
			if strings.EqualFold(key, "Set-Cookie") {
				if secondary {
					continue
				}
				value = probeCookieDomain.ReplaceAllString(value, "")
			}
			dst.Add(key, value)
		}
	}
}

func (p *Probe) stream(w http.ResponseWriter, r *http.Request, response *http.Response, secondary bool) {
	started := p.options.Now()
	reason := "eof"
	stream := strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream")
	if stream {
		defer func() {
			if r.Context().Err() != nil {
				reason = "cancelled"
			}
			p.event(map[string]any{"operation": "native_event_stream", "secondary": secondary, "status": response.StatusCode, "end": reason, "lifetime_ms": p.options.Now().Sub(started).Milliseconds()})
		}()
	}
	probeResponseHeaders(w.Header(), response.Header, secondary, false)
	if !stream {
		if length := response.Header.Get("Content-Length"); length != "" {
			w.Header().Set("Content-Length", length)
		}
	}
	w.WriteHeader(response.StatusCode)
	if f, ok := w.(http.Flusher); ok && stream {
		f.Flush()
	}
	if r.Method == "HEAD" {
		return
	}
	buffer := make([]byte, 65536)
	for {
		n, err := response.Body.Read(buffer)
		if n > 0 {
			if _, err := w.Write(buffer[:n]); err != nil {
				reason = "write_error"
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				reason = "read_error"
			}
			return
		}
	}
}

func (p *Probe) forward(w http.ResponseWriter, r *http.Request, owner *Account) {
	summary := map[string]any{"method": r.Method, "path": r.URL.EscapedPath(), "native_primary_passthrough": owner == nil}
	if owner != nil {
		summary["owner_file_download"] = true
		summary["routed_owner_alias"] = owner.Alias
	}
	defer func() { p.event(summary) }()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.options.Origin+r.URL.RequestURI(), r.Body)
	if err != nil {
		w.WriteHeader(502)
		return
	}
	req.Header = probeHeaders(r.Header)
	req.ContentLength = r.ContentLength
	if owner != nil {
		req.Header = OwnerHeaders(req.Header, *owner)
	}
	observe := r.Method == "GET" && r.URL.Path == "/backend-api/tbo/primary"
	if observe {
		req.Header.Set("Accept-Encoding", "identity")
	}
	response, err := p.options.HTTP.Do(req)
	if err != nil {
		summary["error_type"] = "upstream"
		w.WriteHeader(502)
		return
	}
	defer response.Body.Close()
	summary["status"] = response.StatusCode
	if observe && response.StatusCode == 200 {
		data, err := probeReadPrefix(response.Body, 1<<20)
		if err == nil && len(data) <= 1<<20 {
			if err := p.observePrimary(r.Context(), r.Header, data); err != nil {
				p.event(map[string]any{"native_primary_observation_failed": true})
			}
		} else if err != nil {
			w.WriteHeader(502)
			return
		}
		response.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(data), response.Body), response.Body}
	}
	p.stream(w, r, response, owner != nil)
}

func probeReadPrefix(reader io.Reader, limit int64) ([]byte, error) {
	var buffer bytes.Buffer
	_, err := buffer.ReadFrom(io.LimitReader(reader, limit+1))
	return buffer.Bytes(), err
}
