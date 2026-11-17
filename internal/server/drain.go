package server

// A Deploy and a Maintenance run restart labd, and no job outlives labd. So
// each first drains: labd sends no further job to a Mac, and the script
// waits until those sent have ended. A job counts from the moment it is
// sent: one waiting for its Mac to be ready or for an Availability window
// is not on any Mac, and a restart loses nothing of it. A job that waits
// stays queued, in the store, and is sent after the restart; should the
// script give up instead, it lifts the drain and the job is sent then. The
// drain is not stored: a labd that starts is never draining.
//
// A job's build comes before the job is sent, so while jobs are drained
// the Builder gets no further build, and the drain is over when the builds
// that run have ended as well.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

type drain struct {
	mu sync.Mutex
	// on: jobs are drained.
	on bool
	// jobs counts the jobs sent to a Mac that have not ended.
	jobs int
}

// sendJob counts a job as sent to its Mac, unless jobs are drained: then it
// reports false, and the job waits. The caller holds the Mac's mu, so the
// job is counted in the same step that gives it the Mac.
func (s *Server) sendJob() bool {
	s.drain.mu.Lock()
	defer s.drain.mu.Unlock()
	if s.drain.on {
		return false
	}
	s.drain.jobs++
	return true
}

// endJob counts a sent job as ended. The caller holds the Mac's mu, so the
// job stops counting in the same step that frees the Mac.
func (s *Server) endJob() {
	s.drain.mu.Lock()
	s.drain.jobs--
	s.drain.mu.Unlock()
}

// drained is whether jobs are drained now.
func (s *Server) drained() bool {
	s.drain.mu.Lock()
	defer s.drain.mu.Unlock()
	return s.drain.on
}

// buildsHeld reports whether the Builder gets no build now: jobs are
// drained.
func (s *Server) buildsHeld() bool { return s.drained() }

// drainJobs starts a drain, or goes on with the one there is, and says how
// many jobs and builds still run.
func (s *Server) drainJobs() (jobs, builds int, err error) {
	s.builds.state.Lock()
	defer s.builds.state.Unlock()
	running, err := s.store.builds(`state='` + string(api.BuildRunning) + `'`)
	if err != nil {
		return 0, 0, err
	}
	s.drain.mu.Lock()
	defer s.drain.mu.Unlock()
	if !s.drain.on {
		s.drain.on = true
		s.log.Printf("jobs are drained: %d jobs and %d builds still run", s.drain.jobs, len(running))
	}
	return s.drain.jobs, len(running), nil
}

func (s *Server) liftDrain() {
	s.drain.mu.Lock()
	lifted := s.drain.on
	if lifted {
		s.drain.on = false
		s.log.Printf("the drain of jobs is lifted")
	}
	s.drain.mu.Unlock()
	if !lifted {
		return
	}
	// The jobs that waited for it are sent now.
	s.mu.Lock()
	devs := make([]*dev, 0, len(s.devs))
	for _, v := range s.devs {
		devs = append(devs, v)
	}
	s.mu.Unlock()
	for _, v := range devs {
		v.mu.Lock()
		v.bump()
		v.mu.Unlock()
	}
}

// partsNow is what a Deploy asks once labd is back: whether the Builder
// asks this labd for work and whether the Out-of-band controller answers
// it. "none" is a part this lab does not have yet.
func (s *Server) partsNow(ctx context.Context) (builder, controller string) {
	builder, controller = partNone, partNone
	if named, err := s.store.builders(); err == nil && len(named) > 0 {
		builder = partOK
		for _, b := range named {
			if alive, _ := s.builderAlive(b.Name); !alive {
				builder = "silent"
			}
		}
	}
	if c := s.cfg.OOB; c != nil {
		if _, err := os.Lstat(c.TokenFile); err == nil {
			actx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			controller = partOK
			// What a port shows, which runs no tool.
			if _, err := c.Port(actx, 0); err != nil {
				controller = partBad
			}
		}
	}
	return builder, controller
}

func (s *Server) opsDrain(w http.ResponseWriter, r *http.Request) {
	jobs, builds, err := s.drainJobs()
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "the builds could not be read")
		return
	}
	fmt.Fprintf(w, "%d %d\n", jobs, builds)
}

func (s *Server) opsLift(w http.ResponseWriter, r *http.Request) {
	s.liftDrain()
	fmt.Fprint(w, "lifted\n")
}

func (s *Server) opsParts(w http.ResponseWriter, r *http.Request) {
	builder, controller := s.partsNow(r.Context())
	fmt.Fprintf(w, "builder %s\ncontroller %s\n", builder, controller)
}
