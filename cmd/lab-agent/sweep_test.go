package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/agent"
)

// The spec's table of `lab-agent` (section 10) and its Unchanged list.
// "The running agent" is what the agent's own units start.
var specTable = map[string]string{
	"update":             "added",
	"boot-macos-once":    "added",
	"performance":        "added",
	"setup":              "changed",
	"move":               "changed",
	"leave":              "changed",
	"status":             "changed",
	"version":            "changed",
	"window":             "unchanged",
	"pause":              "unchanged",
	"open":               "unchanged",
	"notify":             "unchanged",
	"access":             "unchanged",
	"publish":            "unchanged",
	"log":                "unchanged",
	"jobs":               "unchanged",
	"uninstall":          "unchanged",
	"preflight":          "unchanged",
	"known-good":         "unchanged",
	"sim":                "unchanged",
	"run":                "the running agent",
	"lab-boot-session":   "the running agent",
	"lab-boot-lock":      "the running agent",
	"lab-boot-deadlines": "the running agent",
}

var helpCommandRE = regexp.MustCompile(`(?m)^  (?:sudo )?lab-agent ([a-z][a-z-]*)`)

// `lab-agent help` lists the table's commands and no other, and main runs
// exactly those.
func TestLabAgentsCommandsAreTheSpecsTable(t *testing.T) {
	var want []string
	for name := range specTable {
		want = append(want, name)
	}
	sort.Strings(want)

	seen := map[string]bool{}
	for _, m := range helpCommandRE.FindAllStringSubmatch(usage, -1) {
		seen[m[1]] = true
	}
	var listed []string
	for name := range seen {
		listed = append(listed, name)
	}
	sort.Strings(listed)
	if !slices.Equal(listed, want) {
		t.Errorf("lab-agent help lists\n  %v\nthe spec's table has\n  %v", listed, want)
	}

	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	runs := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}
		if fn.Name.Name != "main" {
			return false
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range clause.List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					name, _ := strconv.Unquote(lit.Value)
					runs[name] = true
				}
			}
			return true
		})
		return false
	})
	var got []string
	for name := range runs {
		got = append(got, name)
	}
	for _, name := range want {
		if agent.IsVolunteerCommand(name) && !runs[name] {
			got = append(got, name)
		}
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("lab-agent runs\n  %v\nthe spec's table has\n  %v", got, want)
	}
	for _, name := range []string{"volunteer", "tailscale", "enroll", "forget", "terms", "accept-terms", "update-all", "console"} {
		if agent.IsVolunteerCommand(name) || runs[name] {
			t.Errorf("lab-agent %s is a command", name)
		}
	}
}

// What lab-agent says of itself names the product, and the retired ways in
// are gone from it.
func TestLabAgentHelpNamesOmarchyMacLab(t *testing.T) {
	if !strings.Contains(usage, "Omarchy Mac Lab") {
		t.Error("lab-agent help does not name Omarchy Mac Lab")
	}
	for _, gone := range []string{"./volunteer", "test lab"} {
		if strings.Contains(usage, gone) {
			t.Errorf("lab-agent help still says %q", gone)
		}
	}
	if !strings.Contains(usage, "--token-stdin") {
		t.Error("lab-agent help lost setup's token on standard input")
	}
}
