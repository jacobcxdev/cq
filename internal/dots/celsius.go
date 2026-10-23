package dots

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// CelsiusSocket is the message boundary used by the owner-pinned broker.
type CelsiusSocket interface {
	ReadMessage() (int, []byte, error)
	WriteMessage(int, []byte) error
	Close() error
}

type CelsiusURLResolver func(context.Context, string) (string, error)

type celsiusGrant struct {
	expires time.Time
	urls    map[string]string
	owners  map[string]Account
	resolve CelsiusURLResolver
}

// CelsiusBroker retains signed URLs and credentials only in process memory.
// Its loopback listener is reached through the native CONNECT proxy.
type CelsiusBroker struct {
	Settings                                             *Settings
	Event                                                func(map[string]any)
	Dial                                                 func(context.Context, string) (CelsiusSocket, error)
	mu                                                   sync.Mutex
	grants                                               map[string]celsiusGrant
	now                                                  func() time.Time
	ackTimeout, retryInitial, retryMaximum, tickInterval time.Duration
}

func NewCelsiusBroker(cfg *Settings, event func(map[string]any)) *CelsiusBroker {
	b := &CelsiusBroker{Settings: cfg, Event: event, grants: make(map[string]celsiusGrant), now: time.Now,
		ackTimeout: 20 * time.Second, retryInitial: time.Second, retryMaximum: 30 * time.Second, tickInterval: time.Second}
	b.Dial = func(ctx context.Context, endpoint string) (CelsiusSocket, error) {
		roots, err := cloudPublicTLS()
		if err != nil {
			return nil, err
		}
		dialer := websocket.Dialer{HandshakeTimeout: 20 * time.Second, EnableCompression: false, TLSClientConfig: roots}
		conn, response, err := dialer.DialContext(ctx, endpoint, nil)
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if err != nil {
			return nil, errors.New("Celsius upstream upgrade failed")
		}
		conn.SetReadLimit(2 * 1024 * 1024)
		return conn, nil
	}
	return b
}

func (b *CelsiusBroker) event(value map[string]any) {
	defer func() { recover() }()
	if b.Event != nil {
		b.Event(value)
	}
}

func celsiusValidateURL(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "wss" || u.Hostname() != "ws.chatgpt.com" || (u.Port() != "" && u.Port() != "443") {
		return errors.New("Unexpected Celsius service origin")
	}
	return nil
}

func celsiusAliases(primary string, owners map[string]Account) []string {
	set := map[string]bool{primary: true}
	for _, owner := range owners {
		set[owner.Alias] = true
	}
	secondary := make([]string, 0, len(set))
	for alias := range set {
		if alias != primary {
			secondary = append(secondary, alias)
		}
	}
	slices.Sort(secondary)
	return append([]string{primary}, secondary...)
}

// Issue grants one native fan-out session. Secondary URL lookup may be deferred
// so a slow owner cannot delay the primary's native connection.
func (b *CelsiusBroker) Issue(_ context.Context, urls map[string]string, owners map[string]Account, resolve CelsiusURLResolver) (string, error) {
	aliases := celsiusAliases(b.Settings.SystemAlias, owners)
	expected := map[string]bool{b.Settings.SystemAlias: true}
	for _, dot := range b.Settings.Dots {
		expected[dot.OwnerAlias] = true
	}
	if len(aliases) != len(expected) {
		return "", errors.New("Configured owner URLs required")
	}
	for _, alias := range aliases {
		if !expected[alias] {
			return "", errors.New("Configured owner URLs required")
		}
	}
	urlCount := len(aliases)
	if resolve != nil {
		urlCount = 1
	}
	if len(urls) != urlCount {
		return "", errors.New("Configured owner URLs required")
	}
	for alias, endpoint := range urls {
		if !expected[alias] || (resolve != nil && alias != b.Settings.SystemAlias) {
			return "", errors.New("Configured owner URLs required")
		}
		if err := celsiusValidateURL(endpoint); err != nil {
			return "", err
		}
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	path := "/cq-owner-events/" + base64.RawURLEncoding.EncodeToString(random[:])
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for key, grant := range b.grants {
		if !grant.expires.After(now) {
			delete(b.grants, key)
		}
	}
	if len(b.grants) >= 16 {
		return "", errors.New("Celsius URL grant limit")
	}
	urlCopy := make(map[string]string, len(urls))
	for alias, endpoint := range urls {
		urlCopy[alias] = endpoint
	}
	ownerCopy := make(map[string]Account, len(owners))
	for resource, owner := range owners {
		viewerIDs := make(map[string]bool, len(owner.ViewerIDs))
		for viewer, value := range owner.ViewerIDs {
			viewerIDs[viewer] = value
		}
		owner.ViewerIDs = viewerIDs
		ownerCopy[resource] = owner
	}
	b.grants[path] = celsiusGrant{now.Add(1500 * time.Second), urlCopy, ownerCopy, resolve}
	return "wss://ws.chatgpt.com" + path, nil
}

// Serve publishes the already-bound loopback port and serves until cancellation.
func (b *CelsiusBroker) Serve(ctx context.Context, listener net.Listener, tlsConfig *tls.Config) error {
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return err
	}
	if err := launchWrite(filepath.Join(b.Settings.RuntimeRoot, "celsius-port"), []byte(port), 0o600); err != nil {
		return err
	}
	if tlsConfig != nil {
		listener = tls.NewListener(listener, tlsConfig)
	}
	b.event(map[string]any{"celsius_broker_port": port})
	server := &http.Server{Handler: b, ReadHeaderTimeout: 20 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	closed := make(chan struct{})
	defer close(closed)
	go func() {
		defer func() {
			if recover() != nil {
				server.Close()
			}
		}()
		select {
		case <-ctx.Done():
			server.Close()
		case <-closed:
		}
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (b *CelsiusBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{HandshakeTimeout: 20 * time.Second, CheckOrigin: func(*http.Request) bool { return true }}
	client, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client.SetReadLimit(2 * 1024 * 1024)
	b.mu.Lock()
	grant, ok := b.grants[r.URL.Path]
	b.mu.Unlock()
	if !ok || !grant.expires.After(b.now()) {
		client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1008, "Unknown or expired local session"), time.Now().Add(3*time.Second))
		client.Close()
		return
	}
	b.handle(r.Context(), client, grant)
}

func celsiusTargets(command map[string]any, primary string, owners map[string]Account) ([]string, error) {
	kind, _ := command["type"].(string)
	topic, _ := command["topic_id"].(string)
	if kind == "connect" || kind == "presence" || ((kind == "subscribe" || kind == "unsubscribe") && celsiusTopic(topic)) {
		return celsiusAliases(primary, owners), nil
	}
	if (kind == "subscribe" || kind == "unsubscribe") && strings.HasPrefix(topic, "calpico-room-live:") {
		owner, ok := owners[strings.TrimPrefix(topic, "calpico-room-live:")]
		if !ok {
			return nil, errors.New("Unknown live room")
		}
		return []string{owner.Alias}, nil
	}
	return []string{primary}, nil
}

func celsiusTopic(topic string) bool {
	return topic == "calpico-chatgpt-messaging" || topic == "calpico-chatgpt-messaging-all"
}
func celsiusClone(value map[string]any) map[string]any {
	data, _ := json.Marshal(value)
	cloned, _ := celsiusDecode(data)
	return cloned.(map[string]any)
}

func celsiusSecondaryFrame(frame map[string]any, owners map[string]Account, source string) map[string]any {
	if frame["type"] != "message" {
		return nil
	}
	payload, ok := frame["payload"].(map[string]any)
	if !ok {
		return nil
	}
	kind, _ := payload["type"].(string)
	switch kind {
	case "calpico-room-read-receipt", "calpico-is-responding-heartbeat", "calpico-message-update", "calpico-room-update", "calpico-message-add", "calpico-room-read", "calpico-room-metadata-update", "calpico-room-leave", "calpico-room-live":
	default:
		return nil
	}
	value, ok := payload["payload"].(map[string]any)
	if !ok {
		return nil
	}
	room, _ := value["room_id"].(string)
	if room == "" {
		if nested, ok := value["room"].(map[string]any); ok {
			room, _ = nested["id"].(string)
		}
	}
	owner, ok := owners[room]
	if !ok || owner.Alias != source {
		return nil
	}
	topic, _ := frame["topic_id"].(string)
	if !celsiusTopic(topic) && topic != "calpico-room-live:"+room {
		return nil
	}
	if celsiusConflicting(payload, owners, source) {
		return nil
	}
	result := celsiusClone(frame)
	delete(result, "offset")
	projected := result["payload"].(map[string]any)["payload"].(map[string]any)
	if message, ok := projected["message"].(map[string]any); ok {
		celsiusProjectMessage(message, owner.ViewerIDs)
	}
	if envelope, ok := projected["room"].(map[string]any); ok {
		celsiusProjectEnvelope(envelope, owner.ViewerIDs)
	}
	return result
}

func celsiusConflicting(value any, owners map[string]Account, source string) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			if key == "room_id" || key == "thread_id" || key == "aeon_id" {
				if resource, ok := item.(string); ok {
					if owner, known := owners[resource]; known && owner.Alias != source {
						return true
					}
				}
			}
			if celsiusConflicting(item, owners, source) {
				return true
			}
		}
	case []any:
		for _, item := range value {
			if celsiusConflicting(item, owners, source) {
				return true
			}
		}
	}
	return false
}
func celsiusProjectMessage(message map[string]any, viewers map[string]bool) {
	_, raw := message["raw_messages"]
	role, exists := message["role"]
	viewer, _ := message["account_user_id"].(string)
	if !raw && (!exists || role == "user") && viewers[viewer] {
		message["is_self"] = true
	}
	if reply, ok := message["reply_to"].(map[string]any); ok {
		if nested, ok := reply["message"].(map[string]any); ok {
			celsiusProjectMessage(nested, viewers)
		}
	}
}
func celsiusProjectEnvelope(envelope map[string]any, viewers map[string]bool) {
	for _, key := range []string{"items", "latest_messages"} {
		if messages, ok := envelope[key].([]any); ok {
			for _, value := range messages {
				if message, ok := value.(map[string]any); ok {
					celsiusProjectMessage(message, viewers)
				}
			}
		}
	}
}

func celsiusID(value any) (int64, bool) {
	switch id := value.(type) {
	case json.Number:
		n, err := strconv.ParseInt(string(id), 10, 64)
		return n, err == nil
	case float64:
		n := int64(id)
		return n, float64(n) == id
	case int:
		return int64(id), true
	case int64:
		return id, true
	}
	return 0, false
}
func celsiusErrorType(err error) string {
	if err == nil || errors.Is(err, io.EOF) {
		return "EOF"
	}
	var close *websocket.CloseError
	if errors.As(err, &close) {
		return "ConnectionClosed"
	}
	return fmt.Sprintf("%T", err)
}
