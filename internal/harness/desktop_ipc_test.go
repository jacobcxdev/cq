package harness

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

const desktopTestChat = "01a198eb-f8e9-72a1-856c-0e986d10f6f7"
const desktopTestOwner = "11111111-2222-3333-4444-555555555555"

func desktopTestServer(t *testing.T, serve func(net.Conn)) DesktopIPC {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stock Desktop Unix IPC")
	}
	dir, err := os.MkdirTemp("", "cq-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ipcDir := filepath.Join(dir, "ipc")
	if err := os.Mkdir(ipcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ipcDir, "ipc.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if value := recover(); value != nil {
				t.Errorf("test peer panic: %v", value)
			}
		}()
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		serve(conn)
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	return DesktopIPC{SocketPath: path}
}

func desktopReadTest(t *testing.T, conn net.Conn) map[string]any {
	t.Helper()
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		t.Error(err)
		return nil
	}
	data := make([]byte, binary.LittleEndian.Uint32(header[:]))
	if _, err := io.ReadFull(conn, data); err != nil {
		t.Error(err)
		return nil
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Error(err)
	}
	return result
}

func desktopWriteTest(t *testing.T, conn net.Conn, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Error(err)
		return
	}
	frame := make([]byte, len(data)+4)
	binary.LittleEndian.PutUint32(frame, uint32(len(data)))
	copy(frame[4:], data)
	if _, err := conn.Write(frame); err != nil {
		t.Error(err)
	}
}

func desktopHandshakeTest(t *testing.T, conn net.Conn, owner string) {
	t.Helper()
	request := desktopReadTest(t, conn)
	if request["method"] != "initialize" {
		t.Errorf("initial method = %v", request["method"])
	}
	desktopWriteTest(t, conn, map[string]any{"type": "response", "requestId": "unrelated-request", "resultType": "error"})
	desktopWriteTest(t, conn, map[string]any{"type": "response", "requestId": request["requestId"], "resultType": "success", "method": "initialize", "result": map[string]any{"clientId": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}})
	request = desktopReadTest(t, conn)
	if request["method"] != "thread-owner-discovery" || request["version"] != float64(1) {
		t.Errorf("owner request = %v", request)
	}
	desktopWriteTest(t, conn, map[string]any{"type": "response", "requestId": request["requestId"], "resultType": "success", "method": "thread-owner-discovery", "handledByClientId": owner, "result": map[string]any{"supportsUntrustedAppInput": true}})
}

func desktopSnapshotTest(t *testing.T, conn net.Conn, owner string, settings map[string]any) {
	t.Helper()
	desktopWriteTest(t, conn, map[string]any{"type": "broadcast", "method": "thread-stream-state-changed", "sourceClientId": owner, "version": 11, "params": map[string]any{"hostId": "local", "conversationId": desktopTestChat, "change": map[string]any{"type": "snapshot", "conversationState": map[string]any{"id": desktopTestChat, "modelProvider": "openai", "latestThreadSettings": settings, "turns": []any{map[string]any{"turnId": "active-turn", "status": "inProgress", "items": []any{map[string]any{"text": "must never be retained"}}}}}}}})
}

func TestDesktopIPCInspectCapturedBaselineAndUnfollowed(t *testing.T) {
	client := desktopTestServer(t, func(conn net.Conn) {
		desktopHandshakeTest(t, conn, desktopTestOwner)
		follow := desktopReadTest(t, conn)
		if follow["method"] != "thread-stream-following-changed" || follow["version"] != float64(1) {
			t.Errorf("follow = %v", follow)
		}
		settings := map[string]any{"model": "glm-5.3", "cwd": "/project", "disabledPluginIds": []string{"user-choice"}, "effort": "medium"}
		desktopSnapshotTest(t, conn, "99999999-2222-3333-4444-555555555555", settings)
		desktopSnapshotTest(t, conn, desktopTestOwner, settings)
		unfollow := desktopReadTest(t, conn)
		if unfollow["params"].(map[string]any)["following"] != false {
			t.Error("did not unfollow")
		}
	})
	snapshot, err := client.Inspect(context.Background(), desktopTestChat)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.PluginBaselineKnown || !reflect.DeepEqual(snapshot.DisabledPluginIDs, []string{"user-choice"}) || snapshot.Model != "glm-5.3" || snapshot.ModelProvider != "openai" || snapshot.Cwd != "/project" || snapshot.OwnerID != desktopTestOwner || snapshot.ActiveTurnID != "active-turn" || snapshot.ReasoningEffort == nil || *snapshot.ReasoningEffort != "medium" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestDesktopIPCInspectKeptMissingBaselineUnknown(t *testing.T) {
	client := desktopTestServer(t, func(conn net.Conn) {
		desktopHandshakeTest(t, conn, desktopTestOwner)
		_ = desktopReadTest(t, conn)
		desktopSnapshotTest(t, conn, desktopTestOwner, map[string]any{"model": "glm-5.3", "cwd": "/project", "effort": nil})
		_ = desktopReadTest(t, conn)
	})
	snapshot, err := client.Inspect(context.Background(), desktopTestChat)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.PluginBaselineKnown {
		t.Fatal("missing baseline was treated as empty")
	}
}

func TestDesktopIPCUpdateGuardedModelEffortAndOwner(t *testing.T) {
	for _, effort := range []*string{nil, desktopString("medium")} {
		t.Run(map[bool]string{true: "null", false: "medium"}[effort == nil], func(t *testing.T) {
			client := desktopTestServer(t, func(conn net.Conn) {
				desktopHandshakeTest(t, conn, desktopTestOwner)
				request := desktopReadTest(t, conn)
				if request["method"] != "thread-follower-update-thread-settings" || request["version"] != float64(2) || request["targetClientId"] != desktopTestOwner {
					t.Errorf("update envelope = %v", request)
				}
				params := request["params"].(map[string]any)
				condition := params["condition"].(map[string]any)
				value, present := condition["ifEffortEquals"]
				if !present || condition["ifModelEquals"] != "glm-5.3" || effort == nil && value != nil || effort != nil && value != *effort {
					t.Errorf("condition = %v", condition)
				}
				if !reflect.DeepEqual(params["threadSettings"].(map[string]any)["disabledPluginIds"], []any{"user-choice", "plugin"}) {
					t.Errorf("settings = %v", params)
				}
				desktopWriteTest(t, conn, map[string]any{"type": "response", "requestId": request["requestId"], "resultType": "success", "method": request["method"], "handledByClientId": desktopTestOwner, "result": map[string]any{"applied": true}})
			})
			applied, err := client.UpdatePlugins(context.Background(), Snapshot{ChatID: desktopTestChat, OwnerID: desktopTestOwner, Model: "glm-5.3", PluginBaselineKnown: true, ReasoningEffort: effort, ReasoningEffortKnown: true}, []string{"user-choice", "plugin"})
			if err != nil || !applied {
				t.Fatalf("update = %v, %v", applied, err)
			}
		})
	}
}

func desktopString(value string) *string { return &value }

func TestDesktopIPCRejectedChangedOwnerBeforeWrite(t *testing.T) {
	client := desktopTestServer(t, func(conn net.Conn) {
		desktopHandshakeTest(t, conn, "99999999-2222-3333-4444-555555555555")
		var one [1]byte
		if _, err := conn.Read(one[:]); !errors.Is(err, io.EOF) {
			t.Errorf("unexpected write after owner change: %v", err)
		}
	})
	_, err := client.UpdatePlugins(context.Background(), Snapshot{ChatID: desktopTestChat, OwnerID: desktopTestOwner, Model: "glm-5.3", PluginBaselineKnown: true, ReasoningEffortKnown: true}, nil)
	if err == nil {
		t.Fatal("changed owner accepted")
	}
}

func TestDesktopIPCCancelledAndUnfollowed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := desktopTestServer(t, func(conn net.Conn) {
		desktopHandshakeTest(t, conn, desktopTestOwner)
		_ = desktopReadTest(t, conn)
		cancel()
		unfollow := desktopReadTest(t, conn)
		if unfollow["params"].(map[string]any)["following"] != false {
			t.Error("cancel did not unfollow")
		}
	})
	_, err := client.Inspect(ctx, desktopTestChat)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestDesktopIPCRejectedOversizedFrame(t *testing.T) {
	client := desktopTestServer(t, func(conn net.Conn) {
		_ = desktopReadTest(t, conn)
		var header [4]byte
		binary.LittleEndian.PutUint32(header[:], 0x10000001)
		_, _ = conn.Write(header[:])
	})
	_, err := client.Inspect(context.Background(), desktopTestChat)
	if err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestDesktopIPCRejectedUnsafeSocket(t *testing.T) {
	client := desktopTestServer(t, func(conn net.Conn) { t.Error("unsafe socket was dialled") })
	if err := os.Chmod(client.SocketPath, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Inspect(context.Background(), desktopTestChat); err == nil {
		t.Fatal("unsafe socket accepted")
	}
}

func TestDesktopIPCRejectedUnsafeParentAndSymlinks(t *testing.T) {
	for _, unsafe := range []string{"parent-mode", "home-mode", "socket-symlink", "parent-symlink"} {
		t.Run(unsafe, func(t *testing.T) {
			client := desktopTestServer(t, func(conn net.Conn) { t.Error("unsafe path was dialled") })
			parent := filepath.Dir(client.SocketPath)
			switch unsafe {
			case "parent-mode":
				if err := os.Chmod(parent, 0o755); err != nil {
					t.Fatal(err)
				}
			case "home-mode":
				if err := os.Chmod(filepath.Dir(parent), 0o777); err != nil {
					t.Fatal(err)
				}
			case "socket-symlink":
				original := client.SocketPath + ".original"
				if err := os.Rename(client.SocketPath, original); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(original, client.SocketPath); err != nil {
					t.Fatal(err)
				}
			case "parent-symlink":
				original := parent + ".original"
				if err := os.Rename(parent, original); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(original, parent); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := client.Inspect(context.Background(), desktopTestChat); err == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
}

func TestDesktopIPCUpdateRejectedWrongResponderAndReportedCondition(t *testing.T) {
	for _, scenario := range []string{"wrong-responder", "condition-declined", "peer-error"} {
		t.Run(scenario, func(t *testing.T) {
			client := desktopTestServer(t, func(conn net.Conn) {
				desktopHandshakeTest(t, conn, desktopTestOwner)
				request := desktopReadTest(t, conn)
				owner, resultType := desktopTestOwner, "success"
				if scenario == "wrong-responder" {
					owner = "99999999-2222-3333-4444-555555555555"
				}
				if scenario == "peer-error" {
					resultType = "error"
				}
				desktopWriteTest(t, conn, map[string]any{"type": "response", "requestId": request["requestId"], "resultType": resultType, "method": request["method"], "handledByClientId": owner, "error": "must not expose private peer detail", "result": map[string]any{"applied": false}})
			})
			applied, err := client.UpdatePlugins(context.Background(), Snapshot{ChatID: desktopTestChat, OwnerID: desktopTestOwner, Model: "glm-5.3", PluginBaselineKnown: true, ReasoningEffortKnown: true}, nil)
			if applied {
				t.Fatal("unapplied update reported success")
			}
			if (err != nil) != (scenario != "condition-declined") {
				t.Fatalf("condition result error = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "private peer") {
				t.Fatal("peer detail leaked")
			}
		})
	}
}

func TestDesktopIPCUpdateRefusedUnknownBaselineBeforeDial(t *testing.T) {
	client := DesktopIPC{DialContext: func(context.Context, string, string) (net.Conn, error) {
		t.Error("unknown baseline dialled Desktop")
		return nil, errors.New("unexpected dial")
	}}
	for _, snapshot := range []Snapshot{
		{ChatID: desktopTestChat, OwnerID: desktopTestOwner, Model: "glm-5.3", ReasoningEffortKnown: true},
		{ChatID: desktopTestChat, OwnerID: desktopTestOwner, Model: "glm-5.3", PluginBaselineKnown: true},
	} {
		if _, err := client.UpdatePlugins(context.Background(), snapshot, nil); err == nil {
			t.Fatal("unknown state accepted")
		}
	}
}

func TestDesktopIPCInspectRejectedBadSnapshotAndUnfollowed(t *testing.T) {
	for _, scenario := range []string{"invalid-plugins", "invalid-effort", "null-baseline", "wrong-version"} {
		t.Run(scenario, func(t *testing.T) {
			client := desktopTestServer(t, func(conn net.Conn) {
				desktopHandshakeTest(t, conn, desktopTestOwner)
				_ = desktopReadTest(t, conn)
				settings := map[string]any{"model": "glm-5.3", "cwd": "/project", "effort": nil, "disabledPluginIds": []string{}}
				switch scenario {
				case "invalid-plugins":
					settings["disabledPluginIds"] = []int{1}
				case "invalid-effort":
					settings["effort"] = 7
				case "null-baseline":
					settings["disabledPluginIds"] = nil
				case "wrong-version":
					desktopWriteTest(t, conn, map[string]any{"type": "broadcast", "method": "thread-stream-state-changed", "sourceClientId": desktopTestOwner, "version": 10, "params": map[string]any{"conversationId": desktopTestChat, "hostId": "local"}})
				}
				if scenario != "wrong-version" {
					desktopSnapshotTest(t, conn, desktopTestOwner, settings)
				}
				unfollow := desktopReadTest(t, conn)
				if unfollow["params"].(map[string]any)["following"] != false {
					t.Error("failure did not unfollow")
				}
			})
			snapshot, err := client.Inspect(context.Background(), desktopTestChat)
			if scenario == "null-baseline" {
				if err != nil || snapshot.PluginBaselineKnown {
					t.Fatalf("null baseline = %+v, %v", snapshot, err)
				}
			} else if err == nil {
				t.Fatal("invalid snapshot accepted")
			}
		})
	}
}

func TestDesktopIPCUsedInjectedDialAndCorrelatedReply(t *testing.T) {
	client := desktopTestServer(t, func(conn net.Conn) {
		desktopHandshakeTest(t, conn, desktopTestOwner)
		_ = desktopReadTest(t, conn)
		desktopSnapshotTest(t, conn, desktopTestOwner, map[string]any{"model": "glm-5.3", "cwd": "/project", "effort": nil, "disabledPluginIds": []string{}})
		_ = desktopReadTest(t, conn)
	})
	called := false
	client.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		called = true
		if network != "unix" || address != client.SocketPath {
			t.Errorf("dial = %q %q", network, address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	if _, err := client.Inspect(context.Background(), desktopTestChat); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("injected dial was ignored")
	}
}

func TestDesktopIPCResolvedDefaultCodexHomeSocket(t *testing.T) {
	client := desktopTestServer(t, func(conn net.Conn) {
		desktopHandshakeTest(t, conn, desktopTestOwner)
		_ = desktopReadTest(t, conn)
		desktopSnapshotTest(t, conn, desktopTestOwner, map[string]any{"model": "glm-5.3", "cwd": "/project", "effort": nil, "disabledPluginIds": []string{}})
		_ = desktopReadTest(t, conn)
	})
	t.Setenv("CODEX_HOME", filepath.Dir(filepath.Dir(client.SocketPath)))
	if _, err := (DesktopIPC{}).Inspect(context.Background(), desktopTestChat); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopIPCReadModelAvoidedFollowerAndGuardedOwner(t *testing.T) {
	for _, scenario := range []string{"success", "wrong-owner", "missing-settings"} {
		t.Run(scenario, func(t *testing.T) {
			client := desktopTestServer(t, func(conn net.Conn) {
				desktopHandshakeTest(t, conn, desktopTestOwner)
				request := desktopReadTest(t, conn)
				if request["method"] != "thread-follower-read-model-settings" || request["version"] != float64(1) || request["targetClientId"] != desktopTestOwner {
					t.Errorf("model envelope = %v", request)
				}
				owner := desktopTestOwner
				if scenario == "wrong-owner" {
					owner = "99999999-2222-3333-4444-555555555555"
				}
				settings := any(map[string]any{"model": "glm-5.3", "modelProvider": "openai", "reasoningEffort": "medium"})
				if scenario == "missing-settings" {
					settings = nil
				}
				desktopWriteTest(t, conn, map[string]any{"type": "response", "requestId": request["requestId"], "resultType": "success", "method": request["method"], "handledByClientId": owner, "result": map[string]any{"settings": settings}})
				var one [1]byte
				if _, err := conn.Read(one[:]); !errors.Is(err, io.EOF) {
					t.Errorf("unexpected follower or other operation: %v", err)
				}
			})
			snapshot, err := client.ReadModel(context.Background(), desktopTestChat)
			if scenario != "success" {
				if err == nil {
					t.Fatal("invalid model result accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Model != "glm-5.3" || snapshot.ModelProvider != "openai" || snapshot.OwnerID != desktopTestOwner || snapshot.PluginBaselineKnown || snapshot.Cwd != "" || !snapshot.ReasoningEffortKnown || snapshot.ReasoningEffort == nil || *snapshot.ReasoningEffort != "medium" {
				t.Fatalf("model snapshot = %+v", snapshot)
			}
		})
	}
}

func TestDesktopIPCInstalledInspectReadOnly(t *testing.T) {
	chatID := os.Getenv("CQ_TEST_DESKTOP_CHAT_ID")
	if chatID == "" {
		t.Skip("set CQ_TEST_DESKTOP_CHAT_ID for an explicit read-only installed-app probe")
	}
	snapshot, err := (DesktopIPC{}).Inspect(context.Background(), chatID)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.PluginBaselineKnown || !snapshot.ReasoningEffortKnown || snapshot.Model == "" || snapshot.OwnerID == "" {
		t.Fatal("installed snapshot lacks required profile settings")
	}
	t.Logf("owner found; model=%s provider=%s baseline_known=%t plugin_count=%d effort_known=%t active=%t", snapshot.Model, snapshot.ModelProvider, snapshot.PluginBaselineKnown, len(snapshot.DisabledPluginIDs), snapshot.ReasoningEffortKnown, snapshot.Active)
	model, err := (DesktopIPC{}).ReadModel(context.Background(), chatID)
	if err != nil {
		t.Fatal(err)
	}
	if model.OwnerID != snapshot.OwnerID || model.Model == "" {
		t.Fatal("installed model read lacks current owner/model")
	}
}
