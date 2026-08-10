package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

const codexCaptureInputLimit = 10 << 20

var runCodexHTTPReadinessEvidenceFn = func(ctx context.Context) (proxy.CodexHTTPReadinessEvidence, error) {
	acceptance, err := proxy.RunCodexHTTPInstalledAcceptance(ctx)
	return proxy.CodexHTTPReadinessEvidence{
		Source:     proxy.CodexHTTPReadinessEvidenceSynthetic,
		Acceptance: acceptance,
	}, err
}

func runCodexValidate(args []string) error {
	if len(args) == 0 || helpRequested(args) {
		fmt.Fprintln(os.Stdout, "Usage: cq codex validate capture ... | http --client-build BUILD [--state-dir DIR]")
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
	var clientBuild, stateDir string
	for index := 0; index < len(args); index++ {
		if index+1 >= len(args) {
			return fmt.Errorf("%s requires a value", args[index])
		}
		value := args[index+1]
		switch args[index] {
		case "--client-build":
			clientBuild = value
		case "--state-dir":
			stateDir = value
		default:
			return fmt.Errorf("unknown Codex HTTP validation argument: %s", args[index])
		}
		index++
	}
	clientBuild = strings.TrimSpace(clientBuild)
	if clientBuild == "" {
		return fmt.Errorf("Codex HTTP validation requires --client-build")
	}
	acceptanceCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	evidence, err := runCodexHTTPReadinessEvidenceFn(acceptanceCtx)
	if err != nil {
		return fmt.Errorf("Codex HTTP installed-listener validation failed: %w", err)
	}
	required, _ := proxy.DefaultCodexRoutingRequirements(version, clientBuild)
	marker, err := proxy.BuildCodexHTTPReadinessMarker(evidence, required, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("Codex HTTP validation failed: %w", err)
	}
	var writeErr error
	if strings.TrimSpace(stateDir) == "" {
		writeErr = proxy.SaveDefaultCodexReadinessMarker(marker)
	} else {
		writeErr = proxy.SaveCodexReadinessMarker(stateDir, marker)
	}
	if writeErr != nil {
		return fmt.Errorf("write Codex HTTP readiness marker: %w", writeErr)
	}
	fmt.Fprintf(os.Stdout, "Codex HTTP installed listener passed: %d turns, %d requests\n", evidence.Acceptance.Turns, evidence.Acceptance.Requests)
	fmt.Fprintln(os.Stdout, "Codex HTTP enforcement readiness recorded; restart CQ to apply")
	return nil
}
