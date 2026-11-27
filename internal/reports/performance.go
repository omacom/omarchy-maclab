package reports

// Performance in reports.db (spec section 22, "Publishing and Counted"):
// labd hands over the whole of what is published each time any of it
// changes, and the tables become exactly that. What is not handed over is
// gone from them.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"time"

	"github.com/omacom/omarchy-maclab/internal/perf"
)

// A Mac's kind on the Performance pages. Counted decides only whose values
// make the headline: Lab-owned and Counted Macs'.
const (
	PerfLabOwned  = "lab_owned"
	PerfCounted   = "counted"
	PerfVolunteer = "volunteer"
)

// A Release's verdict state on the Performance pages: the only two a
// published Release has.
const (
	PerfQualified    = "qualified"
	PerfNoVerdictYet = "no verdict yet"
)

// PerfMac is one Mac with published values: an opaque key, never its name,
// its kind and its Configuration.
type PerfMac struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"`
	Board string `json:"board"`
	CPU   int    `json:"cpu"`
	GPU   int    `json:"gpu"`
}

// PerfRelease is a Release as the Performance pages name it: its kernel
// version and verdict state, an opaque key, and Position, 1 for the oldest
// frozen of the Releases published.
type PerfRelease struct {
	Key      string `json:"key"`
	Kernel   string `json:"kernel"`
	Verdict  string `json:"verdict"`
	Position int    `json:"position"`
}

// PerfValue is one published value: of a Mac, set, metric and Release (on
// Omarchy) or macOS build (in macOS), with bench.json's fields. Value and
// Reason: exactly one is set. Capacity is the SSD's, on a Storage value.
type PerfValue struct {
	Mac     string `json:"mac"`
	Set     string `json:"set"`
	Side    string `json:"side"`
	Release string `json:"release,omitempty"`
	// Adds are the packages the lab added to the Release for the value,
	// "omarchy-mlx" or "a+b": such a value is never the Release's own.
	Adds         string            `json:"adds,omitempty"`
	MacOSVersion string            `json:"macos_version,omitempty"`
	MacOSBuild   string            `json:"macos_build,omitempty"`
	Metric       string            `json:"metric"`
	Value        *float64          `json:"value"`
	Spread       *float64          `json:"spread"`
	Unit         string            `json:"unit"`
	API          string            `json:"api,omitempty"`
	Capacity     *int64            `json:"capacity,omitempty"`
	Reason       *string           `json:"reason"`
	Booted       map[string]string `json:"booted,omitempty"`
	Measured     time.Time         `json:"measured"`
}

// Performance is everything published of Performance: the Macs and Releases
// the values name, and the values.
type Performance struct {
	Macs     []PerfMac     `json:"macs"`
	Releases []PerfRelease `json:"releases"`
	Values   []PerfValue   `json:"values"`
}

var (
	rePerfKey     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	rePerfBoard   = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	rePerfSet     = regexp.MustCompile(`^bench-[1-9][0-9]{0,5}$`)
	rePerfMetric  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)
	rePerfVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._:~-]{0,63}$`)
	rePerfPackage = regexp.MustCompile(`^[a-z0-9][a-z0-9+._-]{0,63}$`)
	rePerfBuild   = regexp.MustCompile(`^[0-9A-Za-z]{1,16}$`)
	rePerfAdds    = regexp.MustCompile(`^[a-z0-9@_][a-z0-9@._-]*(\+[a-z0-9@_][a-z0-9@._-]*)*$`)
	rePerfMacOS   = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){0,3}$`)
	rePerfText    = regexp.MustCompile(`^[ -~]{0,64}$`)
)

// Pruned is p without what PublishPerformance would refuse, and why each
// was left out: labd copies the rest, so that one row it cannot publish
// holds back neither the others nor a deletion.
func (p Performance) Pruned() (Performance, []error) {
	var out Performance
	var errs []error
	macs, releases, positions := map[string]bool{}, map[string]bool{}, map[int]bool{}
	for _, m := range p.Macs {
		if err := checkPerfMac(m, macs); err != nil {
			errs = append(errs, err)
			continue
		}
		macs[m.Key] = true
		out.Macs = append(out.Macs, m)
	}
	for _, r := range p.Releases {
		if err := checkPerfRelease(r, releases, positions); err != nil {
			errs = append(errs, err)
			continue
		}
		releases[r.Key], positions[r.Position] = true, true
		out.Releases = append(out.Releases, r)
	}
	for _, v := range p.Values {
		if err := checkPerfValue(v, macs, releases); err != nil {
			errs = append(errs, err)
			continue
		}
		out.Values = append(out.Values, v)
	}
	return out, errs
}

func (p *Performance) check() error {
	if _, errs := p.Pruned(); len(errs) > 0 {
		return errs[0]
	}
	return nil
}

func checkPerfMac(m PerfMac, seen map[string]bool) error {
	switch {
	case !rePerfKey.MatchString(m.Key):
		return fmt.Errorf("a Mac's key %q is not opaque", m.Key)
	case seen[m.Key]:
		return fmt.Errorf("Mac %s twice", m.Key)
	case m.Kind != PerfLabOwned && m.Kind != PerfCounted && m.Kind != PerfVolunteer:
		return fmt.Errorf("Mac %s: kind %q", m.Key, m.Kind)
	case !rePerfBoard.MatchString(m.Board) || m.CPU <= 0 || m.GPU <= 0:
		return fmt.Errorf("Mac %s: Configuration %q %dc-%dg", m.Key, m.Board, m.CPU, m.GPU)
	}
	return nil
}

func checkPerfRelease(r PerfRelease, seen map[string]bool, positions map[int]bool) error {
	switch {
	case !rePerfKey.MatchString(r.Key):
		return fmt.Errorf("a Release's key %q is not opaque", r.Key)
	case seen[r.Key]:
		return fmt.Errorf("Release %s twice", r.Key)
	case r.Verdict != PerfQualified && r.Verdict != PerfNoVerdictYet:
		return fmt.Errorf("Release %s: verdict %q is not published", r.Key, r.Verdict)
	case !rePerfVersion.MatchString(r.Kernel):
		return fmt.Errorf("Release %s: kernel %q", r.Key, r.Kernel)
	case r.Position <= 0 || positions[r.Position]:
		return fmt.Errorf("Release %s: position %d", r.Key, r.Position)
	}
	return nil
}

func checkPerfValue(v PerfValue, macs, releases map[string]bool) error {
	at := fmt.Sprintf("%s %s %s", v.Mac, v.Set, v.Metric)
	switch {
	case !macs[v.Mac]:
		return fmt.Errorf("%s: no such Mac", at)
	case !rePerfSet.MatchString(v.Set) || !rePerfMetric.MatchString(v.Metric):
		return fmt.Errorf("%s: not a set and metric", at)
	case v.Side == perf.SideOmarchy && (!releases[v.Release] || v.MacOSBuild != "" || v.MacOSVersion != ""):
		return fmt.Errorf("%s: an Omarchy value names a published Release and no macOS build", at)
	case v.Adds != "" && (v.Side != perf.SideOmarchy || !rePerfAdds.MatchString(v.Adds)):
		return fmt.Errorf("%s: only an Omarchy value names packages the lab added, by name", at)
	case v.Side == perf.SideMacOS && (v.Release != "" || !rePerfBuild.MatchString(v.MacOSBuild) || (v.MacOSVersion != "" && !rePerfMacOS.MatchString(v.MacOSVersion)) || len(v.Booted) > 0):
		return fmt.Errorf("%s: a macOS value names its macOS version and build, and no Release", at)
	case v.Side != perf.SideOmarchy && v.Side != perf.SideMacOS:
		return fmt.Errorf("%s: side %q", at, v.Side)
	case (v.Value == nil) == (v.Reason == nil):
		return fmt.Errorf("%s: a value or a reason code, not both or neither", at)
	case v.Reason != nil && !perf.KnownReason(*v.Reason):
		return fmt.Errorf("%s: %q is not a reason code", at, *v.Reason)
	case !rePerfText.MatchString(v.Unit) || !rePerfText.MatchString(v.API):
		return fmt.Errorf("%s: unit or api", at)
	case v.Measured.IsZero():
		return fmt.Errorf("%s: no time measured", at)
	}
	for name, version := range v.Booted {
		if !rePerfPackage.MatchString(name) || !rePerfVersion.MatchString(version) {
			return fmt.Errorf("%s: booted %q %q", at, name, version)
		}
	}
	return nil
}

// sort puts p in the order Performance reads it back in, with no empty
// booted map.
func (p *Performance) sort() {
	sort.Slice(p.Macs, func(i, j int) bool { return p.Macs[i].Key < p.Macs[j].Key })
	sort.Slice(p.Releases, func(i, j int) bool { return p.Releases[i].Position < p.Releases[j].Position })
	for i := range p.Values {
		if len(p.Values[i].Booted) == 0 {
			p.Values[i].Booted = nil
		}
		p.Values[i].Measured = time.UnixMicro(p.Values[i].Measured.UnixMicro())
	}
	sort.Slice(p.Values, func(i, j int) bool {
		a, b := p.Values[i], p.Values[j]
		for _, c := range [][2]string{{a.Mac, b.Mac}, {a.Set, b.Set}, {a.Side, b.Side}, {a.Release, b.Release}, {a.MacOSBuild, b.MacOSBuild}} {
			if c[0] != c[1] {
				return c[0] < c[1]
			}
		}
		return a.Metric < b.Metric
	})
	if p.Macs == nil {
		p.Macs = []PerfMac{}
	}
	if p.Releases == nil {
		p.Releases = []PerfRelease{}
	}
	if p.Values == nil {
		p.Values = []PerfValue{}
	}
}

// PublishPerformance makes reports.db's Performance exactly p, in one step,
// and says whether anything changed: a row that stays is kept, with when it
// first came (now, for a new one). p is refused whole when any of it names
// what is not published: a Mac by anything but an opaque key, a Release
// not qualified, a reason that is not a code.
func (s *Store) PublishPerformance(p Performance, now time.Time) (bool, error) {
	p.sort()
	if err := p.check(); err != nil {
		return false, fmt.Errorf("reports: performance: %w", err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	was, err := readPerformance(tx)
	if err != nil {
		return false, err
	}
	if reflect.DeepEqual(was, p) {
		return false, nil
	}
	at := now.UnixMicro()
	exec := func(q string, args ...any) {
		if err == nil {
			_, err = tx.Exec(q, args...)
		}
	}

	oldMacs := map[string]PerfMac{}
	for _, m := range was.Macs {
		oldMacs[m.Key] = m
	}
	for _, m := range p.Macs {
		old, ok := oldMacs[m.Key]
		delete(oldMacs, m.Key)
		switch {
		case !ok:
			exec(`INSERT INTO perf_macs (mac_key, kind, board, cpu_cores, gpu_cores, created_at) VALUES (?, ?, ?, ?, ?, ?)`, m.Key, m.Kind, m.Board, m.CPU, m.GPU, at)
		case old != m:
			exec(`UPDATE perf_macs SET kind = ?, board = ?, cpu_cores = ?, gpu_cores = ? WHERE mac_key = ?`, m.Kind, m.Board, m.CPU, m.GPU, m.Key)
		}
	}
	for key := range oldMacs {
		exec(`DELETE FROM perf_macs WHERE mac_key = ?`, key)
	}

	oldReleases := map[string]PerfRelease{}
	for _, r := range was.Releases {
		oldReleases[r.Key] = r
	}
	for _, r := range p.Releases {
		old, ok := oldReleases[r.Key]
		delete(oldReleases, r.Key)
		switch {
		case !ok:
			exec(`INSERT INTO perf_releases (release_key, kernel, verdict, position, created_at) VALUES (?, ?, ?, ?, ?)`, r.Key, r.Kernel, r.Verdict, r.Position, at)
		case old != r:
			exec(`UPDATE perf_releases SET kernel = ?, verdict = ?, position = ? WHERE release_key = ?`, r.Kernel, r.Verdict, r.Position, r.Key)
		}
	}
	for key := range oldReleases {
		exec(`DELETE FROM perf_releases WHERE release_key = ?`, key)
	}

	oldValues := map[perfValueKey]PerfValue{}
	for _, v := range was.Values {
		oldValues[v.key()] = v
	}
	for _, v := range p.Values {
		old, ok := oldValues[v.key()]
		delete(oldValues, v.key())
		if ok && reflect.DeepEqual(old, v) {
			continue
		}
		booted := []byte("{}")
		if len(v.Booted) > 0 {
			booted, _ = json.Marshal(v.Booted)
		}
		exec(`INSERT INTO perf_values (mac_key, set_name, side, release_key, adds, macos_version, macos_build, metric,
value, spread, unit, api, capacity, reason, booted, measured_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (mac_key, set_name, side, release_key, macos_build, metric) DO UPDATE SET macos_version = excluded.macos_version, adds = excluded.adds,
value = excluded.value, spread = excluded.spread, unit = excluded.unit, api = excluded.api, capacity = excluded.capacity,
reason = excluded.reason, booted = excluded.booted, measured_at = excluded.measured_at`,
			v.Mac, v.Set, v.Side, v.Release, v.Adds, v.MacOSVersion, v.MacOSBuild, v.Metric,
			v.Value, v.Spread, v.Unit, v.API, v.Capacity, v.Reason, string(booted), v.Measured.UnixMicro())
	}
	for k := range oldValues {
		exec(`DELETE FROM perf_values WHERE mac_key = ? AND set_name = ? AND side = ? AND release_key = ? AND adds = ? AND macos_build = ? AND metric = ?`,
			k.mac, k.set, k.side, k.release, k.adds, k.build, k.metric)
	}
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

type perfValueKey struct{ mac, set, side, release, adds, build, metric string }

func (v PerfValue) key() perfValueKey {
	return perfValueKey{v.Mac, v.Set, v.Side, v.Release, v.Adds, v.MacOSBuild, v.Metric}
}

// Performance is everything published of Performance, for the pages and
// their data.
func (s *Store) Performance() (Performance, error) {
	return readPerformance(s.db)
}

type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func readPerformance(tx querier) (Performance, error) {
	var p Performance
	rows, err := tx.Query(`SELECT mac_key, kind, board, cpu_cores, gpu_cores FROM perf_macs`)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var m PerfMac
		if err := rows.Scan(&m.Key, &m.Kind, &m.Board, &m.CPU, &m.GPU); err != nil {
			rows.Close()
			return p, err
		}
		p.Macs = append(p.Macs, m)
	}
	rows.Close()
	if rows, err = tx.Query(`SELECT release_key, kernel, verdict, position FROM perf_releases`); err != nil {
		return p, err
	}
	for rows.Next() {
		var r PerfRelease
		if err := rows.Scan(&r.Key, &r.Kernel, &r.Verdict, &r.Position); err != nil {
			rows.Close()
			return p, err
		}
		p.Releases = append(p.Releases, r)
	}
	rows.Close()
	if rows, err = tx.Query(`SELECT mac_key, set_name, side, release_key, adds, macos_version, macos_build, metric,
value, spread, unit, api, capacity, reason, booted, measured_at FROM perf_values`); err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var v PerfValue
		var value, spread sql.NullFloat64
		var capacity sql.NullInt64
		var reason sql.NullString
		var booted string
		var measured int64
		if err := rows.Scan(&v.Mac, &v.Set, &v.Side, &v.Release, &v.Adds, &v.MacOSVersion, &v.MacOSBuild, &v.Metric,
			&value, &spread, &v.Unit, &v.API, &capacity, &reason, &booted, &measured); err != nil {
			return p, err
		}
		if value.Valid {
			v.Value = &value.Float64
		}
		if spread.Valid {
			v.Spread = &spread.Float64
		}
		if capacity.Valid {
			v.Capacity = &capacity.Int64
		}
		if reason.Valid {
			v.Reason = &reason.String
		}
		if booted != "{}" {
			json.Unmarshal([]byte(booted), &v.Booted)
		}
		v.Measured = time.UnixMicro(measured)
		p.Values = append(p.Values, v)
	}
	if err := rows.Err(); err != nil {
		return p, err
	}
	p.sort()
	return p, nil
}
