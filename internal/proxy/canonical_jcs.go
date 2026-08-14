package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const (
	frozenBlueprintSHA256   = "bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38"
	frozenReviewSHA256      = "f65a7a0693ddc6c426e706f7d29ed96619cf0cba95cf257c20029b11162719e6"
	frozenReviewBaseline    = "9fe30df8d4101f69084d6487740ed324a5d0b59d"
	blueprintReviewPath     = "docs/superpowers/specs/2026-08-13-proxy-resilience-and-routing-blueprint.md"
	maxFrozenBlueprintBytes = 4 << 20
	maxReviewSiblingJCS     = 4_288
	maxReviewSiblingFile    = 4_289
	maxReviewRecordJCS      = 558
	maxReviewResultJCS      = 2_097_152
	maxJCSSafeInteger       = 9_007_199_254_740_991
)

var (
	lowerHex64Pattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	lowerHex40Pattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	findingIDPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	reviewerTaskIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,256}$`)
	reviewedAtPattern     = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)
)

var blueprintReviewLenses = [...]string{
	"architecture",
	"cli_operations",
	"routing_continuity",
	"security_privacy",
	"protocol_fidelity",
	"verification_release",
	"coverage_source_consistency",
}

// ParseBlueprintReviewResultV1 parses one complete bounded reviewer result.
func ParseBlueprintReviewResultV1(reader io.Reader) (CQProxyBlueprintReviewResultV1, error) {
	var result CQProxyBlueprintReviewResultV1
	data, err := readBoundedBytes(reader, maxReviewResultJCS)
	if err != nil {
		return result, fmt.Errorf("read blueprint review result: %w", err)
	}
	decoded, err := decodeStrictJSON(data)
	if err != nil {
		return result, fmt.Errorf("decode blueprint review result: %w", err)
	}
	canonical, err := appendCanonicalJSON(make([]byte, 0, len(data)), decoded)
	if err != nil {
		return result, err
	}
	if !bytes.Equal(canonical, data) {
		return result, fmt.Errorf("blueprint review result is not canonical JCS")
	}
	if err := decodeClosedJSON(data, &result); err != nil {
		return result, fmt.Errorf("decode blueprint review result schema: %w", err)
	}
	if err := validateBlueprintReviewResult(&result); err != nil {
		return result, err
	}
	return result, nil
}

func validateBlueprintReviewResult(result *CQProxyBlueprintReviewResultV1) error {
	if result.SchemaVersion != 1 || result.Kind != "cq_proxy_blueprint_review_result_v1" {
		return fmt.Errorf("invalid blueprint review result schema or kind")
	}
	if !isBlueprintReviewLens(result.Lens) {
		return fmt.Errorf("invalid blueprint review lens %q", result.Lens)
	}
	if !lowerHex64Pattern.MatchString(result.BlueprintSHA256) {
		return fmt.Errorf("invalid blueprint digest")
	}
	if !lowerHex40Pattern.MatchString(result.AuthorityBaselineCommit) {
		return fmt.Errorf("invalid authority baseline commit")
	}
	if result.Findings == nil {
		return fmt.Errorf("findings must be an array")
	}
	switch result.Verdict {
	case "clean":
		if len(result.Findings) != 0 {
			return fmt.Errorf("clean result contains findings")
		}
	case "not_clean":
		if len(result.Findings) == 0 || len(result.Findings) > 64 {
			return fmt.Errorf("not_clean result must contain 1 to 64 findings")
		}
	default:
		return fmt.Errorf("invalid blueprint review verdict %q", result.Verdict)
	}
	seen := make(map[string]struct{}, len(result.Findings))
	priorRank := -1
	priorID := ""
	for index := range result.Findings {
		finding := &result.Findings[index]
		if finding.SchemaVersion != 1 {
			return fmt.Errorf("finding %d has invalid schema_version", index)
		}
		if !findingIDPattern.MatchString(finding.ID) || len(finding.ID) > 64 {
			return fmt.Errorf("finding %d has invalid id", index)
		}
		if _, duplicate := seen[finding.ID]; duplicate {
			return fmt.Errorf("finding %d duplicates id %q", index, finding.ID)
		}
		seen[finding.ID] = struct{}{}
		rank, ok := reviewSeverityRank(finding.Severity)
		if !ok {
			return fmt.Errorf("finding %d has invalid severity", index)
		}
		if rank < priorRank || (rank == priorRank && finding.ID <= priorID) {
			return fmt.Errorf("findings are not ordered by severity and id")
		}
		priorRank, priorID = rank, finding.ID
		for _, field := range []struct {
			name  string
			value string
			limit int
		}{
			{name: "location", value: finding.Location, limit: 1_024},
			{name: "evidence", value: finding.Evidence, limit: 8_192},
			{name: "correction", value: finding.Correction, limit: 8_192},
		} {
			if field.value == "" || !utf8.ValidString(field.value) || len(field.value) > field.limit || strings.IndexByte(field.value, 0) >= 0 {
				return fmt.Errorf("finding %d has invalid %s", index, field.name)
			}
		}
	}
	return nil
}

func isBlueprintReviewLens(lens string) bool {
	for _, allowed := range blueprintReviewLenses {
		if lens == allowed {
			return true
		}
	}
	return false
}

func reviewSeverityRank(severity string) (int, bool) {
	switch severity {
	case "critical":
		return 0, true
	case "high":
		return 1, true
	case "medium":
		return 2, true
	case "low":
		return 3, true
	default:
		return 0, false
	}
}

// BlueprintReviewFindingV1 is the closed finding object retained by a review.
type BlueprintReviewFindingV1 struct {
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"id"`
	Severity      string `json:"severity"`
	Location      string `json:"location"`
	Evidence      string `json:"evidence"`
	Correction    string `json:"correction"`
}

// CQProxyBlueprintReviewResultV1 is the closed result returned by one lens.
type CQProxyBlueprintReviewResultV1 struct {
	SchemaVersion           int                        `json:"schema_version"`
	Kind                    string                     `json:"kind"`
	Lens                    string                     `json:"lens"`
	BlueprintSHA256         string                     `json:"blueprint_sha256"`
	AuthorityBaselineCommit string                     `json:"authority_baseline_commit"`
	Verdict                 string                     `json:"verdict"`
	Findings                []BlueprintReviewFindingV1 `json:"findings"`
}

// BlueprintReviewRecordV1 is one retained terminal lens record.
type BlueprintReviewRecordV1 struct {
	Lens               string                     `json:"lens"`
	ReviewerTaskID     string                     `json:"reviewer_task_id"`
	ReviewedAt         string                     `json:"reviewed_at"`
	Verdict            string                     `json:"verdict"`
	Findings           []BlueprintReviewFindingV1 `json:"findings"`
	ReviewResultSHA256 string                     `json:"review_result_sha256"`
	RecordSHA256       string                     `json:"record_sha256"`
}

// BlueprintReviewSiblingV1 is the terminal recursive-review sibling.
type BlueprintReviewSiblingV1 struct {
	SchemaVersion           int                       `json:"schema_version"`
	Kind                    string                    `json:"kind"`
	BlueprintPath           string                    `json:"blueprint_path"`
	BlueprintSHA256         string                    `json:"blueprint_sha256"`
	AuthorityBaselineCommit string                    `json:"authority_baseline_commit"`
	Round                   uint64                    `json:"round"`
	Records                 []BlueprintReviewRecordV1 `json:"records"`
	AggregateSHA256         string                    `json:"aggregate_sha256"`
}

// CanonicalJSONV1 serialises a JSON value using the RFC 8785 JSON
// Canonicalization Scheme. It returns the canonical object bytes without a
// trailing newline.
func CanonicalJSONV1(value any) ([]byte, error) {
	if err := validateJSONStrings(reflect.ValueOf(value), make(map[visit]bool)); err != nil {
		return nil, err
	}
	marshalled, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal JSON value: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(marshalled))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode JSON value: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	canonical := make([]byte, 0, len(marshalled))
	canonical, err = appendCanonicalJSON(canonical, decoded)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

type visit struct {
	typeOf  reflect.Type
	pointer uintptr
}

func validateJSONStrings(value reflect.Value, seen map[visit]bool) error {
	if !value.IsValid() {
		return nil
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		return validateJSONStrings(value.Elem(), seen)
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		key := visit{typeOf: value.Type(), pointer: value.Pointer()}
		if seen[key] {
			return nil
		}
		seen[key] = true
		defer delete(seen, key)
		return validateJSONStrings(value.Elem(), seen)
	}
	switch value.Kind() {
	case reflect.String:
		if !utf8.ValidString(value.String()) {
			return fmt.Errorf("JSON string is not valid UTF-8")
		}
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			if err := validateJSONStrings(iterator.Key(), seen); err != nil {
				return err
			}
			if err := validateJSONStrings(iterator.Value(), seen); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if !value.IsNil() {
			key := visit{typeOf: value.Type(), pointer: value.Pointer()}
			if seen[key] {
				return nil
			}
			seen[key] = true
			defer delete(seen, key)
		}
		fallthrough
	case reflect.Array:
		for index := range value.Len() {
			if err := validateJSONStrings(value.Index(index), seen); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for index := range value.NumField() {
			if value.Type().Field(index).PkgPath == "" {
				if err := validateJSONStrings(value.Field(index), seen); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func appendCanonicalJSON(destination []byte, value any) ([]byte, error) {
	switch typed := value.(type) {
	case nil:
		return append(destination, "null"...), nil
	case bool:
		return strconv.AppendBool(destination, typed), nil
	case string:
		return appendCanonicalString(destination, typed), nil
	case json.Number:
		return appendCanonicalNumber(destination, typed)
	case []any:
		destination = append(destination, '[')
		for index, item := range typed {
			if index > 0 {
				destination = append(destination, ',')
			}
			var err error
			destination, err = appendCanonicalJSON(destination, item)
			if err != nil {
				return nil, err
			}
		}
		return append(destination, ']'), nil
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(left, right int) bool {
			return lessUTF16(keys[left], keys[right])
		})
		destination = append(destination, '{')
		for index, key := range keys {
			if index > 0 {
				destination = append(destination, ',')
			}
			destination = appendCanonicalString(destination, key)
			destination = append(destination, ':')
			var err error
			destination, err = appendCanonicalJSON(destination, typed[key])
			if err != nil {
				return nil, err
			}
		}
		return append(destination, '}'), nil
	default:
		return nil, fmt.Errorf("unsupported JSON value %T", value)
	}
}

func appendCanonicalString(destination []byte, value string) []byte {
	const hex = "0123456789abcdef"
	destination = append(destination, '"')
	for _, character := range value {
		switch character {
		case '"', '\\':
			destination = append(destination, '\\', byte(character))
		case '\b':
			destination = append(destination, `\b`...)
		case '\t':
			destination = append(destination, `\t`...)
		case '\n':
			destination = append(destination, `\n`...)
		case '\f':
			destination = append(destination, `\f`...)
		case '\r':
			destination = append(destination, `\r`...)
		default:
			if character < 0x20 {
				destination = append(destination, '\\', 'u', '0', '0', hex[byte(character)>>4], hex[byte(character)&0xf])
			} else {
				destination = utf8.AppendRune(destination, character)
			}
		}
	}
	return append(destination, '"')
}

func appendCanonicalNumber(destination []byte, number json.Number) ([]byte, error) {
	representation := number.String()
	if representation == "-0" {
		representation = "0"
	}
	if _, err := strconv.ParseFloat(representation, 64); err != nil {
		return nil, fmt.Errorf("invalid JSON number %q: %w", representation, err)
	}
	return append(destination, representation...), nil
}

func lessUTF16(left, right string) bool {
	leftUnits := utf16.Encode([]rune(left))
	rightUnits := utf16.Encode([]rune(right))
	for index := 0; index < min(len(leftUnits), len(rightUnits)); index++ {
		if leftUnits[index] != rightUnits[index] {
			return leftUnits[index] < rightUnits[index]
		}
	}
	return len(leftUnits) < len(rightUnits)
}

// VerifyBlueprintReview verifies that the frozen blueprint and its retained
// recursive-review sibling are the exact bytes admitted for construction.
func VerifyBlueprintReview(path, sibling string) error {
	var siblingBytes []byte
	for _, file := range []struct {
		name string
		path string
		want string
		max  int
	}{
		{name: "blueprint", path: path, want: frozenBlueprintSHA256, max: maxFrozenBlueprintBytes},
		{name: "review sibling", path: sibling, want: frozenReviewSHA256, max: maxReviewSiblingFile},
	} {
		data, err := readStaticNoFollow(file.path, file.max)
		if err != nil {
			return fmt.Errorf("read %s: %w", file.name, err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != file.want {
			return fmt.Errorf("%s digest %s, want %s", file.name, got, file.want)
		}
		if file.name == "review sibling" {
			siblingBytes = data
		}
	}
	if _, err := parseBlueprintReviewSiblingV1(bytes.NewReader(siblingBytes), frozenBlueprintSHA256, frozenReviewBaseline); err != nil {
		return err
	}
	return nil
}

func readStaticNoFollow(path string, limit int) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("authority path is not a regular non-symlink file")
	}
	opener, ok := any(fsutil.OSFileSystem{}).(fsutil.NoFollowFileOpener)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	opened, err := opener.OpenNoFollow(path)
	if err != nil {
		return nil, err
	}
	openedInfo, statErr := opened.Stat()
	if statErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(before, openedInfo) {
		_ = opened.Close()
		if statErr != nil {
			return nil, statErr
		}
		return nil, fmt.Errorf("authority path changed before read")
	}
	data, readErr := readBoundedBytes(opened, limit)
	closeErr := opened.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, after) {
		return nil, fmt.Errorf("authority path changed during read")
	}
	return data, nil
}

func parseBlueprintReviewSiblingV1(reader io.Reader, expectedBlueprintSHA256, expectedBaseline string) (BlueprintReviewSiblingV1, error) {
	var sibling BlueprintReviewSiblingV1
	fileBytes, err := readBoundedBytes(reader, maxReviewSiblingFile)
	if err != nil {
		return sibling, fmt.Errorf("read review sibling: %w", err)
	}
	if len(fileBytes) == 0 || fileBytes[len(fileBytes)-1] != '\n' {
		return sibling, fmt.Errorf("review sibling must end in exactly one LF")
	}
	jcs := fileBytes[:len(fileBytes)-1]
	if len(jcs) > maxReviewSiblingJCS {
		return sibling, fmt.Errorf("review sibling JCS exceeds %d bytes", maxReviewSiblingJCS)
	}
	decoded, err := decodeStrictJSON(jcs)
	if err != nil {
		return sibling, fmt.Errorf("decode review sibling: %w", err)
	}
	canonical, err := appendCanonicalJSON(make([]byte, 0, len(jcs)), decoded)
	if err != nil {
		return sibling, err
	}
	if !bytes.Equal(canonical, jcs) {
		return sibling, fmt.Errorf("review sibling is not canonical JCS")
	}
	var raw struct {
		SchemaVersion           int               `json:"schema_version"`
		Kind                    string            `json:"kind"`
		BlueprintPath           string            `json:"blueprint_path"`
		BlueprintSHA256         string            `json:"blueprint_sha256"`
		AuthorityBaselineCommit string            `json:"authority_baseline_commit"`
		Round                   uint64            `json:"round"`
		Records                 []json.RawMessage `json:"records"`
		AggregateSHA256         string            `json:"aggregate_sha256"`
	}
	if err := decodeClosedJSON(jcs, &raw); err != nil {
		return sibling, fmt.Errorf("decode review sibling object: %w", err)
	}
	for index, record := range raw.Records {
		if len(record) > maxReviewRecordJCS {
			return sibling, fmt.Errorf("review record %d exceeds %d bytes", index, maxReviewRecordJCS)
		}
	}
	if err := decodeClosedJSON(jcs, &sibling); err != nil {
		return sibling, fmt.Errorf("decode review sibling schema: %w", err)
	}
	if err := validateBlueprintReviewSibling(&sibling, expectedBlueprintSHA256, expectedBaseline); err != nil {
		return sibling, err
	}
	return sibling, nil
}

func validateBlueprintReviewSibling(sibling *BlueprintReviewSiblingV1, expectedBlueprintSHA256, expectedBaseline string) error {
	if sibling.SchemaVersion != 1 || sibling.Kind != "cq_proxy_blueprint_recursive_review" {
		return fmt.Errorf("invalid review sibling schema or kind")
	}
	if sibling.BlueprintPath != blueprintReviewPath {
		return fmt.Errorf("review sibling blueprint_path %q", sibling.BlueprintPath)
	}
	if !lowerHex64Pattern.MatchString(sibling.BlueprintSHA256) || sibling.BlueprintSHA256 != expectedBlueprintSHA256 {
		return fmt.Errorf("stale or invalid review sibling blueprint digest")
	}
	if !lowerHex40Pattern.MatchString(sibling.AuthorityBaselineCommit) || sibling.AuthorityBaselineCommit != expectedBaseline {
		return fmt.Errorf("stale or invalid review sibling authority baseline")
	}
	if sibling.Round == 0 || sibling.Round > maxJCSSafeInteger {
		return fmt.Errorf("review sibling round %d is outside the JCS-safe interval", sibling.Round)
	}
	if len(sibling.Records) != len(blueprintReviewLenses) {
		return fmt.Errorf("review sibling has %d records, want %d", len(sibling.Records), len(blueprintReviewLenses))
	}
	taskIDs := make(map[string]struct{}, len(sibling.Records))
	recordDigests := make([][]byte, 0, len(sibling.Records))
	for index := range sibling.Records {
		record := &sibling.Records[index]
		if record.Lens != blueprintReviewLenses[index] {
			return fmt.Errorf("review record %d lens %q, want %q", index, record.Lens, blueprintReviewLenses[index])
		}
		if !reviewerTaskIDPattern.MatchString(record.ReviewerTaskID) || len(record.ReviewerTaskID) > 256 {
			return fmt.Errorf("review record %d has invalid reviewer_task_id", index)
		}
		if _, duplicate := taskIDs[record.ReviewerTaskID]; duplicate {
			return fmt.Errorf("review record %d reuses reviewer_task_id", index)
		}
		taskIDs[record.ReviewerTaskID] = struct{}{}
		if !reviewedAtPattern.MatchString(record.ReviewedAt) {
			return fmt.Errorf("review record %d has invalid reviewed_at", index)
		}
		parsedTime, err := time.Parse("2006-01-02T15:04:05Z", record.ReviewedAt)
		if err != nil || parsedTime.UTC().Format("2006-01-02T15:04:05Z") != record.ReviewedAt {
			return fmt.Errorf("review record %d has invalid reviewed_at", index)
		}
		if record.Verdict != "clean" || len(record.Findings) != 0 {
			return fmt.Errorf("terminal review record %d is not clean with empty findings", index)
		}
		result := CQProxyBlueprintReviewResultV1{
			SchemaVersion:           1,
			Kind:                    "cq_proxy_blueprint_review_result_v1",
			Lens:                    record.Lens,
			BlueprintSHA256:         sibling.BlueprintSHA256,
			AuthorityBaselineCommit: sibling.AuthorityBaselineCommit,
			Verdict:                 record.Verdict,
			Findings:                record.Findings,
		}
		resultDigest, err := canonicalFramedSHA256("cq/proxy-blueprint-review-result/v1\x00", result)
		if err != nil {
			return err
		}
		if record.ReviewResultSHA256 != resultDigest {
			return fmt.Errorf("review record %d result digest mismatch", index)
		}
		recordWithoutDigest := struct {
			Lens               string                     `json:"lens"`
			ReviewerTaskID     string                     `json:"reviewer_task_id"`
			ReviewedAt         string                     `json:"reviewed_at"`
			Verdict            string                     `json:"verdict"`
			Findings           []BlueprintReviewFindingV1 `json:"findings"`
			ReviewResultSHA256 string                     `json:"review_result_sha256"`
		}{record.Lens, record.ReviewerTaskID, record.ReviewedAt, record.Verdict, record.Findings, record.ReviewResultSHA256}
		recordDigest, err := canonicalFramedSHA256("cq/proxy-blueprint-review-record/v1\x00", recordWithoutDigest)
		if err != nil {
			return err
		}
		if record.RecordSHA256 != recordDigest {
			return fmt.Errorf("review record %d record digest mismatch", index)
		}
		decoded, _ := hex.DecodeString(recordDigest)
		recordDigests = append(recordDigests, decoded)
	}
	aggregate, err := blueprintReviewAggregateDigest(sibling, recordDigests)
	if err != nil {
		return err
	}
	if sibling.AggregateSHA256 != aggregate {
		return fmt.Errorf("review sibling aggregate digest mismatch")
	}
	return nil
}

func canonicalFramedSHA256(domain string, value any) (string, error) {
	canonical, err := CanonicalJSONV1(value)
	if err != nil {
		return "", err
	}
	if uint64(len(canonical)) > uint64(^uint32(0)) {
		return "", fmt.Errorf("canonical object is too large to frame")
	}
	hash := sha256.New()
	_, _ = io.WriteString(hash, domain)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(canonical)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func blueprintReviewAggregateDigest(sibling *BlueprintReviewSiblingV1, recordDigests [][]byte) (string, error) {
	blueprintDigest, err := hex.DecodeString(sibling.BlueprintSHA256)
	if err != nil || len(blueprintDigest) != sha256.Size {
		return "", fmt.Errorf("decode blueprint digest")
	}
	baseline, err := hex.DecodeString(sibling.AuthorityBaselineCommit)
	if err != nil || len(baseline) != 20 {
		return "", fmt.Errorf("decode authority baseline")
	}
	hash := sha256.New()
	_, _ = io.WriteString(hash, "cq/proxy-blueprint-review/v1\x00")
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(sibling.BlueprintPath)))
	_, _ = hash.Write(length[:])
	_, _ = io.WriteString(hash, sibling.BlueprintPath)
	_, _ = hash.Write(blueprintDigest)
	_, _ = hash.Write(baseline)
	var round [8]byte
	binary.BigEndian.PutUint64(round[:], sibling.Round)
	_, _ = hash.Write(round[:])
	for _, digest := range recordDigests {
		if len(digest) != sha256.Size {
			return "", fmt.Errorf("invalid record digest length")
		}
		_, _ = hash.Write(digest)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readBoundedBytes(reader io.Reader, limit int) ([]byte, error) {
	result := make([]byte, 0, limit)
	buffer := make([]byte, 4<<10)
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			if len(result)+count > limit {
				return nil, fmt.Errorf("input exceeds %d bytes", limit)
			}
			result = append(result, buffer[:count]...)
		}
		if err == io.EOF {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		if count == 0 {
			return nil, io.ErrNoProgress
		}
	}
}

func decodeStrictJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeStrictJSONValue(decoder)
	if err != nil {
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	return value, nil
}

func decodeStrictJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("object member name is not a string")
			}
			if _, duplicate := object[key]; duplicate {
				return nil, fmt.Errorf("duplicate object member %q", key)
			}
			value, err := decodeStrictJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return object, nil
	case '[':
		var array []any
		for decoder.More() {
			value, err := decodeStrictJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected delimiter %q", delimiter)
	}
}

func decodeClosedJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}
