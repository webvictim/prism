package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strconv"
	"strings"
	"testing"
)

// Subcommands deliberately kept out of the usage text: internal entry
// points, and flag aliases of a command that is already listed.
var undocumentedSubcommands = map[string]bool{
	"__daemon":  true, // internal; mentioned on its own line
	"--version": true, // alias of `prism version`
	"-v":        true,
	"help":      true, // printing the help doesn't need to advertise itself
	"--help":    true,
	"-h":        true,
}

// prism dispatches on os.Args[1], and the usage text is maintained by
// hand, so the two can drift: `prism pi config` shipped for several
// releases without ever appearing in the help. Every case a user is
// meant to type must show up in the usage output.
func TestEverySubcommandIsDocumented(t *testing.T) {
	names := dispatchedSubcommands(t)
	if len(names) == 0 {
		t.Fatal("no subcommand cases found in main.go — has the dispatch switch moved?")
	}

	var buf bytes.Buffer
	writeUsage(&buf)
	help := buf.String()

	for _, name := range names {
		if undocumentedSubcommands[name] {
			continue
		}
		if !strings.Contains(help, "prism "+name) {
			t.Errorf("subcommand %q is dispatched but missing from the usage text", name)
		}
	}
}

// The inverse: a command advertised in the help has to actually exist,
// so a rename can't leave the help pointing at nothing.
func TestUsageOnlyListsRealSubcommands(t *testing.T) {
	dispatched := make(map[string]bool)
	for _, name := range dispatchedSubcommands(t) {
		dispatched[name] = true
	}

	var buf bytes.Buffer
	writeUsage(&buf)

	for _, line := range strings.Split(buf.String(), "\n") {
		// Command entries are indented under "Usage:"; the unindented
		// first line is the banner, where the second field is the
		// version rather than a subcommand.
		if !strings.HasPrefix(line, "  prism ") {
			continue
		}
		name := strings.Fields(line)[1]
		if !dispatched[name] {
			t.Errorf("usage text lists %q, which no case in main.go dispatches", name)
		}
	}
}

// dispatchedSubcommands returns the case literals of main's dispatch
// switch, read from the source so the test can't go stale.
func dispatchedSubcommands(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var names []string
	ast.Inspect(file, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || sw.Tag == nil {
			return true
		}
		// The dispatch switch is the one keyed on os.Args.
		if !strings.Contains(types.ExprString(sw.Tag), "os.Args") {
			return true
		}
		for _, stmt := range sw.Body.List {
			clause, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, expr := range clause.List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				name, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote case %s: %v", lit.Value, err)
				}
				names = append(names, name)
			}
		}
		return true
	})
	return names
}
