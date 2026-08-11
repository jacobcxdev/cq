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

var runCodexInstalledWebSocketValidationFn = proxy.RunCodexInstalledWebSocketValidation

func runCodexValidate(args []string) error {
	if len(args) == 0 || helpRequested(args) {
		fmt.Fprintln(os.Stdout, "Usage: cq codex validate capture ... | http --client-build BUILD [--state-dir DIR] | websocket --client-build BUILD [--client-executable PATH] [--state-dir DIR]")
		return nil
	}
	if args[0] == "http" {
		return runCodexHTTPValidation(args[1:])
	}
	if args[0] == "websocket" {
		return runCodexWebSocketValidation(args[1:])
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

func runCodexWebSocketValidation(args []string) error {
	var clientBuild, clientExecutable, stateDir string
	for index := 0; index < len(args); index++ {
		if index+1 >= len(args) {
			return fmt.Errorf("%s requires a value", args[index])
		}
		value := args[index+1]
		switch args[index] {
		case "--client-build":
			clientBuild = value
		case "--client-executable":
			clientExecutable = value
		case "--state-dir":
			stateDir = value
		default:
			return fmt.Errorf("unknown Codex WebSocket validation argument: %s", args[index])
		}
		index++
	}
	clientBuild = strings.TrimSpace(clientBuild)
	if clientBuild == "" {
		return fmt.Errorf("Codex WebSocket validation requires --client-build")
	}
	marker, err := runCodexInstalledWebSocketValidationFn(context.Background(), version, clientBuild, clientExecutable, stateDir)
	if err != nil {
		return fmt.Errorf("Codex WebSocket isolated validation failed: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Codex WebSocket isolated readiness recorded for %s (validated %s)\n", marker.ClientBuild, marker.ValidatedAt.UTC().Format(time.RFC3339))
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
	required, _ := proxy.DefaultCodexRoutingRequirements(version, clientBuild)
	var marker proxy.CodexReadinessMarker
	var err error
	if strings.TrimSpace(stateDir) == "" {
		marker, err = proxy.LoadDefaultCodexReadinessMarker(proxy.CodexRoutingHTTP)
	} else {
		marker, err = proxy.LoadCodexReadinessMarker(stateDir, proxy.CodexRoutingHTTP)
	}
	if err != nil {
		return fmt.Errorf("Codex HTTP readiness is unavailable; run the installed service in explicit startup validation mode: %w", err)
	}
	if err := proxy.ValidateCodexReadinessMarker(marker, required); err != nil {
		return fmt.Errorf("Codex HTTP readiness marker is not current: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Codex HTTP installed-listener readiness is recorded for %s (validated %s)\n", marker.ClientBuild, marker.ValidatedAt.UTC().Format(time.RFC3339))
	return nil
}
