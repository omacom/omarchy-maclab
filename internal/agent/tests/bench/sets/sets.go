// Package sets holds the Benchmark sets, sets/bench-N.json, and loads and
// checks them. A set is every benchmark the lab runs on both systems, each
// with one metric id, and the files each system needs, pinned by sha256. A
// set is frozen: a change is a new set.
package sets

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

//go:embed bench-*.json
var embedded embed.FS

// Placeholder stands for a sha256 or a version not known yet: a file the lab
// builds itself before a Lab release publishes it, or one not yet pinned.
const Placeholder = "placeholder"

const (
	Omarchy = "omarchy"
	MacOS   = "macos"
)

// Systems are the two systems a set runs on.
var Systems = []string{Omarchy, MacOS}

// OmarchyOnly is the category run on Omarchy alone.
const OmarchyOnly = "Omarchy only"

// Categories are a set's categories, in page order.
var Categories = []string{"CPU", "GPU", "Storage", "Memory", "Web", "Video", "AI", "Power", OmarchyOnly}

const (
	Quick = "quick"
	Full  = "full"
)

// Set is one Benchmark set.
type Set struct {
	Name      string            `json:"set"`
	Schema    int               `json:"schema"`
	Decisions []string          `json:"decisions,omitempty"`
	Metrics   []Metric          `json:"metrics"`
	Files     map[string][]File `json:"files"`
	N         int               `json:"-"`
}

// Metric is one benchmark of a set, the same on both systems.
type Metric struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Category   string         `json:"category"`
	Version    string         `json:"version"`
	Settings   map[string]any `json:"settings,omitempty"`
	Unit       string         `json:"unit"`
	Direction  string         `json:"direction"`
	Tolerance  float64        `json:"tolerance"`
	List       string         `json:"list"`
	APIDiffers bool           `json:"api_differs,omitempty"`
	Omarchy    *Side          `json:"omarchy,omitempty"`
	MacOS      *Side          `json:"macos,omitempty"`
}

// Side is how one system runs a metric.
type Side struct {
	API   string   `json:"api,omitempty"`
	Files []string `json:"files"`
}

// File is one file a system needs: a program, a model, a clip or a page.
type File struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	Kind     string `json:"kind"`
	URL      string `json:"url,omitempty"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size,omitempty"`
	Root     string `json:"root,omitempty"`
	Entry    string `json:"entry,omitempty"`
	Note     string `json:"note,omitempty"`
	LabBuilt bool   `json:"lab_built,omitempty"`
}

// Pinned is whether the file's bytes are known.
func (f File) Pinned() bool { return f.SHA256 != Placeholder }

// HigherIsBetter is the metric's direction.
func (m Metric) HigherIsBetter() bool { return m.Direction == "higher" }

// Side is the metric on the system, nil when it does not run there.
func (m Metric) Side(system string) *Side {
	switch system {
	case Omarchy:
		return m.Omarchy
	case MacOS:
		return m.MacOS
	}
	return nil
}

var (
	reName   = regexp.MustCompile(`^bench-([1-9][0-9]*)$`)
	reID     = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
	reSHA    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	kinds    = []string{"web", "pacman", "zip", "tarball", "source", "file"}
	ErrNoSet = errors.New("no Benchmark set without a placeholder")
)

// Parse reads and checks the set named name ("bench-N").
func Parse(name string, b []byte) (*Set, error) {
	m := reName.FindStringSubmatch(name)
	if m == nil {
		return nil, fmt.Errorf("%q: a set is named bench-N", name)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var s Set
	if err := d.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	s.N, _ = strconv.Atoi(m[1])
	if err := s.check(name); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &s, nil
}

func (s *Set) check(name string) error {
	if s.Name != name {
		return fmt.Errorf("names itself %q", s.Name)
	}
	if s.Schema != 1 {
		return fmt.Errorf("schema %d, want 1", s.Schema)
	}
	files := map[string]map[string]File{}
	for sys, fs := range s.Files {
		if !slices.Contains(Systems, sys) {
			return fmt.Errorf("files for %q, which is not a system", sys)
		}
		files[sys] = map[string]File{}
		for _, f := range fs {
			if err := checkFile(f); err != nil {
				return fmt.Errorf("%s file %q: %w", sys, f.ID, err)
			}
			if _, dup := files[sys][f.ID]; dup {
				return fmt.Errorf("%s file %q is given twice", sys, f.ID)
			}
			files[sys][f.ID] = f
		}
	}
	used := map[string]map[string]bool{Omarchy: {}, MacOS: {}}
	seen := map[string]bool{}
	quick := map[string][]string{}
	for _, mt := range s.Metrics {
		if !reID.MatchString(mt.ID) {
			return fmt.Errorf("metric id %q", mt.ID)
		}
		if seen[mt.ID] {
			return fmt.Errorf("metric %s is given twice", mt.ID)
		}
		seen[mt.ID] = true
		if err := checkMetric(mt); err != nil {
			return fmt.Errorf("metric %s: %w", mt.ID, err)
		}
		for _, sys := range Systems {
			side := mt.Side(sys)
			if side == nil {
				continue
			}
			for _, id := range side.Files {
				if _, ok := files[sys][id]; !ok {
					return fmt.Errorf("metric %s needs %s file %q, which the set does not list", mt.ID, sys, id)
				}
				used[sys][id] = true
			}
		}
		if mt.List == Quick {
			quick[mt.Category] = append(quick[mt.Category], mt.ID)
		}
	}
	for _, c := range Categories {
		if !slices.ContainsFunc(s.Metrics, func(m Metric) bool { return m.Category == c }) {
			return fmt.Errorf("no metric in %s", c)
		}
		if len(quick[c]) != 1 {
			return fmt.Errorf("the Quick list has %d metrics in %s (%s), want one", len(quick[c]), c, strings.Join(quick[c], ", "))
		}
	}
	for sys, fs := range files {
		for id := range fs {
			if !used[sys][id] {
				return fmt.Errorf("%s file %q is used by no metric", sys, id)
			}
		}
	}
	return nil
}

func checkMetric(m Metric) error {
	switch {
	case m.Name == "":
		return errors.New("no name")
	case !slices.Contains(Categories, m.Category):
		return fmt.Errorf("category %q", m.Category)
	case m.Version == "":
		return errors.New("no version")
	case m.Unit == "":
		return errors.New("no unit")
	case m.Direction != "higher" && m.Direction != "lower":
		return fmt.Errorf("direction %q, want higher or lower", m.Direction)
	case !(m.Tolerance > 0 && m.Tolerance < 100):
		return fmt.Errorf("tolerance %v, want a percentage above 0", m.Tolerance)
	case m.List != Quick && m.List != Full:
		return fmt.Errorf("list %q, want quick or full", m.List)
	case m.Omarchy == nil:
		return errors.New("missing on omarchy")
	case m.Category == OmarchyOnly && m.MacOS != nil:
		return errors.New("Omarchy only, but given for macos")
	case m.Category != OmarchyOnly && m.MacOS == nil:
		return errors.New("missing on macos")
	case m.APIDiffers && (m.Omarchy.API == "" || m.MacOS == nil || m.MacOS.API == "" || m.Omarchy.API == m.MacOS.API):
		return errors.New("API differs, but does not name two APIs")
	}
	return nil
}

func checkFile(f File) error {
	switch {
	case !reID.MatchString(f.ID):
		return errors.New("bad id")
	case f.Version == "":
		return errors.New("no version")
	case !slices.Contains(kinds, f.Kind):
		return fmt.Errorf("kind %q", f.Kind)
	case f.URL != "" && !strings.HasPrefix(f.URL, "https://") && !strings.HasPrefix(f.URL, "http://"):
		return fmt.Errorf("address %q", f.URL)
	case f.SHA256 == Placeholder:
		if f.Size != 0 {
			return errors.New("a placeholder with a size")
		}
	case !reSHA.MatchString(f.SHA256):
		return fmt.Errorf("sha256 %q is neither a digest nor the placeholder", f.SHA256)
	case f.URL == "":
		return errors.New("pinned with no address")
	case f.Size <= 0:
		return errors.New("pinned with no size")
	case f.Version == Placeholder:
		return errors.New("pinned at a placeholder version")
	}
	return nil
}

// Placeholders are what the set still holds as placeholders, as
// "system/file" or "metric", in file order; a set with any is never current.
func (s *Set) Placeholders() []string {
	var out []string
	for _, m := range s.Metrics {
		if m.Version == Placeholder {
			out = append(out, m.ID)
		}
	}
	for _, sys := range Systems {
		for _, f := range s.Files[sys] {
			if !f.Pinned() || f.Version == Placeholder {
				out = append(out, sys+"/"+f.ID)
			}
		}
	}
	return out
}

// List is the set's Quick list, or its Full list (every metric).
func (s *Set) List(list string) []Metric {
	if list == Full {
		return slices.Clone(s.Metrics)
	}
	var out []Metric
	for _, m := range s.Metrics {
		if m.List == list {
			out = append(out, m)
		}
	}
	return out
}

// Metric is the set's metric of that id.
func (s *Set) Metric(id string) (Metric, bool) {
	i := slices.IndexFunc(s.Metrics, func(m Metric) bool { return m.ID == id })
	if i < 0 {
		return Metric{}, false
	}
	return s.Metrics[i], true
}

// File is the system's file of that id.
func (s *Set) File(system, id string) (File, bool) {
	fs := s.Files[system]
	i := slices.IndexFunc(fs, func(f File) bool { return f.ID == id })
	if i < 0 {
		return File{}, false
	}
	return fs[i], true
}

// FilesFor is every file the system needs for the metrics, each once, in
// the set's file order.
func (s *Set) FilesFor(system string, metrics []Metric) []File {
	need := map[string]bool{}
	for _, m := range metrics {
		if side := m.Side(system); side != nil {
			for _, id := range side.Files {
				need[id] = true
			}
		}
	}
	var out []File
	for _, f := range s.Files[system] {
		if need[f.ID] {
			out = append(out, f)
		}
	}
	return out
}

// All is every set in sets/, oldest first.
func All() ([]*Set, error) { return Read(embedded) }

// Read reads every bench-N.json at the top of fsys, oldest first.
func Read(fsys fs.FS) ([]*Set, error) {
	names, err := fs.Glob(fsys, "bench-*.json")
	if err != nil {
		return nil, err
	}
	var out []*Set
	for _, n := range names {
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return nil, err
		}
		s, err := Parse(strings.TrimSuffix(n, ".json"), b)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N < out[j].N })
	return out, nil
}

// Load is the set of that name.
func Load(name string) (*Set, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if s.Name == name {
			return s, nil
		}
	}
	return nil, fmt.Errorf("no Benchmark set %q", name)
}

// Newest is the newest set, placeholders or not.
func Newest() (*Set, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, errors.New("no Benchmark set")
	}
	return all[len(all)-1], nil
}

// Current is the newest set without a placeholder.
func Current() (*Set, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	return current(all)
}

func current(all []*Set) (*Set, error) {
	for i := len(all) - 1; i >= 0; i-- {
		if len(all[i].Placeholders()) == 0 {
			return all[i], nil
		}
	}
	return nil, ErrNoSet
}
