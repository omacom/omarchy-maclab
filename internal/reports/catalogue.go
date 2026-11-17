package reports

import (
	"fmt"
	"math/big"
	"sort"
)

// Catalogue is the feature catalogue shared with the CLI
// (catalogue/catalogue.json). The lab knows a check id only if the catalogue
// does, and words each outcome the way the CLI does.
type Catalogue struct {
	data          *Object
	version       int
	checks        *Object
	features      []*Object
	featureByID   map[string]*Object
	checksFor     map[string][]string
	knownCheckIDs map[string]bool
}

// The layers a feature belongs to, in the order they are shown.
var Layers = []string{"asahi", "aurora", "omarchy"}

// AsahiSupported are the Asahi states that count as supported.
var AsahiSupported = []string{"upstream", "linux-asahi", "yes"}

var LayerWords = map[string]string{"asahi": "Asahi hardware", "aurora": "Aurora addition", "omarchy": "Omarchy integration"}

func LoadCatalogue(data []byte) (*Catalogue, error) {
	value, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("reports: the catalogue is not JSON")
	}
	root, _ := value.(*Object)
	c := &Catalogue{data: root, featureByID: map[string]*Object{}, checksFor: map[string][]string{}, knownCheckIDs: map[string]bool{}}
	if root == nil {
		return nil, fmt.Errorf("reports: the catalogue is not an object")
	}
	version, _ := root.members["catalogue_version"].(*big.Int)
	c.checks, _ = root.members["checks"].(*Object)
	features, _ := root.members["features"].([]any)
	if version == nil || !version.IsInt64() || c.checks == nil || features == nil {
		return nil, fmt.Errorf("reports: the catalogue lacks its version, checks or features")
	}
	c.version = int(version.Int64())
	for _, id := range c.checks.keys {
		feature, _ := c.checks.members[id].(string)
		c.knownCheckIDs[id] = true
		c.checksFor[feature] = append(c.checksFor[feature], id)
	}
	for _, feature := range features {
		feature, _ := feature.(*Object)
		id, _ := member(feature, "id").(string)
		if id == "" {
			return nil, fmt.Errorf("reports: a feature of the catalogue has no id")
		}
		c.features = append(c.features, feature)
		c.featureByID[id] = feature
	}
	return c, nil
}

func member(object *Object, path ...string) any {
	var value any = object
	for _, key := range path {
		object, _ := value.(*Object)
		if object == nil {
			return nil
		}
		value = object.members[key]
	}
	return value
}

func (c *Catalogue) Version() int { return c.version }

// CheckIDs are the check ids the catalogue knows, in its order.
func (c *Catalogue) CheckIDs() []string { return append([]string(nil), c.checks.keys...) }

// OutcomeWords words a classification outcome as the CLI does.
func (c *Catalogue) OutcomeWords(outcome string) string {
	if words, ok := member(c.data, "outcomes", outcome).(string); ok {
		return words
	}
	return outcome
}

func (c *Catalogue) Features() []*Object { return c.features }

// Chips are the chip generations by key ("m2-pro-max-ultra").
func (c *Catalogue) Chips() *Object {
	chips, _ := c.data.members["chips"].(*Object)
	return chips
}

// Boards is every Apple Silicon Mac board: its name, chip, SoC, lineup and hardware facts.
func (c *Catalogue) Boards() *Object {
	boards, _ := c.data.members["boards"].(*Object)
	return boards
}

func (c *Catalogue) Feature(id string) *Object { return c.featureByID[id] }

func (c *Catalogue) FeatureName(id string) string {
	if name, ok := member(c.featureByID[id], "name").(string); ok {
		return name
	}
	return id
}

// ChecksFor gives the check ids of a feature, in catalogue order.
func (c *Catalogue) ChecksFor(featureID string) []string { return c.checksFor[featureID] }

// TestedFeatures are the features some check tests: Asahi's layer first,
// then Aurora's, then Omarchy's.
func (c *Catalogue) TestedFeatures() []*Object {
	var tested []*Object
	for _, feature := range c.features {
		if id, _ := feature.members["id"].(string); len(c.checksFor[id]) > 0 {
			tested = append(tested, feature)
		}
	}
	layer := func(feature *Object) int {
		name, _ := feature.members["layer"].(string)
		for i, known := range Layers {
			if known == name {
				return i
			}
		}
		return len(Layers)
	}
	sort.SliceStable(tested, func(i, j int) bool { return layer(tested[i]) < layer(tested[j]) })
	return tested
}

// ChipForSoC gives the chip generation key ("m2-pro-max-ultra") of a SoC id
// ("t6021"), or "".
func (c *Catalogue) ChipForSoC(soc string) string {
	chips := c.Chips()
	if chips == nil {
		return ""
	}
	for _, key := range chips.keys {
		socs, _ := member(chips, key, "socs").([]any)
		for _, known := range socs {
			if known == soc {
				return key
			}
		}
	}
	return ""
}

func (c *Catalogue) ChipName(key string) string {
	if name, ok := member(c.Chips(), key, "name").(string); ok {
		return name
	}
	return key
}

// Expected gives each layer's expected state ({"status": ..., "version" or
// "cell": ...}) for a chip generation and a board ("" for none), as the
// CLI's classifier resolves it; nil when the catalogue doesn't cover the chip.
func (c *Catalogue) Expected(feature *Object, chip, board string) *Object {
	states, _ := member(feature, "chips", chip).(*Object)
	if chip == "" || states == nil {
		return nil
	}
	states = &Object{keys: append([]string(nil), states.keys...), members: copyMembers(states.members)}
	if override, _ := member(feature, "models", board).(*Object); board != "" && override != nil {
		for _, layer := range override.keys {
			states.put(layer, override.members[layer])
		}
	}
	if _, ok := states.members["asahi"]; !ok {
		linkedID, _ := feature.members["asahi_feature"].(string)
		if linked := c.featureByID[linkedID]; linked != nil {
			if asahi, ok := member(c.Expected(linked, chip, board), "asahi").(*Object); ok {
				states.put("asahi", asahi)
			}
		}
	}
	return states
}

func copyMembers(members map[string]any) map[string]any {
	out := make(map[string]any, len(members))
	for key, value := range members {
		out[key] = value
	}
	return out
}

func (o *Object) put(key string, value any) {
	if _, ok := o.members[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.members[key] = value
}

// UnknownCheckIDs gives the check ids in the report that the catalogue
// doesn't know, each once, in the report's order.
func (c *Catalogue) UnknownCheckIDs(report *Object) []string {
	var unknown []string
	seen := map[string]bool{}
	checks, _ := report.members["checks"].([]any)
	for _, check := range checks {
		id, ok := member(asObject(check), "id").(string)
		if ok && !c.knownCheckIDs[id] && !seen[id] {
			seen[id] = true
			unknown = append(unknown, id)
		}
	}
	return unknown
}

func asObject(value any) *Object {
	object, _ := value.(*Object)
	return object
}
