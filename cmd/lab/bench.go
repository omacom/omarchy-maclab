package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench"
	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/client"
)

const benchUsage = "lab bench mirror [--from <dir>]"

// benchCmd is `lab bench mirror`: every Benchmark set's pinned files, and
// the runner's legacy items, downloaded here, checked against their sha256
// and put in labd's artifact store, from where the Macs fetch them. A Mac
// never reaches the internet for one.
func benchCmd(ctx context.Context, c *client.Client, args []string) error {
	if len(args) == 0 || args[0] != "mirror" {
		return fmt.Errorf("usage: %s", benchUsage)
	}
	fs := flag.NewFlagSet("bench mirror", flag.ContinueOnError)
	from := fs.String("from", "", "a folder holding some of the files already, under their download names")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	all, err := sets.All()
	if err != nil {
		return err
	}
	return mirrorBench(ctx, mirrorItems(all, bench.Legacy(), os.Stdout), *from, http.DefaultClient, c.Upload, os.Stdout)
}

// mirrorItems is every set's pinned files and the runner's legacy items,
// each sha256 once, named set/system/file or runner/name; a placeholder is
// said and left.
func mirrorItems(all []*sets.Set, legacy []bench.Item, w io.Writer) []bench.Item {
	var out []bench.Item
	seen := map[string]bool{}
	add := func(it bench.Item) {
		if !seen[it.SHA256] {
			seen[it.SHA256] = true
			out = append(out, it)
		}
	}
	for _, s := range all {
		for _, sys := range sets.Systems {
			for _, f := range s.Files[sys] {
				name := s.Name + "/" + sys + "/" + f.ID
				if !f.Pinned() {
					fmt.Fprintf(w, "%s: a placeholder, not mirrored\n", name)
					continue
				}
				add(bench.Item{Name: name, Version: f.Version, Kind: f.Kind, URL: f.URL, SHA256: f.SHA256, Size: f.Size})
			}
		}
	}
	for _, it := range legacy {
		it.Name = "runner/" + it.Name
		add(it)
	}
	return out
}

func mirrorBench(ctx context.Context, items []bench.Item, from string, hc *http.Client,
	upload func(context.Context, string) (string, error), w io.Writer) error {
	tmp, err := os.MkdirTemp("", "lab-bench-mirror-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, it := range items {
		name := path.Base(it.URL)
		p := filepath.Join(from, name)
		if from == "" || !isFile(p) {
			p = filepath.Join(tmp, it.SHA256)
			if err := download(ctx, hc, it.URL, p); err != nil {
				return fmt.Errorf("%s: %w", it.Name, err)
			}
		}
		if got, err := sumFile(p); err != nil {
			return fmt.Errorf("%s: %w", it.Name, err)
		} else if got != it.SHA256 {
			return fmt.Errorf("%s %s: sha256 %s, but it is pinned at %s", it.Name, it.Version, got, it.SHA256)
		}
		sha, err := upload(ctx, p)
		if err != nil {
			return fmt.Errorf("%s: %w", it.Name, err)
		}
		if sha != it.SHA256 {
			return fmt.Errorf("%s: labd stored %s, want %s", it.Name, sha, it.SHA256)
		}
		fmt.Fprintf(w, "%s %s %s\n", it.Name, it.Version, sha)
		if p != filepath.Join(from, name) {
			os.Remove(p)
		}
	}
	return nil
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func download(ctx context.Context, hc *http.Client, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func sumFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
