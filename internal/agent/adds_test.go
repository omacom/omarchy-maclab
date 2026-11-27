package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// An offline stage installs with the candidate's own pacman options and no
// repository at all.
func TestTheOfflineConfigurationHasTheOptionsAndNoRepository(t *testing.T) {
	cand := t.TempDir()
	os.MkdirAll(filepath.Join(cand, "etc"), 0o755)
	os.WriteFile(filepath.Join(cand, "etc/pacman.conf"), []byte(`#
[options]
HoldPkg = pacman glibc
Architecture = auto
Include = /etc/pacman.d/extra-options
SigLevel = Required DatabaseOptional

[core]
Include = /etc/pacman.d/mirrorlist

[omarchy]
Server = https://pkgs.omarchy.org/$arch
`), 0o644)
	if err := writeOfflineConf(cand); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(cand, offlineConf))
	got := string(b)
	for _, want := range []string{"[options]", "HoldPkg = pacman glibc", "SigLevel = Required DatabaseOptional"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	for _, not := range []string{"[core]", "[omarchy]", "Server", "Include"} {
		if strings.Contains(got, not) {
			t.Errorf("%q in\n%s", not, got)
		}
	}
}

// The files pacman downloaded are named by package and version, each with
// its sha256 and the parts it is sent in; a signature is not a package.
func TestAddsFilesAreThePackagesWithTheirParts(t *testing.T) {
	was := api.AddsPart
	api.AddsPart = 4
	t.Cleanup(func() { api.AddsPart = was })
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "omarchy-mlx-0.7.28-5-aarch64.pkg.tar.zst"), []byte("123456789"), 0o644)
	os.WriteFile(filepath.Join(dir, "omarchy-mlx-0.7.28-5-aarch64.pkg.tar.zst.sig"), []byte("sig"), 0o644)
	os.WriteFile(filepath.Join(dir, "blas-3.12.1-2-aarch64.pkg.tar.xz"), []byte("1234"), 0o644)
	got, err := addsFiles(dir)
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	if b := got[0]; b.Name != "blas" || b.Version != "3.12.1-2" || b.Parts != 1 || b.Size != 4 {
		t.Errorf("blas: %+v", b)
	}
	if m := got[1]; m.Name != "omarchy-mlx" || m.Version != "0.7.28-5" || m.Parts != 3 || len(m.SHA256) != 64 {
		t.Errorf("omarchy-mlx: %+v", m)
	}
	if _, err := addsFiles(t.TempDir()); err == nil {
		t.Error("no file was taken as packages")
	}
	for _, bad := range [][]string{nil, {"--config=/x"}, {"a b"}} {
		if checkAdds(bad) == nil {
			t.Errorf("%q taken", bad)
		}
	}
}

// A resolve keeps the Mac's repositories and none of the options that name
// its own database, cache, keyring, log or hooks.
func TestTheResolveConfigurationKeepsTheRepositoriesAndNoHostPath(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "pacman.conf"), filepath.Join(dir, "resolve.conf")
	os.WriteFile(from, []byte("[options]\nCacheDir = /var/cache/pacman/pkg/\nGPGDir=/etc/pacman.d/gnupg\nLogFile = /var/log/pacman.log\nHookDir = /etc/pacman.d/hooks/\nSigLevel = Required\n\n[core]\nInclude = /etc/pacman.d/mirrorlist\n"), 0o644)
	if err := writeResolveConf(from, to); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(to)
	got := string(b)
	for _, not := range []string{"CacheDir", "GPGDir", "LogFile", "HookDir"} {
		if strings.Contains(got, not) {
			t.Errorf("%s kept:\n%s", not, got)
		}
	}
	for _, want := range []string{"SigLevel = Required", "[core]", "Include = /etc/pacman.d/mirrorlist"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q:\n%s", want, got)
		}
	}
}

// The parts of a file are its bytes, in order.
func TestAPartsCopyIsTheFileInOrder(t *testing.T) {
	was := api.AddsPart
	api.AddsPart = 4
	t.Cleanup(func() { api.AddsPart = was })
	dir := t.TempDir()
	src := filepath.Join(dir, "f")
	os.WriteFile(src, []byte("abcdefghij"), 0o644)
	in, _ := os.Open(src)
	defer in.Close()
	var all []string
	for n := 0; n < 3; n++ {
		part := filepath.Join(dir, "p"+string(rune('0'+n)))
		if err := copyPart(in, part); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(part)
		all = append(all, string(b))
	}
	if strings.Join(all, "|") != "abcd|efgh|ij" {
		t.Errorf("parts %q", all)
	}
}
