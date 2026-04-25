package modelregistry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// modelOption is the shape Claude Code's additionalModelOptionsCache expects.
type modelOption struct {
	Value               string `json:"value"`
	Label               string `json:"label"`
	Description         string `json:"description,omitempty"`
	DescriptionForModel string `json:"descriptionForModel,omitempty"`
	CQManaged           string `json:"cqManaged,omitempty"`
}

// claudeJSON is the minimal envelope we read/write from ~/.claude.json.
// We use a RawMessage map to preserve all unrelated fields.
type claudeJSON = map[string]json.RawMessage

// managedValuesKey is retained for migration from earlier cq versions that
// tracked ownership by value instead of per-entry fingerprints.
const managedValuesKey = "additionalModelOptionsCacheCQManagedValues"

// managedFingerprintsKey tracks the exact additionalModelOptionsCache entries cq wrote.
const managedFingerprintsKey = "additionalModelOptionsCacheCQManagedFingerprints"

const managedMarker = "cq:modelregistry:v1"

// optionsCacheKey is the Claude Code field we publish into.
const optionsCacheKey = "additionalModelOptionsCache"

const oneMillionContextWindow = 1_000_000

var claudeCodeTempCounter atomic.Uint64

// ClaudeCodeOptionsProjection returns the ModelOption entries cq injects for
// Claude Code's /model picker: Codex models plus Anthropic overlays. Native
// Anthropic models are already hard-coded by Claude Code itself.
func ClaudeCodeOptionsProjection(snap Snapshot) []modelOption {
	entries := claudeCodeProjectionEntries(snap)
	var opts []modelOption
	for _, e := range entries {
		opts = append(opts, newManagedModelOption(e, e.ID))
		if claudeCodeOptionContextWindow(e) >= oneMillionContextWindow && !strings.HasSuffix(e.ID, "[1m]") {
			opts = append(opts, newManagedModelOption(e, e.ID+"[1m]"))
		}
	}
	return opts
}

func claudeCodeProjectionEntries(snap Snapshot) []Entry {
	entries := make([]Entry, 0, len(snap.Entries))
	for _, e := range snap.Entries {
		if e.Provider != ProviderCodex && !(e.Provider == ProviderAnthropic && e.Source == SourceOverlay) {
			continue
		}
		if e.Provider == ProviderCodex && e.ID == "codex-auto-review" {
			continue
		}
		entries = append(entries, e)
	}
	priorities := projectionPriorities(entries)
	sort.SliceStable(entries, func(i, j int) bool {
		pi := priorities[entries[i].ID]
		pj := priorities[entries[j].ID]
		if pi != pj {
			return pi < pj
		}
		return cloneOrderLess(entries[i], entries[j])
	})
	return entries
}

func cloneOrderLess(a, b Entry) bool {
	rankA := projectionSortRank(a)
	rankB := projectionSortRank(b)
	if rankA.family != rankB.family {
		return rankA.family < rankB.family
	}
	if cmp := compareVersionTokens(rankA.version, rankB.version); cmp != 0 {
		return cmp > 0
	}
	if rankA.nonVersion != rankB.nonVersion {
		return !rankA.nonVersion
	}
	return a.ID < b.ID
}

type projectionRank struct {
	family     string
	version    []int
	nonVersion bool
}

func projectionSortRank(e Entry) projectionRank {
	family, sourceVersion, ok := cloneFamily(e)
	if !ok {
		return projectionRank{family: e.ID, version: versionTokens(e.ID), nonVersion: true}
	}
	version := versionTokens(e.ID)
	nonVersion := e.Source == SourceOverlay && !isVersionedSuccessor(e.ID, family)
	if e.Source != SourceOverlay || nonVersion {
		version = sourceVersion
	}
	return projectionRank{family: family, version: version, nonVersion: nonVersion}
}

func cloneFamily(e Entry) (string, []int, bool) {
	if e.Source != SourceOverlay {
		return e.ID, versionTokens(e.ID), true
	}
	src := e.CloneFrom
	if src == "" {
		src = e.InferredFrom
	}
	if src == "" {
		return "", nil, false
	}
	return src, versionTokens(src), true
}

func compareVersionTokens(a, b []int) int {
	limit := max(len(a), len(b))
	for i := 0; i < limit; i++ {
		ai := versionTokenAt(a, i)
		bi := versionTokenAt(b, i)
		if ai != bi {
			return ai - bi
		}
	}
	return 0
}

func versionTokenAt(tokens []int, i int) int {
	if i < len(tokens) {
		return tokens[i]
	}
	return 0
}

func isVersionedSuccessor(overlayID, srcID string) bool {
	if overlayID == srcID {
		return false
	}
	overlayVer := versionTokens(overlayID)
	srcVer := versionTokens(srcID)
	if len(overlayVer) == 0 || len(srcVer) == 0 || compareVersionTokens(overlayVer, srcVer) == 0 {
		return false
	}
	return nonNumericIDTokens(overlayID) == nonNumericIDTokens(srcID)
}

func versionTokens(id string) []int {
	var tokens []int
	for _, tok := range tokenise(id) {
		if !isNumericToken(tok) {
			continue
		}
		n := 0
		for _, r := range tok {
			n = n*10 + int(r-'0')
		}
		tokens = append(tokens, n)
	}
	return tokens
}

func nonNumericIDTokens(id string) string {
	var tokens []string
	for _, tok := range tokenise(id) {
		if !isNumericToken(tok) {
			tokens = append(tokens, tok)
		}
	}
	return strings.Join(tokens, "-")
}

func projectionPriorities(entries []Entry) map[string]int {
	priorities := make(map[string]int, len(entries))
	for _, e := range entries {
		if e.Priority != 0 {
			priorities[e.ID] = e.Priority
		} else {
			priorities[e.ID] = 1_000_000
		}
	}
	for _, e := range entries {
		if e.Source != SourceOverlay || e.Priority != 0 {
			continue
		}
		if e.InferredFrom != "" && priorities[e.InferredFrom] != 0 {
			priorities[e.ID] = priorities[e.InferredFrom]
			continue
		}
		if e.CloneFrom != "" && priorities[e.CloneFrom] != 0 {
			priorities[e.ID] = priorities[e.CloneFrom]
		}
	}
	return priorities
}

func claudeCodeOptionContextWindow(e Entry) int {
	if e.MaxContextWindow > e.ContextWindow {
		return e.MaxContextWindow
	}
	return e.ContextWindow
}

func newManagedModelOption(e Entry, value string) modelOption {
	return modelOption{
		Value:               value,
		Label:               displayLabel(e, value),
		Description:         e.Description,
		DescriptionForModel: e.Description,
		CQManaged:           managedMarker,
	}
}

func displayLabel(e Entry, value string) string {
	if e.Provider == ProviderCodex {
		return value
	}
	if e.DisplayName != "" {
		if strings.HasSuffix(value, "[1m]") && !strings.HasSuffix(e.DisplayName, "[1m]") {
			return e.DisplayName + " [1m]"
		}
		return e.DisplayName
	}
	return value
}

// PublishClaudeCodeOptions merges cq-managed model options into ~/.claude.json
// without disturbing unrelated fields or user-added entries.
//
// Algorithm:
//  1. Read existing ~/.claude.json (treat missing file as empty object).
//  2. Read previous cq-managed fingerprints.
//  3. Remove only entries cq previously wrote, keeping user and malformed entries.
//  4. Append the current cq-managed entries.
//  5. Write the updated ownership fingerprints.
//  6. Atomically write back via unique tmp+rename (0o600 file, 0o700 dir).
func PublishClaudeCodeOptions(fsys fsutil.FileSystem, path string, snap Snapshot) error {
	// 1. Read existing config.
	raw, err := fsys.ReadFile(path)
	if err != nil {
		if !isNotExist(err) {
			return fmt.Errorf("publish claude code options: read %s: %w", path, err)
		}
		raw = []byte("{}")
	}

	var cfg claudeJSON
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("publish claude code options: parse %s: %w", path, err)
	}
	if cfg == nil {
		cfg = make(claudeJSON)
	}

	// 2. Read previous managed fingerprints.
	prevManaged := readStringSlice(cfg, managedFingerprintsKey)
	prevManagedSet := make(map[string]struct{}, len(prevManaged))
	for _, v := range prevManaged {
		prevManagedSet[v] = struct{}{}
	}
	prevManagedValueSet := make(map[string]struct{})
	if len(prevManaged) == 0 {
		for _, v := range readStringSlice(cfg, managedValuesKey) {
			prevManagedValueSet[v] = struct{}{}
		}
	}

	// 3. Read existing options; remove stale cq-managed entries.
	existingOpts := readRawSlice(cfg, optionsCacheKey)
	var kept []json.RawMessage
	for _, item := range existingOpts {
		if isManagedOption(item, prevManagedSet, prevManagedValueSet) {
			continue
		}
		kept = append(kept, item)
	}

	// 4. Build new cq-managed entries and collect their fingerprints.
	newOpts := ClaudeCodeOptionsProjection(snap)
	newManagedFingerprints := make([]string, 0, len(newOpts))
	newManagedValues := make([]string, 0, len(newOpts))
	for _, o := range newOpts {
		b, _ := json.Marshal(o)
		kept = append(kept, json.RawMessage(b))
		newManagedFingerprints = append(newManagedFingerprints, managedOptionFingerprint(b))
		newManagedValues = append(newManagedValues, o.Value)
	}
	sort.Strings(newManagedFingerprints)

	// 5. Update cfg.
	cfg[optionsCacheKey] = marshalRawSlice(kept)
	fingerprintsJSON, _ := json.Marshal(newManagedFingerprints)
	cfg[managedFingerprintsKey] = json.RawMessage(fingerprintsJSON)
	managedJSON, _ := json.Marshal(newManagedValues)
	cfg[managedValuesKey] = json.RawMessage(managedJSON)

	// 6. Write atomically.
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("publish claude code options: marshal: %w", err)
	}

	dir := filepath.Dir(path)
	if err := fsys.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("publish claude code options: mkdir %s: %w", dir, err)
	}

	tmp := uniqueTempPath(path)
	if err := fsys.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("publish claude code options: write tmp: %w", err)
	}
	if err := fsys.Rename(tmp, path); err != nil {
		_ = fsys.Remove(tmp)
		return fmt.Errorf("publish claude code options: rename: %w", err)
	}

	return nil
}

func ClaudeCodeOptionsNeedPublish(fsys fsutil.FileSystem, path string, snap Snapshot) (bool, error) {
	raw, err := fsys.ReadFile(path)
	if err != nil {
		if isNotExist(err) {
			return len(ClaudeCodeOptionsProjection(snap)) > 0, nil
		}
		return false, fmt.Errorf("check claude code options: read %s: %w", path, err)
	}
	var cfg claudeJSON
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return false, fmt.Errorf("check claude code options: parse %s: %w", path, err)
	}
	current := readRawSlice(cfg, optionsCacheKey)
	present := make(map[string]struct{})
	for _, item := range current {
		var opt struct {
			Value     string `json:"value"`
			CQManaged string `json:"cqManaged"`
		}
		if err := json.Unmarshal(item, &opt); err != nil || opt.Value == "" || opt.CQManaged == "" {
			continue
		}
		present[opt.Value] = struct{}{}
	}
	for _, opt := range ClaudeCodeOptionsProjection(snap) {
		if _, ok := present[opt.Value]; !ok {
			return true, nil
		}
	}
	return false, nil
}

func isManagedOption(item json.RawMessage, prevManagedSet, prevManagedValueSet map[string]struct{}) bool {
	var opt struct {
		Value     string `json:"value"`
		CQManaged string `json:"cqManaged"`
	}
	if err := json.Unmarshal(item, &opt); err != nil {
		return false
	}
	if opt.CQManaged != "" {
		return true
	}
	if _, ok := prevManagedSet[managedOptionFingerprint(item)]; ok {
		return true
	}
	if opt.Value != "" && len(prevManagedSet) == 0 {
		_, ok := prevManagedValueSet[opt.Value]
		return ok
	}
	return false
}

func managedOptionFingerprint(item []byte) string {
	var value any
	if err := json.Unmarshal(item, &value); err != nil {
		return string(item)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return string(item)
	}
	return string(canonical)
}

func uniqueTempPath(path string) string {
	return fmt.Sprintf("%s.tmp.%d.%d", path, time.Now().UnixNano(), claudeCodeTempCounter.Add(1))
}

// readStringSlice unmarshals a JSON array of strings from cfg[key].
// Returns nil when the key is absent or the value is not a string array.
func readStringSlice(cfg claudeJSON, key string) []string {
	raw, ok := cfg[key]
	if !ok {
		return nil
	}
	var s []string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	return s
}

// readRawSlice unmarshals a JSON array of raw messages from cfg[key].
func readRawSlice(cfg claudeJSON, key string) []json.RawMessage {
	raw, ok := cfg[key]
	if !ok {
		return nil
	}
	var s []json.RawMessage
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	return s
}

// marshalRawSlice encodes a []json.RawMessage as a JSON array.
func marshalRawSlice(items []json.RawMessage) json.RawMessage {
	if items == nil {
		return json.RawMessage("[]")
	}
	b, _ := json.Marshal(items)
	return json.RawMessage(b)
}

// isNotExist reports whether err is a file-not-found error.
func isNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
