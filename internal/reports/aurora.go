package reports

import (
	"sort"
	"time"
)

// AuroraSupport is the Aurora feature-support table: what the Aurora kernel
// (omacom/linux) supports per chip generation, from the published Verified
// runs with the linux-aurora package. A cell is the state the Verified
// machines on that chip generation agree on, each machine counted once with
// its latest run that tested the feature, partial when they disagree. It
// sits next to the catalogue's expected Aurora and Asahi states, and its
// sources are the runs behind it and the Aurora versions they ran. Kernel
// features only: the Asahi and Aurora layers, not Omarchy's integration.
type AuroraSupport struct {
	pub   *Published
	runs  []*Run
	chips []*AuroraChip
	done  bool
}

// AuroraPackage is the kernel's package.
const AuroraPackage = "linux-aurora"

var auroraLayers = []string{"asahi", "aurora"}

// AuroraCell is one feature on one chip generation.
type AuroraCell struct {
	Feature  *Object
	Chip     string
	Result   *Cell
	Expected *Object
}

func (c *AuroraCell) State() string {
	if state := c.Result.State(); state != "" {
		return state
	}
	return "not-tested"
}

func (c *AuroraCell) Tested() bool { return len(c.Result.Tallies) > 0 }

// Runs are the runs that tested it, newest first.
func (c *AuroraCell) Runs() []*Run  { return c.Result.Runs }
func (c *AuroraCell) Machines() int { return c.Result.VerifiedMachines() }

func (c *AuroraCell) AuroraVersions() []string { return uniqTexts(c.Runs(), (*Run).AuroraVersion) }

func (c *AuroraCell) ExpectedIn(layer string) *Object {
	state, _ := member(c.Expected, layer).(*Object)
	return state
}

func (c *AuroraCell) Summary() string {
	if c.Tested() {
		return c.Result.Summary()
	}
	return "no Verified run on Aurora tested this"
}

// AuroraChip is one chip generation with Verified runs on Aurora. Runs are
// newest first.
type AuroraChip struct {
	Key   string
	Runs  []*Run
	cells map[string]*AuroraCell
}

func (c *AuroraChip) Cell(feature string) *AuroraCell { return c.cells[feature] }
func (c *AuroraChip) Machines() int                   { return MachineCount(c.Runs) }
func (c *AuroraChip) Models() []string                { return uniqTexts(c.Runs, (*Run).ShortModel) }
func (c *AuroraChip) AuroraVersions() []string        { return uniqTexts(c.Runs, (*Run).AuroraVersion) }
func (c *AuroraChip) SoCs() []string                  { return uniqTexts(c.Runs, (*Run).SoC) }

// Aurora is the table from the published runs.
func (p *Published) Aurora() *AuroraSupport {
	a := &AuroraSupport{pub: p}
	for _, run := range p.runs {
		if run.Verified && run.AuroraVersion() != "" {
			a.runs = append(a.runs, run)
		}
	}
	return a
}

// Features are the kernel's features some check tests.
func (a *AuroraSupport) Features() []*Object {
	var features []*Object
	for _, feature := range a.pub.cat.TestedFeatures() {
		if oneIn(auroraLayers, layer(feature)) {
			features = append(features, feature)
		}
	}
	return features
}

func (a *AuroraSupport) Empty() bool { return len(a.Chips()) == 0 }

// Chips are the chip generations with Verified runs on Aurora, in catalogue order.
func (a *AuroraSupport) Chips() []*AuroraChip {
	if a.done {
		return a.chips
	}
	a.done = true
	cat := a.pub.cat
	groups := map[string][]*Run{}
	var keys []string
	for _, run := range a.runs {
		key := cat.ChipForSoC(run.SoC())
		if key == "" {
			continue
		}
		if _, seen := groups[key]; !seen {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], run)
	}
	sort.SliceStable(keys, func(i, j int) bool { return cat.generationRank(keys[i]) < cat.generationRank(keys[j]) })
	for _, key := range keys {
		chip := &AuroraChip{Key: key, Runs: newestFirst(groups[key]), cells: map[string]*AuroraCell{}}
		for _, feature := range a.Features() {
			chip.cells[id(feature)] = &AuroraCell{Feature: feature, Chip: key, Result: cellFrom(groups[key], id(feature), true), Expected: cat.Expected(feature, key, "")}
		}
		a.chips = append(a.chips, chip)
	}
	return a.chips
}

// UntestedChips are the catalogue's chip generations with no Verified run on Aurora yet.
func (a *AuroraSupport) UntestedChips() []string {
	tested := map[string]bool{}
	for _, chip := range a.Chips() {
		tested[chip.Key] = true
	}
	var untested []string
	if chips := a.pub.cat.Chips(); chips != nil {
		for _, key := range chips.keys {
			if !tested[key] {
				untested = append(untested, key)
			}
		}
	}
	return untested
}

func expectedJSON(state *Object) any {
	if state == nil {
		return nil
	}
	return state
}

// JSON is the table as the export carries it. urlFor gives a report's link.
func (a *AuroraSupport) JSON(urlFor func(*Run) string) *Object {
	cat := a.pub.cat
	features := []any{}
	for _, feature := range a.Features() {
		features = append(features, obj("id", id(feature), "name", text(member(feature, "name")), "layer", layer(feature)))
	}
	chips := []any{}
	for _, chip := range a.Chips() {
		cells := obj()
		for _, feature := range a.Features() {
			cell := chip.Cell(id(feature))
			links := []any{}
			for _, run := range cell.Runs() {
				links = append(links, urlFor(run))
			}
			cells.put(id(feature), obj("state", cell.State(), "tester_machines", cell.Result.VerifiedTallies,
				"expected", obj("aurora", expectedJSON(cell.ExpectedIn("aurora")), "asahi", expectedJSON(cell.ExpectedIn("asahi"))),
				"aurora_versions", cell.AuroraVersions(), "reports", links))
		}
		chips = append(chips, obj("chip", chip.Key, "name", cat.ChipName(chip.Key), "socs", chip.SoCs(), "models", chip.Models(), "machines", chip.Machines(),
			"runs", len(chip.Runs), "aurora_versions", chip.AuroraVersions(), "cells", cells))
	}
	asahi := obj()
	if source, _ := member(cat.data, "sources", "asahi").(*Object); source != nil {
		for _, key := range []string{"title", "url"} {
			if value, ok := source.Get(key); ok {
				asahi.put(key, value)
			}
		}
	}
	return obj("catalogue_version", cat.version,
		"method", "per chip generation, from visible tester runs with the "+AuroraPackage+" package: each tester machine counts once, with its latest run that tested the feature; "+
			"state is what the tester machines agree on, partial when they disagree, not-tested without one",
		"sources", obj("runs", "tester-verified reports on this site, linked per cell", "aurora", member(cat.data, "sources", "aurora"), "asahi", asahi),
		"features", features, "chips", chips, "untested_chips", a.UntestedChips())
}

// AuroraJSON is the Aurora export.
func (p *Published) AuroraJSON(urlFor func(*Run) string, at time.Time) []byte {
	export := obj("license", license(), "generated_at", iso8601(at))
	body := p.Aurora().JSON(urlFor)
	for _, key := range body.keys {
		export.put(key, body.members[key])
	}
	return JSON(export)
}

// Source is one of the catalogue's sources ("aurora", "asahi"): a member of it, "" for none.
func (c *Catalogue) Source(name, field string) string {
	return text(member(c.data, "sources", name, field))
}
