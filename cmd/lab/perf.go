package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/client"
)

const perfUse = "lab perf rerun <mac> <release> [--no-wait]"

// perfWait is how long lab perf rerun waits for its job: a Candidate boot
// and the Full list.
const perfWait = 3 * time.Hour

// perfCmd is `lab perf rerun <mac> <release>`.
func perfCmd(ctx context.Context, c *client.Client, args []string) error {
	if len(args) < 1 || args[0] != "rerun" {
		return errors.New(perfUse)
	}
	fs := flag.NewFlagSet("perf rerun", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	noWait := fs.Bool("no-wait", false, "print the job and return")
	pos, err := parseSet(fs, args[1:])
	if err != nil || len(pos) != 2 {
		return errors.New(perfUse)
	}
	j, err := c.PerfRerun(ctx, api.PerfRerunRequest{Mac: pos[0], Release: pos[1]})
	if err != nil {
		return err
	}
	fmt.Printf("job %s: a Performance rerun of Release %s on %s\n", j.ID, pos[1], pos[0])
	if *noWait {
		return nil
	}
	if j, err = c.Wait(ctx, j.ID, perfWait); err != nil {
		return err
	}
	fmt.Printf("%s: %s\n", j.Outcome, j.Summary)
	return nil
}
