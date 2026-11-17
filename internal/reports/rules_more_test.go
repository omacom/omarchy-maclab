package reports

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/schema"
)

func TestABodyThatIsntJSONIsABadRequest(t *testing.T) {
	judged, refusal := upload(t, "not json")

	refused(t, refusal, judged, 400, "The report is not valid JSON.")
	if body, _ := json.Marshal(refusal); string(body) != `{"error":"The report is not valid JSON."}` {
		t.Errorf("answer %s", body)
	}
}

func TestARefusalIsAnsweredAsTheOldSiteAnsweredIt(t *testing.T) {
	_, refusal := upload(t, put(golden(t, "m2-max-image2"), "made.up", "checks", 0, "id"))

	body, _ := json.Marshal(refusal)
	want := `{"error":"The report has checks this site doesn't know (feature catalogue v18). Update omarchy-m-test (curl -fsSL http://www.example.com/install | bash) and run it again.","details":["unknown check id made.up"]}`
	if string(body) != want {
		t.Errorf("answer %s\nwant   %s", body, want)
	}
}

func TestAReportInAnOutdatedSchemaIsRefusedWithAnUpgradeMessage(t *testing.T) {
	for _, version := range []string{"0", "-1"} {
		judged, refusal := upload(t, put(golden(t, "m2-max-image2"), bigInteger(t, version), "schema_version"))

		refused(t, refusal, judged, 422, "This report uses report schema v"+version+", which is outdated: the site accepts v1. "+
			"Update omarchy-m-test (curl -fsSL http://www.example.com/install | bash) and run it again.", "schema_version "+version+" is older than 1")
	}
}

// Where the old site failed with an error of its own (HTTP 500), the lab refuses.
func TestWhatTheOldSiteFailedOnIsRefused(t *testing.T) {
	sent := string(encode(t, sign(t, golden(t, "m2-max-image2"), "a")))

	judged, refusal := upload(t, strings.Replace(sent, `"consent_version":6`, `"consent_version":1e999`, 1))
	refused(t, refusal, judged, 422, "The report does not match report schema v1.", "value at `/consent_version` is not an integer")

	judged, refusal = upload(t, strings.Replace(sent, `"kernel":"`, "\"kernel\":\"\xff", 1))
	refused(t, refusal, judged, 400, "The report is not valid JSON.")
}

func TestASchemaTheseRulesDontFullyKnowIsRefused(t *testing.T) {
	if _, err := LoadSchema(schema.ReportV1); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		`{"oneOf":[{"type":"string"}]}`,
		`{"properties":{"a":{"$ref":"#/x"}}}`,
		`{"type":"date"}`,
		`{"type":["string","null"]}`,
		`{"pattern":"^\\d+$"}`,
		`{"pattern":"(?i)a"}`,
		`{"pattern":"a\\"}`,
		`{"enum":["a",1]}`,
		`{"enum":["say \"a\""]}`,
		`{"const":1.5}`,
		`{"minimum":0.5}`,
		`{"maxLength":-1}`,
		`{"additionalProperties":{"type":"string"}}`,
		`{"items":[{}]}`,
		`[]`,
		`{`,
	} {
		if _, err := LoadSchema([]byte(text)); err == nil {
			t.Errorf("%s was taken", text)
		}
	}
}

func TestTheSchemaComparesAsRubyDoes(t *testing.T) {
	problems := func(schemaText, document string) []string {
		t.Helper()
		s, err := LoadSchema([]byte(schemaText))
		if err != nil {
			t.Fatal(err)
		}
		value, err := Parse([]byte(document))
		if err != nil {
			t.Fatal(err)
		}
		return s.Errors(value)
	}
	for _, c := range []struct {
		schema, document string
		want             []string
	}{
		{`{"const":1}`, `1.0`, nil},
		{`{"const":1}`, `"1"`, []string{"value at root is not: 1"}},
		{`{"const":true}`, `1`, []string{"value at root is not: true"}},
		{`{"type":"integer","minimum":1}`, `0.5`, []string{"value at root is not an integer", "number at root is less than: 1"}},
		{`{"type":"integer","maximum":3}`, `3.0`, nil},
		{`{"type":"number"}`, `1e999`, nil},
		{`{"type":"null"}`, `false`, []string{"value at root is not null"}},
		{`{"type":"boolean"}`, `null`, []string{"value at root is not a boolean"}},
		{`{"maxLength":2}`, `"ééé"`, []string{"string length at root is greater than: 2"}},
		{`{"maxLength":2}`, `"éé"`, nil},
		{`{"properties":{"a":{"type":"string"}}}`, `{"a":1,"b":2}`, []string{"value at `/a` is not a string"}},
		{`{"items":{"type":"string"},"minItems":3}`, `[1,"a"]`, []string{"value at `/0` is not a string", "array size at root is less than: 3"}},
	} {
		if got := problems(c.schema, c.document); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s on %s: %q, want %q", c.schema, c.document, got, c.want)
		}
	}
}

func TestTheCatalogue(t *testing.T) {
	c := testRules(t).Catalogue()
	if c.Version() != 18 {
		t.Errorf("catalogue version %d, want 18", c.Version())
	}
	if ids := c.CheckIDs(); len(ids) != 86 || ids[0] != "system.identity" {
		t.Errorf("%d check ids, the first %q", len(ids), ids[0])
	}
	if got := c.OutcomeWords("not-in-aurora"); got != "not yet supported by Aurora" {
		t.Errorf("outcome words %q", got)
	}
	if got := c.OutcomeWords("made-up"); got != "made-up" {
		t.Errorf("an unknown outcome is worded %q", got)
	}
	if got := c.ChipForSoC("t6021"); got != "m2-pro-max-ultra" || c.ChipName(got) != "M2 Pro/Max/Ultra" || c.ChipForSoC("t0000") != "" || c.ChipName("m9") != "m9" {
		t.Errorf("t6021 is chip %q, named %q", got, c.ChipName(got))
	}
	if c.FeatureName("thunderbolt") != "Thunderbolt" || c.FeatureName("made-up") != "made-up" || c.Feature("made-up") != nil {
		t.Error("feature names")
	}
	if got := c.ChecksFor("boot-chain"); !reflect.DeepEqual(got, []string{"boot.kernel-package", "boot.chain"}) && len(got) < 2 {
		t.Errorf("checks of boot-chain: %q", got)
	}
	if len(c.Boards().Keys()) != 59 || len(c.Chips().Keys()) != 15 || len(c.Features()) != 79 {
		t.Error("boards, chips or features are not all there")
	}

	last := -1
	for _, feature := range c.TestedFeatures() {
		id, _ := feature.Get("id")
		layer, _ := feature.Get("layer")
		at := map[string]int{"asahi": 0, "aurora": 1, "omarchy": 2}[layer.(string)]
		if at < last || len(c.ChecksFor(id.(string))) == 0 {
			t.Errorf("%s is out of place among the tested features", id)
		}
		last = at
	}

	thunderbolt := c.Feature("thunderbolt")
	status := func(states *Object, layer string) any { return member(states, layer, "status") }
	if states := c.Expected(thunderbolt, "m2", ""); status(states, "asahi") != "wip" || status(states, "aurora") != "supported" {
		t.Errorf("thunderbolt on m2: %s", encode(t, states))
	}
	if states := c.Expected(thunderbolt, "m2", "j700"); status(states, "aurora") != "absent" || status(states, "asahi") != "wip" {
		t.Errorf("thunderbolt on the j700: %s", encode(t, states))
	}
	if status(c.Expected(thunderbolt, "m2", ""), "aurora") != "supported" {
		t.Error("a board's states were written into the catalogue")
	}
	if c.Expected(thunderbolt, "", "") != nil || c.Expected(thunderbolt, "m9", "j700") != nil {
		t.Error("a chip the catalogue doesn't cover has expected states")
	}
	linked := 0
	for _, feature := range c.Features() {
		target, _ := member(feature, "asahi_feature").(string)
		if target == "" || member(feature, "chips", "m1", "asahi") != nil {
			continue
		}
		linked++
		want := member(c.Feature(target), "chips", "m1", "asahi")
		if got := member(c.Expected(feature, "m1", ""), "asahi"); want == nil || !reflect.DeepEqual(got, want) {
			id, _ := feature.Get("id")
			t.Errorf("%s doesn't take %s's Asahi state", id, target)
		}
	}
	if linked == 0 {
		t.Error("no feature takes its Asahi state from another")
	}
}

func TestTheMacOSReference(t *testing.T) {
	reference := MacOSReferenceFor("j616s")
	if reference == nil {
		t.Fatal("no reference for the j616s")
	}
	if got, want := reference.Conditions(), "macOS 27.0, on AC, brightness about 382 nits, 3 displays, 2026-10-31"; got != want {
		t.Errorf("conditions %q, want %q", got, want)
	}
	if reference.HasStorage() || reference.Value("benchmark.storage", "1TB") != nil || reference.Value("benchmark.h264-decode", "") != nil || reference.Value("benchmark.opengl", "") != nil {
		t.Error("the j616s reference has no decode or storage numbers")
	}
	for _, board := range []string{"j999", "", "../catalogue", "j616s.json", "J616S"} {
		if MacOSReferenceFor(board) != nil {
			t.Errorf("a reference for %q", board)
		}
	}
	if !MacOSCompared("benchmark.hevc-decode") || !MacOSCompared("benchmark.storage") || MacOSCompared("benchmark.vulkan") {
		t.Error("which benchmarks macOS is compared on")
	}

	other, err := Parse([]byte(`{"captured":"2026-11-01","macos":{"version":"27.1"},"conditions":{"power":"battery","brightness":"half","displays":1},
		"metrics":{"h264_decode_fps":812},"storage":{"1TB":{"read_mbps":5210}}}`))
	if err != nil {
		t.Fatal(err)
	}
	reference = &MacOSReference{other.(*Object)}
	if got, want := reference.Conditions(), "macOS 27.1, on battery, brightness half, 1 display, 2026-11-01"; got != want {
		t.Errorf("conditions %q, want %q", got, want)
	}
	if !reference.HasStorage() || rubyToS(reference.Value("benchmark.storage", "1TB")) != "5210" || reference.Value("benchmark.storage", "2TB") != nil ||
		rubyToS(reference.Value("benchmark.h264-decode", "")) != "812" {
		t.Error("the numbers of a reference with decode and storage")
	}
}
