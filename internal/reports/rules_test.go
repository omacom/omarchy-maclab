package reports

import (
	"bytes"
	"crypto/ed25519"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Translated from site/test/integration/machine_signature_test.rb and
// report_evidence_test.rb, with the helpers of site/test/test_helper.rb.

const (
	installURL = "http://www.example.com/install"
	// Stands in for the old site's SECRET_KEY_BASE in every comparison with it.
	paritySecretKeyBase = "report parity: not a secret"
	goldenDir           = "../../schema/golden"
)

// The machine-id key as Rails derives it: key_generator.generate_key("report machine id from key").
func railsMachineIDKey(t testing.TB, secretKeyBase string) []byte {
	t.Helper()
	key, err := pbkdf2.Key(sha256.New, secretKeyBase, []byte("report machine id from key"), 1000, 64)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testRules(t testing.TB) *Rules {
	t.Helper()
	rules, err := NewRules(railsMachineIDKey(t, paritySecretKeyBase))
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func goldenPaths(t testing.TB, pattern string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(goldenDir, pattern))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	return paths
}

func read(t testing.TB, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func parse(t testing.TB, data []byte) *Object {
	t.Helper()
	value, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return value.(*Object)
}

func golden(t testing.TB, name string) *Object {
	t.Helper()
	return parse(t, read(t, filepath.Join(goldenDir, name+".json")))
}

// encode writes a document without floats as JSON.
func encode(t testing.TB, value any) []byte {
	t.Helper()
	text, err := Canonical(value)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func (o *Object) set(key string, value any) *Object {
	o.put(key, value)
	return o
}

func (o *Object) clone() *Object {
	copy := &Object{keys: append([]string(nil), o.keys...), members: make(map[string]any, len(o.members))}
	for key, value := range o.members {
		copy.members[key] = value
	}
	return copy
}

func integer(n int64) *big.Int { return big.NewInt(n) }

func object(pairs ...any) *Object {
	o := &Object{members: map[string]any{}}
	for i := 0; i < len(pairs); i += 2 {
		o.set(pairs[i].(string), pairs[i+1])
	}
	return o
}

// at follows member names and array indexes into a document.
func at(value any, path ...any) any {
	for _, step := range path {
		switch step := step.(type) {
		case string:
			value = value.(*Object).members[step]
		case int:
			value = value.([]any)[step]
		}
	}
	return value
}

// put sets the value at a path, in place.
func put(root *Object, value any, path ...any) *Object {
	parent := at(root, path[:len(path)-1]...)
	switch last := path[len(path)-1].(type) {
	case string:
		parent.(*Object).set(last, value)
	case int:
		parent.([]any)[last] = value
	}
	return root
}

// Test machines: an ed25519 key per name, made from the name.
func testMachine(name string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("test machine " + name))
	return ed25519.NewKeyFromSeed(seed[:])
}

func pack(parts ...[]byte) []byte {
	var out []byte
	for _, part := range parts {
		out = binary.BigEndian.AppendUint32(out, uint32(len(part)))
		out = append(out, part...)
	}
	return out
}

func testKeyBlob(name string) []byte {
	return pack([]byte("ssh-ed25519"), testMachine(name).Public().(ed25519.PublicKey))
}

func testPublicKey(name string) string {
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(testKeyBlob(name))
}

func armor(blob []byte) string {
	text := base64.StdEncoding.EncodeToString(blob)
	var lines []string
	for len(text) > 70 {
		lines = append(lines, text[:70])
		text = text[70:]
	}
	lines = append(lines, text)
	return "-----BEGIN SSH SIGNATURE-----\n" + strings.Join(lines, "\n") + "\n-----END SSH SIGNATURE-----"
}

func unarmor(t testing.TB, armored string) []byte {
	t.Helper()
	lines := strings.Split(armored, "\n")
	blob, err := base64.StdEncoding.DecodeString(strings.Join(lines[1:len(lines)-1], ""))
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

// sshsig is an SSH signature's parts, to build right and wrong ones from.
type sshsig struct {
	magic     string
	version   uint32
	keyBlob   []byte
	namespace string
	reserved  string
	hash      string
	keyType   string
	signature []byte
	innerTail []byte
	tail      []byte
}

func (s sshsig) blob() []byte {
	out := binary.BigEndian.AppendUint32([]byte(s.magic), s.version)
	inner := append(pack([]byte(s.keyType), s.signature), s.innerTail...)
	out = append(out, pack(s.keyBlob, []byte(s.namespace), []byte(s.reserved), []byte(s.hash), inner)...)
	return append(out, s.tail...)
}

// signed is the document with the signature `name`'s key makes over it (any
// old one replaced). before changes what is signed, after only what is sent.
func signed(t testing.TB, document *Object, name, namespace string, before, after func(*sshsig)) *Object {
	t.Helper()
	document = document.without("signature")
	sig := sshsig{magic: "SSHSIG", version: 1, keyBlob: testKeyBlob(name), namespace: namespace, hash: "sha512", keyType: "ssh-ed25519"}
	if before != nil {
		before(&sig)
	}
	var digest []byte
	if sig.hash == "sha256" {
		sum := sha256.Sum256(encode(t, document))
		digest = sum[:]
	} else {
		sum := sha512.Sum512(encode(t, document))
		digest = sum[:]
	}
	message := append([]byte("SSHSIG"), pack([]byte(sig.namespace), []byte(sig.reserved), []byte(sig.hash), digest)...)
	sig.signature = ed25519.Sign(testMachine(name), message)
	if after != nil {
		after(&sig)
	}
	copy := document.clone()
	return copy.set("signature", object("public_key", testPublicKey(name), "signature", armor(sig.blob())))
}

func sign(t testing.TB, report *Object, name string) *Object {
	t.Helper()
	return signed(t, report, name, NamespaceReport, nil, nil)
}

// upload judges a report as the old site's upload did: a document is signed
// by machine "a" first, text is sent as it is.
func upload(t testing.TB, report any) (*Judged, *Refusal) {
	t.Helper()
	var body []byte
	switch report := report.(type) {
	case *Object:
		body = encode(t, sign(t, report, "a"))
	case []byte:
		body = report
	case string:
		body = []byte(report)
	}
	return testRules(t).Judge(body, installURL)
}

func refused(t *testing.T, refusal *Refusal, judged *Judged, status int, message string, details ...string) {
	t.Helper()
	if refusal == nil || judged != nil {
		t.Fatalf("accepted; want %d %q", status, message)
	}
	if refusal.Status != status || refusal.Error != message {
		t.Errorf("refused with %d %q, want %d %q", refusal.Status, refusal.Error, status, message)
	}
	if details != nil && !reflect.DeepEqual(refusal.Details, details) {
		t.Errorf("details %q, want %q", refusal.Details, details)
	}
}

func accepted(t *testing.T, judged *Judged, refusal *Refusal) *Judged {
	t.Helper()
	if refusal != nil {
		t.Fatalf("refused: %d %s %q", refusal.Status, refusal.Error, refusal.Details)
	}
	return judged
}

const (
	invalidSignature = "The report's signature is not valid, so it was refused."
	changedAfter     = "signature: it doesn't match the report: the report was changed after it was signed"
)

func TestTheReportsTheCLISignedAreAcceptedAsTheyAre(t *testing.T) {
	paths := goldenPaths(t, "signed/*.json")
	if len(paths) != 2 {
		t.Fatalf("%d signed golden reports, want 2", len(paths))
	}
	machines := map[string]bool{}
	var kernels []string
	for _, path := range paths {
		judged, refusal := upload(t, read(t, path))
		accepted(t, judged, refusal)
		machines[judged.MachineID] = true
		kernels = append(kernels, at(judged.Report, "machine", "kernel").(string))
	}
	if len(machines) != 1 {
		t.Errorf("machine ids %v, want one", machines)
	}
	if want := `7.1.12-"asahi"\ärm/💻-ARCH`; !strings.Contains(strings.Join(kernels, "\n"), want) {
		t.Errorf("kernels %q lack %q", kernels, want)
	}
}

func TestAReportChangedAfterItWasSignedIsRefused(t *testing.T) {
	first := goldenPaths(t, "signed/*.json")[0]
	tampered := []*Object{
		put(parse(t, read(t, first)), "fail", "checks", 0, "status"),
		put(parse(t, read(t, first)), "7.1.13-1-ARCH", "machine", "kernel"),
		put(sign(t, golden(t, "m2-max-image2"), "a"), integer(99), "catalogue_version"),
	}
	for _, report := range tampered {
		judged, refusal := upload(t, encode(t, report))
		refused(t, refusal, judged, 422, invalidSignature, changedAfter)
	}
}

func TestASignatureByAnotherKeyThanTheReportsPublicKeyIsRefused(t *testing.T) {
	report := sign(t, golden(t, "m2-max-image2"), "mallory")
	put(report, testPublicKey("a"), "signature", "public_key")

	judged, refusal := upload(t, encode(t, report))

	refused(t, refusal, judged, 422, invalidSignature, "signature: it was made with a different key than the report's public_key")
}

func TestSignaturesThatArentSSHReportSignaturesAreRefused(t *testing.T) {
	armored := at(sign(t, golden(t, "m2-max-image2"), "a"), "signature", "signature").(string)
	blob := unarmor(t, armored)
	otherNamespace := bytes.Replace(blob, []byte("omarchy-m-test-report"), []byte("omarchy-m-test-xxxxxx"), 1)
	cases := []struct{ armored, detail string }{
		{armor(otherNamespace), "signature: it isn't a report signature (namespace)"},
		{armor(blob[:40]), "signature: it is cut short"},
		{armor(append(append([]byte(nil), blob...), 'x')), "signature: it has trailing bytes"},
		{"-----BEGIN SSH SIGNATURE-----\nnot base64!\n-----END SSH SIGNATURE-----", ""},
	}
	for _, c := range cases {
		report := put(sign(t, golden(t, "m2-max-image2"), "a"), c.armored, "signature", "signature")

		judged, refusal := upload(t, encode(t, report))

		if refusal == nil || judged != nil || refusal.Status != 422 {
			t.Fatalf("%q: not refused with 422: %+v", c.detail, refusal)
		}
		if c.detail != "" && !reflect.DeepEqual(refusal.Details, []string{c.detail}) {
			t.Errorf("details %q, want %q", refusal.Details, c.detail)
		}
	}
}

func TestAnUnsignedReportIsRefusedWithUpgradeInstructions(t *testing.T) {
	judged, refusal := upload(t, encode(t, golden(t, "m2-max-image2")))

	refused(t, refusal, judged, 422,
		"This report isn't signed with its Mac's key, which the site requires. omarchy-m-test signs every report by itself "+
			"(it needs ssh-keygen, from openssh). Update omarchy-m-test (curl -fsSL http://www.example.com/install | bash) and run it again.",
		"signature is missing")
}

func TestTheMachineIsAKeyedDigestOfItsKeyAndTheKeyAndSignatureArentKept(t *testing.T) {
	judged, refusal := upload(t, golden(t, "m2-max-image2"))
	accepted(t, judged, refusal)

	rules := testRules(t)
	if want := rules.MachineID(testKeyBlob("a")); judged.MachineID != want {
		t.Errorf("machine id %s, want %s", judged.MachineID, want)
	}
	if !regexp.MustCompile(`\Akey:[0-9a-f]{20}\z`).MatchString(judged.MachineID) {
		t.Errorf("machine id %q", judged.MachineID)
	}
	if want := encode(t, golden(t, "m2-max-image2")); !bytes.Equal(judged.Body, want) || !bytes.Equal(encode(t, judged.Report), want) {
		t.Error("the report kept is not the golden report")
	}
	key := strings.Fields(testPublicKey("a"))[1]
	if bytes.Contains(judged.Body, []byte(key)) || bytes.Contains(judged.Body, []byte("SSH SIGNATURE")) {
		t.Error("the report kept holds the key or the signature")
	}
	if judged.SchemaVersion != 1 {
		t.Errorf("schema version %d", judged.SchemaVersion)
	}
}

func TestReportsSignedByTheSameKeyAreOneMachineAndDifferentKeysAreDifferentMachines(t *testing.T) {
	rules := testRules(t)
	id := func(name string) string {
		judged, refusal := rules.Judge(encode(t, sign(t, golden(t, "m2-max-image2"), name)), installURL)
		return accepted(t, judged, refusal).MachineID
	}
	if id("a") != id("a") {
		t.Error("one key gave two machine ids")
	}
	if id("a") == id("b") {
		t.Error("two keys gave one machine id")
	}
}

func TestAMachineIDFromATestKeyIsTheOneRailsComputes(t *testing.T) {
	// SECRET_KEY_BASE "report parity: not a secret", the CLI's fixture key
	// (cli/tests/fixtures/test-machine-key.pub): computed by the old site's
	// MachineSignature.machine_id_for.
	const fixtureKey = "AAAAC3NzaC1lZDI1NTE5AAAAIHWZLEbyxAyRBj1yRyeHbRhQvVhcrUqpMaNH7ivMU1DW"
	blob, err := base64.StdEncoding.DecodeString(fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := testRules(t).MachineID(blob); got != railsFixtureMachineID {
		t.Errorf("machine id %s, want %s", got, railsFixtureMachineID)
	}
	if len(railsMachineIDKey(t, paritySecretKeyBase)) != 64 {
		t.Error("the machine-id key is not 64 bytes")
	}
	if _, err := NewRules(make([]byte, 32)); err == nil {
		t.Error("a machine-id key of 32 bytes was taken")
	}
}

func TestTheSignedSignInDocumentsVerifyAsSignIns(t *testing.T) {
	rules := testRules(t)
	reportKey, err := VerifySignature(parse(t, read(t, goldenPaths(t, "signed/*.json")[0])), NamespaceReport)
	if err != nil {
		t.Fatal(err)
	}
	paths := goldenPaths(t, "sign-in/*.json")
	if len(paths) != 3 {
		t.Fatalf("%d sign-in documents, want 3", len(paths))
	}
	for _, path := range paths {
		document := parse(t, read(t, path))
		key, err := VerifySignature(document, NamespaceSignIn)
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(path), err)
			continue
		}
		if rules.MachineID(key) != rules.MachineID(reportKey) {
			t.Errorf("%s: not the machine of the signed golden reports", filepath.Base(path))
		}
		if _, err := VerifySignature(document, NamespaceReport); err == nil || err.Error() != "it isn't a report signature (namespace)" {
			t.Errorf("%s as a report: %v", filepath.Base(path), err)
		}
	}
	_, err = VerifySignature(parse(t, read(t, goldenPaths(t, "signed/*.json")[0])), NamespaceSignIn)
	if err == nil || err.Error() != "it isn't a sign-in signature (namespace)" {
		t.Errorf("a report as a sign-in: %v", err)
	}
}

func withChecks(t testing.TB, evidence ...[]any) *Object {
	report := golden(t, "m2-max-image2")
	first := at(report, "checks", 0).(*Object)
	var checks []any
	for _, lines := range evidence {
		checks = append(checks, first.clone().set("evidence", lines))
	}
	return report.set("checks", checks)
}

func lines(count int, line string) []any {
	out := make([]any, count)
	for i := range out {
		out[i] = line
	}
	return out
}

func TestEveryGoldenReportIsWithinTheRules(t *testing.T) {
	rules := testRules(t)
	paths := append(goldenPaths(t, "*.json"), goldenPaths(t, "benchmarks/*.json")...)
	if len(paths) != 6 {
		t.Fatalf("%d golden reports, want 5 and 1 benchmark", len(paths))
	}
	for _, path := range paths {
		name := filepath.Base(path)
		report := parse(t, read(t, path))
		if problems := rules.SchemaErrors(report); len(problems) != 0 {
			t.Errorf("%s: schema: %q", name, problems)
		}
		if problems := EvidenceErrors(report); len(problems) != 0 {
			t.Errorf("%s: evidence: %q", name, problems)
		}
		if unknown := rules.Catalogue().UnknownCheckIDs(report); len(unknown) != 0 {
			t.Errorf("%s: unknown check ids %q", name, unknown)
		}
		judged, refusal := upload(t, read(t, path))
		if refusal == nil || !reflect.DeepEqual(refusal.Details, []string{"signature is missing"}) {
			t.Errorf("%s as it is: %+v, want it refused as unsigned", name, refusal)
		}
		judged, refusal = upload(t, report)
		accepted(t, judged, refusal)
	}
}

func TestAReportWithMoreThan64KiBOfEvidenceIsRefused(t *testing.T) {
	judged, refusal := upload(t, withChecks(t, lines(50, strings.Repeat("x", 500)), lines(50, strings.Repeat("x", 500)), lines(50, strings.Repeat("x", 500))))

	refused(t, refusal, judged, 422, "The report's evidence must be text and at most 64 KiB.", "evidence is 75000 bytes; a report carries at most 65536")
}

func TestSixtyFourKiBOfEvidenceIsAccepted(t *testing.T) {
	last := append(lines(31, strings.Repeat("x", 500)), strings.Repeat("x", 36))

	judged, refusal := upload(t, withChecks(t, lines(50, strings.Repeat("x", 500)), lines(50, strings.Repeat("x", 500)), last))

	accepted(t, judged, refusal)
}

func TestEvidenceThatIsntTextIsRefused(t *testing.T) {
	for _, line := range []string{"PNG\u0000\u001a", "two\nlines", "caf�"} {
		judged, refusal := upload(t, withChecks(t, []any{"ok", line}))

		refused(t, refusal, judged, 422, "The report's evidence must be text and at most 64 KiB.", "evidence line 2 is not text")
	}
}
