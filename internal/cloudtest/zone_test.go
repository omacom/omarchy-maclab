package cloudtest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const zoneScript = "zone-settings.sh"

func specText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo(t), "docs", "spec", "spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// specList is the spec's list after "- **Off**:" or "- **On**:" under "Edge
// settings", with what stands in brackets left out.
func specList(t *testing.T, label string) []string {
	t.Helper()
	text := specText(t)
	start := strings.Index(text, "### Edge settings")
	if start < 0 {
		t.Fatal("the spec has no section Edge settings")
	}
	section := text[start:]
	section = section[:strings.Index(section, "### Cache")]
	m := regexp.MustCompile(`(?m)^- \*\*` + label + `\*\*: (.*)\.$`).FindStringSubmatch(section)
	if m == nil {
		t.Fatalf("the spec's Edge settings has no %s list", label)
	}
	return strings.Split(regexp.MustCompile(` \([^)]*\)`).ReplaceAllString(m[1], ""), ", ")
}

// Every setting the spec lists as off or on is in edge.json with that state,
// and edge.json has no other.
func TestTheSettingsAreTheSpecsList(t *testing.T) {
	e := readEdge(t)
	want := map[string]string{}
	for _, name := range specList(t, "Off") {
		want[name] = "off"
	}
	for _, name := range specList(t, "On") {
		want[name] = "on"
	}
	if len(want) != 16 {
		t.Fatalf("the spec lists %d settings; the test knows 9 off and 7 on: %v", len(want), want)
	}
	seen := map[string]bool{}
	for _, s := range e.Settings {
		if s.Name == browserTTL {
			continue
		}
		name := regexp.MustCompile(` \([^)]*\)$`).ReplaceAllString(s.Name, "")
		state, ok := want[name]
		if !ok {
			t.Errorf("edge.json sets %q, which the spec does not list", s.Name)
			continue
		}
		if s.State != state {
			t.Errorf("%s is %s in edge.json and %s in the spec", s.Name, s.State, state)
		}
		seen[name] = true
		if !strings.HasPrefix(s.Path, "/zones/{zone}/") {
			t.Errorf("%s is set outside the zone: %s", s.Name, s.Path)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("the spec's %q is not in edge.json", name)
		}
	}
}

const browserTTL = "Browser Cache TTL"

// The browser keeps to labd's max-age: Cloudflare's default of four hours
// would rewrite a lower one, max-age=0 on the public pages among them.
func TestTheBrowserCacheTTLRespectsLabdsHeaders(t *testing.T) {
	text := specText(t)
	section := text[strings.Index(text, "### Edge settings"):]
	section = section[:strings.Index(section, "### Cache")]
	if !strings.Contains(section, "- **"+browserTTL+"**: Respect Existing Headers") {
		t.Errorf("the spec's Edge settings does not say %s is Respect Existing Headers", browserTTL)
	}
	var found bool
	for _, s := range readEdge(t).Settings {
		if s.Name != browserTTL {
			continue
		}
		found = true
		if s.Path != "/zones/{zone}/settings/browser_cache_ttl" || s.State != "Respect Existing Headers" ||
			fmt.Sprint(s.Body) != "map[value:0]" || fmt.Sprint(s.Accept) != "[0]" {
			t.Errorf("%s is not set to 0 and checked for 0: %+v", browserTTL, s)
		}
	}
	if !found {
		t.Errorf("edge.json does not set %s", browserTTL)
	}
}

func rule(t *testing.T, e edge, phase string) []map[string]any {
	t.Helper()
	for _, r := range e.Rules {
		if r.Phase == phase {
			return r.Want
		}
	}
	t.Fatalf("edge.json has no rules for %s", phase)
	return nil
}

func TestTheRulesAreTheSpecs(t *testing.T) {
	e := readEdge(t)
	api := `starts_with(http.request.uri.path, "/api/")`
	onLab := `http.host eq "` + labHost + `"`

	skip := rule(t, e, "http_request_firewall_custom")
	if len(skip) != 1 {
		t.Fatalf("%d skip rules, want the one", len(skip))
	}
	b, _ := json.Marshal(skip[0])
	for _, want := range []string{`"action":"skip"`, `"products":["bic"]`, `"phases":["http_request_firewall_managed"]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the skip rule lacks %s: %s", want, b)
		}
	}
	if x := skip[0]["expression"].(string); !strings.Contains(x, api) || !strings.Contains(x, onLab) {
		t.Errorf("the skip rule is not on /api/* at the Lab address: %s", x)
	}
	// The Join command's script goes through as the Join command's API does.
	if x := skip[0]["expression"].(string); !strings.Contains(x, `(`+api+` or http.request.uri.path eq "/join")`) {
		t.Errorf("the skip rule is not on /join too: %s", x)
	}

	rate := rule(t, e, "http_ratelimit")
	if len(rate) != 1 {
		t.Fatalf("%d rate rules, want the one", len(rate))
	}
	m := regexp.MustCompile(`(\d+) requests in (\d+) s per address, whole site`).FindStringSubmatch(specText(t))
	if m == nil {
		t.Fatal("the spec no longer words the flood cap as N requests in M s per address, whole site")
	}
	rl := rate[0]["ratelimit"].(map[string]any)
	if fmt.Sprint(rl["requests_per_period"]) != m[1] || fmt.Sprint(rl["period"]) != m[2] {
		t.Errorf("the rate rule is %v requests in %v s; the spec says %s in %s", rl["requests_per_period"], rl["period"], m[1], m[2])
	}
	if got := fmt.Sprint(rl["characteristics"]); !strings.Contains(got, "ip.src") {
		t.Errorf("the rate rule does not count per address: %s", got)
	}
	// Whole site, both names: the rule names no host and no path.
	if x := rate[0]["expression"]; x != "true" {
		t.Errorf("the rate rule does not cover the whole site and both names: %v", x)
	}

	cache := rule(t, e, "http_request_cache_settings")
	if len(cache) != 3 {
		t.Fatalf("%d cache rules, want the public pages, /api/* and a Lab token or the session cookie", len(cache))
	}
	public, bypass, guard := cache[0], cache[1], cache[2]
	px := public["expression"].(string)
	const token = `any(lower(http.request.headers.names[*])[*] eq "authorization")`
	for _, want := range []string{onLab, `not http.cookie contains "lab_session="`, `not ` + token, `"/terms"`, `"/privacy"`, `"/install"`} {
		if !strings.Contains(px, want) {
			t.Errorf("the public pages' cache rule lacks %s", want)
		}
	}
	// Under /api/ the rule names the exports and nothing else: each by its
	// whole path, never a prefix.
	if px == "true" || strings.Contains(px, `"/api/"`) || strings.Contains(strings.ReplaceAll(px, `"/api/v1/`, ""), "/api") {
		t.Errorf("the public pages' cache rule reaches beyond public paths: %s", px)
	}
	if public["action_parameters"].(map[string]any)["cache"] != true {
		t.Error("the public pages' rule does not make them cacheable")
	}
	bx := bypass["expression"].(string)
	if !strings.Contains(bx, api) || bypass["action_parameters"].(map[string]any)["cache"] != false {
		t.Errorf("the last cache rule does not bypass cache on /api/*: %v", bypass)
	}
	// The exports are cached as the public pages are, so the bypass leaves
	// out exactly the paths under /api/ the first rule names, and only for
	// a request without the session cookie.
	inSet := regexp.MustCompile(`http\.request\.uri\.path in \{([^}]*)\}`)
	exports := regexp.MustCompile(`"/api/v1/[^"]*"`).FindAllString(inSet.FindStringSubmatch(px)[1], -1)
	left := inSet.FindStringSubmatch(bx)
	if len(exports) == 0 || left == nil || strings.Join(exports, " ") != left[1] {
		t.Errorf("the bypass leaves out %v; the exports the first rule names are %v", left, exports)
	}
	if !strings.HasSuffix(bx, ` and (http.cookie contains "lab_session=" or not http.request.uri.path in {`+strings.Join(exports, " ")+`}))`) {
		t.Errorf("the bypass does not cover an export asked for with the session cookie: %s", bx)
	}
	// Last, so it wins over both: whatever the path or the name, a request
	// with a Lab token or the session cookie is never answered from the cache.
	if x := guard["expression"]; x != `(`+token+` or http.cookie contains "lab_session=")` || guard["action_parameters"].(map[string]any)["cache"] != false {
		t.Errorf("the last cache rule does not bypass cache for a Lab token or the session cookie: %v", guard)
	}
	// Never a forced edge lifetime, never "cache everything".
	all, _ := json.Marshal(cache)
	for _, never := range []string{"edge_ttl", "browser_ttl", `"expression":"true"`} {
		if strings.Contains(string(all), never) {
			t.Errorf("a cache rule has %s", never)
		}
	}
	// The skip rule and the cache rules stay written for the Lab address.
	for _, r := range append(append([]map[string]any{}, skip...), cache...) {
		if strings.Contains(r["expression"].(string), "maclab.omarchy.org") {
			t.Errorf("a rule names the Alias: %v", r["expression"])
		}
	}

	if got := rule(t, e, "http_request_dynamic_redirect"); len(got) != 0 {
		t.Errorf("edge.json has a redirect rule: %v", got)
	}
	if e.Mail.Address != "privacy@"+labDomain {
		t.Errorf("the forwarded address is %q", e.Mail.Address)
	}
	// DNSSEC and the two alerts are the owner's: checked, with no way to set them.
	if len(e.Owner) != 3 {
		t.Errorf("%d things are the owner's, want DNSSEC and the two tunnel alerts", len(e.Owner))
	}
	for _, o := range e.Owner {
		if o.Method != "" || o.Body != nil || o.How == "" {
			t.Errorf("%s: the owner's, yet edge.json says how to set it, or not how the owner does", o.Name)
		}
	}
}

func TestCheckIsGreenOnAZoneAsTheSpecHasIt(t *testing.T) {
	f := asSpec(t)
	out, code := f.run(zoneScript, "--check", "--forward-to", mailbox)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if w := writes(f.sent()); len(w) != 0 {
		t.Errorf("a check wrote:\n%s", describe(w))
	}
	for _, s := range readEdge(t).Settings {
		if !strings.Contains(out, "ok       "+s.Name+": ") {
			t.Errorf("no line for %s:\n%s", s.Name, out)
		}
	}
	if strings.Contains(out, "drifted") || strings.Contains(out, mailbox) {
		t.Errorf("the output names a drift, or the mailbox:\n%s", out)
	}
}

// drift is one way a zone can differ from the spec, and the name the script
// must give it.
type drift struct {
	name   string
	change func(*fake)
}

func drifts(t *testing.T) []drift {
	e := readEdge(t)
	var out []drift
	for _, s := range e.Settings {
		s := s
		out = append(out, drift{s.Name, func(f *fake) {
			switch s.Name {
			case "Bot Fight Mode":
				f.bot["fight_mode"] = true
			case "AI-bot blocking":
				f.bot["ai_bots_protection"] = "block"
			case "AI Labyrinth":
				f.bot["crawler_protection"] = "enabled"
			default:
				f.values[strings.TrimPrefix(s.Path, "/zones/{zone}/")] = unlike(s)
			}
		}})
	}
	edit := func(phase string, change func(rules []map[string]any) []map[string]any) func(*fake) {
		return func(f *fake) { f.rules[phase] = change(f.rules[phase]) }
	}
	another := map[string]any{"id": "x", "action": "block", "expression": `(http.request.uri.path eq "/x")`, "enabled": true}
	const custom, rate, cache, redirect = "http_request_firewall_custom", "http_ratelimit", "http_request_cache_settings", "http_request_dynamic_redirect"
	out = append(out,
		drift{"the skip rule on /api/* and /join", func(f *fake) { delete(f.rules, custom) }},
		drift{"the skip rule on /api/* and /join", edit(custom, func(r []map[string]any) []map[string]any { return append(r, another) })},
		drift{"the skip rule on /api/* and /join", edit(custom, func(r []map[string]any) []map[string]any {
			r[0]["action_parameters"] = map[string]any{"phases": []any{"http_request_firewall_managed"}}
			return r
		})},
		drift{"the skip rule on /api/* and /join", edit(custom, func(r []map[string]any) []map[string]any { r[0]["enabled"] = false; return r })},
		drift{"the rate rule", func(f *fake) { delete(f.rules, rate) }},
		drift{"the rate rule", edit(rate, func(r []map[string]any) []map[string]any {
			r[0]["ratelimit"].(map[string]any)["requests_per_period"] = 3000
			return r
		})},
		drift{"the rate rule", edit(rate, func(r []map[string]any) []map[string]any {
			r[0]["ratelimit"].(map[string]any)["period"] = 60
			return r
		})},
		drift{"the rate rule", edit(rate, func(r []map[string]any) []map[string]any {
			r[0]["expression"] = `(http.host eq "` + labHost + `")`
			return r
		})},
		drift{"the rate rule", edit(rate, func(r []map[string]any) []map[string]any { return append(r, another) })},
		drift{"the cache rules", func(f *fake) { delete(f.rules, cache) }},
		drift{"the cache rules", edit(cache, func(r []map[string]any) []map[string]any { return r[:1] })},
		drift{"the cache rules", edit(cache, func(r []map[string]any) []map[string]any { return r[:2] })},
		drift{"the cache rules", edit(cache, func(r []map[string]any) []map[string]any {
			r[0]["expression"] = strings.Replace(r[0]["expression"].(string), ` and not any(lower(http.request.headers.names[*])[*] eq "authorization")`, "", 1)
			return r
		})},
		drift{"the cache rules", edit(cache, func(r []map[string]any) []map[string]any {
			r[0]["action_parameters"].(map[string]any)["edge_ttl"] = map[string]any{"mode": "override_origin", "default": 3600}
			return r
		})},
		drift{"the cache rules", edit(cache, func(r []map[string]any) []map[string]any {
			r[0]["expression"] = "true"
			return r
		})},
		drift{"the cache rules", edit(cache, func(r []map[string]any) []map[string]any {
			r[0]["expression"] = strings.Replace(r[0]["expression"].(string), ` and not http.cookie contains "lab_session="`, "", 1)
			return r
		})},
		drift{"the cache rules", edit(cache, func(r []map[string]any) []map[string]any {
			return append(r, map[string]any{"action": "set_cache_settings", "expression": "true", "action_parameters": map[string]any{"cache": true}, "enabled": true})
		})},
		drift{"no redirect rule", func(f *fake) {
			f.rules[redirect] = []map[string]any{{"action": "redirect", "expression": "true", "enabled": true}}
		}},
		drift{"the mail forwarding rule", func(f *fake) { f.mailRules = nil }},
		drift{"the mail forwarding rule", func(f *fake) { f.mailRules[0]["enabled"] = false }},
		drift{"the mail forwarding rule", func(f *fake) { f.routing = false }},
		drift{"the mail forwarding rule", func(f *fake) { f.addresses[0]["verified"] = nil }},
		drift{"the mail forwarding rule", func(f *fake) {
			f.mailRules[0]["actions"] = []any{map[string]any{"type": "forward", "value": []any{"someone-else@example.org"}}}
			f.addresses = append(f.addresses, map[string]any{"email": "someone-else@example.org", "verified": "2026-11-14T00:00:00Z"})
		}},
		drift{"the mail forwarding rule", func(f *fake) {
			f.mailRules[0]["actions"] = []any{map[string]any{"type": "drop"}}
		}},
		drift{"DNSSEC", func(f *fake) { f.dnssec = "pending" }},
		drift{"the tunnel-health alert", func(f *fake) { f.alerts = f.alerts[1:] }},
		drift{`the "tunnel created or deleted" alert`, func(f *fake) { f.alerts[1]["enabled"] = false }},
	)
	return out
}

// A drifted setting makes the script exit non-zero and name it, and only it;
// --apply then puts it right with one write, except what is the owner's.
func TestEachDriftIsNamedAndPutRight(t *testing.T) {
	for i, d := range drifts(t) {
		d := d
		owners := d.name == "DNSSEC" || strings.Contains(d.name, "alert")
		t.Run(fmt.Sprintf("%02d %s", i, d.name), func(t *testing.T) {
			t.Parallel()
			f := asSpec(t)
			d.change(f)
			unconfirmed := f.addresses[0]["verified"] == nil

			out, code := f.run(zoneScript, "--check", "--forward-to", mailbox)
			if code != 1 {
				t.Fatalf("exit %d, want 1\n%s", code, out)
			}
			if !strings.Contains(out, "drifted  "+d.name+": ") {
				t.Errorf("the drift is not named:\n%s", out)
			}
			if n := strings.Count(out, "drifted  "); n != 1 {
				t.Errorf("%d drifts named, want only %s:\n%s", n, d.name, out)
			}
			if !strings.Contains(out, "1 not as the spec has it: "+d.name) {
				t.Errorf("the last line does not name it:\n%s", out)
			}
			if w := writes(f.sent()); len(w) != 0 {
				t.Errorf("a check wrote:\n%s", describe(w))
			}
			if strings.Contains(out, mailbox) || strings.Contains(out, "someone-else@") {
				t.Errorf("the output names a mailbox:\n%s", out)
			}

			// --apply ends with a check of its own.
			out, code = f.run(zoneScript, "--apply", "--forward-to", mailbox)
			w := writes(f.sent())
			if owners || unconfirmed {
				if code != 1 || len(w) != 0 {
					t.Fatalf("--apply: exit %d and %d writes; want it named and nothing written\n%s%s", code, len(w), out, describe(w))
				}
				return
			}
			if code != 0 {
				t.Fatalf("--apply: exit %d\n%s", code, out)
			}
			if len(w) != 1 {
				t.Errorf("%d writes, want the one that puts %s right:\n%s", len(w), d.name, describe(w))
			}
		})
	}
}

func TestApplySetsAZoneWithNothingRightAndThenChangesNothing(t *testing.T) {
	f := newFake(t)
	f.dnssec = "active"
	f.alerts = asSpec(t).alerts
	e := readEdge(t)

	// The mailbox must confirm first: the first run asks Cloudflare to mail it.
	out, code := f.run(zoneScript, "--apply", "--forward-to", mailbox)
	if code != 1 || !strings.Contains(out, "has not confirmed") {
		t.Fatalf("exit %d; want 1 until the mailbox confirms\n%s", code, out)
	}
	if strings.Contains(out, mailbox) {
		t.Errorf("the output names the mailbox:\n%s", out)
	}
	first := f.sent()
	f.mu.Lock()
	if len(f.addresses) != 1 || f.addresses[0]["email"] != mailbox {
		t.Fatalf("the mailbox was not made a destination: %v", f.addresses)
	}
	f.addresses[0]["verified"] = "2026-11-15T00:00:00Z"
	f.mu.Unlock()

	out, code = f.run(zoneScript, "--apply", "--forward-to", mailbox)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	all := describe(writes(append(first, f.sent()...)))
	for _, s := range e.Settings {
		method, body := s.Method, s.Body
		if method == "" {
			method = "PATCH"
		}
		if body == nil {
			body = map[string]any{"value": s.State}
		}
		b, _ := json.Marshal(body)
		var got bool
		for _, line := range strings.Split(all, "\n") {
			if strings.HasPrefix(line, method+" "+strings.Replace(s.Path, "{zone}", zoneID, 1)+" ") {
				var sent, want any
				json.Unmarshal([]byte(strings.SplitN(line, " ", 3)[2]), &sent)
				json.Unmarshal(b, &want)
				got = fmt.Sprint(sent) == fmt.Sprint(want)
			}
		}
		if !got {
			t.Errorf("%s was not set with %s %s %s:\n%s", s.Name, method, s.Path, b, all)
		}
	}
	if n := strings.Count(all, "/bot_management"); n != 1 {
		t.Errorf("the bot settings were written %d times, want once", n)
	}
	for _, want := range []string{
		"PUT /zones/" + zoneID + "/rulesets/phases/http_request_firewall_custom/entrypoint",
		"PUT /zones/" + zoneID + "/rulesets/phases/http_ratelimit/entrypoint",
		"PUT /zones/" + zoneID + "/rulesets/phases/http_request_cache_settings/entrypoint",
		"POST /zones/" + zoneID + "/email/routing/dns",
		"POST /accounts/" + accountID + "/email/routing/addresses",
		"POST /zones/" + zoneID + "/email/routing/rules",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("no %s:\n%s", want, all)
		}
	}
	if strings.Contains(all, "http_request_dynamic_redirect") {
		t.Errorf("a zone with no redirect rule had one written:\n%s", all)
	}

	out, code = f.run(zoneScript, "--apply", "--forward-to", mailbox)
	if w := writes(f.sent()); code != 0 || len(w) != 0 {
		t.Errorf("a second --apply: exit %d, wrote:\n%s%s", code, describe(w), out)
	}
}

func TestApplyNeedsTheMailbox(t *testing.T) {
	f := asSpec(t)
	out, code := f.run(zoneScript, "--apply")
	if code == 0 || !strings.Contains(out, "--forward-to") || len(f.sent()) != 0 {
		t.Errorf("exit %d, %q; want a refusal naming --forward-to before any call", code, out)
	}
	// A check without it still wants a rule that forwards to a confirmed mailbox.
	if out, code := f.run(zoneScript, "--check"); code != 0 {
		t.Errorf("a check without --forward-to: exit %d\n%s", code, out)
	}
}

func TestEveryCallNamesItsClientAndCarriesTheToken(t *testing.T) {
	f := asSpec(t)
	f.run(zoneScript, "--check")
	calls := f.sent()
	if len(calls) < 20 {
		t.Fatalf("%d calls", len(calls))
	}
	for _, c := range calls {
		if c.Auth != "Bearer "+f.token || c.Agent != "omarchy-mac-lab-scripts" {
			t.Fatalf("%s %s: token or User-Agent missing (%q)", c.Method, c.Path, c.Agent)
		}
	}
}

func TestARefusedTokenIsNamedAndNothingIsPrinted(t *testing.T) {
	f := asSpec(t)
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte("tok-another-one-entirely\n"), 0o600)
	out, code := script(t, f, nil, zoneScript, "--check", "--token-file", file)
	if code == 0 || !strings.Contains(out, "[10000] Authentication error") || !strings.Contains(out, "ray "+ray) {
		t.Errorf("exit %d\n%s", code, out)
	}
	if strings.Contains(out, "tok-another") {
		t.Errorf("the token was printed:\n%s", out)
	}
}

// A web error page is never pasted into output: status and ray id only.
func TestAWebErrorPageIsNotPrinted(t *testing.T) {
	f := asSpec(t)
	f.broken = "/settings/http3"
	out, code := f.run(zoneScript, "--check")
	if code == 0 || !strings.Contains(out, "status 502") || !strings.Contains(out, ray) {
		t.Errorf("exit %d; want the status and the ray id\n%s", code, out)
	}
	if strings.Contains(out, "<html") || strings.Contains(out, "Bad Gateway") {
		t.Errorf("the page was printed:\n%s", out)
	}
}

func TestNoZoneIsSaid(t *testing.T) {
	f := asSpec(t)
	f.noZone = true
	out, code := f.run(zoneScript, "--check")
	if code == 0 || !strings.Contains(out, "no zone "+labDomain) {
		t.Errorf("exit %d\n%s", code, out)
	}
}
