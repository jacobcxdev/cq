package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

const codexCaptureInputLimit = 10 << 20

func runCodexValidate(args []string) error {
	if len(args) == 0 || helpRequested(args) {
		fmt.Fprintln(os.Stdout, "Usage: cq codex validate capture --input PATH --output PATH [--content-encoding ENCODING] [--metadata JSON]")
		return nil
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
