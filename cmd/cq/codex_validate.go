package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

const codexCaptureInputLimit = 10 << 20

func runCodexValidate(args []string) error {
	if len(args) == 0 || helpRequested(args) {
		fmt.Fprintln(os.Stdout, "Usage: cq codex validate capture ... | http --client-build BUILD --fixture-hash HASH --installed-result passed --gate NAME [...]")
		return nil
	}
	if args[0] == "http" {
		return runCodexHTTPValidation(args[1:])
	}
	if args[0] != "capture" {
		return fmt.Errorf("unknown Codex validation command: %s", args[0])
	}
	var inputPath, outputPath, contentEncoding, metadata string
	for index := 1; index < len(args); index++ {
		if index+1 >= len(args) {
			return fmt.Errorf("%s requires a value", args[index])
		}
		value := args[index+1]
		switch args[index] {
		case "--input":
			inputPath = value
		case "--output":
			outputPath = value
		case "--content-encoding":
			contentEncoding = value
		case "--metadata":
			metadata = value
		default:
			return fmt.Errorf("unknown Codex capture argument: %s", args[index])
		}
		index++
	}
	if inputPath == "" || outputPath == "" {
		return fmt.Errorf("Codex capture requires --input and --output")
	}
	input, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("open Codex capture input: %w", err)
	}
	defer input.Close()
	body, err := io.ReadAll(io.LimitReader(input, codexCaptureInputLimit+1))
	if err != nil {
		return fmt.Errorf("read Codex capture input: %w", err)
	}
	if len(body) > codexCaptureInputLimit {
		return fmt.Errorf("Codex capture input exceeds 10 MiB")
	}
	fixture, err := proxy.BuildSanitisedCodexFixture(body, contentEncoding, metadata, time.Now())
	if err != nil {
		return fmt.Errorf("sanitise Codex capture: %w", err)
	}
	if err := proxy.WriteSanitisedCodexFixture(outputPath, fixture); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Wrote sanitised Codex fixture to %s\n", outputPath)
	return nil
}

func runCodexHTTPValidation(args []string) error {
	var clientBuild, fixtureHash, installedResult, stateDir string
	var gates []string
	for index := 0; index < len(args); index++ {
		if index+1 >= len(args) {
			return fmt.Errorf("%s requires a value", args[index])
		}
		value := args[index+1]
		switch args[index] {
		case "--client-build":
			clientBuild = value
		case "--fixture-hash":
			fixtureHash = value
		case "--installed-result":
			installedResult = value
		case "--gate":
			gates = append(gates, value)
		case "--state-dir":
			stateDir = value
		default:
			return fmt.Errorf("unknown Codex HTTP validation argument: %s", args[index])
		}
		index++
	}
	required, _ := proxy.DefaultCodexRoutingRequirements(version, clientBuild)
	marker := proxy.CodexReadinessMarker{
		Version:         proxy.CodexReadinessMarkerVersion,
		Transport:       proxy.CodexRoutingHTTP,
		CQBuild:         version,
		ParserSchema:    proxy.CurrentCodexParserSchema,
		LeaseSchema:     proxy.CurrentCodexLeaseSchema,
		ClientBuild:     clientBuild,
		RetryBudget:     required.RetryBudget,
		FixtureHash:     fixtureHash,
		InstalledResult: installedResult,
		CompletedGates:  gates,
		ValidatedAt:     time.Now().UTC(),
	}
	if err := proxy.ValidateCodexReadinessMarker(marker, required); err != nil {
		return fmt.Errorf("Codex HTTP validation failed: %w", err)
	}
	var err error
	if strings.TrimSpace(stateDir) == "" {
		err = proxy.SaveDefaultCodexReadinessMarker(marker)
	} else {
		err = proxy.SaveCodexReadinessMarker(stateDir, marker)
	}
	if err != nil {
		return fmt.Errorf("write Codex HTTP readiness marker: %w", err)
	}
	fmt.Fprintln(os.Stdout, "Codex HTTP enforcement readiness recorded; restart CQ to apply")
	return nil
}
