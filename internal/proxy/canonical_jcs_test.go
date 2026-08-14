package proxy

import (
	"bytes"
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

func TestParseBlueprintReviewResultAcceptsCleanAndNotClean(t *testing.T) {
	for _, input := range []string{
		`{"authority_baseline_commit":"9fe30df8d4101f69084d6487740ed324a5d0b59d","blueprint_sha256":"bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38","findings":[],"kind":"cq_proxy_blueprint_review_result_v1","lens":"architecture","schema_version":1,"verdict":"clean"}`,
		`{"authority_baseline_commit":"9fe30df8d4101f69084d6487740ed324a5d0b59d","blueprint_sha256":"bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38","findings":[{"correction":"fix","evidence":"proof","id":"R1","location":"line 1","schema_version":1,"severity":"high"}],"kind":"cq_proxy_blueprint_review_result_v1","lens":"security_privacy","schema_version":1,"verdict":"not_clean"}`,
	} {
		if _, err := ParseBlueprintReviewResultV1(strings.NewReader(input)); err != nil {
			t.Fatalf("ParseBlueprintReviewResultV1() error = %v", err)
		}
	}
}

func TestParseBlueprintReviewResultRejectsFindingOrderAndOversizeInput(t *testing.T) {
	wrongOrder := `{"authority_baseline_commit":"9fe30df8d4101f69084d6487740ed324a5d0b59d","blueprint_sha256":"bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38","findings":[{"correction":"fix","evidence":"proof","id":"L1","location":"line 1","schema_version":1,"severity":"low"},{"correction":"fix","evidence":"proof","id":"C1","location":"line 2","schema_version":1,"severity":"critical"}],"kind":"cq_proxy_blueprint_review_result_v1","lens":"architecture","schema_version":1,"verdict":"not_clean"}`
	if _, err := ParseBlueprintReviewResultV1(strings.NewReader(wrongOrder)); err == nil {
		t.Fatal("accepted findings in the wrong severity order")
	}
	if _, err := ParseBlueprintReviewResultV1(strings.NewReader(strings.Repeat(" ", maxReviewResultJCS+1))); err == nil {
		t.Fatal("accepted result above the streaming cap")
	}
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
