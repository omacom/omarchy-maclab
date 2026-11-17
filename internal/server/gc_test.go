package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// What the artifact store's budget clears, and what it never does. The
// store is 20 GB when labd is given no other size.
func TestWhatTheArtifactBudgetClears(t *testing.T) {
	if artifactBudget != 20<<30 {
		t.Fatalf("the artifact store is %d bytes, want 20 GB", int64(artifactBudget))
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Room for two of the 1000-byte artifacts below.
	b := &builderLab{t: t, ctx: ctx, dir: t.TempDir(), budget: 2500}
	b.start()
	put := func(name string, age time.Duration) string {
		t.Helper()
		sha, _, err := b.s.storeArtifact(strings.NewReader(name + strings.Repeat(".", 1000-len(name))))
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(b.s.artifactPath(sha), at, at); err != nil {
			t.Fatal(err)
		}
		return sha
	}
	const day = 24 * time.Hour
	build := func(id string, state api.BuildState, kind string, change func(*api.Build)) {
		t.Helper()
		bd := &api.Build{ID: id, Key: "key-" + id, Kind: kind, State: state, Created: time.Now().Add(-3 * day),
			Source: api.Source{Repo: "https://github.com/labtest/linux", SHA: strings.Repeat("a", 40)}}
		change(bd)
		if err := b.s.store.saveBuild(bd); err != nil {
			t.Fatal(err)
		}
	}

	// labd's start-up GC pass runs on its own goroutine. Landing between a
	// backdated put and the build that refers to it, it would delete what
	// this pass must keep. No pass runs while the fixture is laid out.
	gcMu.Lock()
	held := true
	defer func() {
		if held {
			gcMu.Unlock()
		}
	}()
	stray := put("nothing refers to it, two days old", 2*day)
	fresh := put("nothing refers to it, ten minutes old", 10*time.Minute)
	oldKernel, newKernel := put("a finished build's kernel, three days old", 3*day), put("a finished build's kernel, two hours old", 2*time.Hour)
	doneConfig := put("a finished build's config, three days old", 3*day)
	queuedConfig := put("a queued build's config, three days old", 3*day)
	recipe := put("a finished package build's recipe, three days old", 3*day)
	macConfig := put("a Mac's kernel config, three days old", 3*day)
	build("b-done-old", api.BuildDone, "", func(bd *api.Build) { bd.ConfigSHA, bd.Artifact = doneConfig, oldKernel })
	build("b-done-new", api.BuildDone, "", func(bd *api.Build) { bd.ConfigSHA, bd.Artifact = doneConfig, newKernel })
	build("b-queued", api.BuildQueued, "", func(bd *api.Build) { bd.ConfigSHA = queuedConfig })
	build("b-package", api.BuildDone, api.BuildPackage, func(bd *api.Build) { bd.Recipe = recipe })
	b.s.store.setConfig("m1-pro", "7.1.12", macConfig)

	// What is not in the store: an unfinished upload, a job's collected
	// files, a build's log.
	artifacts := filepath.Join(b.dir, "artifacts")
	abandoned, going := filepath.Join(artifacts, ".upload-old"), filepath.Join(artifacts, ".upload-new")
	collected := filepath.Join(b.s.jobDir("j-old"), "boot0", "journal.txt")
	buildLog := filepath.Join(b.s.buildDir("b-done-old"), "build.log")
	old := time.Now().Add(-400 * day)
	for _, f := range []string{filepath.Join(abandoned, "0"), filepath.Join(going, "0"), collected, buildLog} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(strings.Repeat("x", 5000)), 0o644); err != nil {
			t.Fatal(err)
		}
		if f != filepath.Join(going, "0") {
			os.Chtimes(f, old, old)
			os.Chtimes(filepath.Dir(f), old, old)
		}
	}

	held = false
	gcMu.Unlock()
	b.s.gcArtifacts()

	has := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}
	for _, c := range []struct {
		what string
		path string
		kept bool
	}{
		{"an artifact nothing refers to, after a day", b.s.artifactPath(stray), false},
		{"an artifact nothing refers to, inside its first hour, over budget", b.s.artifactPath(fresh), true},
		{"the oldest finished build's kernel, over budget", b.s.artifactPath(oldKernel), false},
		{"a finished build's config, over budget", b.s.artifactPath(doneConfig), false},
		{"a finished build's kernel two hours old, over budget", b.s.artifactPath(newKernel), false},
		{"a queued build's config", b.s.artifactPath(queuedConfig), true},
		{"a package build's recipe", b.s.artifactPath(recipe), true},
		{"a Mac's kernel config", b.s.artifactPath(macConfig), true},
		{"an upload nobody finished, after a day", abandoned, false},
		{"an upload still arriving", going, true},
		{"a job's collected file, over a year old", collected, true},
		{"a build's log, over a year old", buildLog, true},
	} {
		if has(c.path) != c.kept {
			t.Errorf("%s: kept %v, want %v", c.what, !c.kept, c.kept)
		}
	}
}
