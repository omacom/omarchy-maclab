package repotest

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The release workflow's rules (spec, section 11, "A Lab release"), read from
// the file: the environment's own rules are the owner's to set on GitHub.
func TestReleaseWorkflow(t *testing.T) {
	top := root(t)
	wf := read(t, top, ".github/workflows/release.yml")

	if !strings.Contains(wf, "on:\n  push:\n    tags: [\"v*\"]\n") {
		t.Error("the trigger is not a pushed v* tag")
	}
	for _, other := range []string{"pull_request", "workflow_dispatch", "schedule:", "branches:"} {
		if strings.Contains(wf, other) {
			t.Errorf("a second trigger: %s", other)
		}
	}
	if strings.Contains(wf, "self-hosted") {
		t.Error("a self-hosted runner")
	}
	// One macOS runner, with a pinned Xcode, builds the reset tool and the
	// macOS benchmark programs and nothing else; one arm64 Linux runner
	// builds every other file.
	mac, rest, ok := strings.Cut(wf, "\n  build:\n")
	if !ok || !strings.Contains(mac, "\n  macos:\n") {
		t.Fatal("no macos job before the build job")
	}
	if runs := regexp.MustCompile(`runs-on: *(.*)`).FindAllStringSubmatch(mac, -1); len(runs) != 1 || runs[0][1] != "macos-15" {
		t.Errorf("the macOS files are built on %v, want one macOS runner", runs)
	}
	var steps []string
	for _, m := range regexp.MustCompile(`(?m)^ +run: *(.*)$`).FindAllStringSubmatch(mac, -1) {
		steps = append(steps, m[1])
	}
	if want := []string{
		`release/reset-tool.sh "$RUNNER_TEMP/reset-tool"`,
		`release/bench.sh darwin "$RUNNER_TEMP/bench/darwin-arm64"`,
		`tar -cf "$RUNNER_TEMP/bench-darwin.tar" -C "$RUNNER_TEMP/bench" darwin-arm64`,
	}; strings.Join(steps, "\n") != strings.Join(want, "\n") {
		t.Errorf("the macOS runner runs %q, want %q: the reset tool and the benchmark programs alone", steps, want)
	}
	xcode := regexp.MustCompile(`(?m)^      DEVELOPER_DIR: /Applications/Xcode_(\d+\.\d+(?:\.\d+)?)\.app/Contents/Developer\n      BENCH_XCODE: Xcode (\S+)$`).FindStringSubmatch(mac)
	if xcode == nil || xcode[1] != xcode[2] {
		t.Errorf("the macOS runner doesn't pin one Xcode and check it is the one: %q", xcode)
	}
	for _, s := range []string{"setup-go", "setup-python", "release/build.sh", "make ", "brew ", "secrets."} {
		if strings.Contains(mac, s) {
			t.Errorf("the macos job has %q: the macOS runner builds only the reset tool and the benchmark programs", s)
		}
	}
	for _, m := range regexp.MustCompile(`runs-on: *(.*)`).FindAllStringSubmatch(rest, -1) {
		if m[1] != "ubuntu-24.04-arm" {
			t.Errorf("runs-on %s: one arm64 Linux runner builds every file but the macOS ones", m[1])
		}
	}
	if strings.Contains(wf, "LAB_RELEASE_WITHOUT") {
		t.Error("the release workflow leaves out files: only a release made on one machine may")
	}
	for _, m := range regexp.MustCompile(`secrets\.(\w+)`).FindAllStringSubmatch(wf, -1) {
		if m[1] != "RELEASE_SIGNING_KEY" {
			t.Errorf("a second secret: %s", m[1])
		}
	}

	build, publish, ok := strings.Cut(wf, "\n  publish:\n")
	if !ok || !strings.Contains(build, "\n  build:\n") {
		t.Fatal("no build job followed by a publish job")
	}
	for _, s := range []string{"secrets.", "environment:", "contents: write", "release/sign.sh", "gh release"} {
		if strings.Contains(build, s) {
			t.Errorf("the build job has %q: it runs before the owner's approval", s)
		}
	}
	for _, s := range []string{
		"release/check.sh",
		`LAB_RELEASE="$GITHUB_REF_NAME" RESET_TOOL="$RUNNER_TEMP/reset-tool/macvdmtool" BENCH_PROGRAMS="$RUNNER_TEMP/bench" release/build.sh`,
		"    needs: macos\n",
		"grep -qx lab-server/bin/macvdmtool",
		`release/bench.sh linux "$RUNNER_TEMP/bench/linux-arm64"`,
		"          CC: clang-18\n          CXX: clang++-18\n",
		"name: bench-darwin\n",
		`for a in "${RELEASE_BENCH[@]}"; do`,
		`git merge-base --is-ancestor HEAD origin/main`,
	} {
		if !strings.Contains(build, s) {
			t.Errorf("the build job lacks %q", s)
		}
	}
	for _, s := range []string{
		"    needs: build\n",
		"    environment: release\n",
		"secrets.RELEASE_SIGNING_KEY",
		"release/sign.sh",
		`release/verify.sh dist release/allowed_signers "$GITHUB_REF_NAME"`,
	} {
		if !strings.Contains(publish, s) {
			t.Errorf("the publish job lacks %q", s)
		}
	}
	for _, s := range []string{"go build", "setup-go", "make ", "release/build.sh", "pip ", "npm "} {
		if strings.Contains(publish, s) {
			t.Errorf("the publish job has %q: the release key shares no job with a build", s)
		}
	}

	// A Release candidate is a pre-release and never latest; vX.Y.Z is latest.
	candidate, final, ok := strings.Cut(publish, "\n          else\n")
	if !ok || !strings.Contains(candidate, `if [[ "$GITHUB_REF_NAME" == *-rc.* ]]; then`) {
		t.Fatal("the publish step doesn't tell a Release candidate from a Lab release")
	}
	if !strings.Contains(candidate, "--prerelease --latest=false") {
		t.Error("a Release candidate is not published as a pre-release that is never latest")
	}
	if strings.Contains(strings.ReplaceAll(candidate, "--latest=false", ""), "--latest") || strings.Contains(candidate, "--generate-notes") {
		t.Error("a Release candidate is marked latest or gets release notes")
	}
	if !strings.Contains(final, "--latest") || strings.Contains(final, "--latest=false") || strings.Contains(final, "--prerelease") {
		t.Error("vX.Y.Z is not published as latest")
	}
	if !strings.Contains(final, "--generate-notes") {
		t.Error("vX.Y.Z gets no generated release notes")
	}

	if tests := read(t, top, ".github/workflows/tests.yml"); !strings.Contains(tests, "release/check.sh") {
		t.Error("the tests workflow doesn't build and check a Lab release's files")
	}
}

// The files of a Lab release, as release/lib.sh lists them for every
// script: the benchmark programs among them (spec, section 22, "Built by
// the lab"), and LAB_RELEASE_WITHOUT leaving out only those.
func TestReleaseFiles(t *testing.T) {
	top := root(t)
	list := func(without string) (string, error) {
		cmd := exec.Command("bash", "-c", `. "$1/release/lib.sh" && printf '%s\n' "${RELEASE_FILES[@]}"`, "files", top)
		cmd.Env = append(os.Environ(), "LAB_RELEASE_WITHOUT="+without)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := list("")
	if err != nil {
		t.Fatalf("release/lib.sh: %v\n%s", err, out)
	}
	want := []string{
		"omarchy-m-test.tar.gz", "VERSION", "lab-darwin-arm64", "lab-linux-arm64", "lab-linux-amd64",
		"lab-server-darwin-arm64.tar.gz", "lab-macos-runner-darwin-arm64.tar.gz", "lab-controller-darwin-arm64.tar.gz",
		"bench-coremark-darwin-arm64.tar.gz", "bench-stream-darwin-arm64.tar.gz", "bench-clpeak-darwin-arm64.tar.gz",
		"bench-fio-darwin-arm64.tar.gz", "bench-ane-bench-darwin-arm64.tar.gz", "bench-smc-power-darwin-arm64.tar.gz",
		"bench-mlx-bench-darwin-arm64.tar.gz",
		"bench-coremark-linux-arm64.tar.gz", "bench-stream-linux-arm64.tar.gz", "bench-clpeak-linux-arm64.tar.gz",
	}
	if got := strings.Fields(out); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("a Lab release's files are\n%q\nwant\n%q", got, want)
	}

	out, err = list("bench-mlx-bench-darwin-arm64.tar.gz bench-clpeak-linux-arm64.tar.gz")
	if err != nil {
		t.Fatalf("leaving out two benchmark archives: %v\n%s", err, out)
	}
	if got := strings.Fields(out); len(got) != len(want)-2 || strings.Contains(out, "mlx-bench") || strings.Contains(out, "clpeak-linux") {
		t.Errorf("LAB_RELEASE_WITHOUT left %q", got)
	}
	for _, other := range []string{"lab-linux-amd64", "VERSION", "bench-geekbench-darwin-arm64.tar.gz"} {
		if out, err := list(other); err == nil {
			t.Errorf("LAB_RELEASE_WITHOUT=%s was taken: %s", other, out)
		}
	}

	for _, rel := range []string{"release/build.sh", "release/check.sh", "release/verify.sh", "release/local.sh"} {
		if !strings.Contains(read(t, top, rel), "bench") {
			t.Errorf("%s doesn't know the benchmark programs", rel)
		}
	}
}

// One key, pinned in three places that must agree.
func TestReleaseKeyPins(t *testing.T) {
	top := root(t)
	signers := strings.TrimSpace(read(t, top, "release/allowed_signers"))
	if strings.Count(signers, "\n") != 0 || !strings.HasPrefix(signers, `release@omarchy-m-testing.org namespaces="omarchy-m-test-release" ssh-ed25519 `) {
		t.Fatalf("release/allowed_signers is not one ed25519 release key: %q", signers)
	}
	for _, rel := range []string{"installer/install.sh", "installer/install-lab.sh", "docs/omarchy-m-test.md"} {
		if !strings.Contains(read(t, top, rel), signers) {
			t.Errorf("%s doesn't pin the key of release/allowed_signers", rel)
		}
	}
	lib := read(t, top, "release/lib.sh")
	agent := read(t, top, "internal/agent/release.go")
	for _, name := range []string{"release@omarchy-m-testing.org", "omarchy-m-test-release"} {
		if !strings.Contains(lib, `"`+name+`"`) || !strings.Contains(agent, `"`+name+`"`) {
			t.Errorf("release/lib.sh and internal/agent/release.go don't both sign as %s", name)
		}
	}
}
