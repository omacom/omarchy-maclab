package main

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"testing"

	"rsc.io/qr"
)

const qrLink = "sgnl://linkdevice?uuid=dGVzdC11dWlk&pub_key=dGVzdC1wdWJsaWMta2V5"

func drawQR(t *testing.T, in string, args ...string) (string, string, int) {
	t.Helper()
	var out, errw bytes.Buffer
	code := qrCode(args, strings.NewReader(in), &out, &errw)
	return out.String(), errw.String(), code
}

// modules reads labd qr's drawing back into rows of modules, true for dark:
// each line of text is two rows, the top one in the half block's upper half.
func modules(t *testing.T, drawing string) [][]bool {
	t.Helper()
	plain := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(drawing, "")
	var rows [][]bool
	for _, line := range strings.Split(strings.TrimSuffix(plain, "\n"), "\n") {
		var top, bottom []bool
		for _, r := range line {
			switch r {
			case '█':
				top, bottom = append(top, false), append(bottom, false)
			case '▀':
				top, bottom = append(top, false), append(bottom, true)
			case '▄':
				top, bottom = append(top, true), append(bottom, false)
			case ' ':
				top, bottom = append(top, true), append(bottom, true)
			default:
				t.Fatalf("labd qr drew %q, which is no half block", r)
			}
		}
		rows = append(rows, top, bottom)
	}
	return rows
}

// labd qr draws the one line it reads as a QR code a phone scans, light on
// dark whatever the terminal's colours, and nothing else: the line itself is
// never written out.
func TestLabdQRDrawsTheLineItReads(t *testing.T) {
	out, errw, code := drawQR(t, qrLink+"\n")
	if code != 0 || errw != "" {
		t.Fatalf("labd qr: status %d, %q", code, errw)
	}
	if strings.Contains(out, "sgnl") || strings.Contains(out, "dGVzdC") {
		t.Fatalf("labd qr wrote the line out:\n%s", out)
	}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if !strings.HasPrefix(line, "\x1b[40;97m") || !strings.HasSuffix(line, "\x1b[0m") {
			t.Fatalf("a line does not set its own colours: %q", line)
		}
	}

	c, err := qr.Encode(qrLink, qrLevel)
	if err != nil {
		t.Fatal(err)
	}
	rows := modules(t, out)
	side := c.Size + 2*qrQuiet
	if len(rows) < side || len(rows) > side+1 {
		t.Fatalf("%d rows of modules, want %d", len(rows), side)
	}
	for y := range rows {
		if len(rows[y]) != side {
			t.Fatalf("row %d has %d modules, want %d", y, len(rows[y]), side)
		}
		for x, dark := range rows[y] {
			cx, cy := x-qrQuiet, y-qrQuiet
			want := cx >= 0 && cy >= 0 && cx < c.Size && cy < c.Size && c.Black(cx, cy)
			if dark != want {
				t.Fatalf("module (%d,%d) is dark %v, want %v", cx, cy, dark, want)
			}
		}
	}

	// Only the first line is drawn, with or without its newline.
	if again, _, _ := drawQR(t, qrLink); again != out {
		t.Error("the line without a newline drew another code")
	}
	if again, _, _ := drawQR(t, qrLink+"\nmore\n"); again != out {
		t.Error("a second line changed the code")
	}
}

// A real device link, a uuid and a 33-byte key in base64, URL-encoded or
// not, draws whole in an ordinary 80x24 terminal: at most 23 lines, so the
// line above it still fits, and at most 45 columns.
func TestLabdQRDrawsARealLinkInAnOrdinaryTerminal(t *testing.T) {
	key := strings.Repeat("A", 43) + "="
	for _, link := range []string{
		"sgnl://linkdevice?uuid=0b9e7f3a-1c2d-4e5f-8a9b-0c1d2e3f4a5b&pub_key=" + key,
		"sgnl://linkdevice?uuid=0b9e7f3a-1c2d-4e5f-8a9b-0c1d2e3f4a5b&pub_key=" + strings.Repeat("A%2B", 6) + strings.Repeat("A", 31) + "%3D",
	} {
		out, errw, code := drawQR(t, link+"\n")
		if code != 0 || errw != "" {
			t.Fatalf("a link of %d bytes: status %d, %q", len(link), code, errw)
		}
		lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		rows := modules(t, out)
		if len(lines) > 23 || len(rows[0]) > 45 {
			t.Errorf("a link of %d bytes draws %d lines of %d columns, want at most 23 of 45", len(link), len(lines), len(rows[0]))
		}
	}
}

// labd qr reads at most 4 KiB looking for its line, however long the input.
func TestLabdQRReadsAtMost4KiB(t *testing.T) {
	in := &endless{}
	var out, errw bytes.Buffer
	if code := qrCode(nil, in, &out, &errw); code == 0 || out.Len() != 0 {
		t.Errorf("an endless line: status %d, %d bytes drawn", code, out.Len())
	}
	if in.n > 4096 {
		t.Errorf("labd qr read %d bytes, want at most 4096", in.n)
	}
	if strings.Contains(errw.String(), "xxxx") {
		t.Errorf("the refusal holds the line: %q", errw.String())
	}
}

// endless is a line that never ends, counting what was read of it.
type endless struct{ n int }

func (e *endless) Read(p []byte) (int, error) {
	if e.n > 1<<20 {
		return 0, io.EOF
	}
	for i := range p {
		p[i] = 'x'
	}
	e.n += len(p)
	return len(p), nil
}

// labd qr refuses an argument (the line is never one), an empty line and a
// line too long for a code, and says so without the line.
func TestLabdQRRefusesWithoutTheLine(t *testing.T) {
	long := "sgnl://" + strings.Repeat("x", 4000)
	for _, c := range []struct {
		name, in string
		args     []string
	}{
		{"an argument", qrLink + "\n", []string{qrLink}},
		{"nothing", "", nil},
		{"an empty line", "\n", nil},
		{"a long line", long + "\n", nil},
	} {
		out, errw, code := drawQR(t, c.in, c.args...)
		if code == 0 || out != "" || errw == "" {
			t.Errorf("%s: status %d, out %q, err %q", c.name, code, out, errw)
		}
		if strings.Contains(errw, "sgnl") || strings.Contains(errw, "xxxx") {
			t.Errorf("%s: the refusal holds the line: %q", c.name, errw)
		}
	}
}
