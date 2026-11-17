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
	"strings"
	"sync"
	"testing"
)

const (
	aliasScript  = "alias.sh"
	recordScript = "rehearsal-record.sh"
	theAlias     = "maclab.omarchy.org"
	rehearsal    = "alias-rehearsal.omarchy-m-testing.org"
)

// edge.json says what the Alias is: the one name spec section 6 gives it,
// led to the Lab address, and not made until the other side says yes.
func TestEdgeJSONSaysWhatTheAliasIs(t *testing.T) {
	e := readEdge(t)
	if e.Alias.Hostname != theAlias || e.Alias.FallbackOrigin != labHost {
		t.Errorf("the Alias is %q with fallback origin %q", e.Alias.Hostname, e.Alias.FallbackOrigin)
	}
	if e.Alias.Wanted {
		t.Error("edge.json wants the Alias made: it is made the day the other side says yes, after Confirmed")
	}
	spec, err := os.ReadFile(filepath.Join(repo(t), "docs", "spec", "spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"`" + theAlias + "` is an Alias", "A Cloudflare for SaaS custom hostname on `omarchy-mac.org` with the tunnel as fallback origin", "creates the fallback origin and the custom hostname and fails if either drifts"} {
		if !strings.Contains(string(spec), want) {
			t.Errorf("the spec no longer says %q", want)
		}
	}
}

// With nothing there, --apply sets the fallback origin and makes the custom
// hostname with a certificate validated over HTTP, which needs nothing but
// the other side's CNAME; a second run writes nothing.
func TestAliasApplyMakesTheFallbackOriginAndTheCustomHostname(t *testing.T) {
	f := asSpec(t)
	out, code := f.run(aliasScript, "--check")
	if code != 1 || !strings.Contains(out, "drifted  the fallback origin") || !strings.Contains(out, "drifted  the custom hostname "+theAlias) {
		t.Fatalf("a check with nothing there: exit %d\n%s", code, out)
	}
	if w := writes(f.sent()); len(w) != 0 {
		t.Errorf("a check wrote:\n%s", describe(w))
	}

	out, code = f.run(aliasScript, "--apply")
	w := writes(f.sent())
	if code != 0 || len(w) != 2 {
		t.Fatalf("exit %d, wrote:\n%s%s", code, describe(w), out)
	}
	if w[0].Method != "PUT" || !strings.HasSuffix(w[0].Path, "/custom_hostnames/fallback_origin") || w[0].Body != `{"origin":"`+labHost+`"}` {
		t.Errorf("the fallback origin's call: %s %s %s", w[0].Method, w[0].Path, w[0].Body)
	}
	var made struct {
		Hostname string
		SSL      struct{ Method, Type string }
	}
	json.Unmarshal([]byte(w[1].Body), &made)
	if w[1].Method != "POST" || made.Hostname != theAlias || made.SSL.Method != "http" || made.SSL.Type != "dv" {
		t.Errorf("the custom hostname's call: %s %s %s", w[1].Method, w[1].Path, w[1].Body)
	}
	if !strings.Contains(out, "CNAME") || !strings.Contains(out, labHost) || !strings.Contains(out, "DNS only") {
		t.Errorf("it does not say which record the other side adds:\n%s", out)
	}

	// Waiting for the record is drift to a check, and not to the run that made it.
	out, code = f.run(aliasScript, "--check")
	if code != 1 || !strings.Contains(out, "state    "+theAlias+": hostname=pending certificate=initializing method=http") {
		t.Errorf("a check of a hostname that waits: exit %d\n%s", code, out)
	}
	f.sent()
	if out, code := f.run(aliasScript, "--apply"); code != 0 || len(writes(f.sent())) != 0 {
		t.Errorf("a second --apply: exit %d, or it wrote\n%s", code, out)
	}

	f.activate(theAlias)
	out, code = f.run(aliasScript, "--check")
	if code != 0 || !strings.Contains(out, "state    "+theAlias+": hostname=active certificate=active method=http") {
		t.Errorf("a check of an active Alias: exit %d\n%s", code, out)
	}
}

// Each way the Alias can drift is named, and a check never writes.
func TestAliasCheckNamesWhatDrifted(t *testing.T) {
	made := func(t *testing.T) *fake {
		f := asSpec(t)
		if out, code := f.run(aliasScript, "--apply"); code != 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		f.activate(theAlias)
		f.sent()
		return f
	}
	for what, c := range map[string]struct {
		drift func(*fake)
		says  string
	}{
		"the record is gone":             {func(f *fake) { f.lapse(theAlias) }, "hostname=moved"},
		"the certificate is not issued":  {func(f *fake) { f.hostname(theAlias)["ssl"].(map[string]any)["status"] = "pending_validation" }, "certificate=pending_validation"},
		"validated another way":          {func(f *fake) { f.hostname(theAlias)["ssl"].(map[string]any)["method"] = "txt" }, "nothing but the CNAME"},
		"the hostname is gone":           {func(f *fake) { f.hostnames = nil }, "there is none"},
		"the fallback origin is another": {func(f *fake) { f.fallback["origin"] = "elsewhere.omarchy-mac.org" }, "drifted  the fallback origin: is elsewhere.omarchy-mac.org, want " + labHost},
		"the fallback origin is unset":   {func(f *fake) { f.fallback = nil }, "drifted  the fallback origin: there is none"},
		"the fallback origin is not up":  {func(f *fake) { f.fallback["status"] = "pending_deployment" }, "drifted  the fallback origin: is pending_deployment, want active"},
	} {
		f := made(t)
		c.drift(f)
		out, code := f.run(aliasScript, "--check")
		if code != 1 || !strings.Contains(out, c.says) || !strings.Contains(out, "drifted  ") {
			t.Errorf("%s: exit %d, want 1 and %q\n%s", what, code, c.says, out)
		}
		if w := writes(f.sent()); len(w) != 0 {
			t.Errorf("%s: a check wrote:\n%s", what, describe(w))
		}
	}
	// A hostname that is there and not active is never made again or deleted
	// by --apply: only its record brings it back.
	f := made(t)
	f.lapse(theAlias)
	f.sent()
	if out, code := f.run(aliasScript, "--apply"); len(writes(f.sent())) != 0 || len(f.hostnames) != 1 {
		t.Errorf("--apply on a lapsed hostname: exit %d, or it wrote\n%s", code, out)
	}
}

// The filter Cloudflare gives is not exact: a name that only holds the
// Alias's is not the Alias.
func TestAliasIsNotTakenForANameThatHoldsIts(t *testing.T) {
	f := asSpec(t)
	f.fallback = map[string]any{"origin": labHost, "status": "active"}
	f.hostnames = []map[string]any{{"id": "11111111-2222-4333-8444-555555555555", "hostname": "not-" + theAlias, "status": "active", "ssl": map[string]any{"status": "active", "method": "http"}}}
	out, code := f.run(aliasScript, "--check")
	if code != 1 || !strings.Contains(out, "drifted  the custom hostname "+theAlias+": there is none") {
		t.Errorf("exit %d\n%s", code, out)
	}
	out, code = f.run(aliasScript, "--apply", "--remove")
	if w := writes(f.sent()); code != 0 || len(w) != 0 || len(f.hostnames) != 1 {
		t.Errorf("--remove deleted a name that is not the Alias: exit %d\n%s%s", code, describe(w), out)
	}
}

// --remove deletes the custom hostname and leaves the fallback origin, which
// is the lab's own; with --check it wants the hostname gone.
func TestAliasRemoveDeletesTheHostnameAndKeepsTheFallbackOrigin(t *testing.T) {
	f := asSpec(t)
	for _, name := range []string{rehearsal, "alias-rehearsal-third.omarchy-m-testing.org"} {
		if out, code := f.run(aliasScript, "--apply", "--name", name); code != 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
	}
	f.activate(rehearsal)
	if h := f.hostname(theAlias); h != nil || len(f.hostnames) != 2 {
		t.Fatalf("the rehearsal's names made the hostnames %v", f.hostnames)
	}
	out, code := f.run(aliasScript, "--check", "--remove", "--name", rehearsal)
	if code != 1 || !strings.Contains(out, "is still there") {
		t.Errorf("a check that wants it gone: exit %d\n%s", code, out)
	}
	f.sent()
	out, code = f.run(aliasScript, "--apply", "--remove", "--name", rehearsal)
	w := writes(f.sent())
	if code != 0 || len(w) != 1 || w[0].Method != "DELETE" || f.hostname(rehearsal) != nil || len(f.hostnames) != 1 {
		t.Fatalf("exit %d, wrote:\n%s%s", code, describe(w), out)
	}
	if f.fallback == nil || f.fallback["origin"] != labHost {
		t.Error("--remove took the fallback origin away")
	}
	if out, code := f.run(aliasScript, "--check", "--remove", "--name", rehearsal); code != 0 || !strings.Contains(out, "hostname=none") {
		t.Errorf("a check after the removal: exit %d\n%s", code, out)
	}
	if out, code := f.run(aliasScript, "--apply", "--remove", "--name", rehearsal); code != 0 || len(writes(f.sent())) != 0 {
		t.Errorf("a second removal: exit %d, or it wrote\n%s", code, out)
	}
}

// The lab has one Alias. The only other names taken are the rehearsal's,
// under the old site's domain.
func TestAliasTakesTheAliasAndTheRehearsalsNamesOnly(t *testing.T) {
	f := asSpec(t)
	for _, name := range []string{labHost, "www.omarchy-mac.org", "omarchy.org", "example.org", "maclab.omarchy.org.example.org", "omarchy-m-testing.org", "www.omarchy-m-testing.org", "a b.omarchy-m-testing.org", "x.omarchy-m-testing.org/../y", "X.omarchy-m-testing.org;id", ""} {
		out, code := f.run(aliasScript, "--apply", "--name", name)
		if code == 0 || len(f.sent()) != 0 {
			t.Errorf("--name %q was taken: exit %d\n%s", name, code, out)
		}
	}
	if out, code := f.run(aliasScript, "--apply", "--everything"); code == 0 || len(f.sent()) != 0 {
		t.Errorf("an option it does not know: exit %d\n%s", code, out)
	}
}

// Before the owner enters payment details Cloudflare refuses these calls:
// the script says what is missing, with the status and ray id and no body.
func TestAliasSaysWhenCustomHostnamesAreNotEnabled(t *testing.T) {
	f := asSpec(t)
	f.noSaaS = true
	for _, mode := range []string{"--check", "--apply"} {
		out, code := f.run(aliasScript, mode)
		if code == 0 || !strings.Contains(out, "payment details") || !strings.Contains(out, "status 403") || !strings.Contains(out, ray) {
			t.Errorf("%s: exit %d\n%s", mode, code, out)
		}
		if w := writes(f.sent()); len(w) != 0 {
			t.Errorf("%s wrote:\n%s", mode, describe(w))
		}
	}
}

// The zone script leaves the Alias alone until edge.json wants it, and from
// then on fails when the fallback origin or the custom hostname drifts.
func TestTheZoneScriptChecksTheAliasOnceItIsWanted(t *testing.T) {
	f := asSpec(t)
	f.noSaaS = true
	out, code := f.run(zoneScript, "--check")
	if code != 0 || !strings.Contains(out, "ok       the Alias: not made yet") {
		t.Fatalf("with the Alias not wanted: exit %d\n%s", code, out)
	}
	for _, c := range f.sent() {
		if strings.Contains(c.Path, "custom_hostnames") {
			t.Errorf("the zone script read %s before the Alias is wanted", c.Path)
		}
	}

	// A copy of the scripts whose edge.json wants the Alias.
	dir := t.TempDir()
	for _, name := range []string{"lib.sh", zoneScript, aliasScript, "edge.json"} {
		text := source(t, name)
		if name == "edge.json" {
			if !strings.Contains(text, `"wanted": false`) {
				t.Fatal(`edge.json does not say "wanted": false`)
			}
			text = strings.Replace(text, `"wanted": false`, `"wanted": true`, 1)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wanted := func(f *fake, args ...string) (string, int) {
		t.Helper()
		return script(t, f, []string{"CLOUDTEST_SCRIPTS=" + dir}, zoneScript, append(args, "--token-file", f.tokenFile())...)
	}
	f = asSpec(t)
	out, code = wanted(f, "--check")
	if code != 1 || !strings.Contains(out, "drifted  the fallback origin") || !strings.Contains(out, "drifted  the custom hostname "+theAlias) {
		t.Fatalf("with the Alias wanted and not there: exit %d\n%s", code, out)
	}
	f.sent()
	out, code = wanted(f, "--apply", "--forward-to", mailbox)
	if w := writes(f.sent()); len(w) != 2 || f.hostname(theAlias) == nil || f.fallback == nil {
		t.Fatalf("--apply with the Alias wanted: exit %d, wrote:\n%s%s", code, describe(w), out)
	}
	f.activate(theAlias)
	if out, code := wanted(f, "--check"); code != 0 || !strings.Contains(out, "ok       the custom hostname "+theAlias) {
		t.Errorf("with the Alias active: exit %d\n%s", code, out)
	}
	f.lapse(theAlias)
	if out, code := wanted(f, "--check"); code != 1 || !strings.Contains(out, "drifted  the custom hostname "+theAlias) {
		t.Errorf("with the Alias lapsed: exit %d\n%s", code, out)
	}
}

// porkbun answers as Porkbun's API does for the old site's domain, whose
// DNS it still holds while the rehearsal runs.
type porkbun struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	records []map[string]string
	calls   []call
	seq     int
}

const (
	porkbunKey    = "pk1_made-up-for-the-test"
	porkbunSecret = "sk1_made-up-for-the-test"
	oldDomain     = "omarchy-m-testing.org"
)

func newPorkbun(t *testing.T) *porkbun {
	t.Helper()
	need(t, "bash", "curl", "jq")
	p := &porkbun{t: t}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *porkbun) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	defer p.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/json/v3")
	p.calls = append(p.calls, call{Method: r.Method, Path: path, Body: string(body)})
	w.Header().Set("Content-Type", "application/json")
	answer := func(v map[string]any) { json.NewEncoder(w).Encode(v) }
	if r.Header.Get("X-API-Key") != porkbunKey || r.Header.Get("X-Secret-API-Key") != porkbunSecret {
		w.WriteHeader(http.StatusForbidden)
		answer(map[string]any{"status": "ERROR", "message": "Invalid API key."})
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/dns/"), "/")
	if !strings.HasPrefix(path, "/dns/") || len(parts) < 2 || parts[1] != oldDomain {
		w.WriteHeader(http.StatusBadRequest)
		answer(map[string]any{"status": "ERROR", "message": "Invalid domain."})
		return
	}
	full := func(sub string) string { return sub + "." + oldDomain }
	switch {
	case parts[0] == "retrieveByNameType" && len(parts) == 4:
		out := []any{}
		for _, rec := range p.records {
			if rec["type"] == parts[2] && rec["name"] == full(parts[3]) {
				out = append(out, rec)
			}
		}
		answer(map[string]any{"status": "SUCCESS", "records": out})
	case parts[0] == "create" && len(parts) == 2:
		var in map[string]string
		json.Unmarshal(body, &in)
		p.seq++
		p.records = append(p.records, map[string]string{"id": fmt.Sprint(1000 + p.seq), "name": full(in["name"]), "type": in["type"], "content": in["content"], "ttl": in["ttl"]})
		answer(map[string]any{"status": "SUCCESS", "id": 1000 + p.seq})
	case parts[0] == "deleteByNameType" && len(parts) == 4:
		var kept []map[string]string
		for _, rec := range p.records {
			if !(rec["type"] == parts[2] && rec["name"] == full(parts[3])) {
				kept = append(kept, rec)
			}
		}
		p.records = kept
		answer(map[string]any{"status": "SUCCESS"})
	default:
		w.WriteHeader(http.StatusBadRequest)
		answer(map[string]any{"status": "ERROR", "message": "Invalid request."})
	}
}

func (p *porkbun) sent() []call {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.calls
	p.calls = nil
	return out
}

// run is the record's script with Porkbun's stand-in and made-up keys. It
// calls Cloudflare for nothing.
func (p *porkbun) run(args ...string) (string, int) {
	p.t.Helper()
	// The stand-in answers on its own goroutines: taking its lock before and
	// after the script orders the test's plain use of p.records with it.
	p.mu.Lock()
	p.mu.Unlock()
	out, code := script(p.t, nil, []string{"LAB_PORKBUN_API=" + p.srv.URL + "/api/json/v3"}, recordScript, append(args, "--porkbun-keys", porkbunKeys(p.t, 0o600))...)
	for _, secret := range []string{porkbunKey, porkbunSecret} {
		if strings.Contains(out, secret) {
			p.t.Errorf("%s printed a key", recordScript)
		}
	}
	p.mu.Lock()
	p.mu.Unlock()
	return out, code
}

// The rehearsal's one record: a CNAME of the Lab address under the old
// site's domain, made at Porkbun, checked and taken away again.
func TestTheRehearsalsRecordIsMadeCheckedAndRemoved(t *testing.T) {
	p := newPorkbun(t)
	out, code := p.run("--check", "--name", rehearsal)
	if code != 1 || !strings.Contains(out, "drifted  the record of "+rehearsal+": there is none") {
		t.Fatalf("a check with no record: exit %d\n%s", code, out)
	}
	for _, c := range p.sent() {
		if !strings.Contains(c.Path, "retrieve") {
			t.Errorf("a check called %s", c.Path)
		}
	}
	out, code = p.run("--apply", "--name", rehearsal)
	if code != 0 || len(p.records) != 1 {
		t.Fatalf("exit %d, records %v\n%s", code, p.records, out)
	}
	if rec := p.records[0]; rec["name"] != rehearsal || rec["type"] != "CNAME" || rec["content"] != labHost || rec["ttl"] != "600" {
		t.Errorf("the record: %v", rec)
	}
	p.sent()
	if out, code := p.run("--apply", "--name", rehearsal); code != 0 || len(p.records) != 1 {
		t.Errorf("a second --apply: exit %d, records %v\n%s", code, p.records, out)
	}
	for _, c := range p.sent() {
		if strings.Contains(c.Path, "create") {
			t.Error("a second --apply made the record again")
		}
	}
	if out, code := p.run("--check", "--name", rehearsal); code != 0 {
		t.Errorf("a check of the record: exit %d\n%s", code, out)
	}
	if out, code := p.run("--check", "--remove", "--name", rehearsal); code != 1 || !strings.Contains(out, "is still there") {
		t.Errorf("a check that wants it gone: exit %d\n%s", code, out)
	}
	if out, code := p.run("--apply", "--remove", "--name", rehearsal); code != 0 || len(p.records) != 0 {
		t.Errorf("the removal: exit %d, records %v\n%s", code, p.records, out)
	}
	if out, code := p.run("--check", "--remove", "--name", rehearsal); code != 0 {
		t.Errorf("a check after the removal: exit %d\n%s", code, out)
	}
}

// A record of that name which is not the rehearsal's is neither written
// over nor deleted, and only a throwaway name under the old site's domain
// is taken: never the site itself.
func TestTheRehearsalsRecordNeverTouchesAnotherRecord(t *testing.T) {
	p := newPorkbun(t)
	p.records = []map[string]string{{"id": "7", "name": rehearsal, "type": "CNAME", "content": "someone.up.railway.app", "ttl": "600"}}
	for _, args := range [][]string{{"--apply"}, {"--apply", "--remove"}} {
		out, code := p.run(append(args, "--name", rehearsal)...)
		if code == 0 || len(p.records) != 1 || p.records[0]["content"] != "someone.up.railway.app" || !strings.Contains(out, "not the rehearsal's") {
			t.Errorf("%v: exit %d, records %v\n%s", args, code, p.records, out)
		}
	}
	p.records = nil
	p.sent()
	for _, name := range []string{oldDomain, "www." + oldDomain, "a.b." + oldDomain, "x.omarchy-mac.org", theAlias, labHost, "x." + oldDomain + ".example.org", "X;id." + oldDomain, ""} {
		out, code := p.run("--apply", "--name", name)
		if code == 0 || len(p.sent()) != 0 || len(p.records) != 0 {
			t.Errorf("--name %q was taken: exit %d\n%s", name, code, out)
		}
	}
	if out, code := p.run("--apply"); code == 0 || len(p.sent()) != 0 {
		t.Errorf("with no name: exit %d\n%s", code, out)
	}
}

// Porkbun's stand-in is the tests' and takes a loopback address only.
func TestThePorkbunStandInTakesOnlyLoopback(t *testing.T) {
	for _, api := range []string{"https://example.org/api/json/v3", "http://127.0.0.1:1@lab.invalid/api", "http://127.0.0.1:/api"} {
		out, code := script(t, nil, []string{"LAB_PORKBUN_API=" + api}, recordScript, "--check", "--name", rehearsal, "--porkbun-keys", porkbunKeys(t, 0o600))
		if code == 0 || !strings.Contains(out, "loopback") {
			t.Errorf("LAB_PORKBUN_API=%s: exit %d\n%s", api, code, out)
		}
	}
	if !strings.Contains(source(t, "lib.sh"), `PORKBUN_API="https://api.porkbun.com/api/json/v3"`) {
		t.Error("lib.sh does not name Porkbun's API")
	}
}

// porkbunKeys is a file of the made-up keys, API key then secret key, one
// to a line, with that mode.
func porkbunKeys(t testing.TB, mode os.FileMode) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "porkbun.keys")
	if err := os.WriteFile(file, []byte(porkbunKey+"\n"+porkbunSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, mode); err != nil {
		t.Fatal(err)
	}
	return file
}

// Porkbun's two keys come from a file only its owner reads, as the
// Cloudflare token does: never from the environment or the Keychain, and a
// file others can read is refused before anything is sent.
func TestPorkbunsKeysComeFromAFileOnlyItsOwnerReads(t *testing.T) {
	p := newPorkbun(t)
	api := "LAB_PORKBUN_API=" + p.srv.URL + "/api/json/v3"
	refused := func(what string, env []string, args ...string) {
		t.Helper()
		out, code := script(t, nil, append([]string{api}, env...), recordScript, append([]string{"--check", "--name", rehearsal}, args...)...)
		if n := len(p.sent()); code == 0 || n != 0 {
			t.Errorf("%s: exit %d, %d calls to Porkbun\n%s", what, code, n, out)
		}
		if strings.Contains(out, porkbunKey) || strings.Contains(out, porkbunSecret) {
			t.Errorf("%s: a key was printed", what)
		}
	}
	refused("keys in the environment", []string{"PORKBUN_API_KEY=" + porkbunKey, "PORKBUN_SECRET_KEY=" + porkbunSecret})
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660} {
		refused(fmt.Sprintf("a file of mode %o", mode), nil, "--porkbun-keys", porkbunKeys(t, mode))
	}
	refused("no such file", nil, "--porkbun-keys", filepath.Join(t.TempDir(), "none"))
	half := porkbunKeys(t, 0o600)
	os.WriteFile(half, []byte(porkbunKey+"\n"), 0o600)
	refused("a file with one key", nil, "--porkbun-keys", half)

	// The keys file by default: ~/.config/omarchy-mac-lab/porkbun.keys.
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".config", "omarchy-mac-lab"), 0o700)
	if err := os.Rename(porkbunKeys(t, 0o600), filepath.Join(home, ".config", "omarchy-mac-lab", "porkbun.keys")); err != nil {
		t.Fatal(err)
	}
	if out, code := script(t, nil, []string{api, "HOME=" + home}, recordScript, "--check", "--name", rehearsal); code != 1 || len(p.sent()) != 1 || strings.Contains(out, "Invalid API key") {
		t.Errorf("with the file in its place: exit %d\n%s", code, out)
	}
	if strings.Contains(source(t, "lib.sh"), "security find-generic-password") {
		t.Error("lib.sh still reads the Keychain")
	}
}

// withACL gives file an access control list that lets another account read
// it, with the file's mode left as it is.
func withACL(t testing.TB, file string) {
	t.Helper()
	if exec.Command("chmod", "+a", "everyone allow read", file).Run() == nil {
		return
	}
	if exec.Command("setfacl", "-m", "u:nobody:r", file).Run() == nil {
		return
	}
	t.Skip("no access control lists here")
}

// A 0600 credential file whose ACL lets another account read it is
// refused before anything is sent: the Porkbun keys and the Cloudflare
// token alike. On macOS the mode column shows @ for any extended
// attribute, and then no + for the ACL.
func TestACredentialFileWithAnACLIsRefused(t *testing.T) {
	p := newPorkbun(t)
	keys := porkbunKeys(t, 0o600)
	withACL(t, keys)
	out, code := script(t, nil, []string{"LAB_PORKBUN_API=" + p.srv.URL + "/api/json/v3"}, recordScript, "--check", "--name", rehearsal, "--porkbun-keys", keys)
	if n := len(p.sent()); code == 0 || n != 0 || !strings.Contains(out, "ACL") {
		t.Errorf("Porkbun keys with an ACL: exit %d, %d calls to Porkbun\n%s", code, n, out)
	}

	f := asSpec(t)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte(f.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withACL(t, token)
	for _, name := range []string{zoneScript, tunnelScript} {
		out, code := script(t, f, nil, name, "--check", "--token-file", token)
		if code == 0 || !strings.Contains(out, "ACL") {
			t.Errorf("%s took a token file with an ACL: exit %d\n%s", name, code, out)
		}
	}
	if n := len(f.sent()); n != 0 {
		t.Errorf("%d calls were made with a token others may read", n)
	}
}
