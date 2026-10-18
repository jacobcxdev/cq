package dots

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type coordinationEvaluator func(context.Context, string) ([]any, error)

func (f coordinationEvaluator) EvaluateAll(ctx context.Context, expr string) ([]any, error) {
	return f(ctx, expr)
}
func (f coordinationEvaluator) Evaluate(ctx context.Context, expr string) (any, error) {
	values, e := f(ctx, expr)
	if e != nil {
		return nil, e
	}
	if len(values) == 0 {
		return nil, errors.New("no renderer")
	}
	return values[0], nil
}
func coordinationSettings(t *testing.T) *Settings {
	t.Helper()
	root := t.TempDir()
	if e := os.Chmod(root, 0o700); e != nil {
		t.Fatal(e)
	}
	return &Settings{RuntimeRoot: root, ModuleAsset: "app-initial-test.js", SystemAlias: "owner-a", Dots: map[string]Dot{"dot-one": {OwnerAlias: "owner-a", ProfileID: "profile-a", RootThreadID: "root-a", DisplayName: "Same"}, "dot-two": {OwnerAlias: "owner-b", ProfileID: "profile-b", RootThreadID: "root-b", DisplayName: "Same"}}}
}
func coordinationNames() []any {
	return []any{map[string]any{"id": "profile-a", "name": "Same"}, map[string]any{"id": "profile-b", "name": "Same"}}
}
func TestCoordinationPythonLedgerHashes(t *testing.T) {
	cases := []struct {
		v      []string
		spaced bool
		want   string
	}{{[]string{"0123456789abcdef0123456789abcdef", "owner-a", "profile-a"}, false, "e11aff2914dc7af97417fe8acfc31d683630d66ea91e08e3238e713deb683664"}, {[]string{"owner-a", "profile-a", "message-a"}, false, "4a6c2fedf8bfd6ecbb6126f2a46524062768227814dd39935d4a96f649c0e093"}, {[]string{"owner-a", "profile-a"}, true, "d0ccea240bc29f7d0b384265e7570f1153c421b6f27d7e5b4d6392b54480fa8e"}, {[]string{"Å", "😀"}, false, "e95926589001517a65788c500ab748a04a6a277ccae027b4d7482acb03a5adc0"}, {[]string{"Å", "😀"}, true, "12862a2360e656928c2894c1baf68d2f2f1e05a81411237737a98cae114202d4"}}
	for _, c := range cases {
		if got := coordinationHash(c.v, c.spaced); got != c.want {
			t.Fatalf("hash %v spaced=%v: %s", c.v, c.spaced, got)
		}
	}
}
func TestAwarenessPreviewSummaryAndAmbiguousNames(t *testing.T) {
	s := coordinationSettings(t)
	n := coordinationEvaluator(func(context.Context, string) ([]any, error) { return nil, errors.New("offline") })
	a := NewAwareness(s, nil, n)
	for _, h := range dotHandles(s) {
		text, e := a.SetupMessage(h)
		if e != nil || !strings.Contains(text, "[CQ:directory:") || strings.Contains(text, "Same") {
			t.Fatalf("preview %q %v", text, e)
		}
	}
	summary, e := a.Summary()
	if e != nil || summary[0]["outcome"] != "not_attempted" {
		t.Fatalf("summary %v %v", summary, e)
	}
	entries, _ := os.ReadDir(s.RuntimeRoot)
	if len(entries) != 0 {
		t.Fatal("preview created state")
	}
	if _, e = a.Resolve(context.Background(), "Same"); e == nil {
		t.Fatal("ambiguous display name accepted")
	}
	if got, e := a.Resolve(context.Background(), "dot-one"); e != nil || got != "dot-one" {
		t.Fatal(got, e)
	}
}
func TestAwarenessPendingManualAndUpgradeIdentity(t *testing.T) {
	ctx := context.Background()
	s := coordinationSettings(t)
	sends := 0
	var a *Awareness
	n := coordinationEvaluator(func(_ context.Context, expr string) ([]any, error) {
		if strings.Contains(expr, "safePost(") {
			sends++
			rows, e := a.Summary()
			if e != nil {
				t.Fatal(e)
			}
			pending := false
			for _, row := range rows {
				pending = pending || row["outcome"] == "pending"
			}
			if !pending && sends == 1 {
				t.Fatal("send preceded durable pending record")
			}
			if _, e := a.Prime(ctx, "dot-one", false, false); e == nil {
				t.Fatal("concurrent automatic setup acquired lock")
			}
			return []any{map[string]any{"accepted": true, "message_id": "synthetic"}}, nil
		}
		return []any{coordinationNames()}, nil
	})
	a = NewAwareness(s, nil, n)
	rows, e := a.Prime(ctx, "dot-one", false, true)
	if e != nil || rows[0]["outcome"] != "accepted" {
		t.Fatal(rows, e)
	}
	rows, e = a.Prime(ctx, "dot-one", false, false)
	if e != nil || rows[0]["skipped"] != true || sends != 1 {
		t.Fatal(rows, e, sends)
	}
	dot := s.Dots["dot-one"]
	delete(s.Dots, "dot-one")
	dot.DisplayName = "Renamed"
	dot.RootThreadID = "new-root"
	s.Dots["new-handle"] = dot
	rows, e = a.Prime(ctx, "new-handle", false, false)
	if e != nil || rows[0]["skipped"] != true || sends != 1 {
		t.Fatal("upgrade redelivered", rows, e, sends)
	}
}
func TestAwarenessUnknownNeverRedeliversAndNoPrivateErrorPersisted(t *testing.T) {
	s := coordinationSettings(t)
	calls := 0
	n := coordinationEvaluator(func(_ context.Context, expr string) ([]any, error) {
		if strings.Contains(expr, "safePost(") {
			calls++
			return nil, errors.New("private upstream body")
		}
		return []any{coordinationNames()}, nil
	})
	a := NewAwareness(s, nil, n)
	ctx := context.Background()
	first, e := a.Prime(ctx, "dot-one", false, false)
	if e != nil || first[0]["outcome"] != "unknown" {
		t.Fatal(first, e)
	}
	second, e := a.Prime(ctx, "dot-one", false, false)
	if e != nil || second[0]["skipped"] != true || calls != 1 {
		t.Fatal(second, e, calls)
	}
	filepath.WalkDir(s.RuntimeRoot, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(path)
			if strings.Contains(string(b), "private upstream body") {
				t.Fatal("persisted private error")
			}
		}
		return err
	})
}
func TestAwarenessExistingPythonPendingGuard(t *testing.T) {
	s := coordinationSettings(t)
	root := filepath.Join(s.RuntimeRoot, "awareness")
	if e := coordinationDirectory(root); e != nil {
		t.Fatal(e)
	}
	id := "0123456789abcdef0123456789abcdef"
	if e := coordinationSave(filepath.Join(root, "installation.json"), map[string]any{"id": id}); e != nil {
		t.Fatal(e)
	}
	hash := "e11aff2914dc7af97417fe8acfc31d683630d66ea91e08e3238e713deb683664"
	if e := coordinationSave(filepath.Join(root, hash+".json"), map[string]any{"target": "old-name", "outcome": "pending"}); e != nil {
		t.Fatal(e)
	}
	n := coordinationEvaluator(func(context.Context, string) ([]any, error) {
		t.Fatal("upgrade attempted native request")
		return nil, nil
	})
	rows, e := NewAwareness(s, nil, n).Prime(context.Background(), "dot-one", false, false)
	if e != nil || rows[0]["skipped"] != true || rows[0]["outcome"] != "pending" {
		t.Fatal(rows, e)
	}
}
func TestAwarenessStateSymlinksFailBeforeSend(t *testing.T) {
	for _, name := range []string{"awareness", "broadcasts"} {
		t.Run(name, func(t *testing.T) {
			s := coordinationSettings(t)
			outside := t.TempDir()
			if e := os.Symlink(outside, filepath.Join(s.RuntimeRoot, name)); e != nil {
				t.Fatal(e)
			}
			n := coordinationEvaluator(func(context.Context, string) ([]any, error) {
				t.Fatal("unsafe state reached native app")
				return nil, nil
			})
			a := NewAwareness(s, nil, n)
			var e error
			if name == "awareness" {
				_, e = a.Prime(context.Background(), "dot-one", false, false)
			} else {
				_, e = a.Broadcast(context.Background(), "Hello")
			}
			if e == nil {
				t.Fatal("symlink state accepted")
			}
			entries, _ := os.ReadDir(outside)
			if len(entries) != 0 {
				t.Fatal("outside state changed")
			}
		})
	}
}
func TestBroadcastBoundsAndPartialFailure(t *testing.T) {
	s := coordinationSettings(t)
	sends := 0
	n := coordinationEvaluator(func(_ context.Context, expr string) ([]any, error) {
		if strings.Contains(expr, "safePost(") {
			sends++
			if sends == 1 {
				return nil, errors.New("private")
			}
			return []any{map[string]any{"accepted": false, "status": float64(403)}}, nil
		}
		return []any{coordinationNames()}, nil
	})
	a := NewAwareness(s, nil, n)
	for _, text := range []string{"", " \n", strings.Repeat("x", 3001)} {
		if _, e := a.Broadcast(context.Background(), text); e == nil {
			t.Fatal("invalid broadcast")
		}
	}
	result, e := a.Broadcast(context.Background(), "Hello, team!")
	if e != nil {
		t.Fatal(e)
	}
	rows := result["deliveries"].([]map[string]any)
	if rows[0]["outcome"] != "unknown" || rows[1]["outcome"] != "rejected" || sends != 2 {
		t.Fatal(rows, sends)
	}
	if _, e := os.Stat(filepath.Join(s.RuntimeRoot, "awareness")); !os.IsNotExist(e) {
		t.Fatal("broadcast reprimes setup")
	}
	filepath.WalkDir(filepath.Join(s.RuntimeRoot, "broadcasts"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := os.ReadFile(path)
			if strings.Contains(string(data), "Hello, team!") {
				t.Fatal("persisted message contents")
			}
		}
		return err
	})
}

func TestAwarenessCorruptOrSymlinkAttemptNeverRedelivers(t *testing.T) {
	for _, corrupt := range []string{"null", "{}", "not-json"} {
		t.Run(corrupt, func(t *testing.T) {
			s := coordinationSettings(t)
			sends := 0
			n := coordinationEvaluator(func(_ context.Context, expr string) ([]any, error) {
				if strings.Contains(expr, "safePost(") {
					sends++
					return []any{map[string]any{"accepted": true}}, nil
				}
				return []any{coordinationNames()}, nil
			})
			a := NewAwareness(s, nil, n)
			if _, e := a.Prime(context.Background(), "dot-one", false, false); e != nil {
				t.Fatal(e)
			}
			root := filepath.Join(s.RuntimeRoot, "awareness")
			installation, e := a.installation(root)
			if e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(root, a.identity(installation, "dot-one")+".json")
			if e := os.WriteFile(path, []byte(corrupt), 0o600); e != nil {
				t.Fatal(e)
			}
			if _, e := a.Prime(context.Background(), "dot-one", false, false); e == nil {
				t.Fatal("corrupt attempt accepted")
			}
			if sends != 1 {
				t.Fatal("corrupt attempt redelivered")
			}
		})
	}
}
