package executil

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsPipeCommandHasNoConsole(t *testing.T) {
	const probe = "CQ_TEST_PIPE_CONSOLE_PROBE"
	if os.Getenv(probe) == "1" {
		if cp, _ := windows.GetConsoleCP(); cp != 0 {
			t.Fatalf("pipe-only child has a console with code page %d", cp)
		}
		fmt.Println("console probe reached stdout")
		return
	}
	t.Setenv(probe, "1")
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWindowsPipeCommandHasNoConsole$")
	SuppressConsoleWindow(command)
	output, err := command.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("console probe reached stdout")) {
		t.Fatalf("pipe-only console probe failed: %v\n%s", err, output)
	}
}
