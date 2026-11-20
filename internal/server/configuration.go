package server

import (
	"io"
	"os"
	"path/filepath"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/detect"
)

// A Mac's Configuration is its board with its CPU and GPU core counts, and
// labd works it out: the board from the Mac's check-in; the counts from the
// repository's table for a board sold with one Configuration, otherwise
// from the kernel log of the Mac's latest job on its Known-good kernel.
// lab-agent reports no core count and is not asked to.

// kernelLogRead is how much of a kernel log labd reads for the two lines:
// both are printed in the first seconds of a boot.
const kernelLogRead = 4 << 20

// configuration is v's Configuration as labd works it out now.
func (s *Server) configuration(v *dev) api.Configuration {
	v.mu.Lock()
	c, look := v.configurationLocked()
	v.mu.Unlock()
	if !look {
		return c
	}
	cpu, gpu := s.coreCountsFromJobs(v)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.coresLooked = true
	// A count the latest log lacks keeps the one an earlier log gave.
	kept, read := v.d.Configuration, api.Configuration{}
	if kept != nil {
		read = *kept
	}
	if cpu > 0 {
		read.CPU = cpu
	}
	if gpu > 0 {
		read.GPU = gpu
	}
	if read.CPU+read.GPU > 0 && (kept == nil || *kept != read) {
		v.d.Configuration = &read
		v.saveLocked()
	}
	c, _ = v.configurationLocked()
	return c
}

// configurationLocked is the Configuration from what labd holds, and
// whether the Mac's jobs should be looked through for its core counts
// first: the table has no row for the board, and nothing has looked since
// the Mac's last job. Caller holds v.mu.
func (v *dev) configurationLocked() (c api.Configuration, look bool) {
	c.Board = detect.Board(v.d.Facts.Compatible)
	if cpu, gpu, ok := detect.OneConfiguration(c.Board); ok {
		c.CPU, c.GPU = cpu, gpu
		return c, false
	}
	if kept := v.d.Configuration; kept != nil {
		c.CPU, c.GPU = kept.CPU, kept.GPU
	}
	return c, !v.coresLooked && v.d.KnownGood != ""
}

// kernelLogsRead is how many of a Mac's kernel logs labd reads, newest
// first, in each pass for a count the newer ones lack.
const kernelLogsRead = 10

// coreCountsFromJobs reads each core count from the kernel log of v's
// latest job that booted its Known-good kernel, in this enrollment, and
// carries it: a count that log lacks comes from an older one. A GPU count
// none of those logs has comes from a log of any kernel the Mac booted:
// the GPU's active cores are the chip's, where a test kernel's command
// line can bring up fewer CPUs.
func (s *Server) coreCountsFromJobs(v *dev) (cpu, gpu int) {
	d := v.snapshot()
	jobs, err := s.store.jobs(d.Name, 200, false)
	if err != nil {
		s.log.Printf("%s: its Configuration: %v", d.Name, err)
		return 0, 0
	}
	read := func(knownGood bool, done func() bool, take func(c, g int)) {
		n := 0
		for _, j := range jobs {
			if done() || n == kernelLogsRead {
				return
			}
			if j.DeviceID != d.ID || !j.Result.Booted || (j.Result.BootKernel == d.KnownGood) != knownGood {
				continue
			}
			f, err := os.Open(filepath.Join(s.jobDir(j.ID), "boot0", "kernel.txt"))
			if err != nil {
				continue
			}
			b, err := io.ReadAll(io.LimitReader(f, kernelLogRead))
			f.Close()
			if err != nil {
				continue
			}
			n++
			take(detect.CoreCounts(string(b)))
		}
	}
	read(true, func() bool { return cpu > 0 && gpu > 0 }, func(c, g int) {
		if cpu == 0 {
			cpu = c
		}
		if gpu == 0 {
			gpu = g
		}
	})
	read(false, func() bool { return gpu > 0 }, func(_, g int) { gpu = g })
	return cpu, gpu
}

// forgetCoreCounts has labd look through v's jobs again the next time its
// Configuration is asked for: a job ended, or its Known-good kernel changed.
func (s *Server) forgetCoreCounts(v *dev) {
	v.mu.Lock()
	v.coresLooked = false
	v.mu.Unlock()
}
