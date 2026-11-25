package repotest

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// Every source a benchmark program is built from is pinned by commit or by
// file, with its checksum, over https (spec, section 22, "Built by the lab").
func TestBenchPins(t *testing.T) {
	top := root(t)
	pins := read(t, top, "release/bench-pins")
	script := read(t, top, "release/bench.sh")
	line := regexp.MustCompile(`^([a-z][a-z0-9-]*) (\S+) ([0-9a-f]{64}) (https://\S+)$`)
	fetched := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)\bfetch ([a-z][a-z0-9-]*)$`).FindAllStringSubmatch(script, -1) {
		fetched[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`(?m)for \w+ in ([a-z0-9 -]+); do fetch "\$\w+"; done`).FindAllStringSubmatch(script, -1) {
		for _, name := range strings.Fields(m[1]) {
			fetched[name] = true
		}
	}
	seen := map[string]bool{}
	for _, l := range strings.Split(pins, "\n") {
		if l = strings.TrimSpace(strings.SplitN(l, "#", 2)[0]); l == "" {
			continue
		}
		m := line.FindStringSubmatch(l)
		if m == nil {
			t.Errorf("release/bench-pins: not name version sha256 https-url: %q", l)
			continue
		}
		name, version, sum, url := m[1], m[2], m[3], m[4]
		if seen[name] {
			t.Errorf("release/bench-pins: two lines for %s", name)
		}
		seen[name] = true
		if sum == strings.Repeat("0", 64) {
			t.Errorf("release/bench-pins: %s has no checksum yet", name)
		}
		if regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(version) {
			if !strings.HasPrefix(url, "https://github.com/") || strings.Count(strings.TrimPrefix(url, "https://github.com/"), "/") != 1 {
				t.Errorf("release/bench-pins: %s is pinned by commit, but %s is not a repository", name, url)
			}
		} else if strings.HasPrefix(url, "https://github.com/") && strings.Count(strings.TrimPrefix(url, "https://github.com/"), "/") == 1 {
			t.Errorf("release/bench-pins: %s names a repository by %q, not by a whole commit", name, version)
		}
		if !fetched[name] {
			t.Errorf("release/bench-pins pins %s, which release/bench.sh never fetches", name)
		}
	}
	for name := range fetched {
		if !seen[name] {
			t.Errorf("release/bench.sh fetches %s, which release/bench-pins doesn't pin", name)
		}
	}
	for _, name := range []string{"coremark", "stream", "clpeak", "fio", "mlx"} {
		if !seen[name] {
			t.Errorf("release/bench-pins has no line for %s", name)
		}
	}
	for _, s := range []string{"STREAM_ARRAY_SIZE=67108864", "PORT_CFLAGS=-O2", "--disable-optimizations", "FETCHCONTENT_FULLY_DISCONNECTED=ON", `BENCH_XCODE`} {
		if !strings.Contains(script, s) {
			t.Errorf("release/bench.sh lacks %q", s)
		}
	}
}

// The lab's own three programs, as the spec has them: Core ML with CPU and
// Neural Engine and its placement proved where macOS can; PSTR through
// AppleSMC once a second; an fp16 matmul synchronised on the GPU.
func TestTheLabsOwnBenchmarkSources(t *testing.T) {
	top := root(t)
	for rel, wants := range map[string][]string{
		"tools/bench-macos/ane-bench/main.swift":      {".cpuAndNeuralEngine", "MLComputePlan.load", "#available(macOS 14.4, *)", `"inferences_per_s"`, `"placement"`, `"model_sha256"`},
		"tools/bench-macos/ane-bench/mobilenet_v1.py": {"SPEC_VERSION", "<{count}e"},
		"tools/bench-macos/smc-power/smc-power.c":     {`IOServiceMatching("AppleSMC")`, `fourcc("PSTR")`, `"mean_watts"`, `"valid"`, `"no_pstr"`, "EXIT_NO_PSTR"},
		"tools/bench-macos/mlx-bench/mlx-bench.cpp":   {"mx::float16", "mx::matmul", "mx::synchronize", "mx::metal::is_available", `\"tflops\"`},
	} {
		src := read(t, top, rel)
		for _, w := range wants {
			if !strings.Contains(src, w) {
				t.Errorf("%s lacks %s", rel, w)
			}
		}
	}
}

// MobileNet v1 is the same bytes wherever it is written.
func TestMobileNetIsAlwaysTheSameBytes(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("needs python3")
	}
	out := filepath.Join(t.TempDir(), "m.mlmodel")
	if b, err := exec.Command("python3", "-I", filepath.Join(root(t), "tools/bench-macos/ane-bench/mobilenet_v1.py"), out).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(b)); got != "f88cb406f7fe4a6db0351b4857e4c1569a02d00441f67c18edbbc04d9cc4e1b0" {
		t.Errorf("the model's sha256 is %s", got)
	}
}

// release/bench.sh against sources made up here: a git repository standing
// in for CoreMark's and a file for STREAM's. It builds what it checked and
// refuses anything else, leaving nothing behind.
func TestBenchProgramsAreBuiltOnlyFromTheirPins(t *testing.T) {
	if runtime.GOARCH != "arm64" || (runtime.GOOS != "darwin" && runtime.GOOS != "linux") {
		t.Skip("benchmark programs are built on arm64 macOS or Linux")
	}
	for _, tool := range []string{"git", "make", "bash", "clang", "curl", "od"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("needs %s", tool)
		}
	}
	top := root(t)
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sum := func(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

	repo := t.TempDir()
	git(t, repo, "init", "--quiet")
	write(filepath.Join(repo, "Makefile"), "compile:\n\t$(CC) $(PORT_CFLAGS) $(XCFLAGS) -o coremark.exe main.c\n")
	write(filepath.Join(repo, "main.c"), "#include <stdio.h>\nint main(void) { puts(\"coremark from the pin\"); return 0; }\n")
	write(filepath.Join(repo, "LICENSE.md"), "the licence\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "--quiet", "-m", "the pinned commit")
	commit := git(t, repo, "rev-parse", "HEAD")
	tar, err := exec.Command("git", "-C", repo, "-c", "tar.umask=0002", "archive", "--format=tar", commit).Output()
	if err != nil {
		t.Fatal(err)
	}
	coreSum := sum(tar)
	write(filepath.Join(repo, "main.c"), "int main(void) { return 1; }\n")
	git(t, repo, "commit", "--quiet", "-am", "a later commit")
	later := git(t, repo, "rev-parse", "HEAD")

	files := t.TempDir()
	stream := "#include <stdio.h>\nint main(void) { printf(\"stream %d\\n\", STREAM_ARRAY_SIZE); return 0; }\n"
	write(filepath.Join(files, "stream.c"), stream)
	write(filepath.Join(files, "LICENSE.txt"), "stream's licence\n")
	streamLine := "stream 5.10 " + sum([]byte(stream)) + " file://" + filepath.Join(files, "stream.c")
	licenceLine := "stream-licence 5.10 " + sum([]byte("stream's licence\n")) + " file://" + filepath.Join(files, "LICENSE.txt")
	coreLine := "coremark " + commit + " " + coreSum + " file://" + repo

	run := func(pins []string, programs ...string) (string, string, error) {
		t.Helper()
		dir := t.TempDir()
		write(filepath.Join(dir, "pins"), "# the tests' pins\n"+strings.Join(pins, "\n")+"\n")
		out := filepath.Join(dir, "out")
		cmd := exec.Command(filepath.Join(top, "release", "bench.sh"), append([]string{runtime.GOOS, out}, programs...)...)
		cmd.Env = append(os.Environ(), "BENCH_PINS="+filepath.Join(dir, "pins"), "CC=clang", "BENCH_XCODE=")
		b, err := cmd.CombinedOutput()
		return out, string(b), err
	}

	out, log, err := run([]string{coreLine, streamLine, licenceLine}, "coremark", "stream")
	if err != nil {
		t.Fatalf("the pinned sources were not built: %v\n%s", err, log)
	}
	for p, said := range map[string]string{"coremark": "coremark from the pin", "stream": "stream 67108864"} {
		b, err := exec.Command(filepath.Join(out, p, "bin", p)).Output()
		if err != nil || strings.TrimSpace(string(b)) != said {
			t.Errorf("%s says %q, %v; want %q", p, b, err, said)
		}
		build := read(t, out, p+"/BUILD.json")
		for _, w := range []string{`"program":"` + p + `"`, `"system":"` + runtime.GOOS + `-arm64"`, `"cc":`, `"flags":"-O2`} {
			if !strings.Contains(build, w) {
				t.Errorf("%s's BUILD.json lacks %s: %s", p, w, build)
			}
		}
	}
	if b := read(t, out, "coremark/BUILD.json"); !strings.Contains(b, coreSum) || !strings.Contains(b, commit) {
		t.Errorf("coremark's BUILD.json doesn't name its commit and checksum: %s", b)
	}
	if _, err := os.Stat(filepath.Join(out, "stream", "LICENSE.txt")); err != nil {
		t.Errorf("STREAM's licence is not beside it: %v", err)
	}

	for why, c := range map[string]struct {
		pins    []string
		program string
	}{
		"another checksum":           {[]string{"coremark " + commit + " " + strings.Repeat("0", 64) + " file://" + repo}, "coremark"},
		"the later commit's files":   {[]string{"coremark " + later + " " + coreSum + " file://" + repo}, "coremark"},
		"a commit nobody has":        {[]string{"coremark " + strings.Repeat("1", 40) + " " + coreSum + " file://" + repo}, "coremark"},
		"a changed file":             {[]string{"stream 5.10 " + strings.Repeat("a", 64) + " file://" + filepath.Join(files, "stream.c"), licenceLine}, "stream"},
		"plain http":                 {[]string{"stream 5.10 " + sum([]byte(stream)) + " http://example.org/stream.c", licenceLine}, "stream"},
		"a fifth word":               {[]string{streamLine + " extra", licenceLine}, "stream"},
		"two lines":                  {[]string{streamLine, streamLine, licenceLine}, "stream"},
		"no line":                    {[]string{licenceLine}, "stream"},
		"a program it doesn't build": {[]string{streamLine, licenceLine}, "geekbench"},
	} {
		out, log, err := run(c.pins, c.program)
		if err == nil {
			t.Errorf("%s: built\n%s", why, log)
		}
		if _, statErr := os.Stat(filepath.Join(out, c.program)); statErr == nil {
			t.Errorf("%s: a program was left behind:\n%s", why, log)
		}
	}

	if !strings.Contains(read(t, top, "release/bench.sh"), `file:///*) [[ -n ${BENCH_PINS:-} ]] || die`) {
		t.Error("release/bench.sh takes a file:// source from release/bench-pins")
	}
}
