package macosrunner

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// unpack unpacks an archive of a set into dir: a zip, a tar (gzip or none)
// in Go, anything else (xz, zstd) with macOS's tar. A path or a link out of
// dir is refused.
func unpack(ctx context.Context, file, dir string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	head, _ := bufio.NewReader(f).Peek(512)
	f.Seek(0, io.SeekStart)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	switch {
	case len(head) >= 4 && string(head[:4]) == "PK\x03\x04":
		return unzip(file, dir)
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		return untar(gz, dir)
	case len(head) >= 262 && string(head[257:262]) == "ustar":
		return untar(f, dir)
	}
	out, err := exec.CommandContext(ctx, "/usr/bin/tar", "-xf", file, "-C", dir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tar: %v: %s", err, lastLine(string(out)))
	}
	return nil
}

// inside is name as a path under dir, or an error when it leaves dir.
func inside(dir, name string) (string, error) {
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%s is outside the archive", name)
	}
	return filepath.Join(dir, clean), nil
}

// linkInside is whether a link at p to target stays under dir.
func linkInside(dir, p, target string) bool {
	if filepath.IsAbs(target) {
		return false
	}
	rel, err := filepath.Rel(dir, filepath.Join(filepath.Dir(p), target))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func untar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		p, err := inside(dir, h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(p, 0o755)
		case tar.TypeReg:
			err = writeFile(p, tr, fs.FileMode(h.Mode))
		case tar.TypeSymlink:
			if !linkInside(dir, p, h.Linkname) {
				return fmt.Errorf("%s links outside the archive", h.Name)
			}
			os.MkdirAll(filepath.Dir(p), 0o755)
			err = os.Symlink(h.Linkname, p)
		}
		if err != nil {
			return err
		}
	}
}

func unzip(file, dir string) error {
	z, err := zip.OpenReader(file)
	if err != nil {
		return err
	}
	defer z.Close()
	for _, f := range z.File {
		p, err := inside(dir, f.Name)
		if err != nil {
			return err
		}
		mode := f.Mode()
		if mode.IsDir() {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		if mode&fs.ModeSymlink != 0 {
			b, err := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if err != nil {
				return err
			}
			if !linkInside(dir, p, string(b)) {
				return fmt.Errorf("%s links outside the archive", f.Name)
			}
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.Symlink(string(b), p); err != nil {
				return err
			}
			continue
		}
		err = writeFile(p, rc, mode)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// writeFile writes a file readable by everyone, and runnable when the
// archive says so: the logged-in user runs the browser.
func writeFile(p string, r io.Reader, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm()&0o755|0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, r)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
