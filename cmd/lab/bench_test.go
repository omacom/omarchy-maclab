package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench"
	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
)

func pinned(name, body, url string) bench.Item {
	s := sha256.Sum256([]byte(body))
	return bench.Item{Name: name, Version: "1", URL: url, SHA256: hex.EncodeToString(s[:]), Size: int64(len(body))}
}

type fakeStore struct{ got map[string]string }

func (f *fakeStore) upload(ctx context.Context, path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	sha := hex.EncodeToString(s[:])
	f.got[sha] = string(b)
	return sha, nil
}

func TestBenchMirrorDownloadsVerifiesAndStoresBySha(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("bytes of " + strings.TrimPrefix(r.URL.Path, "/")))
	}))
	defer srv.Close()
	items := []bench.Item{pinned("a", "bytes of a.tar.gz", srv.URL+"/a.tar.gz"), pinned("b", "bytes of b.pkg", srv.URL+"/b.pkg")}
	st := &fakeStore{got: map[string]string{}}
	var out bytes.Buffer
	if err := mirrorBench(context.Background(), items, "", srv.Client(), st.upload, &out); err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if _, ok := st.got[it.SHA256]; !ok {
			t.Errorf("%s not stored", it.Name)
		}
		if !strings.Contains(out.String(), it.Name+" 1 "+it.SHA256) {
			t.Errorf("output lacks %s: %s", it.Name, out.String())
		}
	}
}

func TestBenchMirrorRefusesBytesThatAreNotPinned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("changed upstream"))
	}))
	defer srv.Close()
	st := &fakeStore{got: map[string]string{}}
	err := mirrorBench(context.Background(), []bench.Item{pinned("a", "the pinned bytes", srv.URL+"/a")}, "", srv.Client(), st.upload, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err %v", err)
	}
	if len(st.got) != 0 {
		t.Errorf("stored %v", st.got)
	}
}

func TestBenchMirrorTakesEverySetsPinnedFilesOnce(t *testing.T) {
	all, err := sets.All()
	if err != nil {
		t.Fatal(err)
	}
	var said bytes.Buffer
	items := mirrorItems(all, bench.Legacy(), &said)
	names, shas := map[string]bool{}, map[string]bool{}
	for _, it := range items {
		if shas[it.SHA256] {
			t.Errorf("%s mirrored twice", it.SHA256)
		}
		names[it.Name], shas[it.SHA256] = true, true
	}
	for _, n := range []string{"bench-1/omarchy/chrome", "bench-1/omarchy/speedometer", "bench-1/omarchy/clip-h264-1080p", "runner/glmark2", "runner/coremark"} {
		if !names[n] {
			t.Errorf("no %s in %v", n, names)
		}
	}
	if names["bench-1/macos/speedometer"] || names["runner/clip"] {
		t.Error("a file both systems share, or the runner's copy of one, was mirrored again")
	}
	for _, n := range []string{"bench-1/macos/chrome: a placeholder", "bench-1/omarchy/coremark: a placeholder"} {
		if !strings.Contains(said.String(), n) {
			t.Errorf("did not say %q: %s", n, said.String())
		}
	}
}

func TestBenchMirrorTakesALocalCopyFirst(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.tar.gz"), []byte("local a"), 0o644)
	st := &fakeStore{got: map[string]string{}}
	it := pinned("a", "local a", "https://unreachable.invalid/a.tar.gz")
	if err := mirrorBench(context.Background(), []bench.Item{it}, dir, http.DefaultClient, st.upload, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if st.got[it.SHA256] != "local a" {
		t.Errorf("stored %v", st.got)
	}
}
