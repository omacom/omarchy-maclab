package agent

import (
	"debug/pe"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The ESP inventory: when a Mac's ESP has no room to stage a test kernel, the
// preflight problem also says what the UKIs in <esp>/EFI/Linux are, which of
// them the Mac still needs, and how much removing the others would free. It
// reads and never deletes: only the Volunteer decides what goes.

// espUKI is one UKI under <esp>/EFI/Linux.
type espUKI struct {
	Name    string   // relative to the ESP, e.g. EFI/Linux/omarchy_linux-aurora.efi
	Size    uint64   // allocated bytes, rounded up to the ESP's allocation unit
	Kernel  string   // its .uname; "" when it has none that can be read
	Entries []string // the Limine menu entries that boot it
	Keep    []string // why it stays: running, booted last, the default entry, the Known-good kernel
}

// espContext is what tells a UKI the Mac needs from one it doesn't.
type espContext struct {
	Running   string // uname -r
	KnownGood string // the Known-good kernel lab-agent recorded; "" when none
	Selected  string // LoaderEntrySelected: the menu entry the Mac booted last; "" when unset
	Menu      []byte // limine.conf; nil when it can't be read
}

// limineMenuEntry is one entry of limine.conf, a directory or a boot entry.
type limineMenuEntry struct {
	Path     string // "/Omarchy/linux-aurora", the names of the entry and its parents
	ID       string // its Boot Loader Interface id, "Omarchy.linux-aurora"
	Resource string // the ESP file it boots, relative to the ESP; "" for none or another volume
	Dir      bool   // a directory: another entry is nested under it
}

// parseLimineMenu reads the entries and default_entry of a limine.conf. It
// never fails: what it can't read it leaves out, and the callers treat what
// they can't tell as needed.
func parseLimineMenu(b []byte) (entries []limineMenuEntry, defaultEntry string) {
	var stack []string
	var depths []int
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "/"):
			d := len(line) - len(strings.TrimLeft(line, "/"))
			name := strings.TrimSpace(strings.TrimLeft(line[d:], "+"))
			if d > len(stack)+1 {
				d = len(stack) + 1
			}
			stack = append(stack[:d-1], name)
			for i := len(entries) - 1; i >= 0; i-- {
				if depths[i] < d {
					entries[i].Dir = true // a parent of this one
					break
				}
				if depths[i] == d {
					break
				}
			}
			path := "/" + strings.Join(stack, "/")
			entries = append(entries, limineMenuEntry{Path: path, ID: bliEntryID(path)})
			depths = append(depths, d)
		case line == "" || strings.HasPrefix(line, "#"):
		default:
			key, val, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			key, val = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(val)
			switch key {
			case "default_entry":
				if len(entries) == 0 {
					defaultEntry = val
				}
			case "path", "kernel_path", "image_path":
				if len(entries) > 0 && entries[len(entries)-1].Resource == "" {
					entries[len(entries)-1].Resource = espResource(val)
				}
			}
		}
	}
	return entries, defaultEntry
}

// espResource is the ESP-relative file of a Limine path on the boot volume,
// "boot():/EFI/Linux/x.efi#<hash>": "EFI/Linux/x.efi". "" when it is on
// another volume or isn't a path.
func espResource(v string) string {
	rest, ok := strings.CutPrefix(v, "boot():")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimPrefix(filepath.Clean("/"+rest), "/")
}

// bliEntryID is the Boot Loader Interface id Limine gives the entry at path:
// the names joined by '.', and any byte outside [A-Za-z0-9+_.@-] made '-'.
func bliEntryID(path string) string {
	var b strings.Builder
	for _, c := range []byte(strings.TrimPrefix(path, "/")) {
		switch {
		case c == '/':
			b.WriteByte('.')
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', strings.IndexByte("+_.@-", c) >= 0:
			b.WriteByte(c)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// defaultEntries are the indexes of the entries default_entry may name, and
// whether it could be told at all. A number counts entries from 1; whether
// Limine counts directories in it differs between versions, so both readings
// are kept. A name matches an entry's path, its last name or its id. Unset is
// entry 1.
func defaultEntries(entries []limineMenuEntry, def string) ([]int, bool) {
	if def == "" {
		def = "1"
	}
	if n, err := strconv.Atoi(def); err == nil {
		var idx []int
		if n >= 1 && n <= len(entries) {
			idx = append(idx, n-1) // directories counted
		}
		seen := 0
		for i, e := range entries {
			if e.Dir {
				continue
			}
			if seen++; seen == n {
				idx = append(idx, i) // directories not counted
				break
			}
		}
		return idx, len(idx) > 0
	}
	var idx []int
	for i, e := range entries {
		if e.Path == def || e.ID == def || filepath.Base(e.Path) == def {
			idx = append(idx, i)
		}
	}
	return idx, len(idx) > 0
}

// ukiUname is the kernel release in a UKI's .uname section, "" when it has
// none that can be read.
func ukiUname(path string) string {
	f, err := pe.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	s := f.Section(".uname")
	if s == nil || s.VirtualSize == 0 || s.VirtualSize > 4096 || s.VirtualSize > s.Size {
		return ""
	}
	b, err := s.Data()
	if err != nil {
		return ""
	}
	name := strings.TrimRight(string(b[:s.VirtualSize]), "\x00\n")
	if strings.ContainsAny(name, "/\x00\r\n") {
		return ""
	}
	return name
}

// espInventory lists the UKIs under <esp>/EFI/Linux, largest first, with the
// menu entries that boot each and why the Mac needs it. ok is false when the
// default entry can't be told: then every UKI is kept.
func espInventory(esp string, allocation uint64, c espContext) (ukis []espUKI, ok bool) {
	entries, def := parseLimineMenu(c.Menu)
	defaults, ok := defaultEntries(entries, def)
	if c.Menu == nil {
		ok = false
	}
	isDefault := map[int]bool{}
	for _, i := range defaults {
		isDefault[i] = true
	}
	filepath.WalkDir(filepath.Join(esp, "EFI", "Linux"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(p), ".efi") {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(esp, p)
		u := espUKI{Name: rel, Size: roundUp(uint64(info.Size()), allocation), Kernel: ukiUname(p)}
		keep := func(why string) {
			for _, k := range u.Keep {
				if k == why {
					return
				}
			}
			u.Keep = append(u.Keep, why)
		}
		if u.Kernel != "" && u.Kernel == c.Running {
			keep("running")
		}
		if u.Kernel != "" && u.Kernel == c.KnownGood {
			keep("the Known-good kernel")
		}
		for i, e := range entries {
			if !strings.EqualFold(e.Resource, rel) {
				continue
			}
			u.Entries = append(u.Entries, e.Path)
			if c.Selected != "" && e.ID == c.Selected {
				keep("booted last")
			}
			if isDefault[i] {
				keep("the default entry")
			}
		}
		if u.Kernel == "" {
			keep("its kernel can't be read")
		}
		ukis = append(ukis, u)
		return nil
	})
	sort.SliceStable(ukis, func(i, j int) bool { return ukis[i].Size > ukis[j].Size })
	return ukis, ok
}

// liveESPInventory is espInventoryText for this Mac's Limine ESP, read now.
func liveESPInventory(lim *limineLayout, free, allocation uint64, knownGood string) string {
	c := espContext{Running: readTrim("/proc/sys/kernel/osrelease"), KnownGood: knownGood}
	if b, err := os.ReadFile(lim.Conf); err == nil {
		c.Menu = b
	}
	if _, data, err := readEfivar("LoaderEntrySelected", bliGUID); err == nil {
		c.Selected = fromUTF16z(data)
	}
	_, size := largestUKI(lim.ESP.Target)
	if size == 0 {
		size = ukiSizeGuess
	}
	need, err := ukiRequiredSpace(size, allocation)
	if err != nil {
		return ""
	}
	ukis, ok := espInventory(lim.ESP.Target, allocation, c)
	return espInventoryText(lim.ESP.Target, ukis, ok, free, need)
}

func roundUp(n, unit uint64) uint64 {
	if unit == 0 {
		return n
	}
	return (n + unit - 1) / unit * unit
}

// espInventoryText says what espInventory found, for the end of the space
// problem: each UKI, then which ones the Mac doesn't need and what removing
// them would free against what staging needs.
func espInventoryText(esp string, ukis []espUKI, ok bool, free, need uint64) string {
	if len(ukis) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, " %s holds:", filepath.Join(esp, "EFI", "Linux"))
	var spare []string
	var freed uint64
	for i, u := range ukis {
		if i > 0 {
			b.WriteString(";")
		}
		kernel := u.Kernel
		if kernel == "" {
			kernel = "unknown"
		}
		fmt.Fprintf(&b, " %s (%.1f MiB, kernel %s", filepath.Base(u.Name), mib(u.Size), kernel)
		if len(u.Keep) > 0 {
			b.WriteString(", " + strings.Join(u.Keep, ", "))
		}
		if len(u.Entries) == 0 {
			b.WriteString(", in no menu entry")
		} else {
			b.WriteString(", menu entry " + strings.Join(u.Entries, " and "))
		}
		b.WriteString(")")
		if len(u.Keep) == 0 {
			spare = append(spare, filepath.Base(u.Name))
			freed += u.Size
		}
	}
	b.WriteString(".")
	switch {
	case !ok:
		b.WriteString(" lab-agent can't tell limine.conf's default entry, so it names none of them as unneeded.")
	case len(spare) == 0:
		b.WriteString(" The Mac needs every one of them.")
	default:
		enough := "enough to stage a test kernel"
		if free+freed < need {
			enough = fmt.Sprintf("still %.1f MiB short", mib(need-free-freed))
		}
		fmt.Fprintf(&b, " Not running, booted last, the default entry or the Known-good kernel: %s; removing them and their menu entries would free %.1f MiB, %s.",
			strings.Join(spare, ", "), mib(freed), enough)
	}
	b.WriteString(" lab-agent removes none of them.")
	return b.String()
}
