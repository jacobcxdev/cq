package modelregistry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// --- PublishClaudeCodeOptions tests ---

func makeCodexSnapshot(entries ...Entry) Snapshot {
	rawByID := make(map[string]json.RawMessage)
	for _, e := range entries {
		raw, _ := json.Marshal(map[string]any{
			"slug":           e.ID,
			"display_name":   e.DisplayName,
			"description":    e.Description,
			"context_window": e.ContextWindow,
			"priority":       e.Priority,
			"visibility":     e.Visibility,
		})
		rawByID[e.ID] = raw
	}
	return Snapshot{
		Entries:      entries,
		CodexRawByID: rawByID,
		FetchedAt:    time.Now(),
	}
}

// TestPublishClaudeCodeOptions_PreservesUnrelatedFields checks that unrelated
// top-level fields in ~/.claude.json are left intact after publishing.
func TestPublishClaudeCodeOptions_PreservesUnrelatedFields(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude.json"

	existing := `{
  "configVersion": 1,
  "theme": "dark",
  "numStartups": 42
}`
	_ = fsys.WriteFile(path, []byte(existing), 0o600)

	snap := makeCodexSnapshot(Entry{
		Provider:    ProviderCodex,
		ID:          "gpt-5.4",
		DisplayName: "GPT-5.4",
		Description: "Flagship model",
		Source:      SourceNative,
	})

	if err := PublishClaudeCodeOptions(fsys, path, snap); err != nil {
		t.Fatalf("PublishClaudeCodeOptions() error = %v", err)
	}

	data, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := got["configVersion"]; !ok {
		t.Error("configVersion field was removed")
	}
	if _, ok := got["theme"]; !ok {
		t.Error("theme field was removed")
	}
	if _, ok := got["numStartups"]; !ok {
		t.Error("numStartups field was removed")
	}
}

// TestPublishClaudeCodeOptions_PreservesUserAddedEntries verifies that
// user-managed entries in additionalModelOptionsCache that are not in
// the cq-managed set are kept.
func TestPublishClaudeCodeOptions_PreservesUserAddedEntries(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude.json"

	existing := `{
  "additionalModelOptionsCache": [
    {"value": "my-custom-model", "label": "My Custom Model", "description": "Hand-crafted"}
  ]
}`
	_ = fsys.WriteFile(path, []byte(existing), 0o600)

	snap := makeCodexSnapshot(Entry{
		Provider:    ProviderCodex,
		ID:          "gpt-5.4",
		DisplayName: "GPT-5.4",
		Source:      SourceNative,
	})

	if err := PublishClaudeCodeOptions(fsys, path, snap); err != nil {
		t.Fatalf("PublishClaudeCodeOptions() error = %v", err)
	}

	data, _ := fsys.ReadFile(path)
	var got struct {
		Options []struct {
			Value string `json:"value"`
		} `json:"additionalModelOptionsCache"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var foundCustom, foundGPT bool
	for _, o := range got.Options {
		if o.Value == "my-custom-model" {
			foundCustom = true
		}
		if o.Value == "gpt-5.4" {
			foundGPT = true
		}
	}
	if !foundCustom {
		t.Error("user-added my-custom-model entry was removed")
	}
	if !foundGPT {
		t.Error("cq-managed gpt-5.4 entry not found")
	}
}

// TestPublishClaudeCodeOptions_IdempotentReplace verifies that running
// PublishClaudeCodeOptions twice does not duplicate cq-managed entries.
func TestPublishClaudeCodeOptions_IdempotentReplace(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude.json"

	snap := makeCodexSnapshot(
		Entry{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", Source: SourceNative},
		Entry{Provider: ProviderCodex, ID: "gpt-4.1", DisplayName: "GPT-4.1", Source: SourceNative},
	)

	// First publish.
	if err := PublishClaudeCodeOptions(fsys, path, snap); err != nil {
		t.Fatalf("first PublishClaudeCodeOptions() error = %v", err)
	}
	// Second publish (idempotency).
	if err := PublishClaudeCodeOptions(fsys, path, snap); err != nil {
		t.Fatalf("second PublishClaudeCodeOptions() error = %v", err)
	}

	data, _ := fsys.ReadFile(path)
	var got struct {
		Options []struct {
			Value string `json:"value"`
		} `json:"additionalModelOptionsCache"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Count occurrences of gpt-5.4.
	count := 0
	for _, o := range got.Options {
		if o.Value == "gpt-5.4" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("gpt-5.4 appears %d times after idempotent publish, want 1", count)
	}
}

// TestPublishClaudeCodeOptions_OutputShape checks the ModelOption shape:
// {value, label, description, descriptionForModel?}
func TestPublishClaudeCodeOptions_OutputShape(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude.json"

	snap := makeCodexSnapshot(Entry{
		Provider:    ProviderCodex,
		ID:          "gpt-5.4",
		DisplayName: "GPT-5.4",
		Description: "Flagship model",
		Source:      SourceNative,
	})

	if err := PublishClaudeCodeOptions(fsys, path, snap); err != nil {
		t.Fatalf("PublishClaudeCodeOptions() error = %v", err)
	}

	data, _ := fsys.ReadFile(path)
	var got struct {
		Options []struct {
			Value               string `json:"value"`
			Label               string `json:"label"`
			Description         string `json:"description"`
			DescriptionForModel string `json:"descriptionForModel"`
		} `json:"additionalModelOptionsCache"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var found bool
	for _, o := range got.Options {
		if o.Value == "gpt-5.4" {
			found = true
			if o.Label == "" {
				t.Error("label is empty for gpt-5.4")
			}
			if o.Description != "Flagship model" {
				t.Errorf("description = %q, want %q", o.Description, "Flagship model")
			}
			if o.DescriptionForModel != "Flagship model" {
				t.Errorf("descriptionForModel = %q, want %q", o.DescriptionForModel, "Flagship model")
			}
			if o.Value == "" {
				t.Error("value is empty")
			}
		}
	}
	if !found {
		t.Error("gpt-5.4 entry not found in additionalModelOptionsCache")
	}
}

// TestPublishClaudeCodeOptions_CQManagedTracking verifies that
// additionalModelOptionsCacheCQManagedValues is written and that removing
// a model from the snapshot removes it from the published cache.
func TestPublishClaudeCodeOptions_CQManagedTracking(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude.json"

	// First publish: two managed models.
	snap1 := makeCodexSnapshot(
		Entry{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", Source: SourceNative},
		Entry{Provider: ProviderCodex, ID: "gpt-4.1", DisplayName: "GPT-4.1", Source: SourceNative},
	)
	if err := PublishClaudeCodeOptions(fsys, path, snap1); err != nil {
		t.Fatalf("first publish error = %v", err)
	}

	// Second publish: only one managed model (gpt-4.1 removed from snapshot).
	snap2 := makeCodexSnapshot(
		Entry{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", Source: SourceNative},
	)
	if err := PublishClaudeCodeOptions(fsys, path, snap2); err != nil {
		t.Fatalf("second publish error = %v", err)
	}

	data, _ := fsys.ReadFile(path)
	var got struct {
		Options []struct {
			Value string `json:"value"`
		} `json:"additionalModelOptionsCache"`
		Managed []string `json:"additionalModelOptionsCacheCQManagedValues"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// gpt-4.1 must have been removed.
	for _, o := range got.Options {
		if o.Value == "gpt-4.1" {
			t.Error("gpt-4.1 should have been removed from options after second publish")
		}
	}

	// The tracking field must reflect current managed set.
	if len(got.Managed) == 0 {
		t.Error("additionalModelOptionsCacheCQManagedValues must not be empty")
	}
	var foundTracked bool
	for _, v := range got.Managed {
		if v == "gpt-5.4" {
			foundTracked = true
		}
	}
	if !foundTracked {
		t.Error("gpt-5.4 not found in additionalModelOptionsCacheCQManagedValues")
	}
}

func TestPublishClaudeCodeOptions_EmitsOneMillionContextDuplicate(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude.json"

	snap := makeCodexSnapshot(
		Entry{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", ContextWindow: 272000, MaxContextWindow: 1050000, Source: SourceNative},
		Entry{Provider: ProviderCodex, ID: "gpt-4.1", DisplayName: "GPT-4.1", ContextWindow: 128000, MaxContextWindow: 128000, Source: SourceNative},
	)

	if err := PublishClaudeCodeOptions(fsys, path, snap); err != nil {
		t.Fatalf("PublishClaudeCodeOptions() error = %v", err)
	}

	data, _ := fsys.ReadFile(path)
	var got struct {
		Options []struct {
			Value string `json:"value"`
		} `json:"additionalModelOptionsCache"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	counts := make(map[string]int)
	for _, o := range got.Options {
		counts[o.Value]++
	}
	if counts["gpt-5.4"] != 1 {
		t.Errorf("gpt-5.4 count = %d, want 1", counts["gpt-5.4"])
	}
	if counts["gpt-5.4[1m]"] != 1 {
		t.Errorf("gpt-5.4[1m] count = %d, want 1", counts["gpt-5.4[1m]"])
	}
	if counts["gpt-4.1[1m]"] != 0 {
		t.Errorf("gpt-4.1[1m] count = %d, want 0", counts["gpt-4.1[1m]"])
	}
}

func TestPublishClaudeCodeOptions_NormalisesCodexLabelsAndSkipsAutoReview(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude.json"

	snap := makeCodexSnapshot(
		Entry{Provider: ProviderCodex, ID: "gpt-5.4-mini", DisplayName: "GPT-5.4-Mini", Source: SourceNative},
		Entry{Provider: ProviderCodex, ID: "codex-auto-review", DisplayName: "Codex Auto Review", Source: SourceNative},
	)

	if err := PublishClaudeCodeOptions(fsys, path, snap); err != nil {
		t.Fatalf("PublishClaudeCodeOptions() error = %v", err)
	}

	data, _ := fsys.ReadFile(path)
	var got struct {
		Options []struct {
			Value string `json:"value"`
			Label string `json:"label"`
		} `json:"additionalModelOptionsCache"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	labels := make(map[string]string)
	for _, o := range got.Options {
		labels[o.Value] = o.Label
	}
	if labels["gpt-5.4-mini"] != "gpt-5.4-mini" {
		t.Errorf("gpt-5.4-mini label = %q, want gpt-5.4-mini", labels["gpt-5.4-mini"])
	}
	if _, ok := labels["codex-auto-review"]; ok {
		t.Error("codex-auto-review must not be published to Claude Code picker")
	}
}

func TestClaudeCodeOptionsProjection_ClonedOverlayEmitsOneMillionVariant(t *testing.T) {
	natives := []Entry{
		{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", ContextWindow: 272000, MaxContextWindow: 1050000, Source: SourceNative},
	}
	overlays := []Entry{
		{Provider: ProviderCodex, ID: "gpt-5.5", CloneFrom: "gpt-5.4", Source: SourceOverlay},
	}
	snap := Snapshot{Entries: Merge(natives, overlays).Active}

	opts := ClaudeCodeOptionsProjection(snap)
	counts := make(map[string]int)
	for _, opt := range opts {
		counts[opt.Value]++
	}
	if counts["gpt-5.5"] != 1 {
		t.Errorf("gpt-5.5 count = %d, want 1", counts["gpt-5.5"])
	}
	if counts["gpt-5.5[1m]"] != 1 {
		t.Errorf("gpt-5.5[1m] count = %d, want 1", counts["gpt-5.5[1m]"])
	}
}

func TestClaudeCodeOptionsProjection_CloneSortsWithSource(t *testing.T) {
	// gpt-5.5 is a versioned clone successor of gpt-5.4; it must sort before
	// gpt-5.4 (newer successor first), and gpt-5.4-mini is a different family
	// at lower priority so it comes last.
	snap := makeCodexSnapshot(
		Entry{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", Priority: 2, Source: SourceNative},
		Entry{Provider: ProviderCodex, ID: "gpt-5.4-mini", DisplayName: "GPT-5.4-Mini", Priority: 4, Source: SourceNative},
		Entry{Provider: ProviderCodex, ID: "gpt-5.5", CloneFrom: "gpt-5.4", DisplayName: "GPT-5.5", Priority: 0, Source: SourceOverlay, InferredFrom: "gpt-5.4"},
	)

	opts := ClaudeCodeOptionsProjection(snap)
	values := make([]string, 0, len(opts))
	for _, opt := range opts {
		values = append(values, opt.Value)
	}
	// gpt-5.5 (versioned successor) sorts before gpt-5.4 (source); both before gpt-5.4-mini.
	want := []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini"}
	for i, v := range want {
		if i >= len(values) || values[i] != v {
			t.Fatalf("values = %v, want prefix %v", values, want)
		}
	}
}

// TestClaudeCodeOptionsProjection_MultipleClonedSuccessorsNewestFirst verifies
// that multiple versioned clone successors of the same source are ordered
// newest-first: gpt-5.6, gpt-5.5, gpt-5.4.
func TestClaudeCodeOptionsProjection_MultipleClonedSuccessorsNewestFirst(t *testing.T) {
	snap := makeCodexSnapshot(
		Entry{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", Priority: 2, Source: SourceNative},
		Entry{Provider: ProviderCodex, ID: "gpt-5.5", CloneFrom: "gpt-5.4", DisplayName: "GPT-5.5", Priority: 0, Source: SourceOverlay, InferredFrom: "gpt-5.4"},
		Entry{Provider: ProviderCodex, ID: "gpt-5.6", CloneFrom: "gpt-5.4", DisplayName: "GPT-5.6", Priority: 0, Source: SourceOverlay, InferredFrom: "gpt-5.4"},
	)

	opts := ClaudeCodeOptionsProjection(snap)
	values := make([]string, 0, len(opts))
	for _, opt := range opts {
		values = append(values, opt.Value)
	}
	want := []string{"gpt-5.6", "gpt-5.5", "gpt-5.4"}
	for i, v := range want {
		if i >= len(values) || values[i] != v {
			t.Fatalf("values = %v, want prefix %v", values, want)
		}
	}
}

// TestClaudeCodeOptionsProjection_NonVersionedOverlayStable verifies that
// non-versioned overlay entries (no clear semantic version relationship with
// source) remain in stable/predictable order without being reordered relative
// to each other or their source.
func TestClaudeCodeOptionsProjection_NonVersionedOverlayStable(t *testing.T) {
	snap := makeCodexSnapshot(
		Entry{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", Priority: 2, Source: SourceNative},
		Entry{Provider: ProviderCodex, ID: "aaa-custom", DisplayName: "AAA Custom", Priority: 0, Source: SourceOverlay, InferredFrom: "gpt-5.4"},
		Entry{Provider: ProviderCodex, ID: "gpt-custom", DisplayName: "GPT-Custom", Priority: 0, Source: SourceOverlay, InferredFrom: "gpt-5.4"},
	)

	opts := ClaudeCodeOptionsProjection(snap)
	values := make([]string, 0, len(opts))
	for _, opt := range opts {
		values = append(values, opt.Value)
	}
	want := []string{"gpt-5.4", "aaa-custom", "gpt-custom"}
	for i, v := range want {
		if i >= len(values) || values[i] != v {
			t.Fatalf("values = %v, want prefix %v", values, want)
		}
	}
}

func TestClaudeCodeOptionsProjection_SamePriorityFamiliesHaveTransitiveOrder(t *testing.T) {
	snap := makeCodexSnapshot(
		Entry{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", Priority: 2, Source: SourceNative},
		Entry{Provider: ProviderCodex, ID: "gpt-5.4-mini", DisplayName: "GPT-5.4-Mini", Priority: 2, Source: SourceNative},
		Entry{Provider: ProviderCodex, ID: "gpt-5.5", CloneFrom: "gpt-5.4", DisplayName: "GPT-5.5", Priority: 0, Source: SourceOverlay, InferredFrom: "gpt-5.4"},
	)

	opts := ClaudeCodeOptionsProjection(snap)
	values := make([]string, 0, len(opts))
	for _, opt := range opts {
		values = append(values, opt.Value)
	}
	want := []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini"}
	for i, v := range want {
		if i >= len(values) || values[i] != v {
			t.Fatalf("values = %v, want prefix %v", values, want)
		}
	}
}

func TestClaudeCodeOptionsProjection_OlderCloneSortsBelowSource(t *testing.T) {
	snap := makeCodexSnapshot(
		Entry{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", Priority: 2, Source: SourceNative},
		Entry{Provider: ProviderCodex, ID: "gpt-5.3", CloneFrom: "gpt-5.4", DisplayName: "GPT-5.3", Priority: 0, Source: SourceOverlay, InferredFrom: "gpt-5.4"},
		Entry{Provider: ProviderCodex, ID: "gpt-5.5", CloneFrom: "gpt-5.4", DisplayName: "GPT-5.5", Priority: 0, Source: SourceOverlay, InferredFrom: "gpt-5.4"},
	)

	opts := ClaudeCodeOptionsProjection(snap)
	values := make([]string, 0, len(opts))
	for _, opt := range opts {
		values = append(values, opt.Value)
	}
	want := []string{"gpt-5.5", "gpt-5.4", "gpt-5.3"}
	for i, v := range want {
		if i >= len(values) || values[i] != v {
			t.Fatalf("values = %v, want prefix %v", values, want)
		}
	}
}

// TestClaudeCodeOptionsProjection_OneMillionVariantAdjacentAfterReorder verifies
// that the [1m] variant remains adjacent to its base model even after versioned
// clone reordering. ClaudeCodeOptionsProjection emits [1m] immediately after
// each base Entry, so the order of entries in claudeCodeProjectionEntries
// controls adjacency.
func TestClaudeCodeOptionsProjection_OneMillionVariantAdjacentAfterReorder(t *testing.T) {
	// gpt-5.5 is a versioned successor of gpt-5.4; both have 1M context windows.
	// Expected emission order: gpt-5.5, gpt-5.5[1m], gpt-5.4, gpt-5.4[1m].
	snap := makeCodexSnapshot(
		Entry{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", Priority: 2, Source: SourceNative, MaxContextWindow: 1_050_000},
		Entry{Provider: ProviderCodex, ID: "gpt-5.5", CloneFrom: "gpt-5.4", DisplayName: "GPT-5.5", Priority: 0, Source: SourceOverlay, InferredFrom: "gpt-5.4", MaxContextWindow: 1_050_000},
	)

	opts := ClaudeCodeOptionsProjection(snap)
	values := make([]string, 0, len(opts))
	for _, opt := range opts {
		values = append(values, opt.Value)
	}
	// gpt-5.5 sorts first (versioned successor), [1m] variant immediately follows.
	want := []string{"gpt-5.5", "gpt-5.5[1m]", "gpt-5.4", "gpt-5.4[1m]"}
	if len(values) != len(want) {
		t.Fatalf("values = %v, want %v", values, want)
	}
	for i, v := range want {
		if values[i] != v {
			t.Fatalf("values[%d] = %q, want %q (full: %v)", i, values[i], v, values)
		}
	}
}

func TestPublishClaudeCodeOptions_IncludesAnthropicOverlays(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude.json"

	snap := Snapshot{
		Entries: []Entry{
			{Provider: ProviderAnthropic, ID: "claude-opus-4", DisplayName: "Claude Opus 4", Source: SourceNative},
			{Provider: ProviderAnthropic, ID: "claude-custom", DisplayName: "Claude Custom", Source: SourceOverlay},
		},
		FetchedAt: time.Now(),
	}

	if err := PublishClaudeCodeOptions(fsys, path, snap); err != nil {
		t.Fatalf("PublishClaudeCodeOptions() error = %v", err)
	}

	data, _ := fsys.ReadFile(path)
	var got struct {
		Options []struct {
			Value string `json:"value"`
		} `json:"additionalModelOptionsCache"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	values := make(map[string]bool)
	for _, o := range got.Options {
		values[o.Value] = true
	}
	if values["claude-opus-4"] {
		t.Error("native Anthropic model must not be injected")
	}
	if !values["claude-custom"] {
		t.Error("Anthropic overlay model must be injected")
	}
}

func TestPublishClaudeCodeOptions_PreservesUserEntryWithSameValue(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude.json"

	existing := `{
  "additionalModelOptionsCache": [
    {"value": "gpt-5.4", "label": "User GPT-5.4", "description": "Hand-crafted same value"}
  ]
}`
	_ = fsys.WriteFile(path, []byte(existing), 0o600)

	snap := makeCodexSnapshot(Entry{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", Source: SourceNative})

	if err := PublishClaudeCodeOptions(fsys, path, snap); err != nil {
		t.Fatalf("first PublishClaudeCodeOptions() error = %v", err)
	}
	if err := PublishClaudeCodeOptions(fsys, path, snap); err != nil {
		t.Fatalf("second PublishClaudeCodeOptions() error = %v", err)
	}

	data, _ := fsys.ReadFile(path)
	var got struct {
		Options []struct {
			Value string `json:"value"`
			Label string `json:"label"`
		} `json:"additionalModelOptionsCache"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var foundUser, foundManaged bool
	for _, o := range got.Options {
		if o.Value == "gpt-5.4" && o.Label == "User GPT-5.4" {
			foundUser = true
		}
		if o.Value == "gpt-5.4" && o.Label == "gpt-5.4" {
			foundManaged = true
		}
	}
	if !foundUser {
		t.Error("same-value user entry was removed")
	}
	if !foundManaged {
		t.Error("cq-managed entry was not added")
	}
}

func TestClaudeCodeOptionsNeedPublishDetectsBootstrapClear(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude.json"

	existing := `{
  "additionalModelOptionsCache": [],
  "additionalModelOptionsCacheCQManagedValues": ["gpt-5.4"],
  "additionalModelOptionsCacheCQManagedFingerprints": ["stale"]
}`
	_ = fsys.WriteFile(path, []byte(existing), 0o600)
	snap := makeCodexSnapshot(Entry{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", Source: SourceNative})

	need, err := ClaudeCodeOptionsNeedPublish(fsys, path, snap)
	if err != nil {
		t.Fatalf("ClaudeCodeOptionsNeedPublish() error = %v", err)
	}
	if !need {
		t.Fatal("ClaudeCodeOptionsNeedPublish() = false, want true after bootstrap clears cq options")
	}

	if err := PublishClaudeCodeOptions(fsys, path, snap); err != nil {
		t.Fatalf("PublishClaudeCodeOptions() error = %v", err)
	}
	need, err = ClaudeCodeOptionsNeedPublish(fsys, path, snap)
	if err != nil {
		t.Fatalf("ClaudeCodeOptionsNeedPublish() after publish error = %v", err)
	}
	if need {
		t.Fatal("ClaudeCodeOptionsNeedPublish() = true, want false after publish restores cq options")
	}
}

func TestPublishClaudeCodeOptions_FreshInstall(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude.json"

	snap := makeCodexSnapshot(Entry{
		Provider:    ProviderCodex,
		ID:          "gpt-5.4",
		DisplayName: "GPT-5.4",
		Source:      SourceNative,
	})

	if err := PublishClaudeCodeOptions(fsys, path, snap); err != nil {
		t.Fatalf("PublishClaudeCodeOptions() error = %v", err)
	}

	data, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if len(data) == 0 {
		t.Fatal("file is empty after fresh install")
	}
}

// TestPublishClaudeCodeOptions_NoAnthropicHardcodedDuplicates verifies that
// normal Anthropic models (not overlay) are not injected into Claude Code's
// picker (those are already hard-coded by Claude Code itself).
func TestPublishClaudeCodeOptions_NoAnthropicHardcodedDuplicates(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude.json"

	snap := Snapshot{
		Entries: []Entry{
			{Provider: ProviderAnthropic, ID: "claude-opus-4", DisplayName: "Claude Opus 4", Source: SourceNative},
			{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", Source: SourceNative},
		},
		FetchedAt: time.Now(),
	}

	if err := PublishClaudeCodeOptions(fsys, path, snap); err != nil {
		t.Fatalf("PublishClaudeCodeOptions() error = %v", err)
	}

	data, _ := fsys.ReadFile(path)
	var got struct {
		Options []struct {
			Value string `json:"value"`
		} `json:"additionalModelOptionsCache"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, o := range got.Options {
		if o.Value == "claude-opus-4" {
			t.Error("native Anthropic model claude-opus-4 must not be injected into Claude Code picker")
		}
	}
}
