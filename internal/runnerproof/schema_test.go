package runnerproof

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestReceiptCanonicalRoundTripAndDomainSeparatedSignatures(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0x42}, 128)))
	if err != nil {
		t.Fatal(err)
	}
	admission := minimalSignedAdmission(t, privateKey)
	canonical, err := CanonicalAdmission(admission)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeAdmission(canonical)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := CanonicalAdmission(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(roundTrip, canonical) {
		t.Fatalf("round trip changed canonical receipt\n got: %s\nwant: %s", roundTrip, canonical)
	}
	payload, err := canonicalAdmissionPayload(admission.Payload)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := hex.DecodeString(admission.Signature)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(publicKey, receiptSignatureInput(KindAdmission, payload), signature) {
		t.Fatal("admission signature did not verify in its domain")
	}
	if ed25519.Verify(publicKey, receiptSignatureInput(KindCleanup, payload), signature) {
		t.Fatal("admission signature verified in cleanup domain")
	}
}

func TestReceiptDecodeRejectsUnknownDuplicateUnboundedAndNoncanonicalJSON(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0x43}, 128)))
	if err != nil {
		t.Fatal(err)
	}
	receipt := minimalSignedAdmission(t, privateKey)
	canonical, err := CanonicalAdmission(receipt)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{
		"unknown":       bytes.Replace(canonical, []byte(`"signature":`), []byte(`"unknown":0,"signature":`), 1),
		"duplicate":     bytes.Replace(canonical, []byte(`"key_id":`), []byte(`"key_id":"duplicate","key_id":`), 1),
		"whitespace":    append([]byte(" "), canonical...),
		"trailing":      append(append([]byte(nil), canonical...), '\n'),
		"invalid UTF-8": append(append([]byte(nil), canonical...), 0xff),
		"null":          []byte("null"),
		"unbounded":     bytes.Repeat([]byte{'x'}, receiptMaxBytes+1),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeAdmission(input); err == nil {
				t.Fatal("DecodeAdmission accepted invalid input")
			}
		})
	}

	uppercaseSignature := receipt
	uppercaseSignature.Signature = strings.ToUpper(uppercaseSignature.Signature)
	if _, err := CanonicalAdmission(uppercaseSignature); err == nil {
		t.Fatal("CanonicalAdmission accepted uppercase signature")
	}
	nullCollection := receipt
	nullCollection.Payload.Labels = nil
	if _, err := CanonicalAdmission(nullCollection); err == nil {
		t.Fatal("CanonicalAdmission emitted a null collection")
	}
}

func TestAdmissionRejectsWrongRunLabelsArchitectureCapabilityExpiryAndReplay(t *testing.T) {
	fixture := newReceiptFixture(t)
	now := fixture.now
	tests := map[string]func(*SignedAdmission, *TrustConfig, *RunIdentity, *[]Capability, *time.Time){
		"run": func(_ *SignedAdmission, _ *TrustConfig, run *RunIdentity, _ *[]Capability, _ *time.Time) {
			run.RunID++
		},
		"labels": func(_ *SignedAdmission, trust *TrustConfig, _ *RunIdentity, _ *[]Capability, _ *time.Time) {
			trust.Labels = append(slices.Clone(trust.Labels), "unexpected")
		},
		"architecture": func(_ *SignedAdmission, trust *TrustConfig, _ *RunIdentity, _ *[]Capability, _ *time.Time) {
			trust.Architecture = ArchitectureARM64
		},
		"capability": func(_ *SignedAdmission, _ *TrustConfig, _ *RunIdentity, capabilities *[]Capability, _ *time.Time) {
			*capabilities = []Capability{CapabilityGate1Native, CapabilityNativeInteractive}
		},
		"expiry": func(_ *SignedAdmission, _ *TrustConfig, _ *RunIdentity, _ *[]Capability, now *time.Time) {
			*now = time.Date(2026, 8, 29, 10, 16, 0, 0, time.UTC)
		},
		"replay nonce": func(admission *SignedAdmission, _ *TrustConfig, _ *RunIdentity, _ *[]Capability, _ *time.Time) {
			admission.Payload.Chain.Nonce = "short"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			admission := fixture.admission
			trust := fixture.trust
			run := fixture.run
			capabilities := slices.Clone(fixture.capabilities)
			at := now
			mutate(&admission, &trust, &run, &capabilities, &at)
			if name == "replay nonce" {
				admission = signAdmission(t, admission.Payload, fixture.privateKey, fixture.trust.KeyID)
			}
			if _, err := VerifyAdmission(admission, nil, trust, run, capabilities, at); err == nil {
				t.Fatal("VerifyAdmission accepted mutated authority")
			}
		})
	}
}

func TestAdmissionRequiresExactSignedGoExecutableAndReadOnlyModuleCacheSubtree(t *testing.T) {
	fixture := newReceiptFixture(t)
	tests := map[string]func(*TreeBinding){
		"missing go":     func(tree *TreeBinding) { tree.Entries = tree.Entries[1:] },
		"duplicate go":   func(tree *TreeBinding) { tree.Entries = append(tree.Entries, tree.Entries[0]) },
		"missing cache":  func(tree *TreeBinding) { tree.Entries = tree.Entries[:1] },
		"writable cache": func(tree *TreeBinding) { tree.Entries[1].SecurityClass = "cq-reviewed-tree-v1" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			admission := fixture.admission
			mutate(&admission.Payload.GoToolchain)
			resealTree(t, &admission.Payload.GoToolchain)
			admission = signAdmission(t, admission.Payload, fixture.privateKey, fixture.trust.KeyID)
			if _, err := VerifyAdmission(admission, nil, fixture.trust, fixture.run, fixture.capabilities, fixture.now); err == nil {
				t.Fatal("VerifyAdmission accepted invalid Go toolchain")
			}
		})
	}
}

func TestAdmissionRequiresSDKLayoutVerifierOnlyForGate3AndGate4(t *testing.T) {
	fixture := newReceiptFixture(t)
	admission := fixture.admission
	admission.Payload.TCPTableSDKLayoutVerifier = validSDKVerifier(t, admission.Payload.NativeTestHarness, ArchitectureAMD64)
	admission = signAdmission(t, admission.Payload, fixture.privateKey, fixture.trust.KeyID)
	if _, err := VerifyAdmission(admission, nil, fixture.trust, fixture.run, fixture.capabilities, fixture.now); err == nil {
		t.Fatal("Gate 1 accepted SDK verifier presence")
	}
}

func TestAdmissionRejectsSDKLayoutVerifierProvenancePEBindingAndSchemaMutation(t *testing.T) {
	fixture := newReceiptFixture(t)
	verifier := validSDKVerifier(t, fixture.admission.Payload.NativeTestHarness, ArchitectureAMD64)
	mutations := []func(*WindowsSDKLayoutVerifierBinding){
		func(value *WindowsSDKLayoutVerifierBinding) { value.CSourceSHA256 = zeroSHA256 },
		func(value *WindowsSDKLayoutVerifierBinding) { value.CompilerToolchainSHA256 = zeroSHA256 },
		func(value *WindowsSDKLayoutVerifierBinding) { value.WindowsSDKHeadersSHA256 = zeroSHA256 },
		func(value *WindowsSDKLayoutVerifierBinding) { value.BuildProvenanceSHA256 = zeroSHA256 },
		func(value *WindowsSDKLayoutVerifierBinding) { value.OutputSchemaSHA256 = zeroSHA256 },
		func(value *WindowsSDKLayoutVerifierBinding) { value.Executable.Value.PEMachine = 0xaa64 },
	}
	for index, mutate := range mutations {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			candidate := verifier
			mutate(&candidate)
			if err := validateSDKVerifier(candidate, fixture.admission.Payload.NativeTestHarness, ArchitectureAMD64); err == nil {
				t.Fatal("SDK verifier validator accepted mutation")
			}
		})
	}
}

func TestNativeInteractiveCapabilityRequiresOnlyOneOrdinaryInteractiveControllerInventory(t *testing.T) {
	fixture := newNativeInteractiveFixture(t)
	if _, err := VerifyGateChain(fixture.admission, fixture.qualification, fixture.cleanup, fixture.trust, fixture.run, fixture.capabilities, fixture.now); err != nil {
		t.Fatal(err)
	}
	qualification := fixture.qualification
	qualification.Payload.Controllers = append(qualification.Payload.Controllers, qualification.Payload.Controllers[0])
	qualification = signQualification(t, qualification.Payload, fixture.privateKey, fixture.trust.KeyID)
	if _, err := VerifyQualification(qualification, fixture.admission, fixture.admissionDigest, fixture.trust, fixture.now); err == nil {
		t.Fatal("native interactive qualification accepted second controller")
	}
}

func TestNativeInteractiveRejectsProtectedInventoryBrokerCodexPolicyAndElevatedOrServiceController(t *testing.T) {
	fixture := newNativeInteractiveFixture(t)
	tests := map[string]func(*SignedAdmission, *SignedQualification){
		"protected inventory": func(admission *SignedAdmission, _ *SignedQualification) {
			admission.Payload.Protected.Present = true
		},
		"broker evidence": func(_ *SignedAdmission, qualification *SignedQualification) {
			qualification.Payload.BrokerEvidenceSHA256 = digestFor("broker")
		},
		"elevated controller": func(_ *SignedAdmission, qualification *SignedQualification) {
			qualification.Payload.Controllers[0].Token.ElevationType = "Full"
		},
		"service controller": func(_ *SignedAdmission, qualification *SignedQualification) {
			qualification.Payload.Controllers[0].Token.SessionID = 0
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			admission := fixture.admission
			qualification := fixture.qualification
			mutate(&admission, &qualification)
			admission = signAdmission(t, admission.Payload, fixture.privateKey, fixture.trust.KeyID)
			admissionBytes, _ := CanonicalAdmission(admission)
			digest := receiptDigest(admissionBytes)
			qualification.Payload.AdmissionSHA256 = digest
			qualification.Payload.Chain.PreviousReceiptSHA256 = digest
			qualification = signQualification(t, qualification.Payload, fixture.privateKey, fixture.trust.KeyID)
			if _, err := VerifyQualification(qualification, admission, digest, fixture.trust, fixture.now); err == nil {
				t.Fatal("native interactive authority accepted protected or elevated state")
			}
		})
	}
}

func TestInitialAdmissionIsTheOnlyZeroPreviousDigestCase(t *testing.T) {
	fixture := newReceiptFixture(t)
	admission := fixture.admission
	admission.Payload.Chain.LeaseGeneration = 2
	admission = signAdmission(t, admission.Payload, fixture.privateKey, fixture.trust.KeyID)
	if _, err := VerifyAdmission(admission, nil, fixture.trust, fixture.run, fixture.capabilities, fixture.now); err == nil {
		t.Fatal("later admission accepted zero previous digest")
	}
}

func TestQualificationBindsAdmissionControllersMaterialisationsPolicyAndResults(t *testing.T) {
	fixture := newReceiptFixture(t)
	mutations := []func(*QualificationPayload){
		func(payload *QualificationPayload) { payload.AdmissionSHA256 = digestFor("wrong admission") },
		func(payload *QualificationPayload) { payload.Materialisations[0].Role = "wrong" },
		func(payload *QualificationPayload) { payload.ResultSHA256 = zeroSHA256 },
		func(payload *QualificationPayload) { payload.HarnessInventorySHA256 = zeroSHA256 },
	}
	for index, mutate := range mutations {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			qualification := fixture.qualification
			mutate(&qualification.Payload)
			qualification = signQualification(t, qualification.Payload, fixture.privateKey, fixture.trust.KeyID)
			if _, err := VerifyQualification(qualification, fixture.admission, fixture.admissionDigest, fixture.trust, fixture.now); err == nil {
				t.Fatal("qualification accepted unbound evidence")
			}
		})
	}
}

func TestTreeBindingRejectsUnsortedTraversalReparseIdentityDigestAndBoundViolations(t *testing.T) {
	fixture := newReceiptFixture(t)
	base := fixture.admission.Payload.RepositorySource
	mutations := []func(*TreeBinding){
		func(tree *TreeBinding) {
			tree.Entries = append(tree.Entries, tree.Entries[0])
			tree.Entries[1].RelativePath = "a"
		},
		func(tree *TreeBinding) { tree.Entries[0].RelativePath = "../outside" },
		func(tree *TreeBinding) { tree.Entries[0].ReparseTag = 1 },
		func(tree *TreeBinding) { tree.Entries[0].FileID = "" },
		func(tree *TreeBinding) { tree.Entries[0].SHA256 = zeroSHA256 },
		func(tree *TreeBinding) { tree.TotalSize++ },
	}
	for index, mutate := range mutations {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			tree := base
			tree.Entries = slices.Clone(base.Entries)
			mutate(&tree)
			if err := validateTreeBinding("cq-source", tree, ArchitectureAMD64); err == nil {
				t.Fatal("tree validator accepted mutation")
			}
		})
	}
}

func TestTreeBindingUses32768EntryBoundOnlyForGoToolchain(t *testing.T) {
	entry := treeEntry("entry-00000")
	ordinary := make([]TreeEntryBinding, 4096)
	for index := range ordinary {
		ordinary[index] = entry
		ordinary[index].RelativePath = fmt.Sprintf("entry-%05d", index)
	}
	tree := TreeBinding{Root: rootBinding("tree"), Entries: ordinary}
	resealTree(t, &tree)
	if err := validateTreeBinding("cq-source", tree, ArchitectureAMD64); err != nil {
		t.Fatalf("ordinary tree rejected 4096 entries: %v", err)
	}
	tree.Entries = append(tree.Entries, treeEntry("entry-04096"))
	resealTree(t, &tree)
	if err := validateTreeBinding("cq-source", tree, ArchitectureAMD64); err == nil {
		t.Fatal("ordinary tree accepted 4097 entries")
	}
	large := make([]TreeEntryBinding, 32768)
	large[0] = readOnlyTreeEntry("bin/go.exe")
	large[1] = readOnlyTreeEntry("cq-module-cache-v1/cache/download/sumdb.txt")
	for index := 2; index < len(large); index++ {
		large[index] = entry
		large[index].RelativePath = fmt.Sprintf("entry-%05d", index-2)
	}
	tree = TreeBinding{Root: rootBinding("tree"), Entries: large}
	resealTree(t, &tree)
	if err := validateTreeBinding("go-toolchain", tree, ArchitectureAMD64); err != nil {
		t.Fatalf("Go tree rejected 32768 entries: %v", err)
	}
	tree.Entries = append(tree.Entries, treeEntry("entry-32766"))
	resealTree(t, &tree)
	if err := validateTreeBinding("go-toolchain", tree, ArchitectureAMD64); err == nil {
		t.Fatal("Go tree accepted 32769 entries")
	}
	oversize := TreeBinding{Root: rootBinding("oversize"), Entries: []TreeEntryBinding{readOnlyTreeEntry("bin/go.exe"), readOnlyTreeEntry("cq-module-cache-v1/cache")}}
	oversize.Entries[0].Size = 8 << 30
	resealTree(t, &oversize)
	if err := validateTreeBinding("go-toolchain", oversize, ArchitectureAMD64); err == nil {
		t.Fatal("Go tree accepted 8 GiB plus one byte")
	}
}

func TestArtifactBindingRequiresIDAndThreeNonzeroLowercasePairwiseDistinctDigestsOnlyForGate5(t *testing.T) {
	artifact := ArtifactBinding{
		Present: true, NumericID: 1,
		GitHubArtifactSHA256:  digestFor("github"),
		BundleSHA256:          digestFor("bundle"),
		ReleaseManifestSHA256: digestFor("manifest"),
	}
	if err := validateArtifact(artifact, true); err != nil {
		t.Fatal(err)
	}
	if err := validateArtifact(artifact, false); err == nil {
		t.Fatal("non-Gate-5 capability accepted artifact")
	}
}

func TestArtifactBindingRejectsZeroMalformedAliasedAndCapabilityInconsistentFields(t *testing.T) {
	base := ArtifactBinding{true, 1, digestFor("github"), digestFor("bundle"), digestFor("manifest")}
	mutations := []func(*ArtifactBinding){
		func(value *ArtifactBinding) { value.NumericID = 0 },
		func(value *ArtifactBinding) { value.GitHubArtifactSHA256 = zeroSHA256 },
		func(value *ArtifactBinding) { value.BundleSHA256 = "BAD" },
		func(value *ArtifactBinding) { value.ReleaseManifestSHA256 = value.BundleSHA256 },
	}
	for index, mutate := range mutations {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			artifact := base
			mutate(&artifact)
			if err := validateArtifact(artifact, true); err == nil {
				t.Fatal("artifact validator accepted mutation")
			}
		})
	}
}

func TestBrokerTranscriptIsCanonicalBoundedAndRejectsReplayDirectLaunchAndPromptDrift(t *testing.T) {
	transcript := validBrokerTranscript(t, CapabilityGate4SourceProtected)
	if err := ValidateBrokerTranscript(transcript, CapabilityGate4SourceProtected); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*BrokerTranscript){
		func(value *BrokerTranscript) { value.Prompts = append(value.Prompts, value.Prompts[0]) },
		func(value *BrokerTranscript) { value.Prompts[0].DirectLaunchCount = 1 },
		func(value *BrokerTranscript) { value.Prompts[0].RequestCount = 2 },
		func(value *BrokerTranscript) { value.Prompts[0].Decision = "denied" },
	}
	for index, mutate := range mutations {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			candidate := transcript
			candidate.Prompts = slices.Clone(transcript.Prompts)
			mutate(&candidate)
			if err := ValidateBrokerTranscript(candidate, CapabilityGate4SourceProtected); err == nil {
				t.Fatal("broker transcript accepted mutation")
			}
		})
	}
}

func TestBrokerTranscriptAcceptsExactGate4AndGate5SixteenAndRejectsEveryPerCaseCountPlusOne(t *testing.T) {
	for _, capability := range []Capability{CapabilityGate4SourceProtected, CapabilityGate5ReleaseProtected} {
		transcript := validBrokerTranscript(t, capability)
		if err := ValidateBrokerTranscript(transcript, capability); err != nil {
			t.Fatal(err)
		}
		transcript.Prompts = append(transcript.Prompts, transcript.Prompts[len(transcript.Prompts)-1])
		if err := ValidateBrokerTranscript(transcript, capability); err == nil {
			t.Fatal("broker transcript accepted 17 prompts")
		}
	}
}

func TestCleanupBindsQualificationAndCompleteRestoredBaseline(t *testing.T) {
	fixture := newReceiptFixture(t)
	mutations := []func(*CleanupPayload){
		func(payload *CleanupPayload) { payload.QualificationSHA256 = digestFor("wrong") },
		func(payload *CleanupPayload) { payload.Absence.Processes = false },
		func(payload *CleanupPayload) { payload.BaselineRestored = false },
		func(payload *CleanupPayload) { payload.RestoredBaselineSHA256 = digestFor("wrong baseline") },
	}
	for index, mutate := range mutations {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			cleanup := fixture.cleanup
			mutate(&cleanup.Payload)
			cleanup = signCleanup(t, cleanup.Payload, fixture.privateKey, fixture.trust.KeyID)
			if _, err := VerifyCleanup(cleanup, fixture.admission, &fixture.qualification, fixture.qualificationDigest, fixture.trust, fixture.now); err == nil {
				t.Fatal("cleanup accepted mutation")
			}
		})
	}
}

func TestAbortedCleanupCanReconcileButCannotPassGate(t *testing.T) {
	fixture := newReceiptFixture(t)
	aborted := abortedCleanup(t, fixture)
	if _, err := VerifyCleanup(aborted, fixture.admission, nil, fixture.admissionDigest, fixture.trust, fixture.now); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyGateChain(fixture.admission, fixture.qualification, aborted, fixture.trust, fixture.run, fixture.capabilities, fixture.now); err == nil {
		t.Fatal("aborted cleanup passed gate")
	}
}

func TestNextAdmissionAcceptsFullyReconciledAbortedCleanupButGateRejectsIt(t *testing.T) {
	fixture := newReceiptFixture(t)
	aborted := abortedCleanup(t, fixture)
	abortedBytes, _ := CanonicalCleanup(aborted)
	next := fixture.admission
	next.Payload.Chain.LeaseGeneration = 2
	next.Payload.Chain.PreviousReceiptSHA256 = receiptDigest(abortedBytes)
	next.Payload.Chain.Nonce = strings.Repeat("b", 32)
	next = signAdmission(t, next.Payload, fixture.privateKey, fixture.trust.KeyID)
	if _, err := VerifyAdmission(next, &aborted, fixture.trust, fixture.run, fixture.capabilities, fixture.now); err != nil {
		t.Fatal(err)
	}
}

func TestGateChainRejectsMissingReorderedOrCrossArchitectureReceipts(t *testing.T) {
	fixture := newReceiptFixture(t)
	if _, err := VerifyGateChain(fixture.admission, fixture.qualification, fixture.cleanup, fixture.trust, fixture.run, fixture.capabilities, fixture.now); err != nil {
		t.Fatal(err)
	}
	wrongTrust := fixture.trust
	wrongTrust.Architecture = ArchitectureARM64
	if _, err := VerifyGateChain(fixture.admission, fixture.qualification, fixture.cleanup, wrongTrust, fixture.run, fixture.capabilities, fixture.now); err == nil {
		t.Fatal("cross-architecture chain passed")
	}
	reordered := fixture.cleanup
	reordered.Payload.Chain.PreviousReceiptSHA256 = fixture.admissionDigest
	reordered = signCleanup(t, reordered.Payload, fixture.privateKey, fixture.trust.KeyID)
	if _, err := VerifyGateChain(fixture.admission, fixture.qualification, reordered, fixture.trust, fixture.run, fixture.capabilities, fixture.now); err == nil {
		t.Fatal("reordered chain passed")
	}
}

func TestReceiptSchemaAndRedactedSummaryContainNoSensitiveFields(t *testing.T) {
	fixture := newReceiptFixture(t)
	summary, err := VerifyGateChain(fixture.admission, fixture.qualification, fixture.cleanup, fixture.trust, fixture.run, fixture.capabilities, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"password", "secret", "token", "path", "sid", "pipe", "task", "wfp", "127.0.0.1", `S-1-5-21`} {
		if strings.Contains(strings.ToLower(string(data)), strings.ToLower(forbidden)) {
			t.Fatalf("redacted summary contains %q: %s", forbidden, data)
		}
	}
}

func TestSyntheticReceiptChainExampleIsCanonical(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "superpowers", "evidence", "windows-runner-receipt-chain-v1.example.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var example struct {
		Schema        uint32              `json:"schema"`
		Admission     SignedAdmission     `json:"admission"`
		Qualification SignedQualification `json:"qualification"`
		Cleanup       SignedCleanup       `json:"cleanup"`
	}
	if err := json.Unmarshal(data, &example); err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalJSON(example)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSuffix(data, []byte{'\n'}), canonical) {
		t.Fatal("synthetic chain example is not canonical")
	}
}

func minimalSignedAdmission(t *testing.T, privateKey ed25519.PrivateKey) SignedAdmission {
	t.Helper()
	payload := minimalAdmissionPayload(t)
	canonical, err := canonicalAdmissionPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	return SignedAdmission{
		Payload:   payload,
		KeyID:     "synthetic-test-key-v1",
		Signature: hex.EncodeToString(ed25519.Sign(privateKey, receiptSignatureInput(KindAdmission, canonical))),
	}
}

func minimalAdmissionPayload(t *testing.T) AdmissionPayload {
	t.Helper()
	return AdmissionPayload{
		Chain: ChainIdentity{
			Schema:       ReceiptSchemaV1,
			Kind:         KindAdmission,
			Capabilities: []Capability{},
		},
		Labels:            []string{},
		RepositorySource:  TreeBinding{Entries: []TreeEntryBinding{}},
		GoToolchain:       TreeBinding{Entries: []TreeEntryBinding{}},
		NativeTestHarness: TreeBinding{Entries: []TreeEntryBinding{}},
		RaceRuntime:       OptionalTreeBinding{Value: TreeBinding{Entries: []TreeEntryBinding{}}},
	}
}

type receiptFixture struct {
	privateKey          ed25519.PrivateKey
	trust               TrustConfig
	run                 RunIdentity
	capabilities        []Capability
	now                 time.Time
	admission           SignedAdmission
	admissionDigest     string
	qualification       SignedQualification
	qualificationDigest string
	cleanup             SignedCleanup
}

func newReceiptFixture(t *testing.T) receiptFixture {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0x51}, 128)))
	if err != nil {
		t.Fatal(err)
	}
	run := RunIdentity{
		Repository: "jacobcxdev/cq",
		Workflow:   "ci",
		Job:        "windows-11-owned-amd64",
		RunID:      123,
		RunAttempt: 1,
		Commit:     strings.Repeat("a", 40),
	}
	capabilities := []Capability{CapabilityGate1Native}
	labels := []string{"Windows", "X64", "self-hosted", "windows-11"}
	issued := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	payload := AdmissionPayload{
		Chain: ChainIdentity{
			Schema: ReceiptSchemaV1, Kind: KindAdmission,
			PreviousReceiptSHA256: zeroSHA256, LeaseGeneration: 1,
			IssuedAt: issued.Format(time.RFC3339), ExpiresAt: issued.Add(15 * time.Minute).Format(time.RFC3339),
			Nonce: strings.Repeat("1", 32), Run: run, Capabilities: slices.Clone(capabilities), Phase: PhaseGate1,
		},
		Labels: labels, Architecture: ArchitectureAMD64,
		WindowsCaptionSHA256: digestFor("Windows 11"), WindowsBuild: 26100, ImageSHA256: digestFor("image"), LeaseID: "lease-opaque-1",
		QualificationDeadline: issued.Add(20 * time.Minute).Format(time.RFC3339), CleanupDeadline: issued.Add(30 * time.Minute).Format(time.RFC3339),
		Coordinator: tokenBinding("coordinator", 0, "Default"),
		Profile:     rootBinding("profile"), RoamingAppData: rootBinding("roaming"), LocalAppData: rootBinding("local"),
		RunnerTemp: rootBinding("runner-temp"), OutputRoot: rootBinding("output"),
		RepositorySource: treeBinding(t, "source", []TreeEntryBinding{treeEntry("go.mod")}),
		GoToolchain: treeBinding(t, "toolchain", []TreeEntryBinding{
			readOnlyTreeEntry("bin/go.exe"),
			readOnlyTreeEntry("cq-module-cache-v1/cache/download/sumdb.txt"),
		}),
		NativeTestHarness: treeBinding(t, "harness", []TreeEntryBinding{
			readOnlyTreeEntry("cq-windows-gate23.exe"),
			readOnlyTreeEntry("cq-windows-runner-proof.exe"),
			readOnlyTreeEntry("cq-windows-tcp-sdk-layout.exe"),
		}),
		TCPTableSDKLayoutVerifier: absentSDKVerifier(),
		RaceRuntime:               OptionalTreeBinding{Present: true, Value: treeBinding(t, "race-runtime", []TreeEntryBinding{readOnlyTreeEntry("bin/libclang_rt.asan_dynamic-x86_64.dll")})},
		NativeInteractive:         absentNativeInteractive(), Protected: absentProtectedInventory(),
		Artifact:           ArtifactBinding{},
		SourceMaterialiser: fileBinding("source-materialiser", ArchitectureAMD64),
		SourceManifest:     fileBinding("source-manifest", ""),
		AdmissionVerifier:  fileBinding("admission-verifier", ArchitectureAMD64),
		PhaseDriver:        OptionalFileBinding{},
		Qualifier:          fileBinding("qualifier", ArchitectureAMD64), CleanupRequester: fileBinding("cleanup-requester", ArchitectureAMD64),
		CleanupObserver: fileBinding("cleanup-observer", ArchitectureAMD64), CleanupObserverAdapter: fileBinding("cleanup-observer-adapter", ArchitectureAMD64),
		SummaryExporter:            fileBinding("summary-exporter", ArchitectureAMD64),
		CleanupObserverClassSHA256: digestFor("observer-class"), BootstrapPolicyVersion: 1, BootstrapPolicySHA256: digestFor("bootstrap-policy"),
		SourceBootstrapRequestSHA256: digestFor("bootstrap-request"), SourceMaterialisationResultSHA256: digestFor("materialisation-result"),
		SourceMaterialisedAt: issued.Add(-time.Minute).Format(time.RFC3339), BaselineSHA256: digestFor("baseline"),
	}
	keyID := "synthetic-test-key-v1"
	admission := signAdmission(t, payload, privateKey, keyID)
	admissionBytes, err := CanonicalAdmission(admission)
	if err != nil {
		t.Fatal(err)
	}
	admissionDigest := receiptDigest(admissionBytes)
	qualificationPayload := QualificationPayload{
		Chain: ChainIdentity{
			Schema: ReceiptSchemaV1, Kind: KindQualification, PreviousReceiptSHA256: admissionDigest, LeaseGeneration: 1,
			IssuedAt: issued.Add(5 * time.Minute).Format(time.RFC3339), ExpiresAt: issued.Add(25 * time.Minute).Format(time.RFC3339),
			Nonce: strings.Repeat("2", 32), Run: run, Capabilities: slices.Clone(capabilities), Phase: PhaseGate1,
		},
		AdmissionSHA256: admissionDigest,
		Controllers:     []ControllerGeneration{},
		Materialisations: []MaterialisationBinding{
			{Role: "cq-source", Kind: "tree", File: OptionalFileBinding{}, Tree: OptionalTreeBinding{Present: true, Value: payload.RepositorySource}},
			{Role: "cq.exe-source", Kind: "file", File: OptionalFileBinding{Present: true, Value: fileBinding("cq.exe-source", ArchitectureAMD64)}, Tree: OptionalTreeBinding{Value: TreeBinding{Entries: []TreeEntryBinding{}}}},
			{Role: "go-toolchain", Kind: "tree", File: OptionalFileBinding{}, Tree: OptionalTreeBinding{Present: true, Value: payload.GoToolchain}},
			{Role: "native-test-harness", Kind: "tree", File: OptionalFileBinding{}, Tree: OptionalTreeBinding{Present: true, Value: payload.NativeTestHarness}},
			{Role: "race-runtime", Kind: "tree", File: OptionalFileBinding{}, Tree: payload.RaceRuntime},
		},
		HarnessInventorySHA256: digestFor("harness-inventory"), ResultSHA256: digestFor("result"),
		BrokerEvidenceSHA256: zeroSHA256, EgressReadbackSHA256: zeroSHA256, DeniedProbesSHA256: zeroSHA256,
		Artifact: ArtifactBinding{},
	}
	qualification := signQualification(t, qualificationPayload, privateKey, keyID)
	qualificationBytes, err := CanonicalQualification(qualification)
	if err != nil {
		t.Fatal(err)
	}
	qualificationDigest := receiptDigest(qualificationBytes)
	cleanupPayload := CleanupPayload{
		Chain: ChainIdentity{
			Schema: ReceiptSchemaV1, Kind: KindCleanup, PreviousReceiptSHA256: qualificationDigest, LeaseGeneration: 1,
			IssuedAt: issued.Add(10 * time.Minute).Format(time.RFC3339), ExpiresAt: issued.Add(30 * time.Minute).Format(time.RFC3339),
			Nonce: strings.Repeat("3", 32), Run: run, Capabilities: slices.Clone(capabilities), Phase: PhaseGate1,
		},
		Outcome: "qualified", AdmissionSHA256: admissionDigest, QualificationSHA256: qualificationDigest,
		Absence: completeAbsenceSet(), NetworkReadbackSHA256: digestFor("network-readback"),
		RestoredBaselineSHA256: payload.BaselineSHA256, RollbackMode: "snapshot", BaselineRestored: true,
	}
	cleanup := signCleanup(t, cleanupPayload, privateKey, keyID)
	return receiptFixture{
		privateKey: privateKey,
		trust:      TrustConfig{KeyID: keyID, PublicKey: publicKey, ProvisionerID: "synthetic-provisioner", Labels: labels, Architecture: ArchitectureAMD64},
		run:        run, capabilities: capabilities, now: issued.Add(12 * time.Minute),
		admission: admission, admissionDigest: admissionDigest,
		qualification: qualification, qualificationDigest: qualificationDigest, cleanup: cleanup,
	}
}

func newNativeInteractiveFixture(t *testing.T) receiptFixture {
	t.Helper()
	fixture := newReceiptFixture(t)
	capabilities := []Capability{CapabilityGate1Native, CapabilityNativeInteractive}
	admission := fixture.admission
	admission.Payload.Chain.Capabilities = slices.Clone(capabilities)
	admission.Payload.Chain.Phase = PhaseGate2
	admission.Payload.NativeInteractive = NativeInteractiveInventory{
		Present: true, PrincipalInventorySHA256: digestFor("principals"), SessionInventorySHA256: digestFor("sessions"),
		ControllerLauncher:      OptionalFileBinding{Present: true, Value: fileBinding("controller-launcher", ArchitectureAMD64)},
		LauncherProtocolVersion: 1, LauncherConfigurationSHA256: digestFor("launcher-config"),
	}
	admission.Payload.PhaseDriver = OptionalFileBinding{Present: true, Value: fileBinding("cq-windows-gate23", ArchitectureAMD64)}
	admission = signAdmission(t, admission.Payload, fixture.privateKey, fixture.trust.KeyID)
	admissionBytes, _ := CanonicalAdmission(admission)
	admissionDigest := receiptDigest(admissionBytes)
	qualification := fixture.qualification
	qualification.Payload.Chain.Capabilities = slices.Clone(capabilities)
	qualification.Payload.Chain.Phase = PhaseGate2
	qualification.Payload.Chain.PreviousReceiptSHA256 = admissionDigest
	qualification.Payload.AdmissionSHA256 = admissionDigest
	qualification.Payload.Controllers = []ControllerGeneration{controllerBinding("interactive-user", "Default", 1)}
	qualification = signQualification(t, qualification.Payload, fixture.privateKey, fixture.trust.KeyID)
	qualificationBytes, _ := CanonicalQualification(qualification)
	qualificationDigest := receiptDigest(qualificationBytes)
	cleanup := fixture.cleanup
	cleanup.Payload.Chain.Capabilities = slices.Clone(capabilities)
	cleanup.Payload.Chain.Phase = PhaseGate2
	cleanup.Payload.Chain.PreviousReceiptSHA256 = qualificationDigest
	cleanup.Payload.AdmissionSHA256 = admissionDigest
	cleanup.Payload.QualificationSHA256 = qualificationDigest
	cleanup = signCleanup(t, cleanup.Payload, fixture.privateKey, fixture.trust.KeyID)
	fixture.capabilities = capabilities
	fixture.admission = admission
	fixture.admissionDigest = admissionDigest
	fixture.qualification = qualification
	fixture.qualificationDigest = qualificationDigest
	fixture.cleanup = cleanup
	return fixture
}

func signAdmission(t *testing.T, payload AdmissionPayload, privateKey ed25519.PrivateKey, keyID string) SignedAdmission {
	t.Helper()
	canonical, err := canonicalAdmissionPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	return SignedAdmission{Payload: payload, KeyID: keyID, Signature: hex.EncodeToString(ed25519.Sign(privateKey, receiptSignatureInput(KindAdmission, canonical)))}
}

func signQualification(t *testing.T, payload QualificationPayload, privateKey ed25519.PrivateKey, keyID string) SignedQualification {
	t.Helper()
	canonical, err := canonicalQualificationPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	return SignedQualification{Payload: payload, KeyID: keyID, Signature: hex.EncodeToString(ed25519.Sign(privateKey, receiptSignatureInput(KindQualification, canonical)))}
}

func signCleanup(t *testing.T, payload CleanupPayload, privateKey ed25519.PrivateKey, keyID string) SignedCleanup {
	t.Helper()
	canonical, err := canonicalCleanupPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	return SignedCleanup{Payload: payload, KeyID: keyID, Signature: hex.EncodeToString(ed25519.Sign(privateKey, receiptSignatureInput(KindCleanup, canonical)))}
}

func digestFor(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func rootBinding(id string) RootBinding {
	return RootBinding{
		Kind: "directory", OpaqueID: id, PathSHA256: digestFor("path:" + id), VolumeSerial: 7, FileID: "file-" + id,
		OwnerSIDHash: digestFor("owner:" + id), SecurityClass: "cq-private-user-system-admin-v1", DACL_SHA256: digestFor("dacl:" + id),
	}
}

func tokenBinding(id string, session uint32, elevation string) TokenBinding {
	return TokenBinding{OpaqueID: id, TokenUserSHA256: digestFor("user:" + id), LogonSIDHash: digestFor("logon:" + id), SessionID: session, ElevationType: elevation, IntegrityRID: 0x2000}
}

func fileBinding(id string, architecture Architecture) FileBinding {
	machine := uint16(0)
	if architecture == ArchitectureAMD64 {
		machine = 0x8664
	} else if architecture == ArchitectureARM64 {
		machine = 0xaa64
	}
	return FileBinding{
		OpaqueID: id, Size: int64(len(id) + 1), SHA256: digestFor("file:" + id), VolumeSerial: 7, FileID: "file-" + id,
		Links: 1, PEMachine: machine, SourceInventorySHA256: digestFor("source:" + id), OwnerSIDHash: digestFor("owner:" + id),
		SecurityClass: "cq-reviewed-binary-v1", DACL_SHA256: digestFor("dacl:" + id),
	}
}

func treeEntry(path string) TreeEntryBinding {
	return TreeEntryBinding{
		RelativePath: path, Size: int64(len(path) + 1), SHA256: digestFor("entry:" + path), VolumeSerial: 7, FileID: "entry-" + strings.ReplaceAll(path, "/", "-"),
		Links: 1, OwnerSIDHash: digestFor("owner:" + path), SecurityClass: "cq-reviewed-tree-v1", DACL_SHA256: digestFor("dacl:" + path),
	}
}

func readOnlyTreeEntry(path string) TreeEntryBinding {
	entry := treeEntry(path)
	entry.SecurityClass = "cq-read-only-tree-v1"
	return entry
}

func treeBinding(t *testing.T, id string, entries []TreeEntryBinding) TreeBinding {
	t.Helper()
	slices.SortFunc(entries, func(left, right TreeEntryBinding) int { return strings.Compare(left.RelativePath, right.RelativePath) })
	tree := TreeBinding{Root: rootBinding(id), Entries: entries}
	resealTree(t, &tree)
	return tree
}

func resealTree(t *testing.T, tree *TreeBinding) {
	t.Helper()
	tree.TotalSize = 0
	for _, entry := range tree.Entries {
		tree.TotalSize += entry.Size
	}
	canonical, err := canonicalJSON(tree.Entries)
	if err != nil {
		t.Fatal(err)
	}
	tree.ManifestSHA256 = receiptDigest(canonical)
}

func absentSDKVerifier() WindowsSDKLayoutVerifierBinding {
	return WindowsSDKLayoutVerifierBinding{
		CSourceSHA256: zeroSHA256, CompilerToolchainSHA256: zeroSHA256, WindowsSDKHeadersSHA256: zeroSHA256,
		BuildProvenanceSHA256: zeroSHA256, OutputSchemaSHA256: zeroSHA256,
	}
}

func absentNativeInteractive() NativeInteractiveInventory {
	return NativeInteractiveInventory{
		PrincipalInventorySHA256: zeroSHA256, SessionInventorySHA256: zeroSHA256, LauncherConfigurationSHA256: zeroSHA256,
	}
}

func absentProtectedInventory() ProtectedInventory {
	return ProtectedInventory{
		PrincipalInventorySHA256: zeroSHA256, SessionInventorySHA256: zeroSHA256, BrokerConfigurationSHA256: zeroSHA256,
		CodexManifestSHA256: zeroSHA256, EgressPolicySHA256: zeroSHA256,
	}
}

func validSDKVerifier(t *testing.T, harness TreeBinding, architecture Architecture) WindowsSDKLayoutVerifierBinding {
	t.Helper()
	var entry TreeEntryBinding
	for _, candidate := range harness.Entries {
		if candidate.RelativePath == "cq-windows-tcp-sdk-layout.exe" {
			entry = candidate
		}
	}
	file := fileBinding("sdk-layout", architecture)
	file.Size = entry.Size
	file.SHA256 = entry.SHA256
	file.VolumeSerial = entry.VolumeSerial
	file.FileID = entry.FileID
	file.Links = entry.Links
	file.ReparseTag = entry.ReparseTag
	file.OwnerSIDHash = entry.OwnerSIDHash
	file.SecurityClass = entry.SecurityClass
	file.DACL_SHA256 = entry.DACL_SHA256
	return WindowsSDKLayoutVerifierBinding{
		Present: true, Executable: OptionalFileBinding{Present: true, Value: file},
		CSourceSHA256: digestFor("c-source"), CompilerToolchainSHA256: digestFor("compiler"), WindowsSDKHeadersSHA256: digestFor("sdk"),
		BuildProvenanceSHA256: digestFor("provenance"), OutputSchemaSHA256: digestFor("schema"),
	}
}

func controllerBinding(role, elevation string, session uint32) ControllerGeneration {
	return ControllerGeneration{
		Role: role, OpaqueID: "controller-" + role, PID: 42, CreationTime: 100,
		Token: tokenBinding("controller-"+role, session, elevation), Profile: rootBinding(role + "-profile"),
		RoamingAppData: rootBinding(role + "-roaming"), LocalAppData: rootBinding(role + "-local"),
	}
}

func completeAbsenceSet() AbsenceSet {
	return AbsenceSet{true, true, true, true, true, true, true, true, true, true, true, true, true}
}

func abortedCleanup(t *testing.T, fixture receiptFixture) SignedCleanup {
	t.Helper()
	payload := fixture.cleanup.Payload
	payload.Chain.PreviousReceiptSHA256 = fixture.admissionDigest
	payload.Outcome = "aborted"
	payload.QualificationSHA256 = zeroSHA256
	return signCleanup(t, payload, fixture.privateKey, fixture.trust.KeyID)
}

func validBrokerTranscript(t *testing.T, capability Capability) BrokerTranscript {
	t.Helper()
	var prompts []BrokerPromptEvidence
	unique := 0
	for _, role := range []string{"limited-admin", "standard-user"} {
		if capability == CapabilityGate4SourceProtected {
			prompts = append(prompts, brokerPrompt(role, "installed-codex-http", 0, unique))
			unique++
			for ordinal := range 7 {
				prompts = append(prompts, brokerPrompt(role, "installed-confinement", ordinal, unique))
				unique++
			}
		} else {
			for ordinal := range 8 {
				prompts = append(prompts, brokerPrompt(role, "release-protected-installed", ordinal, unique))
				unique++
			}
		}
	}
	return BrokerTranscript{
		Schema: 1, AdmissionSHA256: digestFor("admission"), HandoffSHA256: digestFor("handoff"),
		BrokerProtocolVersion: 1, Prompts: prompts,
	}
}

func brokerPrompt(role, caseID string, ordinal, unique int) BrokerPromptEvidence {
	requested := time.Date(2026, 8, 29, 10, 0, unique, 0, time.UTC)
	promptKind := "credential"
	if role == "limited-admin" {
		promptKind = "consent"
	}
	return BrokerPromptEvidence{
		CaseID: caseID, LaunchOrdinal: uint32(ordinal), ControllerRole: role, ControllerOpaqueID: "controller-" + role,
		ControllerPID: 42, ControllerCreationTime: 100, Helper: fileBinding(fmt.Sprintf("helper-%02d", unique), ArchitectureAMD64),
		HelperPID: uint32(100 + unique), HelperCreationTime: uint64(200 + unique), HelperToken: tokenBinding(fmt.Sprintf("helper-%02d", unique), 1, "Full"),
		PromptNonce: fmt.Sprintf("%032x", unique+1), PromptKind: promptKind, RequestedAt: requested.Format(time.RFC3339), DecidedAt: requested.Add(time.Second).Format(time.RFC3339),
		RequestCount: 1, DecisionCount: 1, SecureDesktop: true, RunAsVerb: true, Decision: "approved",
	}
}
