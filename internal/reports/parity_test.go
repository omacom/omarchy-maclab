package reports

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The corpus both implementations judge. The old site's verdicts on it are
// testdata/rails-verdicts.jsonl, as site/test/integration/report_parity_test.rb
// writes them. With REPORT_PARITY_DIR set, the corpus is written there for
// that test to read; with REPORT_PARITY_VERDICTS set, the verdicts compared
// with are that file's, which is how CI holds the two side by side.

const railsVerdicts = "testdata/rails-verdicts.jsonl"

// The machine id the old site gives the CLI's fixture key under paritySecretKeyBase.
const railsFixtureMachineID = "key:2b91cc19290a39e7e0df"

type parityCase struct {
	path string
	body []byte
}

func parityCorpus(t testing.TB) []parityCase {
	t.Helper()
	var cases []parityCase
	count := map[string]int{}
	add := func(kind, name string, body []byte) {
		count[kind]++
		cases = append(cases, parityCase{fmt.Sprintf("%s/%03d-%s", kind, count[kind], name), body})
	}
	g := func() *Object { return golden(t, "m2-max-image2") }
	// A report signed by machine "a" after it was changed.
	report := func(name string, r *Object) { add("reports", name, encode(t, sign(t, r, "a"))) }
	text := func(name, body string) { add("reports", name, []byte(body)) }
	sent := func(r *Object) string { return string(encode(t, sign(t, r, "a"))) }
	member := func(r *Object, path ...any) *Object { return at(r, path...).(*Object) }
	drop := func(r *Object, key string, path ...any) *Object {
		if len(path) == 0 {
			return r.without(key)
		}
		return put(r, member(r, path...).without(key), path...)
	}
	sig := func(name string, before, after func(*sshsig)) {
		add("reports", name, encode(t, signed(t, g(), "a", NamespaceReport, before, after)))
	}
	armored := func(name, armor string) {
		add("reports", name, encode(t, put(sign(t, g(), "a"), armor, "signature", "signature")))
	}

	// Accepted.
	for _, path := range append(goldenPaths(t, "*.json"), goldenPaths(t, "benchmarks/*.json")...) {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		add("reports", "golden-unsigned-"+name, read(t, path))
		report("golden-"+name, parse(t, read(t, path)))
	}
	for _, path := range goldenPaths(t, "signed/*.json") {
		add("reports", "cli-signed-"+strings.TrimSuffix(filepath.Base(path), ".json"), read(t, path))
	}
	add("reports", "machine-b", encode(t, sign(t, g(), "b")))
	report("large-integers", put(put(g(), bigInteger(t, "12345678901234567890123"), "inventory", "nodes", 0, "count"),
		bigInteger(t, "340282366920938463463374607431768211456"), "inventory", "kernel_config", "omitted"))
	report("awkward-text", put(put(g(), "tab\there \u007f \u2028 \u00e9 \U0001F4BB \"quoted\" back\\slash </script>", "checks", 0, "evidence", 0),
		"7.1.12-\u0001-\u001f-\u00a0", "machine", "kernel"))
	report("evidence-64-kib", withChecks(t, lines(50, strings.Repeat("x", 500)), lines(50, strings.Repeat("x", 500)), append(lines(31, strings.Repeat("x", 500)), strings.Repeat("x", 36))))
	report("model-120-characters", put(g(), "Apple "+strings.Repeat("é", 114), "machine", "model"))
	report("unattended", g().set("unattended", true))
	sig("sha256-signature", func(s *sshsig) { s.hash = "sha256" }, nil)
	sig("reserved-signed", func(s *sshsig) { s.reserved = "kept" }, nil)

	// Not JSON.
	text("not-json-empty", "")
	text("not-json-cut", sent(g())[:2000])
	text("not-json-comment", "/* a report */"+sent(g()))
	text("not-json-member-twice", strings.Replace(sent(g()), `"consent_version":6`, `"consent_version":6,"consent_version":6`, 1))
	text("not-json-trailing-comma", `{"schema_version":1,}`)
	text("not-json-lone-surrogate", strings.Replace(sent(g()), `"kernel":"`, `"kernel":"\ud83d`, 1))
	text("not-json-control-character", strings.Replace(sent(g()), `"kernel":"`, "\"kernel\":\"\t", 1))
	text("not-json-byte-order-mark", "\ufeff"+sent(g()))
	text("not-json-too-deep", strings.Repeat(`{"a":`, 101)+"1"+strings.Repeat("}", 101))

	// Outdated and unsigned come before the schema.
	report("outdated-0", put(g(), integer(0), "schema_version"))
	add("reports", "outdated-unsigned", encode(t, put(g(), integer(0), "schema_version")))
	report("outdated-negative", put(g(), integer(-3), "schema_version"))
	text("outdated-and-nothing-else", `{"schema_version":0}`)
	add("reports", "unsigned-and-wrong", encode(t, drop(g(), "machine")))
	text("unsigned-empty-object", `{}`)

	// The schema.
	text("schema-root-array", `[]`)
	text("schema-root-string", `"report"`)
	text("schema-root-number", `1`)
	text("schema-root-null", `null`)
	text("schema-root-true", `true`)
	report("schema-newer-version", put(g(), integer(2), "schema_version"))
	report("schema-version-string", put(g(), "1", "schema_version"))
	text("schema-version-fraction", strings.Replace(sent(g()), `"schema_version":1`, `"schema_version":0.5`, 1))
	report("schema-missing-two", drop(drop(g(), "machine"), "checks"))
	report("schema-missing-nested", drop(drop(g(), "board", "machine"), "arch", "machine"))
	report("schema-unknown-members", g().set("serial", "C02").set("a/b~c", integer(1)).set("", nil))
	text("schema-unknown-members-in-document-order", `{"zeta":1,"alpha":2,`+sent(g())[1:])
	report("schema-unknown-nested", put(put(g(), "x", "machine", "serial"), "y", "checks", 3, "note"))
	report("schema-const-tool", put(g(), "omarchy-m-tset", "tool", "name"))
	report("schema-const-arch", put(g(), "x86_64", "machine", "arch"))
	report("schema-const-unattended", g().set("unattended", false))
	report("schema-const-answered", put(g(), "yes", "checks", 0, "answered_by_default"))
	report("schema-type-integer", put(g(), "6", "consent_version"))
	text("schema-type-fraction", strings.Replace(sent(g()), `"consent_version":6`, `"consent_version":6.5`, 1))
	report("schema-type-object", put(g(), []any{}, "machine"))
	report("schema-type-array", put(g(), object(), "checks"))
	report("schema-type-string", put(put(g(), integer(7), "machine", "model"), integer(12), "checks", 0, "evidence", 1))
	report("schema-type-null", put(g(), nil, "system", "packages"))
	report("schema-type-bool", put(g(), true, "machine", "kernel"))
	report("schema-minimum", put(put(g(), integer(0), "consent_version"), integer(0), "inventory", "nodes", 2, "count"))
	report("schema-score", put(g(), object("value", integer(1000000001), "unit", "bogomips", "tool", "", "suite", integer(0)), "checks", 0, "score"))
	report("schema-score-negative", put(g(), object("value", integer(-1), "unit", "fps", "tool", "ffmpeg n8.0.1", "suite", integer(1), "run", integer(2)), "checks", 0, "score"))
	report("schema-pattern", put(put(put(g(), "J416c", "machine", "board"), "t60211", "machine", "soc"), "M2 Maxx", "machine", "chip"))
	report("schema-pattern-second-line", put(g(), "omarchy\n", "system", "distro"))
	report("schema-pattern-version", put(g(), "1.0", "tool", "version"))
	report("schema-max-length", put(g(), "Apple "+strings.Repeat("é", 115), "machine", "model"))
	report("schema-min-length", put(g(), "", "machine", "kernel"))
	report("schema-max-length-kernel", put(g(), strings.Repeat("k", 101), "machine", "kernel"))
	report("schema-enum", put(put(put(g(), "3TB", "machine", "storage"), "maybe", "checks", 1, "status"), integer(1), "system", "stack"))
	report("schema-enum-expected", put(g(), object("asahi", "soon", "aurora", "supported", "linux", "yes"), "checks", 2, "classification", "expected"))
	report("schema-min-items", put(g(), []any{}, "checks"))
	report("schema-max-items", put(g(), lines(51, "x"), "checks", 0, "evidence"))
	report("schema-evidence-line-length", put(g(), strings.Repeat("x", 501), "checks", 5, "evidence", 0))
	report("schema-check-shape", put(g(), object("id", "Made.Up", "kind", "robot", "extra", nil), "checks", 4))
	report("schema-check-not-object", put(put(g(), "check", "checks", 0), integer(1), "checks", 1))
	report("schema-many", put(put(put(drop(g().set("zzz", integer(1)), "tool"), "x", "machine", "soc"), "", "system", "distro"), integer(0), "catalogue_version"))
	add("reports", "schema-signature-string", encode(t, g().set("signature", "signed")))
	add("reports", "schema-signature-empty", encode(t, g().set("signature", object())))
	add("reports", "schema-signature-extra", encode(t, put(sign(t, g(), "a"), "me@mac", "signature", "comment")))
	add("reports", "schema-public-key-comment", encode(t, put(sign(t, g(), "a"), testPublicKey("a")+" me@mac", "signature", "public_key")))
	add("reports", "schema-public-key-rsa", encode(t, put(sign(t, g(), "a"), "ssh-rsa AAAAB3NzaC1yc2E", "signature", "public_key")))
	armored("schema-armor-not-base64", "-----BEGIN SSH SIGNATURE-----\nnot base64!\n-----END SSH SIGNATURE-----")
	armored("schema-armor-trailing-line", at(sign(t, g(), "a"), "signature", "signature").(string)+"\n")
	sig("schema-armor-too-long", nil, func(s *sshsig) { s.tail = make([]byte, 600) })
	text("schema-then-signature-float", strings.Replace(sent(g()), `"consent_version":6`, `"consent_version":6.0`, 1))
	text("schema-then-signature-exponent", strings.Replace(sent(g()), `"catalogue_version":19`, `"catalogue_version":1.9e1`, 1))
	text("schema-version-float", strings.Replace(sent(g()), `"schema_version":1`, `"schema_version":1.0`, 1))

	// The signature.
	first := goldenPaths(t, "signed/*.json")[0]
	add("reports", "signature-status-changed", encode(t, put(parse(t, read(t, first)), "fail", "checks", 0, "status")))
	add("reports", "signature-kernel-changed", encode(t, put(parse(t, read(t, first)), "7.1.13-1-ARCH", "machine", "kernel")))
	add("reports", "signature-catalogue-changed", encode(t, put(sign(t, g(), "a"), integer(99), "catalogue_version")))
	add("reports", "signature-member-added", encode(t, sign(t, g(), "a").set("unattended", true)))
	add("reports", "signature-other-key", encode(t, put(sign(t, g(), "mallory"), testPublicKey("a"), "signature", "public_key")))
	blob := unarmor(t, at(sign(t, g(), "a"), "signature", "signature").(string))
	armored("signature-other-namespace", armor(bytes.Replace(blob, []byte("omarchy-m-test-report"), []byte("omarchy-m-test-xxxxxx"), 1)))
	armored("signature-cut-short", armor(blob[:40]))
	armored("signature-cut-in-magic", armor(blob[:3]))
	armored("signature-cut-in-version", armor(blob[:8]))
	armored("signature-trailing-bytes", armor(append(append([]byte(nil), blob...), 'x')))
	armored("signature-armor-bad-base64", "-----BEGIN SSH SIGNATURE-----\nAAA=A\n-----END SSH SIGNATURE-----")
	armored("signature-armor-padding-bits", "-----BEGIN SSH SIGNATURE-----\nU1NIU0lHAB==\n-----END SSH SIGNATURE-----")
	armored("signature-armor-only-newlines", "-----BEGIN SSH SIGNATURE-----\n\n\n-----END SSH SIGNATURE-----")
	armored("signature-armor-one-line", "-----BEGIN SSH SIGNATURE-----\n"+base64.StdEncoding.EncodeToString(blob)+"\n-----END SSH SIGNATURE-----")
	sig("signature-sign-in-namespace", func(s *sshsig) { s.namespace = NamespaceSignIn }, nil)
	sig("signature-unknown-hash", func(s *sshsig) { s.hash = "md5" }, nil)
	sig("signature-not-sshsig", nil, func(s *sshsig) { s.magic = "SSHSIX" })
	sig("signature-version-2", nil, func(s *sshsig) { s.version = 2 })
	sig("signature-rsa-inside", nil, func(s *sshsig) { s.keyType = "ssh-rsa" })
	sig("signature-inner-trailing-bytes", nil, func(s *sshsig) { s.innerTail = []byte{0} })
	sig("signature-63-bytes", nil, func(s *sshsig) { s.signature = s.signature[:63] })
	sig("signature-65-bytes", nil, func(s *sshsig) { s.signature = append(s.signature, 0) })
	sig("signature-zeroes", nil, func(s *sshsig) { s.signature = make([]byte, 64) })
	sig("signature-empty", nil, func(s *sshsig) { s.signature = nil })
	sig("signature-hash-changed-after", nil, func(s *sshsig) { s.hash = "sha256" })
	sig("signature-reserved-changed-after", nil, func(s *sshsig) { s.reserved = "x" })
	longKey := append(pack([]byte("ssh-ed25519")), 0, 0, 0, 33)
	longKey = append(longKey, make([]byte, 32)...)
	add("reports", "signature-public-key-cut-short", encode(t, put(sign(t, g(), "a"), "ssh-ed25519 "+base64.StdEncoding.EncodeToString(longKey), "signature", "public_key")))

	// Check ids, then evidence; the earlier rule answers first.
	report("unknown-check-id", put(g(), "made.up", "checks", 0, "id"))
	report("unknown-check-ids", put(put(put(g(), "zz.last", "checks", 0, "id"), "made.up", "checks", 1, "id"), "zz.last", "checks", 2, "id"))
	report("evidence-too-much", withChecks(t, lines(50, strings.Repeat("x", 500)), lines(50, strings.Repeat("x", 500)), lines(50, strings.Repeat("x", 500))))
	report("evidence-one-byte-too-much", withChecks(t, lines(50, strings.Repeat("x", 500)), lines(50, strings.Repeat("x", 500)), append(lines(31, strings.Repeat("x", 500)), strings.Repeat("x", 37))))
	report("evidence-counted-in-bytes", withChecks(t, lines(50, strings.Repeat("é", 250)), lines(50, strings.Repeat("é", 250)), append(lines(31, strings.Repeat("é", 250)), strings.Repeat("é", 20))))
	report("evidence-not-text-nul", withChecks(t, []any{"ok", "PNG\u0000\u001a"}))
	report("evidence-not-text-newline", withChecks(t, []any{"ok", "two\nlines"}))
	report("evidence-not-text-replacement", withChecks(t, []any{"ok", "caf\ufffd"}))
	report("evidence-not-text-escape", withChecks(t, []any{"\u001b[31mred"}, []any{"fine", "a\bb", "tab\tis text", "del\u007fis text"}))
	report("evidence-ten-problems", withChecks(t, lines(12, "\r"), lines(50, strings.Repeat("x", 500)), lines(50, strings.Repeat("x", 500)), lines(50, strings.Repeat("x", 500))))
	report("unknown-check-id-before-evidence", put(withChecks(t, []any{"two\nlines"}), "made.up", "checks", 0, "id"))
	add("reports", "signature-before-unknown-check-id", encode(t, put(put(sign(t, g(), "a"), "made.up", "checks", 0, "id"), "7.1.13", "machine", "kernel")))
	add("reports", "schema-before-signature", encode(t, put(put(sign(t, g(), "a"), "J416c", "machine", "board"), "7.1.13", "machine", "kernel")))

	// Sign-in documents, judged by their signature alone.
	signIn := func(name string, document *Object) { add("signins", name, encode(t, document)) }
	for _, path := range goldenPaths(t, "sign-in/*.json") {
		add("signins", "golden-"+strings.TrimSuffix(filepath.Base(path), ".json"), read(t, path))
	}
	status := func() *Object { return parse(t, read(t, filepath.Join(goldenDir, "sign-in", "tester-status.json"))) }
	request := func() *Object {
		return object("request_version", integer(1), "request", "status", "requested_at", integer(1790553600), "note", []any{"é", object("b", nil, "a", true)})
	}
	asSignIn := func(before, after func(*sshsig)) *Object {
		return signed(t, request(), "a", NamespaceSignIn, before, after)
	}
	key := func(document *Object, publicKey any) *Object {
		return put(document, publicKey, "signature", "public_key")
	}
	signIn("test-machine", asSignIn(nil, nil))
	signIn("changed", put(status(), integer(1790553601), "requested_at"))
	signIn("member-added", status().set("extra", "x"))
	signIn("report-namespace", signed(t, request(), "a", NamespaceReport, nil, nil))
	signIn("signature-string", request().set("signature", "signed"))
	signIn("signature-null", request().set("signature", nil))
	signIn("signature-array", request().set("signature", []any{}))
	signIn("signature-empty-object", request().set("signature", object()))
	signIn("public-key-missing", put(asSignIn(nil, nil), member(asSignIn(nil, nil), "signature").without("public_key"), "signature"))
	signIn("public-key-number", key(asSignIn(nil, nil), integer(5)))
	signIn("public-key-comment", key(asSignIn(nil, nil), testPublicKey("a")+" me@mac"))
	signIn("public-key-spaced", key(asSignIn(nil, nil), "  ssh-ed25519\t"+strings.Fields(testPublicKey("a"))[1]+" \n"))
	signIn("public-key-rsa", key(asSignIn(nil, nil), "ssh-rsa "+strings.Fields(testPublicKey("a"))[1]))
	signIn("public-key-not-base64", key(asSignIn(nil, nil), "ssh-ed25519 !!!!"))
	signIn("public-key-unpadded", key(asSignIn(nil, nil), "ssh-ed25519 "+strings.Fields(testPublicKey("a"))[1]+"A"))
	signIn("public-key-type-inside", key(asSignIn(nil, nil), "ssh-ed25519 "+base64.StdEncoding.EncodeToString(pack([]byte("ssh-ed25518"), make([]byte, 32)))))
	signIn("public-key-31-bytes", key(asSignIn(nil, nil), "ssh-ed25519 "+base64.StdEncoding.EncodeToString(pack([]byte("ssh-ed25519"), make([]byte, 31)))))
	signIn("public-key-trailing-bytes", key(asSignIn(nil, nil), "ssh-ed25519 "+base64.StdEncoding.EncodeToString(append(testKeyBlob("a"), 0))))
	signIn("public-key-cut-short", key(asSignIn(nil, nil), "ssh-ed25519 "+base64.StdEncoding.EncodeToString(testKeyBlob("a")[:20])))
	signIn("public-key-empty", key(asSignIn(nil, nil), ""))
	signIn("armor-missing", put(asSignIn(nil, nil), member(asSignIn(nil, nil), "signature").without("signature"), "signature"))
	signIn("armor-number", put(asSignIn(nil, nil), integer(1), "signature", "signature"))
	signIn("armor-carriage-returns", put(asSignIn(nil, nil), strings.ReplaceAll(at(asSignIn(nil, nil), "signature", "signature").(string), "\n", "\r\n"), "signature", "signature"))
	signIn("armor-trailing-newline", put(asSignIn(nil, nil), at(asSignIn(nil, nil), "signature", "signature").(string)+"\n", "signature", "signature"))
	signIn("sha256", asSignIn(func(s *sshsig) { s.hash = "sha256" }, nil))
	signIn("unknown-hash", asSignIn(func(s *sshsig) { s.hash = "sha1" }, nil))
	signIn("other-key", key(signed(t, request(), "mallory", NamespaceSignIn, nil, nil), testPublicKey("a")))
	add("signins", "float", []byte(strings.Replace(string(encode(t, asSignIn(nil, nil))), `"requested_at":1790553600`, `"requested_at":1790553600.0`, 1)))

	// Canonical JSON, and what is JSON at all.
	canonical := func(name, document string) { add("canonical", name, []byte(document)) }
	canonical("key-order", `{"b":1,"a":2,"B":3,"aa":4,"":5,"é":6,"z":7,"\ud83d\udcbb":8,"\uffff":9,"a b":10,"a\"":11,"10":12,"9":13}`)
	canonical("nested", `{"z":[{"b":[],"a":{}},[[1,[2,{"y":null,"x":[true,false]}]]]],"a":{"c":{"b":{"a":"deep"}}}}`)
	canonical("whitespace", " \t\r\n{ \"a\" :\t[ 1 ,\n2 ] , \"b\" : \"c d\" }\r\n")
	canonical("integers", `[0,-0,1,-1,9007199254740993,18446744073709551616,-340282366920938463463374607431768211456,1000000000000000000000000000000000000000000]`)
	canonical("escapes", `["\"","\\","\/","\b\f\n\r\t","\u0000\u0001\u001f\u007f\u0080","\u00e9é","\u2028\u2029","\ud83d\ude00😀","\u003c\u003e\u0026<>&'","\uFFFD\ufeff"]`)
	canonical("scalars-string", `"a"`)
	canonical("scalars-integer", `12`)
	canonical("scalars-null", `null`)
	canonical("scalars-true", ` true `)
	canonical("empty-object", `{}`)
	canonical("empty-array", `[]`)
	canonical("float", `{"a":[1,2.0]}`)
	canonical("float-exponent", `1e2`)
	canonical("float-negative-zero", `-0.0`)
	canonical("float-first-of-two-problems", `{"b":1.5,"a":0.5}`)
	canonical("not-json-member-twice", `{"a":1,"a":1}`)
	canonical("not-json-member-twice-escaped", `{"a":1,"\u0061":2}`)
	canonical("not-json-comment-line", "// c\n1")
	canonical("not-json-comment-block", `[1,/* c */2]`)
	canonical("not-json-trailing-comma-array", `[1,]`)
	canonical("not-json-trailing-comma-object", `{"a":1,}`)
	canonical("not-json-lone-high-surrogate", `"\ud800"`)
	canonical("not-json-lone-low-surrogate", `"\udc00"`)
	canonical("not-json-high-surrogate-twice", `"\ud800\ud800"`)
	canonical("not-json-surrogates-swapped", `"\udc00\ud800"`)
	canonical("not-json-control-character", "\"a\nb\"")
	canonical("not-json-byte-order-mark", "\ufeff1")
	canonical("not-json-leading-zero", `01`)
	canonical("not-json-plus", `+1`)
	canonical("not-json-bare-fraction", `.5`)
	canonical("not-json-open-fraction", `1.`)
	canonical("not-json-open-exponent", `1e`)
	canonical("not-json-minus", `-`)
	canonical("not-json-nan", `NaN`)
	canonical("not-json-infinity", `-Infinity`)
	canonical("not-json-two-documents", `1 2`)
	canonical("not-json-form-feed", "\f1")
	canonical("not-json-single-quotes", `{'a':1}`)
	canonical("not-json-bare-key", `{a:1}`)
	canonical("not-json-unknown-escape", `"\x41"`)
	canonical("not-json-short-escape", `"\u12"`)
	canonical("not-json-literal-case", `True`)
	canonical("not-json-literal-cut", `nul`)
	canonical("not-json-empty", ``)
	canonical("not-json-only-whitespace", ` `)
	canonical("not-json-unclosed", `{"a":[1,2]`)
	canonical("not-json-missing-colon", `{"a" 1}`)
	canonical("not-json-missing-comma", `[1 2]`)
	canonical("depth-100-arrays", strings.Repeat("[", 100)+"1"+strings.Repeat("]", 100))
	canonical("depth-101-arrays", strings.Repeat("[", 101)+"1"+strings.Repeat("]", 101))
	canonical("depth-101-arrays-innermost-empty", strings.Repeat("[", 101)+strings.Repeat("]", 101))
	canonical("depth-102-arrays-innermost-empty", strings.Repeat("[", 102)+strings.Repeat("]", 102))
	canonical("depth-100-objects", strings.Repeat(`{"a":`, 100)+"{}"+strings.Repeat("}", 100))
	canonical("depth-101-objects", strings.Repeat(`{"a":`, 101)+"{}"+strings.Repeat("}", 101))
	canonical("depth-mixed-100", strings.Repeat(`[{"a":`, 50)+"null"+strings.Repeat("}]", 50))
	canonical("depth-mixed-101", strings.Repeat(`[{"a":`, 50)+"[0]"+strings.Repeat("}]", 50))
	canonical("depth-siblings", `[`+strings.Repeat(strings.Repeat("[", 99)+"1"+strings.Repeat("]", 99)+",", 3)+`0]`)

	// Machine ids.
	add("keys", "test-machine-a", []byte(testPublicKey("a")))
	add("keys", "test-machine-b", []byte(testPublicKey("b")))
	add("keys", "cli-fixture", read(t, "../../cli/tests/fixtures/test-machine-key.pub"))
	return cases
}

func bigInteger(t testing.TB, digits string) any {
	t.Helper()
	value, err := Parse([]byte(digits))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// parityVerdict is this implementation's verdict on a case, in the shape the
// old site's test writes.
func parityVerdict(t testing.TB, rules *Rules, c parityCase) map[string]any {
	t.Helper()
	verdict := map[string]any{"case": c.path}
	switch kind, _, _ := strings.Cut(c.path, "/"); kind {
	case "reports":
		judged, refusal := rules.Judge(c.body, installURL)
		if refusal != nil {
			verdict["status"] = refusal.Status
			if refusal.Status != 400 {
				verdict["error"], verdict["details"] = refusal.Error, refusal.Details
			}
			break
		}
		sum := sha256.Sum256(judged.Body)
		verdict["status"], verdict["machine_id"], verdict["canonical_sha256"] = 201, judged.MachineID, hex.EncodeToString(sum[:])
	case "signins":
		document, err := Parse(c.body)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		key, err := VerifySignature(document.(*Object), NamespaceSignIn)
		if err != nil {
			verdict["valid"], verdict["reason"] = false, err.Error()
			break
		}
		verdict["valid"], verdict["machine_id"] = true, rules.MachineID(key)
	case "canonical":
		document, err := Parse(c.body)
		if err != nil {
			verdict["not_json"] = true
			break
		}
		text, err := Canonical(document)
		if err != nil {
			verdict["invalid"] = err.Error()
			break
		}
		verdict["canonical"] = string(text)
	case "keys":
		blob, err := base64.StdEncoding.DecodeString(strings.Fields(string(c.body))[1])
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		verdict["machine_id"] = rules.MachineID(blob)
	}
	return verdict
}

func jsonLine(t testing.TB, verdict map[string]any) []byte {
	t.Helper()
	var line bytes.Buffer
	encoder := json.NewEncoder(&line)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(verdict); err != nil {
		t.Fatal(err)
	}
	return line.Bytes()
}

func TestVerdictsAreTheOldSites(t *testing.T) {
	rules := testRules(t)
	cases := parityCorpus(t)
	dir := os.Getenv("REPORT_PARITY_DIR")
	var ours bytes.Buffer
	verdicts := map[string]map[string]any{}
	for _, c := range cases {
		if dir != "" {
			path := filepath.Join(dir, "corpus", c.path)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, c.body, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		line := jsonLine(t, parityVerdict(t, rules, c))
		ours.Write(line)
		var verdict map[string]any
		if err := json.Unmarshal(line, &verdict); err != nil {
			t.Fatal(err)
		}
		verdicts[c.path] = verdict
	}
	if dir != "" {
		if err := os.WriteFile(filepath.Join(dir, "go-verdicts.jsonl"), ours.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	theirs := railsVerdicts
	if path := os.Getenv("REPORT_PARITY_VERDICTS"); path != "" {
		theirs = path
	}
	file, err := os.Open(theirs)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	seen := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var theirs map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &theirs); err != nil {
			t.Fatal(err)
		}
		name, _ := theirs["case"].(string)
		mine, ok := verdicts[name]
		if !ok {
			t.Errorf("%s: the old site judged a case this corpus lacks", name)
			continue
		}
		seen++
		if !reflect.DeepEqual(mine, theirs) {
			t.Errorf("%s:\n  here:     %s  old site: %s", name, jsonLine(t, mine), scanner.Bytes())
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != len(cases) {
		t.Errorf("the old site judged %d of %d cases", seen, len(cases))
	}
}
