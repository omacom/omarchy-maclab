package repotest

import (
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
	// One macOS runner builds only the reset tool; one arm64 Linux runner
	// builds every other file.
	tool, rest, ok := strings.Cut(wf, "\n  build:\n")
	if !ok || !strings.Contains(tool, "\n  reset-tool:\n") {
		t.Fatal("no reset-tool job before the build job")
	}
	if runs := regexp.MustCompile(`runs-on: *(.*)`).FindAllStringSubmatch(tool, -1); len(runs) != 1 || runs[0][1] != "macos-15" {
		t.Errorf("the reset tool is built on %v, want one macOS runner", runs)
	}
	if steps := regexp.MustCompile(`(?m)^ +run: *(.*)$`).FindAllStringSubmatch(tool, -1); len(steps) != 1 || steps[0][1] != `release/reset-tool.sh "$RUNNER_TEMP/reset-tool"` {
		t.Errorf("the macOS runner runs %v: it builds only the reset tool", steps)
	}
	for _, s := range []string{"setup-go", "setup-python", "release/build.sh", "make ", "brew "} {
		if strings.Contains(tool, s) {
			t.Errorf("the reset-tool job has %q: the macOS runner builds only the reset tool", s)
		}
	}
	for _, m := range regexp.MustCompile(`runs-on: *(.*)`).FindAllStringSubmatch(rest, -1) {
		if m[1] != "ubuntu-24.04-arm" {
			t.Errorf("runs-on %s: one arm64 Linux runner builds every file but the reset tool", m[1])
		}
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
		`LAB_RELEASE="$GITHUB_REF_NAME" RESET_TOOL="$RUNNER_TEMP/reset-tool/macvdmtool" release/build.sh`,
		"    needs: reset-tool\n",
		"grep -qx lab-server/bin/macvdmtool",
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

// One key, pinned in four places that must agree.
func TestReleaseKeyPins(t *testing.T) {
	top := root(t)
	signers := strings.TrimSpace(read(t, top, "release/allowed_signers"))
	if strings.Count(signers, "\n") != 0 || !strings.HasPrefix(signers, `release@omarchy-m-testing.org namespaces="omarchy-m-test-release" ssh-ed25519 `) {
		t.Fatalf("release/allowed_signers is not one ed25519 release key: %q", signers)
	}
	for _, rel := range []string{"installer/install.sh", "installer/install-lab.sh", "release/macos-runner/install.sh", "docs/omarchy-m-test.md"} {
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
