package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"rsc.io/qr"
)

const qrUsage = `usage: labd qr < LINE

Draws the one line it reads on its input as a QR code, in this terminal, and
writes nothing else: no file, no log, not the line itself. It takes no
argument, so no command line holds the line. scripts/mini/signal link shows
signal-cli's device link with it, for the owner's phone to scan.
`

// Level L and a quiet zone of two modules: a device link (about 112 bytes)
// is then version 6, 23 lines of text, which an 80x24 terminal shows whole
// with a line above it. At M with four modules it was 27 lines. A screen
// loses no modules to dirt, and the code is drawn on its own light zone.
const (
	qrLevel = qr.L
	qrQuiet = 2

	// More than any line a QR code holds: labd qr reads no further.
	qrMaxInput = 4096
)

// qrCode is `labd qr`. It returns the command's exit status.
func qrCode(args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) != 0 {
		fmt.Fprint(errw, qrUsage)
		return 2
	}
	line, err := bufio.NewReader(io.LimitReader(in, qrMaxInput)).ReadString('\n')
	line = strings.TrimSuffix(line, "\n")
	if line == "" {
		if err != nil && err != io.EOF {
			fmt.Fprintf(errw, "labd qr: reading the line: %v\n", err)
			return 1
		}
		fmt.Fprintln(errw, "labd qr: no line on the input: nothing to draw")
		return 2
	}
	c, err := qr.Encode(line, qrLevel)
	if err != nil {
		fmt.Fprintf(errw, "labd qr: a line of %d bytes does not fit in a QR code\n", len(line))
		return 1
	}
	// Light modules are drawn as bright white blocks on black, set on every
	// line, so the code reads the same in a light or a dark terminal. Each
	// line of text holds two rows of modules.
	light := func(x, y int) bool { return x < 0 || y < 0 || x >= c.Size || y >= c.Size || !c.Black(x, y) }
	var b strings.Builder
	for y := -qrQuiet; y < c.Size+qrQuiet; y += 2 {
		b.WriteString("\x1b[40;97m")
		for x := -qrQuiet; x < c.Size+qrQuiet; x++ {
			switch top, bottom := light(x, y), light(x, y+1); {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\x1b[0m\n")
	}
	if _, err := io.WriteString(out, b.String()); err != nil {
		return 1
	}
	return 0
}
