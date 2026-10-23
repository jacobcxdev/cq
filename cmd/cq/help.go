package main

import (
	"fmt"
	"github.com/jacobcxdev/cq/internal/cli"
	"io"
	"strings"
)

// Retained domain engines may request help; use the single public catalogue.
func manualHelp(path []string) (string, bool) {
	if text, ok := cli.Help(strings.Join(path, " ")); ok {
		return text, true
	}
	inv, err := cli.Parse(append(append([]string{}, path...), "--help"))
	if err != nil {
		return "", false
	}
	return cli.Help(inv.Path)
}
func writeManualHelp(w io.Writer, path []string) error {
	text, ok := manualHelp(path)
	if !ok {
		return fmt.Errorf("no help for command path: %s", strings.Join(path, " "))
	}
	_, err := io.WriteString(w, text)
	return err
}
func helpRequested(args []string) bool {
	for _, arg := range args {
		if isHelpToken(arg) {
			return true
		}
	}
	return false
}
func isHelpToken(arg string) bool { return arg == "--help" || arg == "-h" || arg == "help" }
