// Package cloudtest runs the cloud scripts (scripts/cloud) against a server
// on this machine that answers as Cloudflare's API does. Nothing here reaches
// Cloudflare, and no token here is a real one.
package cloudtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

const (
	zoneID    = "0123456789abcdef0123456789abcdef"
	accountID = "fedcba9876543210fedcba9876543210"
	labDomain = "omarchy-mac.org"
	labHost   = "lab.omarchy-mac.org"
	mailbox   = "owner@example.org"
	ray       = "8f00ba4dfeed1234-BNE"
)

func repo(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test")
		}
		dir = parent
	}
}

func need(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("needs %s", tool)
		}
	}
}

// edge is scripts/cloud/edge.json.
type edge struct {
	Settings []setting `json:"settings"`
	Rules    []struct {
		Name  string           `json:"name"`
		Phase string           `json:"phase"`
		Never []string         `json:"never"`
		Want  []map[string]any `json:"want"`
	} `json:"rules"`
	Mail struct {
		Address string `json:"address"`
		Name    string `json:"name"`
	} `json:"mail"`
	Owner []setting `json:"owner"`
	Alias struct {
		Hostname       string `json:"hostname"`
		FallbackOrigin string `json:"fallback_origin"`
		Wanted         bool   `json:"wanted"`
	} `json:"alias"`
}

type setting struct {
	Name   string         `json:"name"`
	State  string         `json:"state"`
	Path   string         `json:"path"`
	Read   string         `json:"read"`
	Accept []string       `json:"accept"`
	Method string         `json:"method"`
	Body   map[string]any `json:"body"`
	How    string         `json:"how"`
}

func readEdge(t *testing.T) edge {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo(t), "scripts", "cloud", "edge.json"))
	if err != nil {
		t.Fatal(err)
	}
	var e edge
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("edge.json: %v", err)
	}
	return e
}

type call struct {
	Method, Path, Body, Auth, Agent string
}

// fake answers as Cloudflare's API does for one zone and its account.
type fake struct {
	t     *testing.T
	srv   *httptest.Server
	token string

	mu        sync.Mutex
	calls     []call
	values    map[string]any              // a setting's path under the zone, to "on", "off" or a number
	bot       map[string]any              // the zone's bot settings
	rules     map[string][]map[string]any // a phase's rules; a phase with no entry has no ruleset
	routing   bool
	mailRules []map[string]any
	addresses []map[string]any
	dnssec    string
	alerts    []map[string]any
	tunnels   []map[string]any
	records   []map[string]any
	noZone    bool
	fallback  map[string]any   // the zone's fallback origin; nil when none is set
	hostnames []map[string]any // the zone's custom hostnames
	noSaaS    bool             // Cloudflare for SaaS is not enabled on the zone
	old       *oldZone         // omarchy-m-testing.org's zone; nil while the account has none
	porkbun   []map[string]any
	purges    []string
	broken    string // a path that answers with a web error page
	seq       int
}

// like is a setting's value as the spec has it; unlike is a value it is
// not, Cloudflare's own default where there is one.
func like(s setting) any {
	if v, ok := s.Body["value"]; ok {
		return v
	}
	return s.State
}

func unlike(s setting) any {
	if s.Path == "/zones/{zone}/settings/browser_cache_ttl" {
		return float64(14400)
	}
	return opposite(s.State)
}

func opposite(v string) string {
	if v == "on" {
		return "off"
	}
	return "on"
}

// newFake is a zone with nothing as the spec has it.
func newFake(t *testing.T) *fake {
	t.Helper()
	need(t, "bash", "curl", "jq")
	f := &fake{t: t, token: "tok-" + strings.Repeat("s3cr3t", 6), values: map[string]any{}, rules: map[string][]map[string]any{}, dnssec: "disabled"}
	f.bot = map[string]any{"fight_mode": true, "ai_bots_protection": "block", "crawler_protection": "enabled", "enable_js": true}
	for _, s := range readEdge(t).Settings {
		if !strings.HasSuffix(s.Path, "/bot_management") {
			f.values[strings.TrimPrefix(s.Path, "/zones/{zone}/")] = unlike(s)
		}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// asSpec is a zone with everything as the spec has it, the owner's part too.
func asSpec(t *testing.T) *fake {
	t.Helper()
	f := newFake(t)
	e := readEdge(t)
	f.bot = map[string]any{"fight_mode": false, "ai_bots_protection": "disabled", "crawler_protection": "disabled", "enable_js": false}
	for _, s := range e.Settings {
		if !strings.HasSuffix(s.Path, "/bot_management") {
			f.values[strings.TrimPrefix(s.Path, "/zones/{zone}/")] = like(s)
		}
	}
	for _, r := range e.Rules {
		if len(r.Want) > 0 {
			f.rules[r.Phase] = f.stored(r.Want)
		}
	}
	f.routing = true
	f.addresses = []map[string]any{{"id": "a1", "email": mailbox, "verified": "2026-11-14T00:00:00Z"}}
	f.mailRules = []map[string]any{{
		"id": "r1", "name": "privacy", "enabled": true, "priority": 0,
		"matchers": []any{map[string]any{"type": "literal", "field": "to", "value": e.Mail.Address}},
		"actions":  []any{map[string]any{"type": "forward", "value": []any{mailbox}}},
	}}
	f.dnssec = "active"
	f.alerts = []map[string]any{
		{"alert_type": "tunnel_health_event", "enabled": true, "mechanisms": map[string]any{"email": []any{map[string]any{"id": mailbox}}}},
		{"alert_type": "tunnel_update_event", "enabled": true, "mechanisms": map[string]any{"email": []any{map[string]any{"id": mailbox}}}},
	}
	return f
}

// stored is a list of rules as Cloudflare gives it back: with ids, versions
// and fields of its own, and a rate rule's characteristics in its own order.
func (f *fake) stored(rules []map[string]any) []map[string]any {
	b, _ := json.Marshal(rules)
	var out []map[string]any
	json.Unmarshal(b, &out)
	for _, r := range out {
		f.seq++
		r["id"] = fmt.Sprintf("rule%d", f.seq)
		r["version"] = "1"
		r["ref"] = r["id"]
		r["last_updated"] = "2026-11-14T00:00:00Z"
		if rl, ok := r["ratelimit"].(map[string]any); ok {
			rl["requests_to_origin"] = false
			if c, ok := rl["characteristics"].([]any); ok && len(c) == 2 {
				c[0], c[1] = c[1], c[0]
			}
		}
	}
	return out
}

func (f *fake) url() string { return f.srv.URL + "/client/v4" }

func (f *fake) ok(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("CF-Ray", ray)
	json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "messages": []any{}, "result": result})
}

func (f *fake) refuse(w http.ResponseWriter, status, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("CF-Ray", ray)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": code, "message": message}}, "result": nil})
}

var (
	phasePath = regexp.MustCompile(`^rulesets/phases/([a-z_0-9]+)/entrypoint$`)
	rulePath  = regexp.MustCompile(`^email/routing/rules/([a-z0-9]+)$`)
	recPath   = regexp.MustCompile(`^dns_records/([a-z0-9]+)$`)
	hostPath  = regexp.MustCompile(`^custom_hostnames/([0-9a-f-]{36})$`)
)

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/client/v4")
	full := path
	if r.URL.RawQuery != "" {
		full += "?" + r.URL.RawQuery
	}
	f.calls = append(f.calls, call{r.Method, full, string(body), r.Header.Get("Authorization"), r.Header.Get("User-Agent")})
	if f.broken != "" && strings.Contains(path, f.broken) {
		w.Header().Set("CF-Ray", ray)
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "<html><body><h1>502 Bad Gateway</h1>cloudflare, a whole page of it</body></html>")
		return
	}
	if strings.HasPrefix(path, "/porkbun/") {
		f.registrar(w, r, strings.TrimPrefix(path, "/porkbun/"))
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		f.refuse(w, http.StatusForbidden, 10000, "Authentication error")
		return
	}
	var in map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			f.refuse(w, http.StatusBadRequest, 6007, "Malformed JSON in request body")
			return
		}
	}
	q := r.URL.Query()
	switch {
	case path == "/zones" && r.Method == "GET" && q.Get("name") == oldDomain:
		if f.old == nil {
			f.ok(w, []any{})
			return
		}
		f.ok(w, []any{f.old.zone()})
	case path == "/zones" && r.Method == "POST":
		if in["name"] != oldDomain || in["type"] != "full" || in["account"].(map[string]any)["id"] != accountID || f.old != nil {
			f.refuse(w, http.StatusBadRequest, 1061, "the zone cannot be made")
			return
		}
		f.old = &oldZone{status: "pending"}
		f.ok(w, f.old.zone())
	case f.old != nil && strings.HasPrefix(path, "/zones/"+oldZoneID+"/"):
		f.oldName(w, r, strings.TrimPrefix(path, "/zones/"+oldZoneID+"/"), in)
	case path == "/zones/"+zoneID+"/purge_cache" && r.Method == "POST":
		f.purges = append(f.purges, string(body))
		f.ok(w, map[string]any{"id": zoneID})
	case path == "/zones" && r.Method == "GET":
		if f.noZone || q.Get("name") != labDomain {
			f.ok(w, []any{})
			return
		}
		f.ok(w, []any{map[string]any{"id": zoneID, "name": labDomain, "status": "active", "account": map[string]any{"id": accountID}}})
	case strings.HasPrefix(path, "/zones/"+zoneID+"/"):
		f.zone(w, r, strings.TrimPrefix(path, "/zones/"+zoneID+"/"), in)
	case strings.HasPrefix(path, "/accounts/"+accountID+"/"):
		f.account(w, r, strings.TrimPrefix(path, "/accounts/"+accountID+"/"), in)
	default:
		f.refuse(w, http.StatusNotFound, 7003, "No route for the URI")
	}
}

func (f *fake) zone(w http.ResponseWriter, r *http.Request, path string, in map[string]any) {
	q := r.URL.Query()
	if _, ok := f.values[path]; ok {
		switch r.Method {
		case "GET":
		case "PATCH":
			v := in["value"]
			if path == "settings/browser_cache_ttl" {
				if n, ok := v.(float64); !ok || (n != 0 && n < 30) {
					f.refuse(w, http.StatusBadRequest, 1007, "Invalid value for zone setting")
					return
				}
			} else if v != "on" && v != "off" {
				f.refuse(w, http.StatusBadRequest, 1007, "Invalid value for zone setting")
				return
			}
			f.values[path] = v
		default:
			f.refuse(w, http.StatusMethodNotAllowed, 10405, "Method not allowed")
			return
		}
		f.ok(w, map[string]any{"id": filepath.Base(path), "value": f.values[path], "editable": true})
		return
	}
	if m := phasePath.FindStringSubmatch(path); m != nil {
		switch r.Method {
		case "GET":
			rules, ok := f.rules[m[1]]
			if !ok {
				f.refuse(w, http.StatusNotFound, 10003, "could not find entrypoint ruleset in the "+m[1]+" phase")
				return
			}
			f.ok(w, map[string]any{"id": "ruleset-" + m[1], "phase": m[1], "kind": "zone", "rules": rules})
		case "PUT":
			var put struct {
				Rules []map[string]any `json:"rules"`
			}
			b, _ := json.Marshal(in)
			json.Unmarshal(b, &put)
			f.rules[m[1]] = f.stored(put.Rules)
			f.ok(w, map[string]any{"id": "ruleset-" + m[1], "phase": m[1], "rules": f.rules[m[1]]})
		}
		return
	}
	if m := rulePath.FindStringSubmatch(path); m != nil && r.Method == "PUT" {
		for i, rule := range f.mailRules {
			if rule["id"] == m[1] {
				in["id"] = m[1]
				f.mailRules[i] = in
				f.ok(w, in)
				return
			}
		}
		f.refuse(w, http.StatusNotFound, 2020, "rule not found")
		return
	}
	if m := recPath.FindStringSubmatch(path); m != nil && r.Method == "PUT" {
		for i, rec := range f.records {
			if rec["id"] == m[1] {
				in["id"] = m[1]
				f.records[i] = in
				f.ok(w, in)
				return
			}
		}
		f.refuse(w, http.StatusNotFound, 81044, "Record does not exist.")
		return
	}
	if strings.HasPrefix(path, "custom_hostnames") {
		f.custom(w, r, path, in)
		return
	}
	switch path + " " + r.Method {
	case "bot_management GET":
		f.ok(w, f.bot)
	case "bot_management PUT":
		for k, v := range in {
			f.bot[k] = v
		}
		f.ok(w, f.bot)
	case "email/routing GET":
		status := "unconfigured"
		if f.routing {
			status = "ready"
		}
		f.ok(w, map[string]any{"enabled": f.routing, "name": labDomain, "status": status})
	case "email/routing/dns POST":
		// Cloudflare takes name only for a subdomain; the apex goes as {} (B5.2, 2026-11-15).
		if _, ok := in["name"]; ok {
			f.refuse(w, http.StatusUnprocessableEntity, 2007, "Invalid Input: must be a subdomains of "+labDomain)
			return
		}
		f.routing = true
		f.ok(w, map[string]any{"enabled": true, "name": labDomain, "status": "ready"})
	case "email/routing/rules GET":
		f.ok(w, f.mailRules)
	case "email/routing/rules POST":
		for _, a := range in["actions"].([]any) {
			for _, to := range a.(map[string]any)["value"].([]any) {
				if !f.confirmed(to.(string)) {
					f.refuse(w, http.StatusBadRequest, 2032, "destination address is not verified")
					return
				}
			}
		}
		f.seq++
		in["id"] = fmt.Sprintf("mr%d", f.seq)
		f.mailRules = append(f.mailRules, in)
		f.ok(w, in)
	case "dnssec GET":
		f.ok(w, map[string]any{"status": f.dnssec})
	case "dnssec PATCH", "dnssec DELETE":
		f.t.Errorf("a script changed DNSSEC, which is the owner's")
		f.refuse(w, http.StatusForbidden, 10000, "Authentication error")
	case "dns_records GET":
		out := []any{}
		for _, rec := range f.records {
			if q.Get("name") == "" || rec["name"] == q.Get("name") {
				out = append(out, rec)
			}
		}
		f.ok(w, out)
	case "dns_records POST":
		f.seq++
		in["id"] = fmt.Sprintf("rec%d", f.seq)
		f.records = append(f.records, in)
		f.ok(w, in)
	default:
		f.refuse(w, http.StatusNotFound, 7003, "No route for the URI")
	}
}

// custom answers for the zone's custom hostnames and their fallback origin
// (Cloudflare for SaaS). A new hostname waits, as Cloudflare's does, until
// the name is a CNAME of the zone's: a test makes it active with activate.
func (f *fake) custom(w http.ResponseWriter, r *http.Request, path string, in map[string]any) {
	if f.noSaaS {
		f.refuse(w, http.StatusForbidden, 1404, "Custom hostnames are not enabled for this zone")
		return
	}
	if m := hostPath.FindStringSubmatch(path); m != nil && r.Method == "DELETE" {
		for i, h := range f.hostnames {
			if h["id"] == m[1] {
				f.hostnames = append(f.hostnames[:i], f.hostnames[i+1:]...)
				f.ok(w, map[string]any{"id": m[1]})
				return
			}
		}
		f.refuse(w, http.StatusNotFound, 1436, "The custom hostname was not found")
		return
	}
	switch path + " " + r.Method {
	case "custom_hostnames/fallback_origin GET":
		if f.fallback == nil {
			f.refuse(w, http.StatusNotFound, 1551, "The fallback origin was not found")
			return
		}
		f.ok(w, f.fallback)
	case "custom_hostnames/fallback_origin PUT":
		f.fallback = map[string]any{"origin": in["origin"], "status": "initializing", "errors": []any{}}
		f.ok(w, f.fallback)
	case "custom_hostnames GET":
		// Cloudflare's filter is not an exact one: it gives every name that holds the text.
		out := []any{}
		for _, h := range f.hostnames {
			if strings.Contains(h["hostname"].(string), r.URL.Query().Get("hostname")) {
				out = append(out, h)
			}
		}
		f.ok(w, out)
	case "custom_hostnames POST":
		name, _ := in["hostname"].(string)
		for _, h := range f.hostnames {
			if h["hostname"] == name {
				f.refuse(w, http.StatusConflict, 1406, "Duplicate custom hostname found")
				return
			}
		}
		ssl, _ := in["ssl"].(map[string]any)
		f.seq++
		h := map[string]any{
			"id": fmt.Sprintf("00000000-0000-4000-9000-%012d", f.seq), "hostname": name, "status": "pending", "created_at": "2026-11-15T00:00:00Z",
			"ssl":                    map[string]any{"status": "initializing", "method": ssl["method"], "type": ssl["type"], "wildcard": false},
			"ownership_verification": map[string]any{"type": "txt", "name": "_cf-custom-hostname." + name, "value": "a-made-up-value"},
		}
		f.hostnames = append(f.hostnames, h)
		f.ok(w, h)
	default:
		f.refuse(w, http.StatusNotFound, 7003, "No route for the URI")
	}
}

// hostname is the zone's custom hostname of that name, or nil.
func (f *fake) hostname(name string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.hostnames {
		if h["hostname"] == name {
			return h
		}
	}
	return nil
}

// activate is what Cloudflare does once the name is a CNAME of the zone's:
// the hostname and its certificate become active, and so does the fallback
// origin.
func (f *fake) activate(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fallback != nil {
		f.fallback["status"] = "active"
	}
	for _, h := range f.hostnames {
		if h["hostname"] == name {
			h["status"] = "active"
			h["ssl"].(map[string]any)["status"] = "active"
		}
	}
}

// lapse is what Cloudflare does once the name's CNAME is gone.
func (f *fake) lapse(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.hostnames {
		if h["hostname"] == name {
			h["status"] = "moved"
		}
	}
}

func (f *fake) confirmed(email string) bool {
	for _, a := range f.addresses {
		if strings.EqualFold(a["email"].(string), email) && a["verified"] != nil {
			return true
		}
	}
	return false
}

func (f *fake) account(w http.ResponseWriter, r *http.Request, path string, in map[string]any) {
	q := r.URL.Query()
	switch path + " " + r.Method {
	case "email/routing/addresses GET":
		f.ok(w, f.addresses)
	case "email/routing/addresses POST":
		f.seq++
		a := map[string]any{"id": fmt.Sprintf("addr%d", f.seq), "email": in["email"], "verified": nil}
		f.addresses = append(f.addresses, a)
		f.ok(w, a)
	case "alerting/v3/policies GET":
		f.ok(w, f.alerts)
	case "alerting/v3/policies POST":
		f.t.Errorf("a script made an alert, which is the owner's")
		f.refuse(w, http.StatusForbidden, 10000, "Authentication error")
	case "cfd_tunnel GET":
		out := []any{}
		for _, tn := range f.tunnels {
			if q.Get("name") == "" || tn["name"] == q.Get("name") {
				out = append(out, tn)
			}
		}
		f.ok(w, out)
	case "cfd_tunnel POST":
		f.seq++
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq)
		tn := map[string]any{"id": id, "name": in["name"], "config_src": in["config_src"], "status": "inactive", "tunnel_secret": in["tunnel_secret"]}
		f.tunnels = append(f.tunnels, tn)
		// Cloudflare's answer carries the credential again; a script must not print it.
		f.ok(w, map[string]any{"id": id, "name": in["name"], "config_src": in["config_src"], "status": "inactive",
			"credentials_file": map[string]any{"AccountTag": accountID, "TunnelID": id, "TunnelSecret": in["tunnel_secret"]}})
	default:
		f.refuse(w, http.StatusNotFound, 7003, "No route for the URI")
	}
}

// sent returns the calls made so far and forgets them.
func (f *fake) sent() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

// writes are the calls among cs that change something.
func writes(cs []call) []call {
	var out []call
	for _, c := range cs {
		if c.Method != "GET" {
			out = append(out, c)
		}
	}
	return out
}

func describe(cs []call) string {
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "%s %s %s\n", c.Method, c.Path, c.Body)
	}
	return b.String()
}

// tokenFile is the fake's token in a file only this account reads.
func (f *fake) tokenFile() string {
	f.t.Helper()
	file := filepath.Join(f.t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(f.token+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return file
}

// script runs one of scripts/cloud with no terminal and returns what it
// printed, on either stream, and its exit status. f may be nil: then any call
// to the API fails the test.
func script(t *testing.T, f *fake, env []string, name string, args ...string) (string, int) {
	t.Helper()
	need(t, "bash", "curl", "jq")
	dir := filepath.Join(repo(t), "scripts", "cloud")
	for _, e := range env {
		// A test's own copy of the scripts, with another edge.json.
		if strings.HasPrefix(e, "CLOUDTEST_SCRIPTS=") {
			dir = strings.TrimPrefix(e, "CLOUDTEST_SCRIPTS=")
		}
	}
	cmd := exec.Command(filepath.Join(dir, name), args...)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "KEYCHAIN_SERVICE=cloudtest-no-such-service")
	if f != nil {
		cmd.Env = append(cmd.Env, "LAB_CLOUD_API="+f.url())
	} else {
		cmd.Env = append(cmd.Env, "LAB_CLOUD_API=http://127.0.0.1:1/unreachable")
	}
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("%s: %v\n%s", name, err, out)
	return "", 0
}

// run is script with the fake's token.
func (f *fake) run(name string, args ...string) (string, int) {
	f.t.Helper()
	out, code := script(f.t, f, nil, name, append(args, "--token-file", f.tokenFile())...)
	if strings.Contains(out, f.token) {
		f.t.Errorf("%s printed the token", name)
	}
	return out, code
}
