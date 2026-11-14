package harness

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const maxDesktopFrameBytes = 0x10000000

var ErrDesktopUnavailable = errors.New("desktop IPC is unavailable")

// Snapshot contains only the settings needed to apply and restore a profile.
// Conversation contents received during follower synchronisation are discarded.
type Snapshot struct {
	ChatID               string
	OwnerID              string
	Model                string
	ModelProvider        string
	Cwd                  string
	DisabledPluginIDs    []string
	PluginBaselineKnown  bool
	ReasoningEffort      *string
	ReasoningEffortKnown bool
	ActiveTurnID         string
	Active               bool
}

// DesktopIPC connects to the stock Desktop router. The zero value uses the
// normal CODEX_HOME IPC socket. DialContext is injectable for transport tests.
type DesktopIPC struct {
	SocketPath  string
	DialContext func(context.Context, string, string) (net.Conn, error)
}

// ReadModel reads the owner's current model without following the conversation
// stream. This protocol does not return cwd or plugin exclusions; retain those
// from Inspect rather than treating their absence as an empty baseline.
func (client DesktopIPC) ReadModel(ctx context.Context, chatID string) (Snapshot, error) {
	if !canonicalChatID(chatID) {
		return Snapshot{}, fmt.Errorf("desktop chat ID must be a canonical UUID")
	}
	session, err := client.connect(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer session.close()
	if err := session.discover(chatID); err != nil {
		return Snapshot{}, err
	}
	response, err := session.request("thread-follower-read-model-settings", 1, map[string]any{"conversationId": chatID}, session.ownerID)
	if err != nil {
		return Snapshot{}, err
	}
	settings := response.Result.Settings
	if response.HandledByClientID != session.ownerID || settings == nil || settings.Model == "" {
		return Snapshot{}, fmt.Errorf("desktop model response identity or settings mismatch")
	}
	snapshot := Snapshot{ChatID: chatID, OwnerID: session.ownerID, Model: settings.Model, ModelProvider: settings.ModelProvider}
	if len(settings.ReasoningEffort) > 0 {
		if err := json.Unmarshal(settings.ReasoningEffort, &snapshot.ReasoningEffort); err != nil {
			return Snapshot{}, fmt.Errorf("invalid desktop reasoning effort")
		}
		snapshot.ReasoningEffortKnown = true
	}
	return snapshot, nil
}

// Inspect briefly follows one owned chat to capture its actual plugin baseline.
// It never stores or logs the conversation snapshot and always disconnects.
func (client DesktopIPC) Inspect(ctx context.Context, chatID string) (Snapshot, error) {
	if !canonicalChatID(chatID) {
		return Snapshot{}, fmt.Errorf("desktop chat ID must be a canonical UUID")
	}
	session, err := client.connect(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer session.close()
	if err := session.discover(chatID); err != nil {
		return Snapshot{}, err
	}
	if err := session.follow(true); err != nil {
		return Snapshot{}, err
	}
	session.following = true
	for range 256 {
		message, err := session.read()
		if err != nil {
			return Snapshot{}, err
		}
		if message.Type != "broadcast" || message.Method != "thread-stream-state-changed" || message.SourceClientID != session.ownerID || message.Params.ConversationID != chatID || message.Params.HostID != "local" {
			continue
		}
		if message.Version != 11 && message.Version != 13 {
			return Snapshot{}, fmt.Errorf("unsupported desktop snapshot version %d", message.Version)
		}
		if message.Params.Change.Type != "snapshot" {
			continue
		}
		state := message.Params.Change.ConversationState
		if state.ID != chatID {
			return Snapshot{}, fmt.Errorf("desktop snapshot chat mismatch")
		}
		snapshot := Snapshot{ChatID: chatID, OwnerID: session.ownerID, ModelProvider: state.ModelProvider, Active: state.ThreadRuntimeStatus.Type == "active"}
		if settings := state.LatestThreadSettings; settings != nil {
			snapshot.Model, snapshot.Cwd = settings.Model, settings.Cwd
			if settings.ModelProvider != "" {
				snapshot.ModelProvider = settings.ModelProvider
			}
			if len(settings.DisabledPluginIDs) > 0 && string(settings.DisabledPluginIDs) != "null" {
				if err := json.Unmarshal(settings.DisabledPluginIDs, &snapshot.DisabledPluginIDs); err != nil {
					return Snapshot{}, fmt.Errorf("invalid desktop plugin baseline")
				}
				if err := validateDesktopPlugins(snapshot.DisabledPluginIDs); err != nil {
					return Snapshot{}, err
				}
				snapshot.PluginBaselineKnown = true
			}
			if len(settings.Effort) > 0 {
				if err := json.Unmarshal(settings.Effort, &snapshot.ReasoningEffort); err != nil {
					return Snapshot{}, fmt.Errorf("invalid desktop reasoning effort")
				}
				snapshot.ReasoningEffortKnown = true
			}
		}
		for index := len(state.Turns) - 1; index >= 0; index-- {
			if state.Turns[index].Status == "inProgress" {
				snapshot.ActiveTurnID = state.Turns[index].TurnID
				snapshot.Active = true
				break
			}
		}
		return snapshot, nil
	}
	return Snapshot{}, fmt.Errorf("desktop snapshot message limit exceeded")
}

// UpdatePlugins replaces only the plugin exclusions for the next turn. It
// rediscovers the exact owner and guards both model and effort. A false result
// means Desktop declined the condition; plugin-list CAS is not supported.
func (client DesktopIPC) UpdatePlugins(ctx context.Context, snapshot Snapshot, newIDs []string) (bool, error) {
	if !canonicalChatID(snapshot.ChatID) || !canonicalChatID(snapshot.OwnerID) || snapshot.Model == "" || !snapshot.PluginBaselineKnown || !snapshot.ReasoningEffortKnown {
		return false, fmt.Errorf("desktop plugin update requires a known snapshot baseline, model and effort")
	}
	if err := validateDesktopPlugins(newIDs); err != nil {
		return false, err
	}
	session, err := client.connect(ctx)
	if err != nil {
		return false, err
	}
	defer session.close()
	if err := session.discover(snapshot.ChatID); err != nil {
		return false, err
	}
	if session.ownerID != snapshot.OwnerID {
		return false, fmt.Errorf("desktop chat owner changed")
	}
	ids := append([]string{}, newIDs...)
	response, err := session.request("thread-follower-update-thread-settings", 2, map[string]any{
		"conversationId": snapshot.ChatID,
		"threadSettings": map[string]any{"disabledPluginIds": ids},
		"condition":      map[string]any{"ifModelEquals": snapshot.Model, "ifEffortEquals": snapshot.ReasoningEffort},
	}, snapshot.OwnerID)
	if err != nil {
		return false, err
	}
	if response.HandledByClientID != snapshot.OwnerID || response.Result.Applied == nil {
		return false, fmt.Errorf("desktop plugin update response identity mismatch")
	}
	return *response.Result.Applied, nil
}

func validateDesktopPlugins(ids []string) error {
	for _, id := range ids {
		if id == "" || len(id) > 1024 || strings.TrimSpace(id) != id || strings.ContainsRune(id, '\x00') {
			return fmt.Errorf("invalid desktop plugin ID")
		}
	}
	return nil
}

type desktopMessage struct {
	Type              string `json:"type"`
	Method            string `json:"method"`
	RequestID         string `json:"requestId"`
	SourceClientID    string `json:"sourceClientId"`
	HandledByClientID string `json:"handledByClientId"`
	ResultType        string `json:"resultType"`
	Version           int    `json:"version"`
	Result            struct {
		ClientID string `json:"clientId"`
		Applied  *bool  `json:"applied"`
		Settings *struct {
			Model           string          `json:"model"`
			ModelProvider   string          `json:"modelProvider"`
			ReasoningEffort json.RawMessage `json:"reasoningEffort"`
		} `json:"settings"`
	} `json:"result"`
	Params struct {
		ConversationID string `json:"conversationId"`
		HostID         string `json:"hostId"`
		Change         struct {
			Type              string `json:"type"`
			ConversationState struct {
				ID                   string `json:"id"`
				ModelProvider        string `json:"modelProvider"`
				LatestThreadSettings *struct {
					Model             string          `json:"model"`
					ModelProvider     string          `json:"modelProvider"`
					Cwd               string          `json:"cwd"`
					Effort            json.RawMessage `json:"effort"`
					DisabledPluginIDs json.RawMessage `json:"disabledPluginIds"`
				} `json:"latestThreadSettings"`
				ThreadRuntimeStatus struct {
					Type string `json:"type"`
				} `json:"threadRuntimeStatus"`
				Turns []struct {
					TurnID string `json:"turnId"`
					Status string `json:"status"`
				} `json:"turns"`
			} `json:"conversationState"`
		} `json:"change"`
	} `json:"params"`
}

type desktopSession struct {
	conn                      net.Conn
	ctx                       context.Context
	cancel                    context.CancelFunc
	stopCancellation          func() bool
	cancellationDone          chan struct{}
	clientID, ownerID, chatID string
	following                 bool
}

func (client DesktopIPC) connect(parent context.Context) (*desktopSession, error) {
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("stock desktop Unix IPC is unavailable on Windows")
	}
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	path := client.SocketPath
	if path == "" {
		home := os.Getenv("CODEX_HOME")
		if home == "" {
			userHome, err := os.UserHomeDir()
			if err != nil {
				cancel()
				return nil, err
			}
			home = filepath.Join(userHome, ".codex")
		}
		path = filepath.Join(home, "ipc", "ipc.sock")
	}
	before, err := validateDesktopSocket(path)
	if err != nil {
		cancel()
		return nil, err
	}
	dial := client.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, "unix", path)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %w", ErrDesktopUnavailable, err)
	}
	after, err := validateDesktopSocket(path)
	if err != nil || !os.SameFile(before, after) {
		_ = conn.Close()
		cancel()
		return nil, fmt.Errorf("desktop IPC socket changed while connecting")
	}
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		cancel()
		return nil, err
	}
	session := &desktopSession{conn: conn, ctx: ctx, cancel: cancel, cancellationDone: make(chan struct{})}
	session.stopCancellation = context.AfterFunc(ctx, func() {
		defer close(session.cancellationDone)
		defer func() { _ = recover() }()
		_ = conn.SetDeadline(time.Now())
	})
	response, err := session.request("initialize", 0, map[string]any{"clientType": "cli"}, "")
	if err != nil {
		session.close()
		return nil, err
	}
	if !canonicalChatID(response.Result.ClientID) {
		session.close()
		return nil, fmt.Errorf("invalid desktop IPC client identity")
	}
	session.clientID = response.Result.ClientID
	return session, nil
}

func validateDesktopSocket(path string) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != "ipc.sock" || filepath.Base(filepath.Dir(path)) != "ipc" {
		return nil, fmt.Errorf("invalid desktop IPC socket path")
	}
	fsys := fsutil.OSFileSystem{}
	if err := fsutil.ValidateSecureDirectory(fsys, filepath.Dir(path)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %w", ErrDesktopUnavailable, err)
		}
		return nil, fmt.Errorf("unsafe desktop IPC directory: %w", err)
	}
	if err := fsutil.ValidateOwnerControlledDirectory(fsys, filepath.Dir(filepath.Dir(path))); err != nil {
		return nil, fmt.Errorf("unsafe desktop home directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %w", ErrDesktopUnavailable, err)
		}
		return nil, fmt.Errorf("inspect desktop IPC socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return nil, fmt.Errorf("unsafe desktop IPC socket type or mode")
	}
	if err := fsutil.ValidateSecureOwner(fsys, info); err != nil {
		return nil, fmt.Errorf("unsafe desktop IPC socket owner: %w", err)
	}
	return info, nil
}

func (session *desktopSession) discover(chatID string) error {
	response, err := session.request("thread-owner-discovery", 1, map[string]any{"hostId": "local", "conversationId": chatID}, "")
	if err != nil {
		return err
	}
	if !canonicalChatID(response.HandledByClientID) {
		return fmt.Errorf("invalid desktop chat owner identity")
	}
	session.ownerID, session.chatID = response.HandledByClientID, chatID
	return nil
}

func (session *desktopSession) follow(enabled bool) error {
	return session.write(map[string]any{"type": "broadcast", "method": "thread-stream-following-changed", "sourceClientId": session.clientID, "targetClientIds": []string{session.ownerID}, "version": 1, "params": map[string]any{"conversationId": session.chatID, "hostId": "local", "following": enabled}})
}

func (session *desktopSession) close() {
	if session.stopCancellation != nil && !session.stopCancellation() {
		<-session.cancellationDone
	}
	if session.following {
		_ = session.conn.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
		_ = session.follow(false)
	}
	_ = session.conn.Close()
	session.cancel()
}

func (session *desktopSession) request(method string, version int, params any, target string) (desktopMessage, error) {
	id := rand.Text()
	message := map[string]any{"type": "request", "requestId": id, "version": version, "method": method, "params": params, "timeoutMs": 5000}
	if session.clientID != "" {
		message["sourceClientId"] = session.clientID
	}
	if target != "" {
		message["targetClientId"] = target
	}
	if err := session.write(message); err != nil {
		return desktopMessage{}, err
	}
	for range 256 {
		response, err := session.read()
		if err != nil {
			return desktopMessage{}, err
		}
		if response.Type != "response" || response.RequestID != id {
			continue
		}
		if response.ResultType != "success" {
			return desktopMessage{}, fmt.Errorf("desktop %s request rejected", method)
		}
		if response.Method != method {
			return desktopMessage{}, fmt.Errorf("desktop response method mismatch")
		}
		return response, nil
	}
	return desktopMessage{}, fmt.Errorf("desktop response message limit exceeded")
}

func (session *desktopSession) write(message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode desktop IPC message: %w", err)
	}
	if len(data) == 0 || len(data) > maxDesktopFrameBytes {
		return fmt.Errorf("desktop IPC frame exceeds size limit")
	}
	var header [4]byte
	binary.LittleEndian.PutUint32(header[:], uint32(len(data)))
	for _, buffer := range [][]byte{header[:], data} {
		for len(buffer) > 0 {
			count, err := session.conn.Write(buffer)
			if err != nil {
				return session.transportError(err)
			}
			if count == 0 {
				return io.ErrShortWrite
			}
			buffer = buffer[count:]
		}
	}
	return nil
}

func (session *desktopSession) read() (desktopMessage, error) {
	var message desktopMessage
	var header [4]byte
	if _, err := io.ReadFull(session.conn, header[:]); err != nil {
		return message, session.transportError(err)
	}
	size := binary.LittleEndian.Uint32(header[:])
	if size == 0 || size > maxDesktopFrameBytes {
		return message, fmt.Errorf("invalid desktop IPC frame size")
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(session.conn, data); err != nil {
		return message, session.transportError(err)
	}
	if err := json.Unmarshal(data, &message); err != nil {
		return desktopMessage{}, fmt.Errorf("invalid desktop IPC JSON")
	}
	return message, nil
}

func (session *desktopSession) transportError(err error) error {
	if cause := session.ctx.Err(); cause != nil {
		return cause
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return fmt.Errorf("desktop IPC transport: %w", err)
}
