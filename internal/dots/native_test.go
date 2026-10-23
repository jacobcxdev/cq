package dots

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testNativePipe(t *testing.T, handler func(map[string]any) map[string]any) (*nativePipe, func() []map[string]any) {
	t.Helper()
	peerRead, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	read, peerWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	p := newNativePipe(read, write)
	var requests []map[string]any
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer func() { _ = recover(); peerRead.Close(); peerWrite.Close(); close(done) }()
		reader := bufio.NewReader(peerRead)
		for {
			data, e := reader.ReadBytes(0)
			if e != nil {
				return
			}
			var request map[string]any
			if json.Unmarshal(data[:len(data)-1], &request) != nil {
				return
			}
			mu.Lock()
			requests = append(requests, request)
			mu.Unlock()
			response := handler(request)
			if response == nil {
				continue
			}
			data, _ = json.Marshal(response)
			_, _ = peerWrite.Write(append([]byte(`{"method":"event"}`), 0))
			_, _ = peerWrite.Write(data[:3])
			_, _ = peerWrite.Write(append(data[3:], 0))
		}
	}()
	t.Cleanup(func() { write.Close(); read.Close(); <-done; <-p.done })
	return p, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), requests...)
	}
}
func nativeFixtureReply(request map[string]any) map[string]any {
	var result map[string]any
	switch request["method"] {
	case "Target.getTargets":
		result = map[string]any{"targetInfos": []any{map[string]any{"type": "page", "url": "https://foreign.invalid", "targetId": "0"}, map[string]any{"type": "page", "url": "app://-/index.html", "targetId": "b"}, map[string]any{"type": "page", "url": "app://-/index.html", "targetId": "a"}}}
	case "Target.attachToTarget":
		result = map[string]any{"sessionId": request["params"].(map[string]any)["targetId"]}
	case "Runtime.evaluate":
		if request["sessionId"] == "a" {
			result = map[string]any{"exceptionDetails": map[string]any{"text": "private failure"}}
		} else {
			result = map[string]any{"result": map[string]any{"value": map[string]any{"installed": true}}}
		}
	default:
		result = map[string]any{}
	}
	return map[string]any{"id": request["id"], "result": result}
}
func TestNativePipeAllWindowsAndSessionDetach(t *testing.T) {
	p, requests := testNativePipe(t, nativeFixtureReply)
	values, err := p.evaluate(context.Background(), "guard()", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].(map[string]any)["installed"] != false || values[1].(map[string]any)["installed"] != true {
		t.Fatalf("unexpected values: %#v", values)
	}
	attached, detached := 0, 0
	for _, r := range requests() {
		if r["method"] == "Target.attachToTarget" {
			attached++
			if r["params"].(map[string]any)["flatten"] != true {
				t.Fatal("session not flattened")
			}
		}
		if r["method"] == "Target.detachFromTarget" {
			detached++
		}
	}
	if attached != 2 || detached != 2 {
		t.Fatalf("attach %d detach %d", attached, detached)
	}
}
func TestNativePipeFailureGenericAndTimeoutPreservesDescriptors(t *testing.T) {
	p, _ := testNativePipe(t, func(r map[string]any) map[string]any {
		return map[string]any{"id": r["id"], "error": map[string]any{"message": "private token"}}
	})
	if _, err := p.call(context.Background(), "bad", nil, ""); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("expected generic failure")
	}
	p, _ = testNativePipe(t, func(map[string]any) map[string]any { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := p.call(ctx, "wait", nil, ""); err == nil {
		t.Fatal("expected timeout")
	}
	if _, err := p.write.Stat(); err != nil {
		t.Fatal("timeout closed pipe")
	}
}
func TestNativePipeRejectsForeignTargetsAndLargeCommands(t *testing.T) {
	p, _ := testNativePipe(t, func(r map[string]any) map[string]any {
		return map[string]any{"id": r["id"], "result": map[string]any{"targetInfos": []any{map[string]any{"type": "page", "url": "https://foreign.invalid", "targetId": "x"}}}}
	})
	if _, err := p.evaluate(context.Background(), "one()", true); err == nil {
		t.Fatal("foreign target accepted")
	}
	if _, err := p.call(context.Background(), "large", map[string]any{"large": strings.Repeat("x", nativeMaxRequest)}, ""); err == nil {
		t.Fatal("large command accepted")
	}
}
func TestNativePipeDropsOversizedUnsolicitedFrame(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	p := newNativePipe(r, pw)
	done := make(chan struct{})
	go func() {
		defer func() { _ = recover(); pr.Close(); w.Close(); close(done) }()
		data, _ := bufio.NewReader(pr).ReadBytes(0)
		var request map[string]any
		_ = json.Unmarshal(data[:len(data)-1], &request)
		_, _ = w.Write(append([]byte(strings.Repeat("x", nativeMaxFrame+100)), 0))
		reply, _ := json.Marshal(map[string]any{"id": request["id"], "result": map[string]any{"ready": true}})
		_, _ = w.Write(append(reply, 0))
	}()
	defer func() { pw.Close(); r.Close(); <-done; <-p.done }()
	value, err := p.call(context.Background(), "target", nil, "")
	if err != nil || value["ready"] != true {
		t.Fatalf("lost reply: %v", err)
	}
}
func nativeTestRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "cqd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return root
}
func TestNativeBrokerPrivateSocketIdentityAndRejectsWrongPID(t *testing.T) {
	root := nativeTestRoot(t)
	b, err := newNativeBroker(root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	b.appPID = 12345
	s := &Settings{RuntimeRoot: root}
	request := func(v map[string]any) (map[string]any, error) {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			defer func() {
				if recover() != nil {
					done <- errors.New("panic")
				}
			}()
			done <- b.once(context.Background())
		}()
		reply, e := nativeRequest(context.Background(), s, v)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		return reply, e
	}
	identity, err := request(map[string]any{"operation": "identity"})
	if err != nil || identity["app_pid"] != float64(12345) {
		t.Fatalf("identity: %v", err)
	}
	if _, err = request(map[string]any{"operation": "evaluate", "app_pid": 12346, "expression": "one()"}); err == nil {
		t.Fatal("wrong pid accepted")
	}
	if _, err = request(map[string]any{"operation": "identity", "extra": true}); err == nil {
		t.Fatal("unexpected fields accepted")
	}
	info, err := os.Stat(filepath.Join(root, nativeSocketName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("socket not private")
	}
	if err = os.Chmod(filepath.Join(root, nativeSocketName), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err = nativeRequest(context.Background(), s, map[string]any{"operation": "identity"}); err == nil {
		t.Fatal("public socket accepted")
	}
}
func TestNativeBrokerNeverReplacesExistingPath(t *testing.T) {
	root := nativeTestRoot(t)
	path := filepath.Join(root, nativeSocketName)
	if err := os.Symlink(filepath.Join(root, "elsewhere"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := newNativeBroker(root); err == nil {
		t.Fatal("existing symlink accepted")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("existing path removed")
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := nativeRoot(root); err == nil {
		t.Fatal("public root accepted")
	}
}
func TestNativeDebuggerURLRejectsRemoteOrAmbiguousAuthority(t *testing.T) {
	for _, v := range []string{"ws://evil.test:9999/devtools/page/x", "ws://127.0.0.1:9998/devtools/page/x", "ws://user@127.0.0.1:9999/devtools/page/x", "wss://127.0.0.1:9999/devtools/page/x", "ws://127.0.0.1:9999/devtools/browser/x"} {
		if _, err := debuggerURL(v, 9999); err == nil {
			t.Fatalf("accepted %s", v)
		}
	}
	if _, err := debuggerURL("ws://127.0.0.1:9999/devtools/page/x", 9999); err != nil {
		t.Fatal(err)
	}
}
func TestNativeGateCachesOnlyVerifiedPrincipal(t *testing.T) {
	now := time.Unix(0, 0)
	calls := 0
	loads := 0
	settings := &Settings{SystemAlias: "system", IntegrationMode: "native"}
	g := &NativeGate{settings: settings, now: func() time.Time { return now }, load: func() (Account, error) {
		loads++
		return Account{Alias: "system", AccountID: "account", Token: "stored-token"}, nil
	}, get: func(context.Context, Account, string, http.Header) (map[string]any, error) { calls++; return nil, nil }}
	if err := g.Require(context.Background(), http.Header{}); err == nil || loads != 0 {
		t.Fatal("unauthenticated request read credentials")
	}
	headers := http.Header{"Authorization": {"Bearer wrong-token"}, "Chatgpt-Account-Id": {"account"}}
	if err := g.Require(context.Background(), headers); err == nil {
		t.Fatal("wrong bearer accepted")
	}
	headers.Set("Authorization", "Bearer stored-token")
	if err := g.Require(context.Background(), headers); err != nil {
		t.Fatal(err)
	}
	before := loads
	if err := g.Require(context.Background(), headers); err != nil || loads != before {
		t.Fatal("cache miss")
	}
	now = now.Add(61 * time.Second)
	if err := g.Require(context.Background(), headers); err != nil || loads != before+1 {
		t.Fatal("cache did not expire")
	}
	if calls != 0 {
		t.Fatal("native mode made upstream call")
	}
}
func TestNativeProjectionPreservesIdentitiesAndAggregates(t *testing.T) {
	msg := map[string]any{"account_user_id": "viewer", "id": "message", "reply_to": map[string]any{"message": map[string]any{"account_user_id": "viewer"}}}
	raw := map[string]any{"account_user_id": "viewer", "raw_messages": []any{}}
	value := map[string]any{"items": []any{msg, raw}}
	if count := ProjectNativeResponse(value, map[string]bool{"viewer": true}, false); count != 2 {
		t.Fatalf("count %d", count)
	}
	if msg["id"] != "message" || msg["account_user_id"] != "viewer" || raw["is_self"] != nil {
		t.Fatal("identity or raw aggregate changed")
	}
}

func TestNativeGateIsolatedVerifiesIncomingTokenAndNeverCachesFailure(t *testing.T) {
	settings := &Settings{SystemAlias: "system", IntegrationMode: "isolated", Dots: map[string]Dot{"one": {OwnerAlias: "system", ProfileID: "profile"}}}
	calls := 0
	valid := false
	g := &NativeGate{settings: settings, now: time.Now, load: func() (Account, error) {
		return Account{Alias: "system", AccountID: "account", Token: "stored-token"}, nil
	}, get: func(_ context.Context, a Account, path string, _ http.Header) (map[string]any, error) {
		calls++
		if a.Token != "native-token" || path != "/backend-api/tbo/primary" {
			t.Fatal("verified stored credential instead of native bearer")
		}
		if !valid {
			return map[string]any{}, nil
		}
		return map[string]any{"profile": map[string]any{"id": "profile", "active_root_thread_id": "root"}, "selection": map[string]any{"available": true, "thread_id": "root"}}, nil
	}}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer native-token")
	headers.Set("ChatGPT-Account-Id", "account")
	for range 2 {
		if err := g.Require(context.Background(), headers); err == nil {
			t.Fatal("invalid remote identity accepted")
		}
	}
	if calls != 2 {
		t.Fatal("failed identity cached")
	}
	valid = true
	if err := g.Require(context.Background(), headers); err != nil {
		t.Fatal(err)
	}
	if err := g.Require(context.Background(), headers); err != nil || calls != 3 {
		t.Fatal("verified identity not cached")
	}
}
func TestValidateNativeRegistrationRejectsForeignOrExpandedIdentity(t *testing.T) {
	s := &Settings{SystemAlias: "primary", SecondaryAlias: "secondary"}
	system := Account{Alias: "primary", AccountID: "main", Token: "stored-token"}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer stored-token")
	headers.Set("ChatGPT-Account-Id", "main")
	proof := map[string]any{"owner_alias": "secondary", "registration_accepted": true, "root_thread_id": "root", "environment_id": "existing"}
	body := []byte(`{"displayName":"Mac","installation_id":"5403b4ce-fdb9-4818-afe5-c936b63b7e8d","selected_capability_roots":[]}`)
	if _, err := ValidateNativeRegistration(s, headers, body, system, proof, "root"); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateNativeRegistration(s, headers, body, system, proof, "stale"); err == nil {
		t.Fatal("stale root accepted")
	}
	headers.Set("Authorization", "Bearer wrong")
	if _, err := ValidateNativeRegistration(s, headers, body, system, proof, "root"); err == nil {
		t.Fatal("foreign credential accepted")
	}
	headers.Set("Authorization", "Bearer stored-token")
	for _, bad := range []string{`{"displayName":"Mac","installation_id":"invalid","selected_capability_roots":[]}`, `{"displayName":"Mac","installation_id":"5403b4ce-fdb9-4818-afe5-c936b63b7e8d","selected_capability_roots":[],"newGrant":true}`, `{"displayName":"Mac","installation_id":"5403b4ce-fdb9-4818-afe5-c936b63b7e8d","selected_capability_roots":null}`} {
		if _, err := ValidateNativeRegistration(s, headers, []byte(bad), system, proof, "root"); err == nil {
			t.Fatal("invalid registration body accepted")
		}
	}
}

func TestNativeIPCFramingRejectsOversizedAndMultipleFrames(t *testing.T) {
	for _, data := range []string{"{\"ok\":true}\n{}\n", strings.Repeat("x", 128) + "\n", "not-json\n"} {
		client, server := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer func() { _ = recover(); close(done) }()
			_, _ = client.Write([]byte(data))
			_ = client.Close()
		}()
		_ = server.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := nativeReceive(server, 64); err == nil {
			t.Fatal("invalid IPC frame accepted")
		}
		_ = server.Close()
		<-done
	}
}

func TestNativeBrokerIdentitySupportsExactLegacyBrokerDuringMigration(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Settings{RuntimeRoot: filepath.Join(root, "runtime")}
	version := filepath.Join(root, "Cellar", "python@3.14", "3.14.8", "Frameworks", "Python.framework", "Versions", "3.14")
	interpreter := filepath.Join(version, "bin", "python3.14")
	framework := filepath.Join(version, "Resources", "Python.app", "Contents", "MacOS", "Python")
	for _, path := range []string{interpreter, framework} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("synthetic Python fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configured := filepath.Join(s.RuntimeRoot, "venv", "bin", "python")
	if err := os.MkdirAll(filepath.Dir(configured), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(interpreter, configured); err != nil {
		t.Fatal(err)
	}
	for _, executable := range []string{configured, framework} {
		args := []string{executable, "-u", filepath.Join(s.RuntimeRoot, "native_debug.py"), "--cq-dots-runtime=" + s.RuntimeRoot}
		if !nativeBrokerMatches(s, os.Getuid(), args, s.RuntimeRoot) {
			t.Fatalf("configured legacy broker rejected: %s", executable)
		}
		if nativeBrokerMatches(s, os.Getuid()+1, args, s.RuntimeRoot) || nativeBrokerMatches(s, os.Getuid(), args, root) {
			t.Fatal("foreign uid/directory accepted")
		}
		args[2] = filepath.Join(root, "native_debug.py")
		if nativeBrokerMatches(s, os.Getuid(), args, s.RuntimeRoot) {
			t.Fatal("foreign legacy script accepted")
		}
	}
	for _, args := range [][]string{{"/other/Python", "-u", filepath.Join(s.RuntimeRoot, "native_debug.py")}, {framework, "-c", filepath.Join(s.RuntimeRoot, "native_debug.py")}, {framework, "-u"}} {
		if nativeBrokerMatches(s, os.Getuid(), args, s.RuntimeRoot) {
			t.Fatal("unqualified legacy process accepted")
		}
	}
	goArgs := []string{filepath.Join(s.RuntimeRoot, "cq-dots-runtime"), "__dots", "native-broker", filepath.Dir(s.RuntimeRoot)}
	if !nativeBrokerMatches(s, os.Getuid(), goArgs, s.RuntimeRoot) {
		t.Fatal("Go broker rejected")
	}
	goArgs[2] = "probe"
	if nativeBrokerMatches(s, os.Getuid(), goArgs, s.RuntimeRoot) {
		t.Fatal("other helper accepted")
	}
}
