package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalJSONV1UsesJCSKeyOrderAndStringEscaping(t *testing.T) {
	got, err := CanonicalJSONV1(map[string]any{
		"\ue000":     "bmp",
		"\U00010000": "supplementary",
		"markup":     "<>&",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"markup\":\"<>&\",\"𐀀\":\"supplementary\",\"\":\"bmp\"}"
	if string(got) != want {
		t.Fatalf("CanonicalJSONV1() = %q, want %q", got, want)
	}
}

func TestCanonicalJSONV1RejectsNonFiniteNumber(t *testing.T) {
	if _, err := CanonicalJSONV1(math.Inf(1)); err == nil {
		t.Fatal("accepted positive infinity")
	}
}

func TestAppendCanonicalJSONUsesECMAScriptNumberSerialisation(t *testing.T) {
	for input, want := range map[string]string{
		"[1.0,1e+09,-0]":             "[1,1000000000,0]",
		"[1e-7,1e-6]":                "[1e-7,0.000001]",
		"[1e20,1e21]":                "[100000000000000000000,1e+21]",
		"[333333333.33333329]":       "[333333333.3333333]",
		"[4.50,2e-3,0.000000000001]": "[4.5,0.002,1e-12]",
	} {
		decoded, err := decodeStrictJSON([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		got, err := appendCanonicalJSON(nil, decoded)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("appendCanonicalJSON(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestVerifyBlueprintReviewAcceptsFrozenRound44(t *testing.T) {
	err := VerifyBlueprintReview(
		"../../docs/superpowers/specs/2026-08-13-proxy-resilience-and-routing-blueprint.md",
		"../../docs/superpowers/specs/2026-08-13-proxy-resilience-and-routing-blueprint.review.json",
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestVerifyBlueprintReviewRejectsSymlinkAuthorityFiles(t *testing.T) {
	blueprint, err := filepath.Abs("../../docs/superpowers/specs/2026-08-13-proxy-resilience-and-routing-blueprint.md")
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := filepath.Abs("../../docs/superpowers/specs/2026-08-13-proxy-resilience-and-routing-blueprint.review.json")
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]struct {
		target string
		other  string
	}{
		"blueprint": {target: blueprint, other: sibling},
		"sibling":   {target: sibling, other: blueprint},
	} {
		t.Run(name, func(t *testing.T) {
			link := filepath.Join(t.TempDir(), name)
			if err := os.Symlink(target.target, link); err != nil {
				t.Fatal(err)
			}
			blueprintPath, siblingPath := link, target.other
			if name == "sibling" {
				blueprintPath, siblingPath = target.other, link
			}
			if err := VerifyBlueprintReview(blueprintPath, siblingPath); err == nil {
				t.Fatal("accepted symlink authority file")
			}
		})
	}
}

func TestParseBlueprintReviewSiblingAcceptsOneByteStreaming(t *testing.T) {
	data, err := os.ReadFile("../../docs/superpowers/specs/2026-08-13-proxy-resilience-and-routing-blueprint.review.json")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseBlueprintReviewSiblingV1(
		oneByteReader{reader: bytes.NewReader(data)},
		frozenBlueprintSHA256,
		"9fe30df8d4101f69084d6487740ed324a5d0b59d",
	)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Round != 44 || parsed.AggregateSHA256 != "3b227af5077cbaab1ad1f29444549062bad5c343baa1d15e254a1994fe2850be" {
		t.Fatalf("parsed sibling = round %d, aggregate %q", parsed.Round, parsed.AggregateSHA256)
	}
}

func TestParseBlueprintReviewSiblingRejectsRecordDigestCorruption(t *testing.T) {
	data, err := os.ReadFile("../../docs/superpowers/specs/2026-08-13-proxy-resilience-and-routing-blueprint.review.json")
	if err != nil {
		t.Fatal(err)
	}
	corrupt := strings.Replace(
		string(data),
		"80ca2be69a88822bdba9273c2c2c183e657321c1008b1359ea95d00fdba64451",
		"00ca2be69a88822bdba9273c2c2c183e657321c1008b1359ea95d00fdba64451",
		1,
	)
	if _, err := parseBlueprintReviewSiblingV1(
		strings.NewReader(corrupt),
		frozenBlueprintSHA256,
		"9fe30df8d4101f69084d6487740ed324a5d0b59d",
	); err == nil {
		t.Fatal("accepted corrupted record digest")
	}
}

func TestParseBlueprintReviewSiblingEnforcesRecordCapBeforeDecodeAndJCS(t *testing.T) {
	data, err := os.ReadFile("../../docs/superpowers/specs/2026-08-13-proxy-resilience-and-routing-blueprint.review.json")
	if err != nil {
		t.Fatal(err)
	}
	oversized := strings.Replace(
		string(data),
		"/root/round44/architecture",
		strings.Repeat("a", maxReviewRecordJCS),
		1,
	)
	instrumentation := &reviewSiblingInstrumentation{}
	if _, err := parseBlueprintReviewSiblingV1Instrumented(
		strings.NewReader(oversized),
		frozenBlueprintSHA256,
		frozenReviewBaseline,
		instrumentation,
	); err == nil {
		t.Fatal("accepted a record above the raw byte cap")
	}
	if instrumentation.RecordsScanned != 1 {
		t.Fatalf("records scanned = %d, want 1", instrumentation.RecordsScanned)
	}
	if instrumentation.Decodes != 0 || instrumentation.Canonicalisations != 0 {
		t.Fatalf(
			"oversized record reached decode/JCS: decodes=%d canonicalisations=%d",
			instrumentation.Decodes,
			instrumentation.Canonicalisations,
		)
	}
}

func TestParseBlueprintReviewResultAcceptsCleanAndNotClean(t *testing.T) {
	for _, input := range []string{
		`{"authority_baseline_commit":"9fe30df8d4101f69084d6487740ed324a5d0b59d","blueprint_sha256":"bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38","findings":[],"kind":"cq_proxy_blueprint_review_result_v1","lens":"architecture","schema_version":1,"verdict":"clean"}`,
		`{"authority_baseline_commit":"9fe30df8d4101f69084d6487740ed324a5d0b59d","blueprint_sha256":"bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38","findings":[{"correction":"fix","evidence":"proof","id":"R1","location":"line 1","schema_version":1,"severity":"high"}],"kind":"cq_proxy_blueprint_review_result_v1","lens":"security_privacy","schema_version":1,"verdict":"not_clean"}`,
	} {
		if _, err := ParseBlueprintReviewResultV1(strings.NewReader(input), frozenBlueprintSHA256, frozenReviewBaseline); err != nil {
			t.Fatalf("ParseBlueprintReviewResultV1() error = %v", err)
		}
	}
}

func TestParseBlueprintReviewResultRejectsFindingOrderAndOversizeInput(t *testing.T) {
	wrongOrder := `{"authority_baseline_commit":"9fe30df8d4101f69084d6487740ed324a5d0b59d","blueprint_sha256":"bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38","findings":[{"correction":"fix","evidence":"proof","id":"L1","location":"line 1","schema_version":1,"severity":"low"},{"correction":"fix","evidence":"proof","id":"C1","location":"line 2","schema_version":1,"severity":"critical"}],"kind":"cq_proxy_blueprint_review_result_v1","lens":"architecture","schema_version":1,"verdict":"not_clean"}`
	if _, err := ParseBlueprintReviewResultV1(strings.NewReader(wrongOrder), frozenBlueprintSHA256, frozenReviewBaseline); err == nil {
		t.Fatal("accepted findings in the wrong severity order")
	}
	if _, err := ParseBlueprintReviewResultV1(strings.NewReader(strings.Repeat(" ", maxReviewResultJCS+1)), frozenBlueprintSHA256, frozenReviewBaseline); err == nil {
		t.Fatal("accepted result above the streaming cap")
	}
}

func TestParseBlueprintReviewResultRejectsAbsentOrStaleAuthority(t *testing.T) {
	input := `{"authority_baseline_commit":"9fe30df8d4101f69084d6487740ed324a5d0b59d","blueprint_sha256":"bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38","findings":[],"kind":"cq_proxy_blueprint_review_result_v1","lens":"architecture","schema_version":1,"verdict":"clean"}`
	for name, authority := range map[string]struct {
		blueprint string
		baseline  string
	}{
		"absent blueprint": {baseline: frozenReviewBaseline},
		"absent baseline":  {blueprint: frozenBlueprintSHA256},
		"stale blueprint":  {blueprint: strings.Repeat("0", 64), baseline: frozenReviewBaseline},
		"stale baseline":   {blueprint: frozenBlueprintSHA256, baseline: strings.Repeat("0", 40)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBlueprintReviewResultV1(strings.NewReader(input), authority.blueprint, authority.baseline); err == nil {
				t.Fatal("accepted absent or stale review authority")
			}
		})
	}
}

func TestBlueprintReviewPositiveVectorMatrix(t *testing.T) {
	t.Run("canonical_clean", func(t *testing.T) {
		parseReviewResultVector(t, reviewResultVector(t, "clean", nil))
	})
	for _, severity := range []string{"critical", "high", "medium", "low"} {
		t.Run("not_clean_"+severity, func(t *testing.T) {
			parseReviewResultVector(t, reviewResultVector(t, "not_clean", []BlueprintReviewFindingV1{reviewFindingVector("A", severity)}))
		})
	}
	t.Run("one_finding", func(t *testing.T) {
		parseReviewResultVector(t, reviewResultVector(t, "not_clean", []BlueprintReviewFindingV1{reviewFindingVector("A", "high")}))
	})
	t.Run("sixty_four_findings", func(t *testing.T) {
		parseReviewResultVector(t, reviewResultVector(t, "not_clean", reviewFindingsVector(64, false)))
	})
	t.Run("maximum_legal_members", func(t *testing.T) {
		parseReviewResultVector(t, reviewResultVector(t, "not_clean", reviewFindingsVector(64, true)))
	})
	t.Run("one_byte_task_labels", func(t *testing.T) {
		labels := []string{"a", "b", "c", "d", "e", "f", "g"}
		parseReviewSiblingVector(t, buildReviewSiblingVector(t, labels, 1))
	})
	t.Run("two_hundred_fifty_six_byte_task_labels", func(t *testing.T) {
		labels := maximumReviewTaskLabels()
		parseReviewSiblingVector(t, buildReviewSiblingVector(t, labels, 1))
	})
	t.Run("exact_558_byte_record", func(t *testing.T) {
		labels := maximumReviewTaskLabels()
		sibling := buildReviewSiblingObjectVector(t, labels, 1)
		record, err := CanonicalJSONV1(sibling.Records[len(sibling.Records)-1])
		if err != nil {
			t.Fatal(err)
		}
		if len(record) != maxReviewRecordJCS {
			t.Fatalf("longest record = %d bytes, want %d", len(record), maxReviewRecordJCS)
		}
	})
	t.Run("exact_4289_byte_sibling", func(t *testing.T) {
		data := buildReviewSiblingVector(t, maximumReviewTaskLabels(), maxJCSSafeInteger)
		if len(data) != maxReviewSiblingFile {
			t.Fatalf("maximum sibling = %d bytes, want %d", len(data), maxReviewSiblingFile)
		}
		parseReviewSiblingVector(t, data)
	})
}

func TestBlueprintReviewStreamingAndDigestVectors(t *testing.T) {
	result := reviewResultVector(t, "not_clean", []BlueprintReviewFindingV1{reviewFindingVector("A", "critical")})
	for split := 0; split <= len(result); split++ {
		if _, err := ParseBlueprintReviewResultV1(&splitReader{chunks: [][]byte{result[:split], result[split:]}}, frozenBlueprintSHA256, frozenReviewBaseline); err != nil {
			t.Fatalf("result split %d: %v", split, err)
		}
	}
	if _, err := ParseBlueprintReviewResultV1(oneByteReader{reader: bytes.NewReader(result)}, frozenBlueprintSHA256, frozenReviewBaseline); err != nil {
		t.Fatalf("one-byte result chunks: %v", err)
	}

	sibling := buildReviewSiblingVector(t, []string{"a", "b", "c", "d", "e", "f", "g"}, 1)
	for split := 0; split <= len(sibling); split++ {
		if _, err := parseBlueprintReviewSiblingV1(&splitReader{chunks: [][]byte{sibling[:split], sibling[split:]}}, frozenBlueprintSHA256, frozenReviewBaseline); err != nil {
			t.Fatalf("sibling split %d: %v", split, err)
		}
	}
	if _, err := parseBlueprintReviewSiblingV1(oneByteReader{reader: bytes.NewReader(sibling)}, frozenBlueprintSHA256, frozenReviewBaseline); err != nil {
		t.Fatalf("one-byte sibling chunks: %v", err)
	}

	object := buildReviewSiblingObjectVector(t, []string{"a", "b", "c", "d", "e", "f", "g"}, 1)
	resultObject := CQProxyBlueprintReviewResultV1{
		SchemaVersion: 1, Kind: "cq_proxy_blueprint_review_result_v1", Lens: object.Records[0].Lens,
		BlueprintSHA256: object.BlueprintSHA256, AuthorityBaselineCommit: object.AuthorityBaselineCommit,
		Verdict: "clean", Findings: []BlueprintReviewFindingV1{},
	}
	if got, want := object.Records[0].ReviewResultSHA256, independentFramedDigestVector(t, "cq/proxy-blueprint-review-result/v1\x00", resultObject); got != want {
		t.Fatalf("review-result digest = %s, want %s", got, want)
	}
	recordWithoutDigest := struct {
		Lens               string                     `json:"lens"`
		ReviewerTaskID     string                     `json:"reviewer_task_id"`
		ReviewedAt         string                     `json:"reviewed_at"`
		Verdict            string                     `json:"verdict"`
		Findings           []BlueprintReviewFindingV1 `json:"findings"`
		ReviewResultSHA256 string                     `json:"review_result_sha256"`
	}{object.Records[0].Lens, object.Records[0].ReviewerTaskID, object.Records[0].ReviewedAt, object.Records[0].Verdict, object.Records[0].Findings, object.Records[0].ReviewResultSHA256}
	if got, want := object.Records[0].RecordSHA256, independentFramedDigestVector(t, "cq/proxy-blueprint-review-record/v1\x00", recordWithoutDigest); got != want {
		t.Fatalf("review-record digest = %s, want %s", got, want)
	}
	if got, want := object.AggregateSHA256, independentAggregateDigestVector(t, object); got != want {
		t.Fatalf("review aggregate digest = %s, want %s", got, want)
	}
}

func TestBlueprintReviewNegativeVectorMatrix(t *testing.T) {
	clean := reviewResultVector(t, "clean", nil)
	oneFinding := reviewResultVector(t, "not_clean", []BlueprintReviewFindingV1{reviewFindingVector("A", "high")})
	resultVectors := map[string][]byte{
		"missing_kind":         bytes.Replace(clean, []byte(`"kind":"cq_proxy_blueprint_review_result_v1",`), nil, 1),
		"wrong_kind":           bytes.Replace(clean, []byte("cq_proxy_blueprint_review_result_v1"), []byte("wrong"), 1),
		"lens_enum_drift":      bytes.Replace(clean, []byte(`"lens":"architecture"`), []byte(`"lens":"unknown"`), 1),
		"verdict_enum_drift":   bytes.Replace(clean, []byte(`"verdict":"clean"`), []byte(`"verdict":"unknown"`), 1),
		"clean_nonempty":       bytes.Replace(oneFinding, []byte(`"verdict":"not_clean"`), []byte(`"verdict":"clean"`), 1),
		"not_clean_empty":      bytes.Replace(clean, []byte(`"verdict":"clean"`), []byte(`"verdict":"not_clean"`), 1),
		"unknown_top_member":   append(bytes.TrimSuffix(clean, []byte("}")), []byte(`,"unknown":true}`)...),
		"invalid_unicode":      bytes.Replace(clean, []byte(`"lens":"architecture"`), []byte(`"lens":"\ud800"`), 1),
		"noncanonical_order":   bytes.Replace(clean, []byte(`{"authority_baseline_commit"`), []byte(`{"z":null,"authority_baseline_commit"`), 1),
		"whitespace":           append([]byte(" "), clean...),
		"integer_width":        bytes.Replace(clean, []byte(`"schema_version":1`), []byte(`"schema_version":1.0`), 1),
		"stale_blueprint":      bytes.Replace(clean, []byte(frozenBlueprintSHA256), []byte(strings.Repeat("0", 64)), 1),
		"stale_baseline":       bytes.Replace(clean, []byte(frozenReviewBaseline), []byte(strings.Repeat("0", 40)), 1),
		"result_2097153_bytes": bytes.Repeat([]byte(" "), maxReviewResultJCS+1),
	}
	invalidFinding := reviewFindingVector("A", "high")
	invalidFinding.Location = ""
	resultVectors["empty_finding_member"] = reviewResultVector(t, "not_clean", []BlueprintReviewFindingV1{invalidFinding})
	invalidFinding = reviewFindingVector("A", "high")
	invalidFinding.Location = strings.Repeat("a", 1_025)
	resultVectors["oversized_finding_member"] = reviewResultVector(t, "not_clean", []BlueprintReviewFindingV1{invalidFinding})
	invalidFinding = reviewFindingVector("_bad", "high")
	resultVectors["invalid_finding_id"] = reviewResultVector(t, "not_clean", []BlueprintReviewFindingV1{invalidFinding})
	resultVectors["duplicate_finding_id"] = reviewResultVector(t, "not_clean", []BlueprintReviewFindingV1{reviewFindingVector("A", "high"), reviewFindingVector("A", "high")})
	resultVectors["wrong_severity_order"] = reviewResultVector(t, "not_clean", []BlueprintReviewFindingV1{reviewFindingVector("A", "low"), reviewFindingVector("B", "critical")})
	resultVectors["finding_65"] = reviewResultVector(t, "not_clean", reviewFindingsVector(65, false))
	for name, data := range resultVectors {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBlueprintReviewResultV1(bytes.NewReader(data), frozenBlueprintSHA256, frozenReviewBaseline); err == nil {
				t.Fatal("accepted invalid review-result vector")
			}
		})
	}

	validSibling := buildReviewSiblingVector(t, []string{"a", "b", "c", "d", "e", "f", "g"}, 1)
	for name, data := range map[string][]byte{
		"round_zero":             buildReviewSiblingVector(t, []string{"a", "b", "c", "d", "e", "f", "g"}, 0),
		"round_above_safe":       buildReviewSiblingVector(t, []string{"a", "b", "c", "d", "e", "f", "g"}, maxJCSSafeInteger+1),
		"empty_task_label":       buildReviewSiblingVector(t, []string{"", "b", "c", "d", "e", "f", "g"}, 1),
		"reused_task_label":      buildReviewSiblingVector(t, []string{"a", "a", "c", "d", "e", "f", "g"}, 1),
		"task_label_257":         buildReviewSiblingVector(t, []string{strings.Repeat("a", 257), "b", "c", "d", "e", "f", "g"}, 1),
		"invalid_task_grammar":   buildReviewSiblingVector(t, []string{"bad label", "b", "c", "d", "e", "f", "g"}, 1),
		"digest_mismatch":        bytes.Replace(validSibling, []byte(`"aggregate_sha256":"`), []byte(`"aggregate_sha256":"0`), 1),
		"schema_mismatch":        bytes.Replace(validSibling, []byte(`"schema_version":1`), []byte(`"schema_version":2`), 1),
		"malformed_timestamp":    bytes.Replace(validSibling, []byte("2026-08-17T10:00:00Z"), []byte("2026-08-17T10:00:99Z"), 1),
		"missing_final_lf":       bytes.TrimSuffix(validSibling, []byte("\n")),
		"doubled_final_lf":       append(append([]byte(nil), validSibling...), '\n'),
		"nonterminal_lf":         append([]byte("\n"), validSibling...),
		"trailing_byte":          append(append([]byte(nil), validSibling...), 'x'),
		"unicode_task_label":     buildReviewSiblingVector(t, []string{"é", "b", "c", "d", "e", "f", "g"}, 1),
		"unicode_normalisation":  buildReviewSiblingVector(t, []string{"e\u0301", "b", "c", "d", "e", "f", "g"}, 1),
		"BOM":                    append([]byte{0xef, 0xbb, 0xbf}, validSibling...),
		"duplicate_member":       bytes.Replace(validSibling, []byte(`{"aggregate_sha256":`), []byte(`{"round":1,"aggregate_sha256":`), 1),
		"key_order_variant":      noncanonicalSiblingKeyOrderVector(t, validSibling),
		"uint64_endian_variant":  aggregateEncodingVariantVector(t, "little_endian"),
		"ASCII_hex_variant":      aggregateEncodingVariantVector(t, "ascii_hex"),
		"record_order_variant":   aggregateEncodingVariantVector(t, "record_order"),
		"record_digest_mismatch": bytes.Replace(validSibling, []byte(`"record_sha256":"`), []byte(`"record_sha256":"0`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseBlueprintReviewSiblingV1(bytes.NewReader(data), frozenBlueprintSHA256, frozenReviewBaseline); err == nil {
				t.Fatal("accepted invalid review-sibling vector")
			}
		})
	}
	t.Run("sibling_4290_bytes", func(t *testing.T) {
		data := append(buildReviewSiblingVector(t, maximumReviewTaskLabels(), maxJCSSafeInteger), '\n')
		if len(data) != maxReviewSiblingFile+1 {
			t.Fatalf("over-cap sibling = %d bytes", len(data))
		}
		if _, err := parseBlueprintReviewSiblingV1(bytes.NewReader(data), frozenBlueprintSHA256, frozenReviewBaseline); err == nil {
			t.Fatal("accepted 4,290-byte sibling")
		}
	})
	t.Run("record_559_bytes", func(t *testing.T) {
		labels := maximumReviewTaskLabels()
		labels[len(labels)-1] += "z"
		data := buildReviewSiblingVector(t, labels, 1)
		if _, err := parseBlueprintReviewSiblingV1(bytes.NewReader(data), frozenBlueprintSHA256, frozenReviewBaseline); err == nil {
			t.Fatal("accepted 559-byte record")
		}
	})
	t.Run("every_truncation_boundary", func(t *testing.T) {
		for end := 0; end < len(validSibling); end++ {
			if _, err := parseBlueprintReviewSiblingV1(bytes.NewReader(validSibling[:end]), frozenBlueprintSHA256, frozenReviewBaseline); err == nil {
				t.Fatalf("accepted truncation at byte %d", end)
			}
		}
	})
	t.Run("later_blueprint_edit", func(t *testing.T) {
		blueprint, err := os.ReadFile("../../docs/superpowers/specs/2026-08-13-proxy-resilience-and-routing-blueprint.md")
		if err != nil {
			t.Fatal(err)
		}
		directory := t.TempDir()
		blueprintPath := filepath.Join(directory, "blueprint.md")
		siblingPath := filepath.Join(directory, "review.json")
		if err := os.WriteFile(blueprintPath, append(blueprint, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(siblingPath, validSibling, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := VerifyBlueprintReview(blueprintPath, siblingPath); err == nil {
			t.Fatal("accepted edited blueprint")
		}
	})
}

func TestBlueprintReviewCapOrderingVectors(t *testing.T) {
	t.Run("result_over_cap_before_decode", func(t *testing.T) {
		instrumentation := &reviewResultInstrumentation{}
		if _, err := parseBlueprintReviewResultV1Instrumented(
			bytes.NewReader(bytes.Repeat([]byte(" "), maxReviewResultJCS+1)),
			frozenBlueprintSHA256,
			frozenReviewBaseline,
			instrumentation,
		); err == nil {
			t.Fatal("accepted result above cap")
		}
		if instrumentation.Decodes != 0 || instrumentation.Canonicalisations != 0 {
			t.Fatalf("over-cap result reached JSON/JCS: %#v", instrumentation)
		}
	})
	t.Run("sibling_over_cap_before_decode", func(t *testing.T) {
		instrumentation := &reviewSiblingInstrumentation{}
		data := append(buildReviewSiblingVector(t, maximumReviewTaskLabels(), maxJCSSafeInteger), '\n')
		if _, err := parseBlueprintReviewSiblingV1Instrumented(bytes.NewReader(data), frozenBlueprintSHA256, frozenReviewBaseline, instrumentation); err == nil {
			t.Fatal("accepted sibling above cap")
		}
		if instrumentation.Decodes != 0 || instrumentation.Canonicalisations != 0 || instrumentation.RecordsScanned != 0 {
			t.Fatalf("over-cap sibling reached token/JSON/JCS: %#v", instrumentation)
		}
	})
	t.Run("record_over_cap_before_decode", func(t *testing.T) {
		labels := maximumReviewTaskLabels()
		labels[len(labels)-1] += "z"
		instrumentation := &reviewSiblingInstrumentation{}
		if _, err := parseBlueprintReviewSiblingV1Instrumented(bytes.NewReader(buildReviewSiblingVector(t, labels, 1)), frozenBlueprintSHA256, frozenReviewBaseline, instrumentation); err == nil {
			t.Fatal("accepted record above cap")
		}
		if instrumentation.RecordsScanned != len(blueprintReviewLenses) || instrumentation.Decodes != 0 || instrumentation.Canonicalisations != 0 {
			t.Fatalf("over-cap record ordering = %#v", instrumentation)
		}
	})
}

func parseReviewResultVector(t *testing.T, data []byte) {
	t.Helper()
	if _, err := ParseBlueprintReviewResultV1(bytes.NewReader(data), frozenBlueprintSHA256, frozenReviewBaseline); err != nil {
		t.Fatal(err)
	}
}

func parseReviewSiblingVector(t *testing.T, data []byte) {
	t.Helper()
	if _, err := parseBlueprintReviewSiblingV1(bytes.NewReader(data), frozenBlueprintSHA256, frozenReviewBaseline); err != nil {
		t.Fatal(err)
	}
}

func reviewFindingVector(id, severity string) BlueprintReviewFindingV1 {
	return BlueprintReviewFindingV1{SchemaVersion: 1, ID: id, Severity: severity, Location: "line 1", Evidence: "proof", Correction: "fix"}
}

func reviewFindingsVector(count int, maximum bool) []BlueprintReviewFindingV1 {
	findings := make([]BlueprintReviewFindingV1, count)
	for index := range findings {
		id := fmt.Sprintf("%02d", index)
		finding := reviewFindingVector(id, "high")
		if maximum {
			finding.ID += strings.Repeat("a", 64-len(finding.ID))
			finding.Location = strings.Repeat("l", 1_024)
			finding.Evidence = strings.Repeat("e", 8_192)
			finding.Correction = strings.Repeat("c", 8_192)
		}
		findings[index] = finding
	}
	return findings
}

func reviewResultVector(t *testing.T, verdict string, findings []BlueprintReviewFindingV1) []byte {
	t.Helper()
	if findings == nil {
		findings = []BlueprintReviewFindingV1{}
	}
	data, err := CanonicalJSONV1(CQProxyBlueprintReviewResultV1{
		SchemaVersion: 1, Kind: "cq_proxy_blueprint_review_result_v1", Lens: "architecture",
		BlueprintSHA256: frozenBlueprintSHA256, AuthorityBaselineCommit: frozenReviewBaseline,
		Verdict: verdict, Findings: findings,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func maximumReviewTaskLabels() []string {
	labels := make([]string, len(blueprintReviewLenses))
	for index := range labels {
		labels[index] = strings.Repeat("a", 255) + string(rune('a'+index))
	}
	return labels
}

func buildReviewSiblingVector(t *testing.T, labels []string, round uint64) []byte {
	t.Helper()
	sibling := buildReviewSiblingObjectVector(t, labels, round)
	data, err := CanonicalJSONV1(sibling)
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func buildReviewSiblingObjectVector(t *testing.T, labels []string, round uint64) BlueprintReviewSiblingV1 {
	t.Helper()
	if len(labels) != len(blueprintReviewLenses) {
		t.Fatalf("labels = %d, want %d", len(labels), len(blueprintReviewLenses))
	}
	sibling := BlueprintReviewSiblingV1{
		SchemaVersion: 1, Kind: "cq_proxy_blueprint_recursive_review", BlueprintPath: blueprintReviewPath,
		BlueprintSHA256: frozenBlueprintSHA256, AuthorityBaselineCommit: frozenReviewBaseline, Round: round,
		Records: make([]BlueprintReviewRecordV1, len(labels)),
	}
	recordDigests := make([][]byte, len(labels))
	for index := range labels {
		result := CQProxyBlueprintReviewResultV1{
			SchemaVersion: 1, Kind: "cq_proxy_blueprint_review_result_v1", Lens: blueprintReviewLenses[index],
			BlueprintSHA256: frozenBlueprintSHA256, AuthorityBaselineCommit: frozenReviewBaseline,
			Verdict: "clean", Findings: []BlueprintReviewFindingV1{},
		}
		resultDigest := independentFramedDigestVector(t, "cq/proxy-blueprint-review-result/v1\x00", result)
		recordWithoutDigest := struct {
			Lens               string                     `json:"lens"`
			ReviewerTaskID     string                     `json:"reviewer_task_id"`
			ReviewedAt         string                     `json:"reviewed_at"`
			Verdict            string                     `json:"verdict"`
			Findings           []BlueprintReviewFindingV1 `json:"findings"`
			ReviewResultSHA256 string                     `json:"review_result_sha256"`
		}{blueprintReviewLenses[index], labels[index], "2026-08-17T10:00:00Z", "clean", []BlueprintReviewFindingV1{}, resultDigest}
		recordDigest := independentFramedDigestVector(t, "cq/proxy-blueprint-review-record/v1\x00", recordWithoutDigest)
		sibling.Records[index] = BlueprintReviewRecordV1{
			Lens: blueprintReviewLenses[index], ReviewerTaskID: labels[index], ReviewedAt: "2026-08-17T10:00:00Z",
			Verdict: "clean", Findings: []BlueprintReviewFindingV1{}, ReviewResultSHA256: resultDigest, RecordSHA256: recordDigest,
		}
		recordDigests[index], _ = hex.DecodeString(recordDigest)
	}
	sibling.AggregateSHA256 = independentAggregateDigestBytesVector(t, sibling, recordDigests)
	return sibling
}

func independentFramedDigestVector(t *testing.T, domain string, value any) string {
	t.Helper()
	canonical, err := CanonicalJSONV1(value)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, _ = io.WriteString(hash, domain)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(canonical)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil))
}

func independentAggregateDigestVector(t *testing.T, sibling BlueprintReviewSiblingV1) string {
	t.Helper()
	digests := make([][]byte, len(sibling.Records))
	for index := range sibling.Records {
		digests[index], _ = hex.DecodeString(sibling.Records[index].RecordSHA256)
	}
	return independentAggregateDigestBytesVector(t, sibling, digests)
}

func independentAggregateDigestBytesVector(t *testing.T, sibling BlueprintReviewSiblingV1, recordDigests [][]byte) string {
	t.Helper()
	blueprint, _ := hex.DecodeString(sibling.BlueprintSHA256)
	baseline, _ := hex.DecodeString(sibling.AuthorityBaselineCommit)
	hash := sha256.New()
	_, _ = io.WriteString(hash, "cq/proxy-blueprint-review/v1\x00")
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(sibling.BlueprintPath)))
	_, _ = hash.Write(length[:])
	_, _ = io.WriteString(hash, sibling.BlueprintPath)
	_, _ = hash.Write(blueprint)
	_, _ = hash.Write(baseline)
	var round [8]byte
	binary.BigEndian.PutUint64(round[:], sibling.Round)
	_, _ = hash.Write(round[:])
	for _, digest := range recordDigests {
		_, _ = hash.Write(digest)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func noncanonicalSiblingKeyOrderVector(t *testing.T, canonical []byte) []byte {
	t.Helper()
	comma := bytes.IndexByte(canonical, ',')
	if comma < 0 || canonical[len(canonical)-1] != '\n' {
		t.Fatal("invalid canonical sibling fixture")
	}
	first := append([]byte(nil), canonical[1:comma]...)
	result := append([]byte{'{'}, canonical[comma+1:len(canonical)-2]...)
	result = append(result, ',')
	result = append(result, first...)
	return append(result, '}', '\n')
}

func aggregateEncodingVariantVector(t *testing.T, variant string) []byte {
	t.Helper()
	sibling := buildReviewSiblingObjectVector(t, []string{"a", "b", "c", "d", "e", "f", "g"}, 1)
	hash := sha256.New()
	_, _ = io.WriteString(hash, "cq/proxy-blueprint-review/v1\x00")
	var pathLength [4]byte
	binary.BigEndian.PutUint32(pathLength[:], uint32(len(sibling.BlueprintPath)))
	_, _ = hash.Write(pathLength[:])
	_, _ = io.WriteString(hash, sibling.BlueprintPath)
	if variant == "ascii_hex" {
		_, _ = io.WriteString(hash, sibling.BlueprintSHA256)
		_, _ = io.WriteString(hash, sibling.AuthorityBaselineCommit)
	} else {
		blueprint, _ := hex.DecodeString(sibling.BlueprintSHA256)
		baseline, _ := hex.DecodeString(sibling.AuthorityBaselineCommit)
		_, _ = hash.Write(blueprint)
		_, _ = hash.Write(baseline)
	}
	var round [8]byte
	if variant == "little_endian" {
		binary.LittleEndian.PutUint64(round[:], sibling.Round)
	} else {
		binary.BigEndian.PutUint64(round[:], sibling.Round)
	}
	_, _ = hash.Write(round[:])
	records := sibling.Records
	if variant == "record_order" {
		records = append([]BlueprintReviewRecordV1(nil), records...)
		for left, right := 0, len(records)-1; left < right; left, right = left+1, right-1 {
			records[left], records[right] = records[right], records[left]
		}
	}
	for _, record := range records {
		digest, _ := hex.DecodeString(record.RecordSHA256)
		_, _ = hash.Write(digest)
	}
	sibling.AggregateSHA256 = hex.EncodeToString(hash.Sum(nil))
	data, err := CanonicalJSONV1(sibling)
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

type splitReader struct {
	chunks [][]byte
	index  int
	offset int
}

func (reader *splitReader) Read(destination []byte) (int, error) {
	for reader.index < len(reader.chunks) && reader.offset == len(reader.chunks[reader.index]) {
		reader.index++
		reader.offset = 0
	}
	if reader.index == len(reader.chunks) {
		return 0, io.EOF
	}
	count := copy(destination, reader.chunks[reader.index][reader.offset:])
	reader.offset += count
	return count, nil
}

type oneByteReader struct {
	reader *bytes.Reader
}

func (reader oneByteReader) Read(destination []byte) (int, error) {
	if len(destination) > 1 {
		destination = destination[:1]
	}
	return reader.reader.Read(destination)
}
