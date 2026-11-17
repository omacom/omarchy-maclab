package cloudtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	oldZoneID   = "abcdef0123456789abcdef0123456789"
	oldScript   = "old-name.sh"
	purgeScript = "purge-cache.sh"
	redirectAt  = "rulesets/phases/http_request_dynamic_redirect/entrypoint"
)

// oldZone is the old name's zone at Cloudflare.
type oldZone struct {
	status      string
	certificate bool
	dnssec      string
	records     []map[string]any
	redirect    []map[string]any
	hasRules    bool
}

func (z *oldZone) zone() map[string]any {
	return map[string]any{"id": oldZoneID, "name": oldDomain, "status": z.status, "account": map[string]any{"id": accountID},
		"name_servers": []string{"ada.ns.cloudflare.com", "bob.ns.cloudflare.com"}}
}

func (f *fake) oldName(w http.ResponseWriter, r *http.Request, path string, in map[string]any) {
	z := f.old
	switch path + " " + r.Method {
	case "dns_records GET":
		out := []any{}
		for _, rec := range z.records {
			out = append(out, rec)
		}
		f.ok(w, out)
	case "dns_records POST":
		f.seq++
		in["id"] = fmt.Sprintf("old%d", f.seq)
		z.records = append(z.records, in)
		f.ok(w, in)
	case "ssl/certificate_packs GET":
		out := []any{}
		if z.certificate {
			out = append(out, map[string]any{"id": "pack1", "type": "universal", "status": "active", "hosts": []string{oldDomain, "*." + oldDomain}})
		}
		f.ok(w, out)
	case "dnssec GET":
		f.ok(w, map[string]any{"status": z.dnssec})
	case redirectAt + " GET":
		if !z.hasRules {
			f.refuse(w, http.StatusNotFound, 10003, "could not find entrypoint ruleset in the http_request_dynamic_redirect phase")
			return
		}
		f.ok(w, map[string]any{"id": "rs1", "rules": f.stored(z.redirect)})
	case redirectAt + " PUT":
		b, _ := json.Marshal(in["rules"])
		z.redirect = nil
		json.Unmarshal(b, &z.redirect)
		z.hasRules = true
		f.ok(w, map[string]any{"id": "rs1", "rules": f.stored(z.redirect)})
	default:
		if r.Method != "GET" {
			f.t.Errorf("a script sent %s %s to the old name's zone", r.Method, path)
		}
		f.refuse(w, http.StatusNotFound, 7003, "No route for the URI")
	}
}

// registrar answers as Porkbun does for the old name's records.
func (f *fake) registrar(w http.ResponseWriter, r *http.Request, path string) {
	w.Header().Set("Content-Type", "application/json")
	if path != "dns/retrieve/"+oldDomain || r.Method != "POST" || r.Header.Get("X-API-Key") != porkbunKey || r.Header.Get("X-Secret-API-Key") != porkbunSecret {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"status": "ERROR", "message": "Invalid API key."})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"status": "SUCCESS", "records": f.porkbun})
}

// atPorkbun is what the registrar serves for the old name today: the site on
// Railway at the apex and www, Railway's proof, mail, and its own NS.
func atPorkbun() []map[string]any {
	return []map[string]any{
		{"id": "1", "name": oldDomain, "type": "ALIAS", "content": "abcd1234.up.railway.app", "ttl": "600", "prio": "0"},
		{"id": "2", "name": "www." + oldDomain, "type": "CNAME", "content": "abcd1234.up.railway.app", "ttl": "600", "prio": ""},
		{"id": "3", "name": "_railway-verify." + oldDomain, "type": "TXT", "content": "railway-verify=made-up", "ttl": "600", "prio": ""},
		{"id": "4", "name": oldDomain, "type": "MX", "content": "fwd1.porkbun.com", "ttl": "600", "prio": "10"},
		{"id": "5", "name": oldDomain, "type": "NS", "content": "maceio.ns.porkbun.com", "ttl": "86400", "prio": ""},
		{"id": "6", "name": oldDomain, "type": "NS", "content": "salvador.ns.porkbun.com", "ttl": "86400", "prio": ""},
	}
}

// old runs old-name.sh with the fake's token and made-up keys for Porkbun.
func (f *fake) oldRun(args ...string) (string, int) {
	f.t.Helper()
	env := []string{"LAB_PORKBUN_API=" + f.srv.URL + "/client/v4/porkbun", "CLOUDFLARE_ACCOUNT_ID=" + accountID}
	out, code := script(f.t, f, env, oldScript, append(args, "--token-file", f.tokenFile(), "--porkbun-keys", porkbunKeys(f.t, 0o600))...)
	for _, secret := range []string{f.token, porkbunKey, porkbunSecret} {
		if strings.Contains(out, secret) {
			f.t.Errorf("%s printed a credential", oldScript)
		}
	}
	return out, code
}

func TestTheOldNamesDryRunsPrintTheCallsAndSendNothing(t *testing.T) {
	f := asSpec(t)
	var all string
	for _, what := range []string{"zone", "redirect", "dnssec"} {
		out, code := script(t, f, nil, oldScript, what)
		if code != 0 || !strings.Contains(out, "nothing is sent") {
			t.Fatalf("old-name.sh %s: exit %d\n%s", what, code, out)
		}
		all += out
	}
	if n := len(f.sent()); n != 0 {
		t.Errorf("the dry runs made %d calls", n)
	}
	golden := filepath.Join(repo(t), "internal", "cloudtest", "testdata", "old-name.dry-run.txt")
	if os.Getenv("CLOUDTEST_UPDATE") != "" {
		if err := os.WriteFile(golden, []byte(all), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want, _ := os.ReadFile(golden); all != string(want) {
		t.Errorf("the dry runs are not what %s holds:\n%s", filepath.Base(golden), all)
	}
	for _, args := range [][]string{nil, {"everything"}, {"zone", "redirect"}} {
		if out, code := script(t, f, nil, oldScript, args...); code != 2 {
			t.Errorf("old-name.sh %q: exit %d\n%s", args, code, out)
		}
	}
}

// Step 19: the zone is made in the lab's account with the records the
// registrar serves, the apex and www proxied and still pointing at Railway;
// no nameserver moves; the check passes only once Cloudflare shows the zone
// active and its certificate issued.
func TestTheOldNamesZoneGetsTheSameRecordsAndIsCheckedActive(t *testing.T) {
	f := asSpec(t)
	f.porkbun = atPorkbun()
	if out, code := f.oldRun("zone", "--check"); code == 0 || !strings.Contains(out, "has no zone "+oldDomain) || len(writes(f.sent())) != 0 {
		t.Fatalf("--check with no zone: exit %d\n%s", code, out)
	}
	out, code := f.oldRun("zone", "--apply")
	if f.old == nil {
		t.Fatalf("no zone was made: exit %d\n%s", code, out)
	}
	// Made, filled, and not active yet: the apply ends as its check does.
	if code != 1 || !strings.Contains(out, "the zone is pending, not active") || !strings.Contains(out, "no certificate is issued") {
		t.Errorf("--apply on the day's first run: exit %d\n%s", code, out)
	}
	got := map[string]map[string]any{}
	for _, rec := range f.old.records {
		got[fmt.Sprint(rec["type"], " ", rec["name"])] = rec
	}
	if len(got) != 4 {
		t.Fatalf("the zone holds %d records, want the registrar's four that are not its own NS: %v", len(got), f.old.records)
	}
	apex, www, proof, mail := got["CNAME "+oldDomain], got["CNAME www."+oldDomain], got["TXT _railway-verify."+oldDomain], got["MX "+oldDomain]
	if apex == nil || apex["content"] != "abcd1234.up.railway.app" || apex["proxied"] != true || www == nil || www["content"] != "abcd1234.up.railway.app" || www["proxied"] != true {
		t.Errorf("the apex and www do not point at Railway through Cloudflare: %v %v", apex, www)
	}
	if proof == nil || proof["content"] != "railway-verify=made-up" || proof["proxied"] != nil || mail == nil || mail["priority"] != float64(10) {
		t.Errorf("the other records are not as they were: %v %v", proof, mail)
	}
	for _, c := range writes(f.sent()) {
		if strings.Contains(c.Path, "porkbun") && !strings.HasSuffix(c.Path, "/dns/retrieve/"+oldDomain) {
			t.Errorf("the script changed something at the registrar: %s %s", c.Method, c.Path)
		}
		if strings.Contains(c.Path, redirectAt) || strings.Contains(c.Path, "dnssec") {
			t.Errorf("step 19 sent %s %s", c.Method, c.Path)
		}
	}
	if !strings.Contains(out, "nameservers.sh --apply") || !strings.Contains(out, "ada.ns.cloudflare.com bob.ns.cloudflare.com") {
		t.Errorf("the owner is not told the pair and how to move the nameservers:\n%s", out)
	}

	// A second run adds nothing.
	before := len(f.old.records)
	if out, _ := f.oldRun("zone", "--apply"); len(f.old.records) != before || !strings.Contains(out, "0 record(s) added") {
		t.Errorf("a second run added records:\n%s", out)
	}
	if w := writes(f.sent()); len(w) != 1 || !strings.HasSuffix(w[0].Path, "/dns/retrieve/"+oldDomain) {
		t.Errorf("a second run sent: %s", describe(w))
	}
	f.old.status = "active"
	if out, code := f.oldRun("zone", "--check"); code != 1 || !strings.Contains(out, "no certificate is issued") || strings.Contains(out, "not active") {
		t.Errorf("active with no certificate: exit %d\n%s", code, out)
	}
	f.old.certificate = true
	f.sent()
	if out, code := f.oldRun("zone", "--check"); code != 0 || !strings.Contains(out, "active and its certificate issued") || len(writes(f.sent())) != 0 {
		t.Errorf("active with its certificate: exit %d\n%s", code, out)
	}
}

// Step 25: the one 308 rule, apex and www, path and query kept. It is set
// once, checked as written, and nothing else on the zone is touched.
func TestTheOldNamesRedirectIsOneRuleSetOnce(t *testing.T) {
	f := asSpec(t)
	if out, code := f.oldRun("redirect", "--apply"); code == 0 || !strings.Contains(out, "has no zone") {
		t.Fatalf("with no zone: exit %d\n%s", code, out)
	}
	f.old = &oldZone{status: "active", certificate: true, dnssec: "active"}
	if out, code := f.oldRun("redirect", "--check"); code != 1 || !strings.Contains(out, "no redirect rule") || len(writes(f.sent())) != 0 {
		t.Fatalf("--check before the opening: exit %d\n%s", code, out)
	}
	out, code := f.oldRun("redirect", "--apply")
	if code != 0 || !strings.Contains(out, "answer 308 to the same path and query at https://"+labHost) {
		t.Fatalf("--apply: exit %d\n%s", code, out)
	}
	if len(f.old.redirect) != 1 {
		t.Fatalf("%d redirect rules", len(f.old.redirect))
	}
	rule := f.old.redirect[0]
	from := rule["action_parameters"].(map[string]any)["from_value"].(map[string]any)
	if rule["action"] != "redirect" || rule["enabled"] != true || from["status_code"] != float64(308) || from["preserve_query_string"] != true ||
		from["target_url"].(map[string]any)["expression"] != `concat("https://`+labHost+`", http.request.uri.path)` ||
		rule["expression"] != `(http.host eq "`+oldDomain+`" or http.host eq "www.`+oldDomain+`")` {
		t.Errorf("the rule: %v", rule)
	}
	w := writes(f.sent())
	if len(w) != 1 || w[0].Method != "PUT" || !strings.HasSuffix(w[0].Path, redirectAt) {
		t.Errorf("the opening's redirect sent: %s", describe(w))
	}
	if out, code := f.oldRun("redirect", "--apply"); code != 0 || len(writes(f.sent())) != 0 {
		t.Errorf("a second --apply wrote again: exit %d\n%s", code, out)
	}
	// A rule someone changed is named, and a second rule too.
	from["status_code"] = float64(302)
	if out, code := f.oldRun("redirect", "--check"); code != 1 || !strings.Contains(out, "not as written") {
		t.Errorf("a 302: exit %d\n%s", code, out)
	}
	f.old.redirect = append(f.old.redirect, map[string]any{"expression": "true", "action": "redirect"})
	if out, code := f.oldRun("redirect", "--check"); code != 1 || !strings.Contains(out, "2 redirect rules") {
		t.Errorf("two rules: exit %d\n%s", code, out)
	}
	// The lab's own zone still has no redirect rule.
	if out, code := f.run(zoneScript, "--check"); code != 0 {
		t.Errorf("the lab's zone after the old name's redirect: exit %d\n%s", code, out)
	}
}

func TestDNSSECOnTheOldNameIsCheckedAndNeverSet(t *testing.T) {
	f := asSpec(t)
	f.old = &oldZone{status: "active", dnssec: "pending"}
	if out, code := f.oldRun("dnssec", "--check"); code != 1 || !strings.Contains(out, "DNSSEC is pending") || !strings.Contains(out, "The owner sets it") {
		t.Errorf("pending: exit %d\n%s", code, out)
	}
	f.sent()
	if out, code := f.oldRun("dnssec", "--apply"); code != 1 || !strings.Contains(out, "the owner's to turn on") || len(f.sent()) != 0 {
		t.Errorf("--apply: exit %d, %d calls\n%s", code, len(f.sent()), out)
	}
	f.old.dnssec = "active"
	f.sent()
	if out, code := f.oldRun("dnssec", "--check"); code != 0 || !strings.Contains(out, "DNSSEC is active") {
		t.Errorf("active: exit %d\n%s", code, out)
	}
	if w := writes(f.sent()); len(w) != 0 {
		t.Errorf("a write was sent: %s", describe(w))
	}
}

// The one purge: everything, on the lab's zone, only with --apply.
func TestThePurgeIsEverythingOnTheLabsZoneAndOnlyWhenApplied(t *testing.T) {
	f := asSpec(t)
	if out, code := f.run(purgeScript, "--check"); code != 0 || !strings.Contains(out, "Nothing was purged") || len(f.purges) != 0 {
		t.Fatalf("--check: exit %d\n%s", code, out)
	}
	out, code := f.run(purgeScript, "--apply")
	if code != 0 || len(f.purges) != 1 || f.purges[0] != `{"purge_everything":true}` || !strings.Contains(out, "purged everything") {
		t.Fatalf("--apply: exit %d, purges %q\n%s", code, f.purges, out)
	}
	if w := writes(f.sent()); len(w) != 1 || !strings.HasSuffix(w[0].Path, "/zones/"+zoneID+"/purge_cache") {
		t.Errorf("the purge sent: %s", describe(w))
	}
	f.noZone = true
	if out, code := f.run(purgeScript, "--apply"); code == 0 || len(f.purges) != 1 {
		t.Errorf("with no zone: exit %d\n%s", code, out)
	}
}
