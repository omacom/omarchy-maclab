package minitest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// A made-up program: bytes that are no text, naming nothing under Homebrew.
const program = "\xcf\xfa\xed\xfe\x0c\x00\x00\x01\x00/usr/lib/libSystem.B.dylib\x00@loader_path\x00"

// keg lays one formula out as Homebrew does: Cellar/<name>/<version> with the
// tool's files (a value starting "-> " is a symbolic link) and Homebrew's own
// notes about the keg, opt/<name> linked to it, and its program linked into
// the prefix's bin. What is there of the formula before goes.
func (m *mini) keg(name, version string, files map[string]string) {
	m.t.Helper()
	prefix := m.path("opt/homebrew")
	keg := filepath.Join(prefix, "Cellar", name, version)
	os.RemoveAll(filepath.Dir(keg))
	for file, text := range files {
		path := filepath.Join(keg, file)
		if to, ok := strings.CutPrefix(text, "-> "); ok {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				m.t.Fatal(err)
			}
			if err := os.Symlink(to, path); err != nil {
				m.t.Fatal(err)
			}
			continue
		}
		mode := os.FileMode(0o444)
		if strings.Contains(file, "bin/") {
			mode = 0o555
		}
		write(m.t, path, text, mode)
	}
	// The receipt of a formula read from a tap names the tap, under the prefix.
	write(m.t, keg+"/INSTALL_RECEIPT.json", `{"poured_from_bottle":true,"source":{"path":"`+prefix+`/Library/Taps/homebrew/homebrew-core/Formula/`+name+`.rb"}}`+"\n", 0o644)
	write(m.t, keg+"/sbom.spdx.json", `{"name":"SBOM-SPDX-`+name+`-`+version+`","documentNamespace":"`+prefix+`/Cellar/`+name+`"}`+"\n", 0o644)
	write(m.t, keg+"/.brew/"+name+".rb", "class Formula\n  # "+prefix+"/opt/"+name+"\nend\n", 0o644)
	for _, link := range [][2]string{{"../Cellar/" + name + "/" + version, "opt/" + name}, {"../Cellar/" + name + "/" + version + "/bin/" + name, "bin/" + name}} {
		os.MkdirAll(filepath.Dir(filepath.Join(prefix, link[1])), 0o755)
		os.Remove(filepath.Join(prefix, link[1]))
		if err := os.Symlink(link[0], filepath.Join(prefix, link[1])); err != nil {
			m.t.Fatal(err)
		}
	}
}

// theLabsKegs are the five tools of deploy/homebrew-formulae, each with the
// files its formula installs. tailscale's command is the tests' stand-in,
// since the set-up asks it whether the mini is on the tailnet.
func (m *mini) theLabsKegs() {
	m.t.Helper()
	prefix := m.path("opt/homebrew")
	write(m.t, m.tree+"/deploy/homebrew-formulae", read(m.t, filepath.Join(repo(m.t), "deploy", "homebrew-formulae")), 0o644)
	m.keg("cloudflared", "2026.10.1", map[string]string{
		"bin/cloudflared":              program + "cloudflared 2026.10.1",
		"share/man/man1/cloudflared.1": ".TH cloudflared 1\n",
		"LICENSE":                      "Apache License\n",
		// Its formula has a service: Homebrew writes both files into the keg,
		// named sh.brew.<formula> since Homebrew 5.
		"sh.brew.cloudflared.plist":   "<plist><string>" + prefix + "/opt/cloudflared/bin/cloudflared</string><string>" + prefix + "/var/log/cloudflared.log</string></plist>\n",
		"sh.brew.cloudflared.service": "[Service]\nExecStart=" + prefix + "/opt/cloudflared/bin/cloudflared\n",
	})
	m.keg("signal-cli", "0.14.9", map[string]string{
		"bin/signal-cli":              program + "signal-cli 0.14.9",
		"share/man/man1/signal-cli.1": ".TH signal-cli 1\n",
		"CHANGELOG.md":                "# Changelog\n",
	})
	m.keg("litestream", "0.5.18", map[string]string{
		"bin/litestream": program + "litestream 0.5.18",
	})
	m.keg("tailscale", "1.104.7", map[string]string{
		"bin/tailscale":                       tailscaleStub,
		"bin/tailscaled":                      program + "tailscaled 1.104.7",
		"etc/bash_completion.d/tailscale":     "# bash completion for tailscale\n",
		"sh.brew.tailscale.plist":             "<plist><string>" + prefix + "/opt/tailscale/bin/tailscaled</string><string>" + prefix + "/var/log/tailscaled.log</string></plist>\n",
		"sh.brew.tailscale.service":           "[Service]\nExecStart=" + prefix + "/opt/tailscale/bin/tailscaled\n",
		"share/zsh/site-functions/_tailscale": "#compdef tailscale\n",
	})
	m.keg("restic", "0.19.1", map[string]string{
		"bin/restic":                       program + "restic 0.19.1",
		"etc/bash_completion.d/restic":     "# bash completion for restic\n",
		"share/zsh/site-functions/_restic": "#compdef restic\n",
		"share/man/man1/restic.1":          ".TH restic 1\n",
	})
}

// signal-cli as Homebrew shipped it before 0.14: a script that sets a Java
// under Homebrew and runs the real script in its own keg, the jars beside it.
func (m *mini) signalCLIOnJava() {
	m.t.Helper()
	prefix := m.path("opt/homebrew")
	m.keg("signal-cli", "0.13.24", map[string]string{
		"bin/signal-cli": "#!/bin/bash\nexport JAVA_HOME=\"${JAVA_HOME:-" + prefix + "/opt/openjdk/libexec/openjdk.jdk/Contents/Home}\"\n" +
			"exec \"" + prefix + "/Cellar/signal-cli/0.13.24/libexec/bin/signal-cli\"  \"$@\"\n",
		"libexec/bin/signal-cli":                "#!/bin/sh\nexec \"$JAVA_HOME/bin/java\" -classpath \"$(dirname \"$0\")/../lib/*\" org.asamk.signal.Main \"$@\"\n",
		"libexec/lib/signal-cli-0.13.24.jar":    "PK\x03\x04 signal-cli",
		"libexec/lib/libsignal-client-0.76.jar": "PK\x03\x04 libsignal",
	})
	write(m.t, prefix+"/deps/signal-cli", "openjdk\n", 0o644)
	m.java("23.0.2", nil)
}

// java is the openjdk keg: the Java tree under libexec, and links to it
// that stay inside the keg. more adds files to the Java's lib folder.
func (m *mini) java(version string, more map[string]string) {
	m.t.Helper()
	home := "libexec/openjdk.jdk/Contents/Home/"
	files := map[string]string{
		home + "bin/java":                                 "#!/bin/sh\necho openjdk 23.0.2\n",
		home + "bin/keytool":                              program + "keytool",
		home + "lib/libjli.dylib":                         program + "libjli",
		home + "lib/server/libjvm.dylib":                  program + "libjvm",
		home + "lib/modules":                              "\xda\xda\xfe\xca modules",
		home + "conf/security/java.policy":                "grant { };\n",
		home + "include/jni.h":                            "/* jni */\n",
		home + "release":                                  "JAVA_VERSION=\"23.0.2\"\n",
		"libexec/openjdk.jdk/Contents/Info.plist":         "<plist></plist>\n",
		"libexec/openjdk.jdk/Contents/MacOS/libjli.dylib": "-> ../Home/lib/libjli.dylib",
		"bin/java":    "-> ../" + home + "bin/java",
		"bin/keytool": "-> ../" + home + "bin/keytool",
		"include":     "-> " + home + "include",
	}
	for name, text := range more {
		files[home+"lib/"+name] = text
	}
	m.keg("openjdk", version, files)
}

// copies lists the copies of the tools the set-up keeps, links left out.
func (m *mini) copies() []string {
	m.t.Helper()
	entries, err := os.ReadDir(m.path("usr/local/lab/tools"))
	if err != nil {
		m.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestTheCopyTakesTheLabsToolsAsHomebrewLaysThemOut(t *testing.T) {
	m := newMini(t)
	k := newKey(t)
	m.theLabsKegs()
	tools := m.path("usr/local/lab/tools")

	out := m.ok("setup", "--release-key", k.signers)

	for name, version := range map[string]string{"cloudflared": "2026.10.1", "signal-cli": "0.14.9", "litestream": "0.5.18", "restic": "0.19.1"} {
		tool := filepath.Join(tools, "current", name, "bin", name)
		if got := read(t, tool); got != program+name+" "+version {
			t.Errorf("%s in the copy is not the keg's:\n%s", name, out)
		}
		if mode(t, tool)&0o111 == 0 {
			t.Errorf("%s in the copy cannot be run: %v", name, mode(t, tool))
		}
	}
	if got := read(t, tools+"/current/VERSIONS"); got != "litestream 0.5.18\ncloudflared 2026.10.1\nrestic 0.19.1\nsignal-cli 0.14.9\ntailscale 1.104.7\n" {
		t.Errorf("VERSIONS: %q", got)
	}
	if !exists(tools + "/current/restic/share/man/man1/restic.1") {
		t.Error("a tool's own files did not come with it")
	}
	// Homebrew's notes about a keg name its prefix, and no daemon reads them:
	// they stay behind, and nothing that came names Homebrew.
	filepath.Walk(tools, func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		if b, _ := os.ReadFile(p); strings.Contains(string(b), "/opt/homebrew") {
			t.Errorf("%s in the copy names Homebrew", strings.TrimPrefix(p, tools))
		}
		return nil
	})
	for _, name := range []string{"cloudflared", "tailscale", "litestream", "restic", "signal-cli"} {
		for _, note := range []string{"INSTALL_RECEIPT.json", ".brew", "sbom.spdx.json"} {
			if exists(tools + "/current/" + name + "/" + note) {
				t.Errorf("Homebrew's own %s/%s is in the copy", name, note)
			}
		}
	}
	for _, note := range []string{"cloudflared/sh.brew.cloudflared.plist", "cloudflared/sh.brew.cloudflared.service", "tailscale/sh.brew.tailscale.plist", "tailscale/sh.brew.tailscale.service"} {
		if exists(tools + "/current/" + note) {
			t.Errorf("Homebrew's own %s is in the copy", note)
		}
	}
	sealed(t, tools)
	if !m.senderLoaded() {
		t.Errorf("the sender was not started from the copy:\n%s", out)
	}

	os.Remove(m.log)
	if out := m.ok("setup"); strings.Contains(out, "copy the Homebrew tools") || len(m.copies()) != 1 {
		t.Errorf("a second run copied the tools again:\n%s", out)
	}
}

// A Java's own links lead to its own files: its copy is whole, and runs.
func TestTheCopyTakesAJavaWithItsLinks(t *testing.T) {
	m := newMini(t)
	k := newKey(t)
	write(t, m.tree+"/deploy/homebrew-formulae", "openjdk\n", 0o644)
	m.java("23.0.2", nil)
	m.ok("setup", "--release-key", k.signers)
	java := m.path("usr/local/lab/tools/current/openjdk/bin/java")
	if out, err := exec.Command(java).CombinedOutput(); err != nil || string(out) != "openjdk 23.0.2\n" {
		t.Errorf("java from the copy: %v\n%s", err, out)
	}
	sealed(t, m.path("usr/local/lab/tools"))
}

// signal-cli as Homebrew ships it on Java, a wrapper script that runs the
// keg's own script on Homebrew's Java: the copy takes the whole Java tree
// beside it and points the wrapper at the two copies, so that nothing it
// runs is under Homebrew. Which of the two ways signal-cli comes is the
// owner's `brew info signal-cli` on the mini; this is the Java one.
func TestTheCopyTakesSignalCLIOnJavaWithItsWholeJavaAndRewritesTheWrapper(t *testing.T) {
	m := newMini(t)
	k := newKey(t)
	m.theLabsKegs()
	m.signalCLIOnJava()
	tools := m.path("usr/local/lab/tools")
	m.ok("setup", "--release-key", k.signers)

	if got := read(t, tools+"/current/VERSIONS"); !strings.Contains(got, "openjdk 23.0.2\n") || !strings.Contains(got, "signal-cli 0.13.24\n") {
		t.Errorf("VERSIONS: %q", got)
	}
	for _, file := range []string{"openjdk/libexec/openjdk.jdk/Contents/Home/lib/server/libjvm.dylib", "openjdk/libexec/openjdk.jdk/Contents/Home/lib/modules",
		"openjdk/libexec/openjdk.jdk/Contents/Home/conf/security/java.policy", "signal-cli/libexec/lib/signal-cli-0.13.24.jar"} {
		if !exists(tools + "/current/" + file) {
			t.Errorf("the copy lacks %s", file)
		}
	}
	current, _ := os.Readlink(tools + "/current")
	copied := tools + "/" + current
	wrapper := tools + "/current/signal-cli/bin/signal-cli"
	text := read(t, wrapper)
	for _, want := range []string{copied + "/openjdk/libexec/openjdk.jdk/Contents/Home", copied + "/signal-cli/libexec/bin/signal-cli"} {
		if !strings.Contains(text, want) {
			t.Errorf("the wrapper does not run %s:\n%s", want, text)
		}
	}
	if mode(t, wrapper)&0o111 == 0 {
		t.Errorf("the wrapper cannot be run: %v", mode(t, wrapper))
	}
	filepath.Walk(tools, func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		if b, _ := os.ReadFile(p); strings.Contains(string(b), "/opt/homebrew") {
			t.Errorf("%s in the copy names Homebrew", strings.TrimPrefix(p, tools))
		}
		return nil
	})
	sealed(t, tools)

	// It runs from the copy with Homebrew gone.
	prefix := m.path("opt/homebrew")
	if err := os.Rename(prefix, prefix+".gone"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(prefix+".gone", prefix)
	run := exec.Command(wrapper)
	run.Env = []string{"PATH=/usr/bin:/bin"}
	if out, err := run.CombinedOutput(); err != nil || string(out) != "openjdk 23.0.2\n" {
		t.Errorf("signal-cli from the copy: %v\n%s", err, out)
	}
}

// refusedCopy runs the set-up over a mini whose tools were copied once and
// whose kegs then changed, and wants the new copy refused for this reason
// with the copy in use left as it was.
func (m *mini) refusedCopy(want string) {
	m.t.Helper()
	tools := m.path("usr/local/lab/tools")
	before, _ := os.Readlink(tools + "/current")
	had := exists(tools + "/previous")
	os.Remove(m.log)

	out := m.refused("setup")

	if !strings.Contains(out, want) || !strings.Contains(out, "would still run files the admin account can write. Nothing was changed") {
		m.t.Errorf("the refusal does not say %q:\n%s", want, out)
	}
	if now, _ := os.Readlink(tools + "/current"); now != before || exists(tools+"/previous") != had {
		m.t.Errorf("the copy in use changed: %s, was %s", now, before)
	}
	if got := m.copies(); len(got) != 1 || got[0] != before {
		m.t.Errorf("the refused copy was left behind: %v", got)
	}
	if strings.Contains(m.calls(), "launchctl boot") {
		m.t.Errorf("a daemon was started or stopped:\n%s", m.calls())
	}
}

func TestTheCopyRefusesAToolThatStillLeansOnHomebrew(t *testing.T) {
	prefix := func(m *mini) string { return m.path("opt/homebrew") }
	for name, c := range map[string]struct {
		lean func(m *mini)
		want string
	}{
		"a link into Homebrew": {
			func(m *mini) {
				write(m.t, prefix(m)+"/opt/other/lib/libother.dylib", program, 0o644)
				m.keg("restic", "0.20.0", map[string]string{"bin/restic": program, "lib": "-> " + prefix(m) + "/opt/other/lib"})
			},
			"restic/lib is a link that leaves the copy (to PREFIX/opt/other/lib)",
		},
		"a link that climbs out of its keg": {
			func(m *mini) {
				write(m.t, prefix(m)+"/opt/other/lib/libother.dylib", program, 0o644)
				m.keg("restic", "0.20.0", map[string]string{"bin/restic": program, "lib/libother.dylib": "-> ../../../../opt/other/lib/libother.dylib"})
			},
			"restic/lib/libother.dylib is a link that leaves the copy (to ../../../../opt/other/lib/libother.dylib)",
		},
		"a link to a folder above the copy": {
			func(m *mini) {
				m.keg("restic", "0.20.0", map[string]string{"bin/restic": program, "share/up": "-> ../../.."})
			},
			"restic/share/up is a link that leaves the copy (to ../../..)",
		},
		"a script that runs a program under Homebrew": {
			func(m *mini) {
				m.keg("restic", "0.20.0", map[string]string{"bin/restic": "#!/bin/bash\nexec " + prefix(m) + "/opt/other/bin/other \"$@\"\n"})
			},
			"restic/bin/restic names Homebrew's folder (/opt/homebrew/opt/other/bin/other)",
		},
		"a script below the keg named like Homebrew's notes": {
			func(m *mini) {
				m.keg("restic", "0.20.0", map[string]string{"bin/restic": program, "libexec/sh.brew.restic.service": "#!/bin/sh\nexec " + prefix(m) + "/opt/other/bin/other\n"})
			},
			"restic/libexec/sh.brew.restic.service names Homebrew's folder (/opt/homebrew/opt/other/bin/other)",
		},
		"a setting that names a file under Homebrew": {
			func(m *mini) {
				m.keg("restic", "0.20.0", map[string]string{"bin/restic": program, "etc/restic.conf": "plugins = /opt/homebrew/lib/restic\n"})
			},
			"restic/etc/restic.conf names Homebrew's folder (/opt/homebrew/lib/restic)",
		},
		"a program nothing can read the loads of": {
			func(m *mini) {
				m.keg("restic", "0.20.0", map[string]string{"bin/restic": program + "/opt/homebrew/opt/openssl@3/lib/libssl.3.dylib\x00\x00"})
			},
			"restic/bin/restic names Homebrew's folder, and otool could not say what it loads",
		},
		"signal-cli as a script on a Java the copy does not hold": {
			func(m *mini) {
				m.signalCLIOnJava()
				os.Remove(prefix(m) + "/deps/signal-cli")
			},
			"signal-cli/bin/signal-cli names Homebrew's folder (/opt/homebrew/opt/openjdk/libexec/openjdk.jdk/Contents/Home)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := newMini(t)
			k := newKey(t)
			m.theLabsKegs()
			m.ok("setup", "--release-key", k.signers)
			c.lean(m)
			m.refusedCopy(strings.ReplaceAll(c.want, "PREFIX", prefix(m)))
			if got := read(t, m.path("usr/local/lab/tools/current/restic/bin/restic")); got != program+"restic 0.19.1" {
				t.Error("the restic in use is not the one copied before")
			}
		})
	}
}

var machO struct {
	once  sync.Once
	files map[string]string
	err   string
}

// built compiles real programs and libraries that carry Homebrew's folder
// the ways a poured keg does, and returns their bytes by name.
func built(t *testing.T) map[string]string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("Mach-O programs are built and read on macOS only")
	}
	need(t, "cc", "otool")
	machO.once.Do(func() {
		dir, err := os.MkdirTemp("", "labkegs")
		if err != nil {
			machO.err = err.Error()
			return
		}
		defer os.RemoveAll(dir)
		for file, text := range map[string]string{
			"one.c": "int one(void) { return 1; }\n",
			"two.c": "int one(void);\nint two(void) { return one() + 1; }\n",
			// A native image keeps the paths of the objects it was linked from.
			"main.c": "const char *made = \"/opt/homebrew/Cellar/graalvm/25.4.4.1.1/libexec/graalvm.jdk/Contents/Home/lib/svm/clibraries/darwin-aarch64/liblibchelper.a(cSunMiscSignal.o)\";\nint main(void) { return made[0] == 0; }\n",
		} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(text), 0o644); err != nil {
				machO.err = err.Error()
				return
			}
		}
		machO.files = map[string]string{}
		for _, c := range [][]string{
			// A library Homebrew poured is named by where it was poured.
			{"libjli.dylib", "-dynamiclib", "-install_name", "/opt/homebrew/opt/openjdk/libexec/openjdk.jdk/Contents/Home/lib/libjli.dylib", "one.c"},
			{"libfreetype.6.dylib", "-dynamiclib", "-install_name", "/opt/homebrew/opt/freetype/lib/libfreetype.6.dylib", "one.c"},
			// The JDK's font library is linked against Homebrew's freetype.
			{"libfontmanager.dylib", "-dynamiclib", "-install_name", "@rpath/libfontmanager.dylib", "two.c", "libfreetype.6.dylib"},
			{"native", "main.c"},
			{"searching", "-Wl,-rpath,/opt/homebrew/lib", "main.c"},
		} {
			cmd := exec.Command("cc", append([]string{"-o", c[0]}, c[1:]...)...)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				machO.err = "cc " + strings.Join(c, " ") + ": " + err.Error() + "\n" + string(out)
				return
			}
			b, err := os.ReadFile(filepath.Join(dir, c[0]))
			if err != nil {
				machO.err = err.Error()
				return
			}
			machO.files[c[0]] = string(b)
		}
	})
	if machO.err != "" {
		t.Fatal(machO.err)
	}
	return machO.files
}

// A program Homebrew poured carries its folder in places that take the
// program nowhere: a library's own name, the paths of the build that made
// it. Only what it has the loader fetch from there makes the copy lean.
func TestTheCopyJudgesAProgramByWhatItLoads(t *testing.T) {
	bin := built(t)
	m := newMini(t)
	k := newKey(t)
	write(t, m.tree+"/deploy/homebrew-formulae", "signal-cli\nopenjdk\n", 0o644)
	native := map[string]string{"bin/signal-cli": bin["native"]}
	m.keg("signal-cli", "0.14.9", native)
	m.java("23.0.2", map[string]string{"libjli.dylib": bin["libjli.dylib"]})
	tools := m.path("usr/local/lab/tools")

	m.ok("setup", "--release-key", k.signers)

	for file, carries := range map[string]string{
		"signal-cli/bin/signal-cli":                                  "/opt/homebrew/Cellar/graalvm/",
		"openjdk/libexec/openjdk.jdk/Contents/Home/lib/libjli.dylib": "/opt/homebrew/opt/openjdk/",
	} {
		if !strings.Contains(read(t, tools+"/current/"+file), carries) {
			t.Fatalf("%s in the copy does not carry %s: the test builds no program as Homebrew pours it", file, carries)
		}
	}
	if out, err := exec.Command(tools + "/current/signal-cli/bin/signal-cli").CombinedOutput(); err != nil {
		t.Errorf("the program in the copy does not run: %v\n%s", err, out)
	}
	sealed(t, tools)

	m.java("23.0.3", map[string]string{"libjli.dylib": bin["libjli.dylib"], "libfontmanager.dylib": bin["libfontmanager.dylib"]})
	m.refusedCopy("openjdk/libexec/openjdk.jdk/Contents/Home/lib/libfontmanager.dylib loads from Homebrew's folder (/opt/homebrew/opt/freetype/lib/libfreetype.6.dylib)")

	m.java("23.0.2", map[string]string{"libjli.dylib": bin["libjli.dylib"]})
	m.keg("signal-cli", "0.14.10", map[string]string{"bin/signal-cli": bin["searching"]})
	m.refusedCopy("signal-cli/bin/signal-cli loads from Homebrew's folder (/opt/homebrew/lib)")

	m.keg("signal-cli", "0.14.10", native)
	m.ok("setup")
	if got := read(t, tools+"/current/VERSIONS"); got != "openjdk 23.0.2\nsignal-cli 0.14.10\n" || !exists(tools+"/previous/signal-cli/bin/signal-cli") {
		t.Errorf("after a whole copy again, VERSIONS is %q", got)
	}
}
