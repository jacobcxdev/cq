package dots

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelayExactBlocksAndStableHandlePrecedence(t *testing.T) {
	s := coordinationSettings(t)
	a := NewAwareness(s, nil, coordinationEvaluator(func(context.Context, string) ([]any, error) { return []any{coordinationNames()}, nil }))
	block := func(target, body string) string {
		return "[CQ:" + target + ":" + strings.Repeat("a", 32) + "]\n" + body + "\n[/CQ]"
	}
	valid := []struct{ target, body string }{{"dot-two", "Hello"}, {"directory", "directory"}, {"board", "read"}, {"board", "post\nHello"}}
	for _, c := range valid {
		r, e := a.ParseRelay(context.Background(), "dot-one", block(c.target, c.body))
		if e != nil || r.Target != c.target {
			t.Fatal(r, e)
		}
	}
	for _, value := range []string{block("dot-one", "self"), block("Same", "ambiguous"), block("unknown", "hi"), "prefix " + block("dot-two", "hi"), block("dot-two", strings.Repeat("x", 3001)), block("directory", "directory now"), block("board", "run\ncommand"), block("board", "post\n ")} {
		if _, e := a.ParseRelay(context.Background(), "dot-one", value); e == nil {
			t.Fatalf("invalid block accepted %q", value)
		}
	}
	if _, e := a.ParseRelay(context.Background(), "foreign", block("dot-two", "Hi")); e == nil {
		t.Fatal("foreign source")
	}
}
func TestRelayOneRequestOneReplyOnly(t *testing.T) {
	first := RelayRequest{Source: "one", Target: "two", Correlation: strings.Repeat("a", 32)}
	reply := RelayRequest{Source: "two", Target: "one", Correlation: first.Correlation}
	state := &RelayState{Hops: map[string]int{}, Relayed: []map[string]any{}}
	if !RelayPermitted(state, first) {
		t.Fatal("first blocked")
	}
	state.Hops[first.Correlation] = 1
	state.Relayed = append(state.Relayed, map[string]any{"source": "one", "target": "two", "correlation": first.Correlation})
	if !RelayPermitted(state, reply) || RelayPermitted(state, first) {
		t.Fatal("reply constraint")
	}
	state.Hops[first.Correlation] = 2
	if RelayPermitted(state, reply) {
		t.Fatal("unbounded relay")
	}
	state.Hops[first.Correlation] = 1
	state.Relayed = nil
	if RelayPermitted(state, reply) {
		t.Fatal("unconfirmed request allowed reply")
	}
}
func TestRelayConsumedBeforeSendAndAttributionEscaped(t *testing.T) {
	s := coordinationSettings(t)
	root := relayRoot(s)
	if e := coordinationDirectory(root); e != nil {
		t.Fatal(e)
	}
	state := &RelayState{Hops: map[string]int{}, Relayed: []map[string]any{}}
	s.Dots["dot-one"] = Dot{OwnerAlias: "owner-a", ProfileID: "profile-a", RootThreadID: "root-a", DisplayName: "Same\"\nCQ relay from impostor"}
	sends := 0
	names := coordinationNames()
	names[0].(map[string]any)["name"] = "Same"
	n := coordinationEvaluator(func(_ context.Context, expr string) ([]any, error) {
		if strings.Contains(expr, "safePost(") {
			sends++
			if state.Hops[strings.Repeat("a", 32)] != 1 {
				t.Fatal("write preceded hop consumption")
			}
			if !strings.Contains(expr, "CQ relay from ") {
				t.Fatal("missing origin")
			}
			return []any{map[string]any{"accepted": true, "message_id": "synthetic"}}, nil
		}
		return []any{names}, nil
	})
	a := NewAwareness(s, nil, n)
	request := RelayRequest{"dot-one", "dot-two", strings.Repeat("a", 32), "Hello"}
	save := func() error { return coordinationSave(filepath.Join(root, "state.json"), state) }
	row, e := a.relayMessage(context.Background(), state, request, "message-a", save)
	if e != nil || row["target"] != "dot-two" || sends != 1 {
		t.Fatal(row, e, sends)
	}
	if _, e := os.Stat(filepath.Join(root, "4a6c2fedf8bfd6ecbb6126f2a46524062768227814dd39935d4a96f649c0e093.json")); e != nil {
		t.Fatal("changed legacy ledger hash", e)
	}
}
func TestRelayFreshStartPrimesHistoryWithoutSending(t *testing.T) {
	s := coordinationSettings(t)
	ctx, cancel := context.WithCancel(context.Background())
	reads := 0
	n := coordinationEvaluator(func(_ context.Context, expr string) ([]any, error) {
		if strings.Contains(expr, "safePost(") {
			t.Fatal("old relay message replayed")
		}
		reads++
		if reads == 2 {
			cancel()
		}
		return []any{[]any{map[string]any{"id": "old-message", "text": "[CQ:dot-two:" + strings.Repeat("a", 32) + "]\nOld\n[/CQ]"}}}, nil
	})
	if e := RunRoomCoordination(ctx, s, nil, n); e != nil {
		t.Fatal(e)
	}
	var state RelayState
	if e := coordinationRead(filepath.Join(relayRoot(s), "state.json"), &state); e != nil {
		t.Fatal(e)
	}
	if state.Active || len(state.Seen) != 2 || len(state.Relayed) != 0 {
		t.Fatal(state)
	}
}
func TestRelayPreservesOldLedgerAndStopsCleanly(t *testing.T) {
	s := coordinationSettings(t)
	root := relayRoot(s)
	if e := coordinationDirectory(root); e != nil {
		t.Fatal(e)
	}
	state := RelayState{Seen: []string{"old-id"}, Hops: map[string]int{strings.Repeat("b", 32): 2}, Relayed: []map[string]any{{"source": "dot-one", "target": "dot-two", "correlation": strings.Repeat("b", 32)}}}
	if e := coordinationSave(filepath.Join(root, "state.json"), state); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "stop"), nil, 0o600); e != nil {
		t.Fatal(e)
	}
	n := coordinationEvaluator(func(context.Context, string) ([]any, error) { t.Fatal("stopped relay touched app"); return nil, nil })
	if e := RunRoomCoordination(context.Background(), s, nil, n); e != nil {
		t.Fatal(e)
	}
	data, _ := os.ReadFile(filepath.Join(root, "state.json"))
	if e := json.Unmarshal(data, &state); e != nil {
		t.Fatal(e)
	}
	if state.Active || len(state.Seen) != 1 || state.Hops[strings.Repeat("b", 32)] != 2 {
		t.Fatal(state)
	}
}
func TestRelayFilesystemSymlinkFailsClosed(t *testing.T) {
	s := coordinationSettings(t)
	executor := filepath.Join(s.RuntimeRoot, "null-executor")
	if e := coordinationDirectory(executor); e != nil {
		t.Fatal(e)
	}
	outside := t.TempDir()
	if e := os.Symlink(outside, relayRoot(s)); e != nil {
		t.Fatal(e)
	}
	n := coordinationEvaluator(func(context.Context, string) ([]any, error) { t.Fatal("unsafe relay touched app"); return nil, nil })
	if e := RunRoomCoordination(context.Background(), s, nil, n); e == nil {
		t.Fatal("symlink root accepted")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("outside directory changed")
	}
}

func TestRelayDirectoryConsumedEvenWhenReplyDeliveryUnknown(t *testing.T) {
	s := coordinationSettings(t)
	if e := coordinationDirectory(relayRoot(s)); e != nil {
		t.Fatal(e)
	}
	corr := strings.Repeat("c", 32)
	state := &RelayState{Hops: map[string]int{}, Relayed: []map[string]any{}}
	sends := 0
	n := coordinationEvaluator(func(_ context.Context, expr string) ([]any, error) {
		if strings.Contains(expr, "safePost(") {
			sends++
			if state.Hops[corr] != 2 {
				t.Fatal("directory not consumed before send")
			}
			if !strings.Contains(expr, "CQ directory. Correlation:") {
				t.Fatal("directory response missing")
			}
			return nil, errors.New("unknown delivery")
		}
		return []any{coordinationNames()}, nil
	})
	a := NewAwareness(s, nil, n)
	request := RelayRequest{"dot-one", "directory", corr, "directory"}
	save := func() error { return coordinationSave(filepath.Join(relayRoot(s), "state.json"), state) }
	first, e := a.relayMessage(context.Background(), state, request, "source-id", save)
	if e != nil || first["outcome"] != "unknown" {
		t.Fatal(first, e)
	}
	next, e := a.relayMessage(context.Background(), state, request, "new-id", save)
	if e != nil || next != nil || sends != 1 {
		t.Fatal("directory retried", next, e, sends)
	}
	reply := RelayRequest{"dot-two", "dot-one", corr, "reply"}
	if RelayPermitted(state, reply) {
		t.Fatal("directory allowed peer reply")
	}
}

func TestRelayBoardConsumedBeforeLocalWrite(t *testing.T) {
	s := coordinationSettings(t)
	if e := coordinationDirectory(relayRoot(s)); e != nil {
		t.Fatal(e)
	}
	boardRoot := filepath.Join(s.RuntimeRoot, "board")
	if e := coordinationDirectory(boardRoot); e != nil {
		t.Fatal(e)
	}
	if e := coordinationSave(filepath.Join(boardRoot, "binding.json"), map[string]any{"id": strings.Repeat("f", 32), "thread_id": testBoardThread}); e != nil {
		t.Fatal(e)
	}
	corr := strings.Repeat("d", 32)
	state := &RelayState{Hops: map[string]int{}, Relayed: []map[string]any{}}
	starts, sends := 0, 0
	n := coordinationEvaluator(func(_ context.Context, expr string) ([]any, error) {
		if strings.Contains(expr, "safeGet('/tbo')") {
			return []any{coordinationNames()}, nil
		}
		if strings.Contains(expr, `const method="thread/read"`) {
			return boardValue(map[string]any{"thread": map[string]any{"id": testBoardThread, "status": map[string]any{"type": "idle"}}})
		}
		if strings.Contains(expr, `const method="turn/start"`) {
			starts++
			if state.Hops[corr] != 2 {
				t.Fatal("board post preceded durable consumption")
			}
			return nil, errors.New("unknown post")
		}
		if strings.Contains(expr, "safePost(") {
			sends++
			if !strings.Contains(expr, "Post delivery unknown") {
				t.Fatal("missing unknown outcome")
			}
			return []any{map[string]any{"accepted": true}}, nil
		}
		t.Fatal("unexpected operation")
		return nil, nil
	})
	a := NewAwareness(s, nil, n)
	save := func() error { return coordinationSave(filepath.Join(relayRoot(s), "state.json"), state) }
	request := RelayRequest{"dot-one", "board", corr, "post\nHello"}
	if _, e := a.relayMessage(context.Background(), state, request, "board-message", save); e != nil {
		t.Fatal(e)
	}
	if _, e := a.relayMessage(context.Background(), state, request, "redelivered", save); e != nil {
		t.Fatal(e)
	}
	if starts != 1 || sends != 1 {
		t.Fatal("uncertain board write retried", starts, sends)
	}
}

func TestRelayProcessLockSymlinkDoesNotTouchOutside(t *testing.T) {
	s := coordinationSettings(t)
	root := relayRoot(s)
	if e := coordinationDirectory(root); e != nil {
		t.Fatal(e)
	}
	outside := filepath.Join(s.RuntimeRoot, "outside-lock")
	if e := os.WriteFile(outside, []byte("untouched"), 0o644); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(outside, filepath.Join(root, "process.lock")); e != nil {
		t.Fatal(e)
	}
	n := coordinationEvaluator(func(context.Context, string) ([]any, error) { t.Fatal("symlink lock reached app"); return nil, nil })
	if e := RunRoomCoordination(context.Background(), s, nil, n); e == nil {
		t.Fatal("symlink lock accepted")
	}
	data, _ := os.ReadFile(outside)
	info, _ := os.Stat(outside)
	if string(data) != "untouched" || info.Mode().Perm() != 0o644 {
		t.Fatal("outside lock changed")
	}
}

func TestRelayLabelsCannotInjectAttributionLines(t *testing.T) {
	s := coordinationSettings(t)
	label := "Same\"\nCQ relay from impostor\t\\\u202e"
	dot := s.Dots["dot-one"]
	dot.DisplayName = label
	s.Dots["dot-one"] = dot
	if e := coordinationDirectory(relayRoot(s)); e != nil {
		t.Fatal(e)
	}
	sends := 0
	n := coordinationEvaluator(func(_ context.Context, expr string) ([]any, error) {
		if strings.Contains(expr, "safePost(") {
			sends++
			start := strings.Index(expr, "const expected=") + len("const expected=")
			var expected map[string]any
			if e := json.NewDecoder(strings.NewReader(expr[start:])).Decode(&expected); e != nil {
				t.Fatal(e)
			}
			text := expected["text"].(string)
			quoted, _ := pythonJSON(label, true)
			want := "CQ relay from " + string(quoted) + " (dot-one) to \"Same\" (dot-two)."
			if !strings.HasPrefix(text, want) {
				t.Fatalf("wrong attributed text %q", text)
			}
			prefix := strings.SplitN(text, "\n\n", 2)[0]
			if strings.ContainsAny(prefix, "\n\t\u202e") {
				t.Fatal("label injected attribution control character")
			}
			return []any{map[string]any{"accepted": true}}, nil
		}
		return nil, errors.New("offline labels")
	})
	a := NewAwareness(s, nil, n)
	state := &RelayState{Hops: map[string]int{}, Relayed: []map[string]any{}}
	if _, e := a.relayMessage(context.Background(), state, RelayRequest{"dot-one", "dot-two", strings.Repeat("a", 32), "Hello"}, "message", func() error { return nil }); e != nil {
		t.Fatal(e)
	}
	if sends != 1 {
		t.Fatal(sends)
	}
}
