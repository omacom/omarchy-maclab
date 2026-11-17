package minitest

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// deployed is a mini with a pinned key, a release server and a labd. Deploy
// is run from a checkout in an admin's home, which the lab account cannot
// read.
func deployed(t *testing.T) (*mini, key, *release) {
	t.Helper()
	m := newMini(t)
	m.env = append(m.env, "STUB_LAB_CANNOT_READ="+m.tree)
	k := newKey(t)
	m.pinned(k)
	m.labd()
	return m, k, newRelease(t)
}

func TestDeployInstallsABundleItsSignatureMatches(t *testing.T) {
	m, k, r := deployed(t)
	tag := "v0.2.0-rc.1"
	bundle(t, k, filepath.Join(r.dir, tag), tag, true)

	out := m.ok("deploy", "--from", r.from(tag), tag)

	if m.current() != tag {
		t.Fatalf("the mini runs %q, not %s\n%s", m.current(), tag, out)
	}
	if got := read(t, m.path("usr/local/lab/current/VERSION")); got != tag+"\n" {
		t.Errorf("the build's VERSION is %q", got)
	}
	if got := read(t, m.path("Library/LaunchDaemons/"+labdLabel+".plist")); !strings.Contains(got, tag) {
		t.Errorf("labd's LaunchDaemon file is not the build's: %q", got)
	}
	calls := m.calls()
	if !strings.Contains(calls, "launchctl bootstrap system "+m.path("Library/LaunchDaemons/"+labdLabel+".plist")) {
		t.Errorf("labd was not started:\n%s", calls)
	}
	for _, never := range []string{"reboot", "shutdown", "brew"} {
		if strings.Contains(calls, never) {
			t.Errorf("a Deploy called %s:\n%s", never, calls)
		}
	}
	if !strings.Contains(out, "Deployed "+tag) {
		t.Errorf("no word that it was Deployed:\n%s", out)
	}
	// Nobody but its owner may write what root will run.
	err := filepath.Walk(m.path("usr/local/lab/builds"), func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o022 != 0 {
			t.Errorf("%s is writable by others: %v", p, info.Mode())
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A Deploy touches no image: when the build names other images for the
// Builder than those it runs, it says to run the set-up, which builds them.
func TestDeploySaysWhenTheBuilderNeedsTheSetUpForItsImages(t *testing.T) {
	m, k, r := deployed(t)
	plist := m.path("Library/LaunchDaemons/" + builderLabel + ".plist")
	write(t, plist, "    <string>--image</string><string>maclab-kbuild:0123456789ab</string>\n", 0o644)
	builder := map[string]string{"builder/Containerfile": "FROM scratch\n", "builder/kbuild": "cp /in/config .config\n"}
	bundleWith(t, k, filepath.Join(r.dir, "v0.2.0"), "v0.2.0", true, builder)
	out := m.ok("deploy", "--from", r.from("v0.2.0"), "v0.2.0")
	image := regexp.MustCompile(`the Builder runs maclab-kbuild:0123456789ab, not (maclab-kbuild:[0-9a-f]{12}), the image of builder in the Deployed build`).FindStringSubmatch(out)
	if image == nil || !strings.Contains(out, "run sudo "+m.path("usr/local/lab/current/scripts/mini/setup")+" to build the Builder's new images") {
		t.Fatalf("the Deploy did not say the Builder needs the set-up:\n%s", out)
	}
	if strings.Contains(m.calls(), "container") {
		t.Errorf("the Deploy reached container:\n%s", m.calls())
	}

	// The set-up started the Builder with the build's image: nothing to say.
	write(t, plist, "    <string>--image</string><string>"+image[1]+"</string>\n", 0o644)
	bundleWith(t, k, filepath.Join(r.dir, "v0.2.1"), "v0.2.1", true, builder)
	if out := m.ok("deploy", "--from", r.from("v0.2.1"), "v0.2.1"); strings.Contains(out, "setup") || strings.Contains(out, "the Builder runs") {
		t.Errorf("with the Builder on the build's image the Deploy said:\n%s", out)
	}
}

func TestDeployRefusesABundleTheKeyDidNotSign(t *testing.T) {
	m, k, r := deployed(t)
	tag := "v0.2.0-rc.1"
	dir := filepath.Join(r.dir, tag)

	check := func(name string) {
		t.Helper()
		out := m.refused("deploy", "--from", r.from(tag), tag)
		if m.current() != "" || len(m.builds()) != 0 {
			t.Errorf("%s: something was installed (%q, %v)\n%s", name, m.current(), m.builds(), out)
		}
		if strings.Contains(m.calls(), "launchctl boot") {
			t.Errorf("%s: a service was touched:\n%s", name, m.calls())
		}
	}

	bundle(t, newKey(t), dir, tag, true)
	check("signed by another key")

	bundle(t, k, dir, tag, true)
	f, err := os.OpenFile(filepath.Join(dir, bundleName), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("x")
	f.Close()
	check("changed after signing")

	bundle(t, k, dir, tag, true)
	os.Remove(filepath.Join(dir, bundleName+".sig"))
	check("no signature")

	// A true bundle of one version under another's name.
	bundle(t, k, dir, "v0.1.9", true)
	check("another version's bundle")
}

func TestDeployRefusesLatestAndAnythingThatIsNotAVersion(t *testing.T) {
	m, k, r := deployed(t)
	bundle(t, k, filepath.Join(r.dir, "latest"), "latest", true)
	for _, v := range []string{"latest", "main", "0.2.0", "v0.2", "v0.2.0-rc", "v0.2.0-beta.1", ""} {
		out := m.refused("deploy", "--from", r.from(v), v)
		if v == "latest" && !strings.Contains(out, "latest") {
			t.Errorf("the refusal of latest doesn't say so:\n%s", out)
		}
	}
	if r.asked() != 0 {
		t.Errorf("a refused version was still downloaded: %v", r.got)
	}
	if m.current() != "" {
		t.Errorf("the mini runs %q", m.current())
	}
}

func TestDeployPutsThePreviousBuildBackWhenTheCheckFails(t *testing.T) {
	m, k, r := deployed(t)
	good, bad := "v0.2.0-rc.1", "v0.2.0-rc.2"
	bundle(t, k, filepath.Join(r.dir, good), good, true)
	bundle(t, k, filepath.Join(r.dir, bad), bad, false)
	m.ok("deploy", "--from", r.from(good), good)

	out := m.refused("deploy", "--from", r.from(bad), bad)

	if m.current() != good {
		t.Fatalf("the mini runs %q, not %s\n%s", m.current(), good, out)
	}
	if got := read(t, m.path("Library/LaunchDaemons/"+labdLabel+".plist")); !strings.Contains(got, good) {
		t.Errorf("labd's LaunchDaemon file is not the previous build's: %q", got)
	}
	if !reflect.DeepEqual(m.builds(), []string{good}) {
		t.Errorf("builds kept: %v", m.builds())
	}
	if !strings.Contains(out, good+" is back") {
		t.Errorf("no word that %s is back:\n%s", good, out)
	}
	if out, code := m.run("deploy", "--from", r.from(good), good); code != 0 || !strings.Contains(out, "already") {
		t.Errorf("the build put back does not answer: exit %d\n%s", code, out)
	}
}

func TestAFirstDeployThatFailsLeavesLabdStopped(t *testing.T) {
	m, k, r := deployed(t)
	bad := "v0.2.0-rc.1"
	bundle(t, k, filepath.Join(r.dir, bad), bad, false)

	out := m.refused("deploy", "--from", r.from(bad), bad)

	if m.current() != "" || len(m.builds()) != 0 {
		t.Errorf("left behind: current %q, builds %v\n%s", m.current(), m.builds(), out)
	}
	if exists(m.path("Library/LaunchDaemons/" + labdLabel + ".plist")) {
		t.Error("labd's LaunchDaemon file was left behind")
	}
	if m.loaded() {
		t.Errorf("labd was left loaded:\n%s", m.calls())
	}
}

func TestAFourthBuildRemovesTheOldest(t *testing.T) {
	m, k, r := deployed(t)
	tags := []string{"v0.2.0-rc.1", "v0.2.0-rc.2", "v0.2.0-rc.3", "v0.2.0"}
	for _, tag := range tags {
		bundle(t, k, filepath.Join(r.dir, tag), tag, true)
		m.ok("deploy", "--from", r.from(tag), tag)
	}
	if want := []string{"v0.2.0", "v0.2.0-rc.2", "v0.2.0-rc.3"}; !reflect.DeepEqual(m.builds(), want) {
		t.Errorf("builds kept: %v, want %v", m.builds(), want)
	}
	if m.current() != "v0.2.0" {
		t.Errorf("the mini runs %q", m.current())
	}
}

func TestDeployDryRunChecksTheSignatureAndChangesNothing(t *testing.T) {
	m, k, r := deployed(t)
	tag := "v0.2.0-rc.1"
	bundle(t, k, filepath.Join(r.dir, tag), tag, true)

	out := m.ok("deploy", "--dry-run", "--from", r.from(tag), tag)

	if m.current() != "" || len(m.builds()) != 0 || strings.Contains(m.calls(), "launchctl boot") {
		t.Errorf("a dry run changed the mini: current %q, builds %v\n%s", m.current(), m.builds(), m.calls())
	}
	for _, want := range []string{"matches its signature", "would switch", "would restart " + labdLabel} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run doesn't say %q:\n%s", want, out)
		}
	}

	bundle(t, newKey(t), filepath.Join(r.dir, tag), tag, true)
	m.refused("deploy", "--dry-run", "--from", r.from(tag), tag)
}

func TestDeployReadsAFolderToo(t *testing.T) {
	m, k, _ := deployed(t)
	tag := "v0.2.0-rc.1"
	dir := t.TempDir()
	bundle(t, k, dir, tag, true)
	m.ok("deploy", "--from", "file://"+dir, tag)
	if m.current() != tag {
		t.Errorf("the mini runs %q", m.current())
	}
	m.refused("deploy", "--from", "ftp://example.org/x", "v0.2.0-rc.2")
	m.refused("deploy", "--from", "http://example.org/x", "v0.2.0-rc.2")
}

func TestDeployNeedsThePinnedKey(t *testing.T) {
	m := newMini(t)
	k := newKey(t)
	r := newRelease(t)
	tag := "v0.2.0-rc.1"
	bundle(t, k, filepath.Join(r.dir, tag), tag, true)
	out := m.refused("deploy", "--from", r.from(tag), tag)
	if !strings.Contains(out, "setup") {
		t.Errorf("the refusal doesn't name the set-up:\n%s", out)
	}
}
