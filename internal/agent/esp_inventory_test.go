package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// m3MaxMenu is the shape of a Volunteer's M3 Max limine.conf whose ESP was
// full: an Omarchy directory holding three test kernels and the distro's own,
// a numeric default_entry and one more test kernel at the top.
const m3MaxMenu = `timeout: 5
remember_last_entry: yes
default_entry: 3

/+Omarchy
  //linux-aurora (secure-os base2 15b1e2e verbose)
  protocol: efi
  path: boot():/EFI/Linux/secureos_base2v.efi
  //linux-aurora (secure-os head2 9c50d3c verbose)
  protocol: efi
  path: boot():/EFI/Linux/secureos_head2v.efi
  //linux-aurora (secure-os head2 9c50d3c7ccc4)
  protocol: efi
  path: boot():/EFI/Linux/secureos_head2.efi
  //linux-aurora
  comment: 7.3.0-1-1-ARCH
  protocol: efi
  path: boot():/EFI/Linux/omarchy_linux-aurora.efi#9e60dfc0f967e031
/AVDTest
  protocol: efi
  path: boot():/EFI/Linux/avdtest.efi
`

// espWithUKIs makes an ESP holding a UKI of each kernel at EFI/Linux/<name>,
// padded to size bytes.
func espWithUKIs(t *testing.T, ukis map[string]string, size int) string {
	t.Helper()
	esp := t.TempDir()
	dir := filepath.Join(esp, "EFI", "Linux")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, kernel := range ukis {
		b := miniDefaultUKI(t, kernel, "root=x", []byte("Image"))
		b = append(b, make([]byte, size-len(b))...)
		if kernel == "" {
			b = make([]byte, size) // not a PE: its kernel can't be read
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return esp
}

func TestParseLimineMenu(t *testing.T) {
	entries, def := parseLimineMenu([]byte(m3MaxMenu))
	if def != "3" {
		t.Fatalf("default_entry = %q", def)
	}
	want := []limineMenuEntry{
		{"/Omarchy", "Omarchy", "", true},
		{"/Omarchy/linux-aurora (secure-os base2 15b1e2e verbose)", "Omarchy.linux-aurora--secure-os-base2-15b1e2e-verbose-", "EFI/Linux/secureos_base2v.efi", false},
		{"/Omarchy/linux-aurora (secure-os head2 9c50d3c verbose)", "Omarchy.linux-aurora--secure-os-head2-9c50d3c-verbose-", "EFI/Linux/secureos_head2v.efi", false},
		{"/Omarchy/linux-aurora (secure-os head2 9c50d3c7ccc4)", "Omarchy.linux-aurora--secure-os-head2-9c50d3c7ccc4-", "EFI/Linux/secureos_head2.efi", false},
		{"/Omarchy/linux-aurora", "Omarchy.linux-aurora", "EFI/Linux/omarchy_linux-aurora.efi", false},
		{"/AVDTest", "AVDTest", "EFI/Linux/avdtest.efi", false},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries:\n got %+v\nwant %+v", entries, want)
	}
	for _, c := range []struct {
		res, want string
	}{
		{"boot():/EFI/Linux/a.efi", "EFI/Linux/a.efi"},
		{"boot():/EFI/Linux/a.efi#abc", "EFI/Linux/a.efi"},
		{"boot():/../EFI/Linux/a.efi", "EFI/Linux/a.efi"},
		{"boot(2):/EFI/Linux/a.efi", ""}, // another volume
		{"uuid(1234):/a.efi", ""},
	} {
		if got := espResource(c.res); got != c.want {
			t.Errorf("espResource(%q) = %q, want %q", c.res, got, c.want)
		}
	}
}

func TestDefaultEntries(t *testing.T) {
	entries, _ := parseLimineMenu([]byte(m3MaxMenu))
	for _, c := range []struct {
		def  string
		want []int
		ok   bool
	}{
		{"3", []int{2, 3}, true}, // directories counted, and not counted
		{"1", []int{0, 1}, true},
		{"", []int{0, 1}, true}, // unset is 1
		{"6", []int{5}, true},   // only with directories counted
		{"9", nil, false},
		{"AVDTest", []int{5}, true},
		{"/Omarchy/linux-aurora", []int{4}, true},
		{"Omarchy.linux-aurora", []int{4}, true},
		{"nothing", nil, false},
	} {
		got, ok := defaultEntries(entries, c.def)
		if ok != c.ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("default_entry %q: %v, %v; want %v, %v", c.def, got, ok, c.want, c.ok)
		}
	}
}

func TestESPInventoryNamesOnlyWhatTheMacDoesntNeed(t *testing.T) {
	const size = 61 << 20
	esp := espWithUKIs(t, map[string]string{
		"omarchy_linux-aurora.efi": "7.3.0-1-1-ARCH",
		"secureos_base2v.efi":      "7.3.0-base2-ARCH+",
		"secureos_head2v.efi":      "7.3.0-ARCH+",
		"secureos_head2.efi":       "7.3.0-ARCH+",
		"avdtest.efi":              "7.3.0-avdtest+",
		"stray.efi":                "7.2.11-14-1-ARCH",
	}, size)
	c := espContext{Running: "7.3.0-1-1-ARCH", KnownGood: "7.2.11-14-1-ARCH", Selected: "Omarchy.linux-aurora", Menu: []byte(m3MaxMenu)}
	ukis, ok := espInventory(esp, 4096, c)
	if !ok || len(ukis) != 6 {
		t.Fatalf("ok %v, %d UKIs: %+v", ok, len(ukis), ukis)
	}
	keep := map[string][]string{}
	for _, u := range ukis {
		keep[filepath.Base(u.Name)] = u.Keep
	}
	for name, want := range map[string][]string{
		"omarchy_linux-aurora.efi": {"running", "booted last"},
		"secureos_head2v.efi":      {"the default entry"},
		"secureos_head2.efi":       {"the default entry"},
		"stray.efi":                {"the Known-good kernel"},
		"secureos_base2v.efi":      nil,
		"avdtest.efi":              nil,
	} {
		if !reflect.DeepEqual(keep[name], want) {
			t.Errorf("%s kept for %v, want %v", name, keep[name], want)
		}
	}

	need := uint64(69 << 20)
	text := espInventoryText(esp, ukis, ok, 22<<20, need)
	for _, want := range []string{
		filepath.Join(esp, "EFI", "Linux") + " holds:",
		"omarchy_linux-aurora.efi (61.0 MiB, kernel 7.3.0-1-1-ARCH, running, booted last, menu entry /Omarchy/linux-aurora)",
		"stray.efi (61.0 MiB, kernel 7.2.11-14-1-ARCH, the Known-good kernel, in no menu entry)",
		"the default entry or the Known-good kernel: avdtest.efi, secureos_base2v.efi;",
		"would free 122.0 MiB, enough to stage a test kernel.",
		"lab-agent removes none of them.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text doesn't say %q:\n%s", want, text)
		}
	}
	if short := espInventoryText(esp, ukis, ok, 0, 200<<20); !strings.Contains(short, "still 78.0 MiB short") {
		t.Errorf("not enough even then: %s", short)
	}
}

func TestESPInventoryKeepsEverythingWhenUnsure(t *testing.T) {
	esp := espWithUKIs(t, map[string]string{"a.efi": "6.1-a", "b.efi": "6.1-b", "c.efi": ""}, 8192)
	menu := "default_entry: 7\n/A\n  path: boot():/EFI/Linux/a.efi\n/B\n  path: boot():/EFI/Linux/b.efi\n"

	// A default_entry that names no entry: nothing is named unneeded.
	ukis, ok := espInventory(esp, 512, espContext{Running: "6.1-a", Menu: []byte(menu)})
	if ok {
		t.Fatal("default_entry 7 of 2 entries was told")
	}
	if text := espInventoryText(esp, ukis, ok, 0, 1<<20); !strings.Contains(text, "names none of them as unneeded") || strings.Contains(text, "would free") {
		t.Errorf("unsure, yet: %s", text)
	}

	// No menu read: the same.
	if _, ok := espInventory(esp, 512, espContext{Running: "6.1-a"}); ok {
		t.Error("no limine.conf, yet the default was told")
	}

	// A UKI whose kernel can't be read is kept.
	ukis, _ = espInventory(esp, 512, espContext{Running: "6.1-a", Menu: []byte("default_entry: 1\n" + menu[len("default_entry: 7\n"):])})
	for _, u := range ukis {
		if filepath.Base(u.Name) == "c.efi" && !reflect.DeepEqual(u.Keep, []string{"its kernel can't be read"}) {
			t.Errorf("c.efi kept for %v", u.Keep)
		}
		if filepath.Base(u.Name) == "b.efi" && len(u.Keep) != 0 {
			t.Errorf("b.efi kept for %v", u.Keep)
		}
	}

	// No UKIs: nothing to say.
	if text := espInventoryText(t.TempDir(), nil, true, 0, 1); text != "" {
		t.Errorf("no UKIs: %q", text)
	}
}
