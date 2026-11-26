package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsServiceCommandsUseConsoleSuppression(t *testing.T) {
	const probe = "CQ_TEST_SERVICE_CONSOLE_PROBE"
	if os.Getenv(probe) == "1" {
		window, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
		if window != 0 && windows.IsWindowVisible(windows.HWND(window)) {
			t.Fatal("Windows service command opened a visible console")
		}
		fmt.Println("service console probe passed")
		return
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "service_windows.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		isCommandBuilder := isFunction && function.Name.Name == "windowsServiceCommand"
		ast.Inspect(declaration, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			target, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := target.X.(*ast.Ident)
			if !ok {
				return true
			}
			if pkg.Name == "exec" && (target.Sel.Name == "Command" || target.Sel.Name == "CommandContext") && !isCommandBuilder {
				t.Errorf("%s starts a Windows service command without the console-suppressed builder", fset.Position(call.Pos()))
			}
			return true
		})
	}
	t.Setenv(probe, "1")
	command := windowsServiceCommand(t.Context(), os.Args[0], "-test.run=^TestWindowsServiceCommandsUseConsoleSuppression$")
	if command.SysProcAttr == nil || command.SysProcAttr.CreationFlags != windows.CREATE_NO_WINDOW {
		t.Fatal("Windows service command builder does not suppress child consoles")
	}
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "service console probe passed") {
		t.Fatalf("service console probe failed: %v\n%s", err, output)
	}
}

func TestWindowsServicePowerShellKeepsPipedOutput(t *testing.T) {
	output, err := runWindowsTaskPowerShell(t.Context(), "Write-Output 'service probe'")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(output)) != "service probe" {
		t.Fatalf("PowerShell output = %q, want service probe", output)
	}
}
