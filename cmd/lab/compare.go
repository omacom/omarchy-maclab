package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/client"
)

const compareUse = "lab compare <mac> --before <set|current> --after <set> [--macos] [--tier default|extended] [--hands] [--no-wait] | done <job> | publish <job> | show <job>"

// compareWait is how long lab compare waits for its job: two sides of the
// Extended tier, a macOS run and a person's hour.
const compareWait = 8 * time.Hour

// compareCmd handles `lab compare ...` (Maintainers).
func compareCmd(ctx context.Context, c *client.Client, args []string) error {
	if len(args) < 1 {
		return errors.New(compareUse)
	}
	switch args[0] {
	case "done", "publish", "show":
		if len(args) != 2 {
			return fmt.Errorf("lab compare %s <job>", args[0])
		}
		var j *api.Job
		var err error
		switch args[0] {
		case "done":
			if err := c.CompareDone(ctx, args[1]); err != nil {
				return err
			}
			fmt.Printf("%s: done; the Compare goes on\n", args[1])
			return nil
		case "publish":
			j, err = c.ComparePublish(ctx, args[1])
		default:
			j, err = c.Job(ctx, args[1])
		}
		if err != nil {
			return err
		}
		return printCompare(os.Stdout, j)
	}
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	before := fs.String("before", api.CompareCurrent, "side A: a Package set, or current (the Mac's known-good system)")
	after := fs.String("after", "", "side B: a Package set, booted as a Candidate boot")
	macos := fs.Bool("macos", false, "run the tier in macOS too (the Mac's macOS runner must be enrolled)")
	tier := fs.String("tier", api.TierDefault, "default (about 10 minutes a side) or extended (about 30)")
	hands := fs.Bool("hands", false, "pause on B's candidate boot for a person until Done")
	noWait := fs.Bool("no-wait", false, "print the job and return")
	pos, err := parseSet(fs, args)
	if err != nil || len(pos) != 1 || *after == "" {
		return errors.New(compareUse)
	}
	j, err := c.Compare(ctx, api.CompareRequest{Mac: pos[0], Before: *before, After: *after, MacOS: *macos, Tier: *tier, Hands: *hands})
	if err != nil {
		return err
	}
	fmt.Printf("job %s: a Compare of %s (A) and %s (B) on %s\n", j.ID, *before, *after, pos[0])
	if *noWait {
		return nil
	}
	if j, err = c.Wait(ctx, j.ID, compareWait); err != nil {
		return err
	}
	return printCompare(os.Stdout, j)
}

func fmtVal(p *float64) string {
	if p == nil {
		return "-"
	}
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", *p), "0"), ".")
}

func fmtPct(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%+.1f%%", *p)
}

func printCompare(out io.Writer, j *api.Job) error {
	res := j.Result.Compare
	if j.Spec.Compare == nil || res == nil {
		return fmt.Errorf("job %s is no Compare", j.ID)
	}
	tier := res.Tier + " tier"
	if res.Bench != "" {
		tier += " of " + res.Bench
	}
	fmt.Fprintf(out, "Compare %s on %s: %s (A) and %s (B), %s: %s\n", j.ID, j.Spec.Device, res.Before, res.After, tier, orDash(j.Outcome, string(j.State)))
	if j.Summary != "" {
		fmt.Fprintln(out, j.Summary)
	}
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	mac := res.Side(api.SideMacOS) != nil
	head := "METRIC\tA\tB\tCHANGE\tVERDICT\tTOLERANCE"
	if mac {
		head += "\tMACOS\tMACOS VS A"
	}
	fmt.Fprintln(w, head+"\tNOTE")
	for _, r := range res.Rows {
		line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t±%g%%", r.Name, fmtVal(r.A), fmtVal(r.B), fmtPct(r.Delta), r.Verdict, r.Tolerance)
		notes := []string{}
		if r.Note != "" {
			notes = append(notes, r.Note)
		}
		if mac {
			line += fmt.Sprintf("\t%s\t%s %s", fmtVal(r.MacOS), fmtPct(r.MacOSDelta), orDash(r.MacOSVerdict, "-"))
			if r.MacOSNote != "" {
				notes = append(notes, "macOS: "+r.MacOSNote)
			}
		}
		fmt.Fprintln(w, line+"\t"+orDash(strings.Join(notes, "; "), "-"))
	}
	w.Flush()
	fmt.Fprintf(out, "omarchy-m-test: %d regressed, %d fixed\n", res.Regressed, res.Fixed)
	for _, c := range res.Checks {
		if c.Change != api.CheckPass {
			if c.Note != "" {
				fmt.Fprintf(out, "  %s: A %s, B %s, %s (%s)\n", c.ID, c.A, c.B, c.Change, c.Note)
			} else {
				fmt.Fprintf(out, "  %s: A %s, B %s, %s\n", c.ID, c.A, c.B, c.Change)
			}
		}
	}
	w = tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SIDE\tSET\tBOOT\tKERNEL\tGPU FIRMWARE\tSIO\tTHUNDERBOLT\tFAILED UNITS")
	for _, s := range res.Sides {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Side, s.Set, orDash(s.Boot, "-"), orDash(s.Kernel, "-"), orDash(s.GPUFirmware, "-"),
			orDash(s.SIO, "-"), orDash(s.Thunderbolt, "-"), orDash(strings.Join(s.FailedUnits, " "), "none"))
	}
	w.Flush()
	for _, s := range res.Sides {
		if s.Error != "" {
			fmt.Fprintf(out, "%s: %s\n", s.Side, s.Error)
		}
	}
	if h := res.Hands; h != nil && h.Done != nil {
		fmt.Fprintf(out, "a person: done by @%s\n", h.By)
	} else if h != nil && h.Note != "" {
		fmt.Fprintf(out, "a person: %s\n", h.Note)
	}
	if res.Published != nil {
		fmt.Fprintf(out, "published by @%s: /compares/%s\n", res.PublishedBy, j.ID)
	} else if j.State == api.JobDone {
		fmt.Fprintf(out, "private to Maintainers: lab compare publish %s\n", j.ID)
	}
	return nil
}
