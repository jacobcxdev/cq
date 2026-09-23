//go:build darwin

package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jacobcxdev/cq/internal/proxy"
)

// This mode deliberately has distinct argv and never produces historical CU
// acceptance. Frozen release evidence only accepts the original CU-ID argv.
func runCLIV2Compatibility(repositoryRoot string) (resultErr error) {
	selection, err := cliV2CompatibilitySelection(repositoryRoot)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(selection)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "CLI-v2 compatibility only; not historical CU acceptance")
	for _, id := range []string{"CU-0", "CU-1"} {
		original, err := proxy.CanonicalCUManifestV1(id)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "%s original manifest SHA256: %x\n", id, sha256.Sum256(original))
	}
	fmt.Fprintf(os.Stdout, "CLI-v2 replacement roster SHA256: %x\n", sha256.Sum256(encoded))
	runner, cleanup, err := newOSCommandRunner(repositoryRoot)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, cleanup()) }()
	return verifyUnit(repositoryRoot, proxy.CUManifestV1{RaceCount: 1, Packages: []proxy.CUTestPackageV1{selection}}, runner)
}

func cliV2CompatibilitySelection(repositoryRoot string) (proxy.CUTestPackageV1, error) {
	raw, err := os.ReadFile(filepath.Join(repositoryRoot, "specs/cli-v2/commands.json"))
	if err != nil {
		return proxy.CUTestPackageV1{}, err
	}
	var catalogue struct{ Commands []struct{ Path, Kind string } }
	if err := json.Unmarshal(raw, &catalogue); err != nil {
		return proxy.CUTestPackageV1{}, err
	}
	selection := proxy.CUTestPackageV1{Package: "./cmd/cq", TopLevelTests: []string{
		"TestCLIV2CompleteRegistry", "TestCLIV2EveryGroupIsPure", "TestCLIV2IntegratedHelpAndInvalidOptionsBeforeIO", "TestCLIV2IntegratedReservedCommands", "TestCLIV2ProxyStatusFacts",
	}}
	selection.FullTestIDs = append(selection.FullTestIDs, selection.TopLevelTests...)
	selection.FullTestIDs = append(selection.FullTestIDs, "TestCLIV2IntegratedHelpAndInvalidOptionsBeforeIO/#00")
	leaves, groups := 0, 0
	for _, command := range catalogue.Commands {
		name := strings.ReplaceAll(command.Path, " ", "_")
		selection.FullTestIDs = append(selection.FullTestIDs, "TestCLIV2IntegratedHelpAndInvalidOptionsBeforeIO/"+name)
		if command.Kind == "group" {
			groups++
			selection.FullTestIDs = append(selection.FullTestIDs, "TestCLIV2EveryGroupIsPure/"+name)
		} else if command.Kind == "command" {
			leaves++
		} else {
			return selection, fmt.Errorf("unknown catalogue kind")
		}
	}
	if leaves != 89 || groups != 37 {
		return selection, fmt.Errorf("CLI-v2 roster requires 89 leaves and 37 groups")
	}
	for _, state := range []string{"absent", "stopped", "indeterminate", "unhealthy"} {
		selection.FullTestIDs = append(selection.FullTestIDs, "TestCLIV2ProxyStatusFacts/"+state)
	}
	sort.Strings(selection.TopLevelTests)
	sort.Strings(selection.FullTestIDs)
	selection.MinimumPassCount = len(selection.FullTestIDs)
	return selection, nil
}
