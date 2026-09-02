package proxy

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

func TestServerNativeCodexOwnsOnlyRoutePolicy(t *testing.T) {
	t.Parallel()

	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "server.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	handler := findCodexNativeServerHandler(parsed)
	if handler == nil {
		t.Fatal("Server.handleNativeCodex not found")
	}

	assertCodexNativeStatementShape(t, fileSet, handler.Body.List, []string{
		"start := time.Now()",
		"var model string",
		"sessionKey, sessionSource := sessionCorrelation(r.Header)",
		"ctx := withCodexTrace(r.Context(), s.Diag, s.PayloadDiag, CodexTraceStart{\n\tTransport: \"http\", SessionKey: sessionKey, SessionSource: sessionSource,\n})",
		"ctx, routeDiag := withRouteDiagnostics(ctx)",
		"ctx = s.withCodexRequestIngressObservation(ctx, r.Method, r.URL.Path, \"codex_native_ingress\")",
		"r = r.WithContext(ctx)",
		"emitCodexTrace(ctx, CodexTraceEvent{\n\tPhase: \"ingress\", Outcome: \"accepted\", Method: r.Method, Path: r.URL.Path,\n})",
		"if wrapped, rec := s.wrapDiagnosticsResponseWriter(w); rec != nil",
		"if s.CodexNativeHTTP != nil",
		"legacy := s.codexLegacyNativeHTTP",
		"if legacy == nil",
		"model = legacy.Handle(w, r)",
	})

	diagnostics := requireCodexNativeIf(t, handler.Body.List[8])
	assertCodexNativeStatementShape(t, fileSet, diagnostics.Body.List, []string{
		"w = wrapped",
		"defer func literal",
	})
	deferred, ok := diagnostics.Body.List[1].(*ast.DeferStmt)
	if !ok {
		t.Fatalf("diagnostics statement 2 = %T, want defer", diagnostics.Body.List[1])
	}
	deferredFunction, ok := deferred.Call.Fun.(*ast.FuncLit)
	if !ok {
		t.Fatalf("diagnostics defer = %T, want function literal", deferred.Call.Fun)
	}
	assertCodexNativeStatementShape(t, fileSet, deferredFunction.Body.List, []string{
		"emitCodexTrace(ctx, CodexTraceEvent{\n\tPhase: \"terminal\", Outcome: codexTraceHTTPOutcome(rec.statusCode()),\n\tStatusCode: rec.statusCode(), LatencyMS: time.Since(start).Milliseconds(),\n\tReason: rec.diagnosticsError(),\n})",
		"event := RouteEvent{...}",
		"event.applyRouteDiagnostics(routeDiag)",
		"event.applySessionCorrelation(r.Header)",
		"event.applyCodexTrace(ctx)",
		"s.emitDiagnostics(event)",
	})

	authoritative := requireCodexNativeIf(t, handler.Body.List[9])
	assertCodexNativeStatementShape(t, fileSet, authoritative.Body.List, []string{
		"if handled, routedModel := s.CodexNativeHTTP.TryServe(w, r, false); handled",
	})
	claimed := requireCodexNativeIf(t, authoritative.Body.List[0])
	assertCodexNativeStatementShape(t, fileSet, claimed.Body.List, []string{
		"model = routedModel",
		"return",
	})

	fallback := requireCodexNativeIf(t, handler.Body.List[11])
	assertCodexNativeStatementShape(t, fileSet, fallback.Body.List, []string{
		"legacy = newLegacyCodexNativeHTTPHandler(s)",
	})

	wantCalls := map[string]int{
		"codexTraceHTTPOutcome":                1,
		"emitCodexTrace":                       2,
		"event.applyCodexTrace":                1,
		"func literal":                         1,
		"event.applyRouteDiagnostics":          1,
		"event.applySessionCorrelation":        1,
		"legacy.Handle":                        1,
		"newLegacyCodexNativeHTTPHandler":      1,
		"r.Context":                            1,
		"r.WithContext":                        1,
		"rec.diagnosticsError":                 2,
		"rec.statusCode":                       3,
		"sessionCorrelation":                   1,
		"s.CodexNativeHTTP.TryServe":           1,
		"s.emitDiagnostics":                    1,
		"s.withCodexRequestIngressObservation": 1,
		"s.wrapDiagnosticsResponseWriter":      1,
		"start.UTC":                            1,
		"time.Now":                             1,
		"time.Since":                           2,
		"time.Since(start).Milliseconds":       2,
		"withCodexTrace":                       1,
		"withRouteDiagnostics":                 1,
	}
	gotCalls := make(map[string]int)
	ast.Inspect(handler.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok {
			gotCalls[codexNativeCallName(fileSet, call)]++
		}
		return true
	})
	for name, got := range gotCalls {
		if want := wantCalls[name]; got != want {
			t.Errorf("Server.handleNativeCodex call %q count = %d, want %d", name, got, want)
		}
	}
	for name, want := range wantCalls {
		if got := gotCalls[name]; got != want {
			t.Errorf("Server.handleNativeCodex call %q count = %d, want %d", name, got, want)
		}
	}
}

func findCodexNativeServerHandler(file *ast.File) *ast.FuncDecl {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == "handleNativeCodex" && function.Recv != nil {
			return function
		}
	}
	return nil
}

func requireCodexNativeIf(t *testing.T, statement ast.Stmt) *ast.IfStmt {
	t.Helper()
	condition, ok := statement.(*ast.IfStmt)
	if !ok {
		t.Fatalf("statement = %T, want if", statement)
	}
	if condition.Else != nil {
		t.Fatal("Server.handleNativeCodex policy if gained an else branch")
	}
	return condition
}

func assertCodexNativeStatementShape(t *testing.T, fileSet *token.FileSet, statements []ast.Stmt, want []string) {
	t.Helper()
	got := make([]string, len(statements))
	for index, statement := range statements {
		got[index] = codexNativeStatementShape(fileSet, statement)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Server.handleNativeCodex statement shape:\n got: %q\nwant: %q", got, want)
	}
}

func codexNativeStatementShape(fileSet *token.FileSet, statement ast.Stmt) string {
	switch statement := statement.(type) {
	case *ast.AssignStmt:
		return codexNativeExpressionList(fileSet, statement.Lhs) + " " + statement.Tok.String() + " " + codexNativeExpressionList(fileSet, statement.Rhs)
	case *ast.DeclStmt:
		return codexNativeNodeText(fileSet, statement.Decl)
	case *ast.DeferStmt:
		if _, ok := statement.Call.Fun.(*ast.FuncLit); ok {
			return "defer func literal"
		}
		return "defer " + codexNativeNodeText(fileSet, statement.Call)
	case *ast.ExprStmt:
		return codexNativeNodeText(fileSet, statement.X)
	case *ast.IfStmt:
		prefix := "if "
		if statement.Init != nil {
			prefix += codexNativeStatementShape(fileSet, statement.Init) + "; "
		}
		return prefix + codexNativeNodeText(fileSet, statement.Cond)
	case *ast.ReturnStmt:
		if len(statement.Results) == 0 {
			return "return"
		}
		return "return " + codexNativeExpressionList(fileSet, statement.Results)
	default:
		return codexNativeNodeText(fileSet, statement)
	}
}

func codexNativeExpressionList(fileSet *token.FileSet, expressions []ast.Expr) string {
	parts := make([]string, len(expressions))
	for index, expression := range expressions {
		if composite, ok := expression.(*ast.CompositeLit); ok {
			parts[index] = codexNativeNodeText(fileSet, composite.Type) + "{...}"
			continue
		}
		parts[index] = codexNativeNodeText(fileSet, expression)
	}
	return strings.Join(parts, ", ")
}

func codexNativeCallName(fileSet *token.FileSet, call *ast.CallExpr) string {
	if _, ok := call.Fun.(*ast.FuncLit); ok {
		return "func literal"
	}
	return codexNativeNodeText(fileSet, call.Fun)
}

func codexNativeNodeText(fileSet *token.FileSet, node any) string {
	var output bytes.Buffer
	if err := format.Node(&output, fileSet, node); err != nil {
		return "<invalid AST>"
	}
	return output.String()
}
