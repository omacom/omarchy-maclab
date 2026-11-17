package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The spec's table of `lab` (section 10), one line per row, and its
// Unchanged list. A row that names two commands is two entries here.
var specTable = map[string]string{
	"version":                   "added",
	"installed list":            "added",
	"installed verdict":         "added",
	"agent-update":              "added",
	"login":                     "changed",
	"exec":                      "changed",
	"installed freeze-entry":    "changed",
	"wait":                      "changed",
	"build-log":                 "changed",
	"upload":                    "changed",
	"lease":                     "changed",
	"release":                   "changed",
	"cancel":                    "changed",
	"status":                    "changed",
	"oob":                       "changed",
	"job":                       "changed",
	"jobs":                      "changed",
	"publish":                   "changed",
	"run":                       "changed",
	"installed status":          "changed",
	"installed promote":         "changed",
	"build":                     "changed",
	"package":                   "changed",
	"enroll":                    "changed",
	"remove":                    "changed",
	"device":                    "changed",
	"reset":                     "changed",
	"mcp":                       "changed",
	"baseline":                  "unchanged",
	"crashtest":                 "unchanged",
	"logs":                      "unchanged",
	"retry":                     "unchanged",
	"gate":                      "unchanged",
	"exclude":                   "unchanged",
	"builds":                    "unchanged",
	"build-get":                 "unchanged",
	"pack":                      "unchanged",
	"audit":                     "unchanged",
	"installed freeze-manifest": "unchanged",
	"builder add":               "unchanged",
	"builder remove":            "unchanged",
	"builder list":              "unchanged",
	"boot-test":                 "unchanged",
}

// The table's retired rows.
var specRetired = []string{"shell", "console", "serial", "recovery-mode", "forget", "maintainer"}

// What the spec keeps "on the site only": no command, flag or tool is named
// for any of it.
var siteOnly = []string{"people", "person", "request", "requests", "lend", "invite", "invites", "block", "unblock",
	"end-all-access", "token", "tokens", "revoke-token", "configuration", "override", "pause", "limit", "renew",
	"hide", "show", "delete", "maintainer", "maintainers", "forget"}

// The commands with no tool, and why: the Lab token is typed by its person
// and never handed to an agent; `lab mcp` is the server itself; `pack` and
// `boot-test` work on files of the person's own machine and ask the lab
// nothing.
var noTool = []string{"login", "mcp", "pack", "boot-test"}

func tableCommands() (top []string, subs map[string][]string) {
	subs = map[string][]string{}
	seen := map[string]bool{}
	for row := range specTable {
		name, sub, has := strings.Cut(row, " ")
		if !seen[name] {
			seen[name] = true
			top = append(top, name)
		}
		if has {
			subs[name] = append(subs[name], sub)
		}
	}
	sort.Strings(top)
	for _, l := range subs {
		sort.Strings(l)
	}
	return top, subs
}

var helpCommandRE = regexp.MustCompile(`(?m)(?:^  |\| )lab ([a-z][a-z-]*)`)

// `lab help` lists the table's commands and no other.
func TestLabHelpIsTheSpecsTable(t *testing.T) {
	want, subs := tableCommands()
	seen := map[string]bool{}
	for _, m := range helpCommandRE.FindAllStringSubmatch(usage, -1) {
		seen[m[1]] = true
	}
	var got []string
	for name := range seen {
		got = append(got, name)
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("lab help lists\n  %v\nthe spec's table has\n  %v", got, want)
	}
	for name, list := range subs {
		for _, sub := range list {
			if !regexp.MustCompile(`lab ` + name + `[^\n]*\b` + sub + `\b`).MatchString(usage) {
				t.Errorf("lab help does not list lab %s %s", name, sub)
			}
		}
	}
	for _, name := range specRetired {
		if seen[name] {
			t.Errorf("lab help lists the retired lab %s", name)
		}
	}
	if !strings.Contains(usage, "Omarchy Mac Lab") {
		t.Error("lab help does not name Omarchy Mac Lab")
	}
}

// Every command of the table runs, and no other word does: a retired one
// answers its sentence, anything else is unknown, and neither asks the lab.
func TestTheCommandsAreTheSpecsTable(t *testing.T) {
	want, subs := tableCommands()
	got := slices.Clone(commands)
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Fatalf("lab's commands are\n  %v\nthe spec's table has\n  %v", got, want)
	}
	asked := 0
	c := labAt(t, func(w http.ResponseWriter, r *http.Request) {
		asked++
		http.Error(w, "the test's lab answers nothing", 500)
	})
	ctx := context.Background()
	for _, name := range want {
		if slices.Contains(atHome, name) {
			continue
		}
		if err := dispatch(ctx, c, name, nil); err == nil || strings.Contains(err.Error(), "unknown") {
			t.Errorf("lab %s says %v", name, err)
		}
		for _, sub := range subs[name] {
			args := []string{sub}
			if name == "installed" {
				args = append(args, "--release", "R")
			}
			if err := dispatch(ctx, c, name, args); err == nil || strings.Contains(err.Error(), "unknown") {
				t.Errorf("lab %s %s says %v", name, sub, err)
			}
		}
	}
	for _, name := range atHome {
		if !slices.Contains(want, name) {
			t.Errorf("lab %s is run by main itself and is not in the spec's table", name)
		}
	}
	asked = 0
	for _, name := range append([]string{"devices", "oob-status", "installed-list"}, siteOnly...) {
		if known(name) && !slices.Contains(specRetired, name) {
			t.Errorf("lab %s is a command", name)
		}
		if err := dispatch(ctx, c, name, []string{"x"}); err == nil {
			t.Errorf("lab %s x did something", name)
		}
	}
	for _, name := range specRetired {
		if err := retired(name); err == nil || !strings.Contains(err.Error(), "Omarchy Mac Lab") {
			t.Errorf("the retired lab %s says %v", name, err)
		}
	}
	for _, sub := range []string{"hide", "delete", "people"} {
		if err := dispatch(ctx, c, "installed", []string{sub, "--release", "R"}); err == nil || !strings.Contains(err.Error(), "unknown") {
			t.Errorf("lab installed %s says %v", sub, err)
		}
	}
	if asked != 0 {
		t.Errorf("a word that is no command asked the lab %d times", asked)
	}
}

// Nothing the spec keeps on the site has a flag either: `lab device` sets a
// label and a model, `lab enroll` names no person, and `lab release --force`
// carries only its reason.
func TestNothingKeptOnTheSiteHasAFlag(t *testing.T) {
	flags := func(fs *flag.FlagSet) (names []string) {
		fs.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })
		return names
	}
	fs := flag.NewFlagSet("device", flag.ContinueOnError)
	deviceFlags(fs)
	if got := flags(fs); !slices.Equal(got, []string{"label", "model"}) {
		t.Errorf("lab device takes %v", got)
	}
	fs = flag.NewFlagSet("enroll", flag.ContinueOnError)
	enrollFlags(fs)
	if got := flags(fs); !slices.Equal(got, []string{"label", "model", "revoke"}) {
		t.Errorf("lab enroll takes %v", got)
	}
	low := strings.ToLower(usage)
	for _, word := range []string{"--volunteer ", "--contact", "--login", "--unlink", "--oob-url", "--oob-token", "--no-oob", "tailscale", "tailnet", "./volunteer"} {
		if strings.Contains(low, word) {
			t.Errorf("lab help still says %q", word)
		}
	}
}

func toolOf(command string) string {
	return "lab_" + strings.NewReplacer(" ", "_", "-", "_").Replace(command)
}

// The tools of `lab mcp` follow the commands one for one: each row of the
// table has the tool of its name, and there is no other tool.
func TestTheToolsOfLabMCPFollowTheCommandsOneForOne(t *testing.T) {
	c := labAt(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("listing the tools asked the lab %s %s", r.Method, r.URL.Path)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	server := newMCP(c)
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if name := session.InitializeResult().ServerInfo.Name; name != "omarchy-mac-lab" {
		t.Errorf("lab mcp calls itself %q", name)
	}
	if said := session.InitializeResult().Instructions; !strings.Contains(said, "Omarchy Mac Lab") || strings.Contains(strings.ToLower(said), "serial") {
		t.Errorf("lab mcp introduces itself with:\n%s", said)
	}
	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got, want []string
	texts := map[string]string{}
	for _, tool := range list.Tools {
		got = append(got, tool.Name)
		schema, _ := json.Marshal(tool.InputSchema)
		texts[tool.Name] = tool.Description + string(schema)
	}
	for row := range specTable {
		if name, _, _ := strings.Cut(row, " "); slices.Contains(noTool, name) {
			continue
		}
		want = append(want, toolOf(row))
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		var missing, extra []string
		for _, name := range want {
			if !slices.Contains(got, name) {
				missing = append(missing, name)
			}
		}
		for _, name := range got {
			if !slices.Contains(want, name) {
				extra = append(extra, name)
			}
		}
		t.Errorf("lab mcp lacks the tools %v and has the tools %v that follow no command", missing, extra)
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range want {
		if !strings.Contains(string(readme), "`"+name+"`") {
			t.Errorf("the README's list of tools lacks %s", name)
		}
	}
	for name, text := range texts {
		low := strings.ToLower(text)
		for _, gone := range []string{"lab_serial", "lab_devices", "lab_log\"", "lab_screenshot", "lab_build_status", "./volunteer", "lab forget", "tailscale", "serial"} {
			if strings.Contains(low, gone) {
				t.Errorf("the tool %s still speaks of %s", name, gone)
			}
		}
	}
}

// A tool hands no secret to an agent: a builder's token and a Join command
// go into a file the person names, and the answer says only where.
func TestAToolNeverAnswersWithAToken(t *testing.T) {
	c := labAt(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/builders":
			io.WriteString(w, `{"name":"mini","kinds":["kernel"],"token":"bld_SECRET","generation":"g1"}`)
		case "POST /api/enroll-tokens":
			io.WriteString(w, `{"token":"enr_SECRET","expires":"2026-11-15T09:00:00Z","join_command":"curl … enr_SECRET"}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, 400)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := newMCP(c).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	dir := t.TempDir()
	for tool, args := range map[string]map[string]any{
		"lab_builder_add": {"name": "mini", "out": dir + "/builder.token"},
		"lab_enroll":      {"name": "m1-pro", "out": dir + "/join.sh"},
	} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		var said string
		for _, part := range res.Content {
			if text, ok := part.(*mcp.TextContent); ok {
				said += text.Text
			}
		}
		if res.IsError || strings.Contains(said, "SECRET") || !strings.Contains(said, dir) {
			t.Errorf("%s answered (error %v):\n%s", tool, res.IsError, said)
		}
		file := args["out"].(string)
		kept, err := os.ReadFile(file)
		info, _ := os.Stat(file)
		if err != nil || !strings.Contains(string(kept), "SECRET") || info.Mode().Perm() != 0o600 {
			t.Errorf("%s left %q in its file (%v)", tool, kept, err)
		}
		delete(args, "out")
		res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err == nil && !res.IsError {
			t.Errorf("%s with no file to write to went on", tool)
		}
	}
}

// A verdict and a Promotion are a person's: the first call of either tool
// changes nothing and answers with the lab's own sentence, and only a call
// with confirm goes on.
func TestAVerdictAndAPromotionAskBeforeTheyAct(t *testing.T) {
	var confirmed []string
	c := labAt(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["confirm"] == true {
			confirmed = append(confirmed, r.URL.Path)
			io.WriteString(w, `{"release":{"name":"12.0","state":"qualified"},"device":"m1-pro"}`)
			return
		}
		io.WriteString(w, `{"confirm":"Record 12.0 as qualified? 1 of 3 Configurations have a passed run."}`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := newMCP(c).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	say := func(tool string, args map[string]any) string {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %+v", tool, err, res)
		}
		var said string
		for _, part := range res.Content {
			if text, ok := part.(*mcp.TextContent); ok {
				said += text.Text
			}
		}
		return said
	}
	for tool, args := range map[string]map[string]any{
		"lab_installed_verdict": {"release": "12.0", "verdict": "qualified", "note": "three Macs passed"},
		"lab_installed_promote": {"release": "12.0", "device": "m1-pro", "job_id": "j1"},
	} {
		first := say(tool, args)
		if !strings.Contains(first, "1 of 3 Configurations") || !strings.Contains(first, "Nothing was done") || len(confirmed) != 0 {
			t.Fatalf("%s, asked first, answered %q and confirmed %v", tool, first, confirmed)
		}
		args["confirm"] = true
		say(tool, args)
		if len(confirmed) != 1 {
			t.Fatalf("%s with confirm: the lab was told %v", tool, confirmed)
		}
		confirmed = nil
	}
}

// confirm set on the first call goes on in that one call, as `lab --yes`
// does: the lab is asked first and its sentence, warnings and all, is in
// the answer beside what was done. Each tool's description says so.
func TestConfirmOnTheFirstCallDoesWhatLabYesDoes(t *testing.T) {
	var asked, confirmed []string
	c := labAt(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["confirm"] == true {
			if len(asked) == len(confirmed) {
				t.Errorf("%s confirmed before the lab was asked", r.URL.Path)
			}
			confirmed = append(confirmed, r.URL.Path)
			io.WriteString(w, `{"release":{"name":"12.0","state":"qualified"},"device":"m1-pro"}`)
			return
		}
		asked = append(asked, r.URL.Path)
		io.WriteString(w, `{"confirm":"Record 12.0 as qualified? Warning: m2-max failed after the verdict."}`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := newMCP(c).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	described := map[string]string{}
	for _, tool := range tools.Tools {
		schema, _ := json.Marshal(tool.InputSchema)
		described[tool.Name] = tool.Description + " " + string(schema)
	}
	for tool, args := range map[string]map[string]any{
		"lab_installed_verdict": {"release": "12.0", "verdict": "qualified", "note": "three Macs passed", "confirm": true},
		"lab_installed_promote": {"release": "12.0", "device": "m1-pro", "job_id": "j1", "confirm": true},
	} {
		if d := described[tool]; !strings.Contains(d, "lab installed") || !strings.Contains(d, "--yes") || !strings.Contains(d, "one call") {
			t.Errorf("%s does not say that confirm in one call does what --yes does: %s", tool, d)
		}
		asked, confirmed = nil, nil
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %+v", tool, err, res)
		}
		var said string
		for _, part := range res.Content {
			if text, ok := part.(*mcp.TextContent); ok {
				said += text.Text
			}
		}
		if len(asked) != 1 || len(confirmed) != 1 {
			t.Errorf("%s in one call: asked %v, confirmed %v", tool, asked, confirmed)
		}
		if !strings.Contains(said, "Warning: m2-max failed after the verdict.") || !strings.Contains(said, "12.0") || strings.Contains(said, "Nothing was done") {
			t.Errorf("%s in one call answered %q", tool, said)
		}
	}
}
