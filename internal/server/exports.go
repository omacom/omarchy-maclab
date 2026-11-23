package server

// The exports: everything the public pages show, for download under CC0.
// Every published report as uploaded (JSON) or one row per report (CSV),
// every check result (CSV), and the matrix, the benchmark comparison and the
// Aurora feature-support table (JSON). The reports, the checks and the
// matrix take the filters the pages do; the matrix takes ?group= too. The
// paths and the member names are the old site's, but for the Performance
// data, spec section 22's.

import (
	"net/http"
	"time"

	"github.com/omacom/omarchy-maclab/internal/reports"
)

func (s *Server) exportRoutes() []route {
	return []route{
		{"GET /api/v1/reports", anyone, s.hReportsJSON},
		{"GET /api/v1/reports.json", anyone, s.hReportsJSON},
		{"GET /api/v1/reports.csv", anyone, s.hReportsCSV},
		{"GET /api/v1/checks", anyone, s.hChecksCSV},
		{"GET /api/v1/checks.csv", anyone, s.hChecksCSV},
		{"GET /api/v1/matrix", anyone, s.hMatrixJSON},
		{"GET /api/v1/matrix.json", anyone, s.hMatrixJSON},
		{"GET /api/v1/benchmarks", anyone, s.hBenchmarksJSON},
		{"GET /api/v1/benchmarks.json", anyone, s.hBenchmarksJSON},
		{"GET /api/v1/aurora", anyone, s.hAuroraJSON},
		{"GET /api/v1/aurora.json", anyone, s.hAuroraJSON},
		{"GET /api/v1/performance.json", anyone, s.hPerformanceJSON},
	}
}

func sendJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(body)
}

func sendCSV(w http.ResponseWriter, name string, body []byte) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write(body)
}

// reportURL gives a report's link at the Lab address.
func (s *Server) reportURL(r *http.Request) func(*reports.Run) string {
	base := s.publicBase(r)
	return func(run *reports.Run) string { return base + reportPath(run) }
}

func (s *Server) hReportsJSON(w http.ResponseWriter, r *http.Request) {
	if p := s.published(w); p != nil {
		sendJSON(w, p.ReportsJSON(p.Filter(r.URL.Query(), reports.ListKeys), s.reportURL(r), time.Now()))
	}
}

func (s *Server) hReportsCSV(w http.ResponseWriter, r *http.Request) {
	if p := s.published(w); p != nil {
		sendCSV(w, "omarchy-mac-lab-reports.csv", p.ReportsCSV(p.Filter(r.URL.Query(), reports.ListKeys), s.reportURL(r)))
	}
}

func (s *Server) hChecksCSV(w http.ResponseWriter, r *http.Request) {
	if p := s.published(w); p != nil {
		sendCSV(w, "omarchy-mac-lab-checks.csv", p.ChecksCSV(p.Filter(r.URL.Query(), nil)))
	}
}

func (s *Server) hMatrixJSON(w http.ResponseWriter, r *http.Request) {
	if p := s.published(w); p != nil {
		sendJSON(w, p.MatrixJSON(r.URL.Query().Get("group"), p.Filter(r.URL.Query(), nil), time.Now()))
	}
}

func (s *Server) hBenchmarksJSON(w http.ResponseWriter, r *http.Request) {
	if p := s.published(w); p != nil {
		sendJSON(w, p.BenchmarksJSON(s.macOSReference, time.Now()))
	}
}

// hPerformanceJSON is the Performance data: ?set= (bench-N or N; the
// current set), ?release= (a kernel version; the newest qualified Release,
// else the newest with no verdict yet) and ?volunteers=false, which hides
// Volunteers' Macs and a Volunteer-only headline with them.
func (s *Server) hPerformanceJSON(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	set, err := reports.PerfSet(q.Get("set"))
	if err != nil {
		httpErr(w, http.StatusNotFound, "no such Benchmark set")
		return
	}
	f, err := s.reports.PerformanceFigures(s.catalogue, set, q.Get("volunteers") != "false")
	if err != nil {
		s.pageFailed(w, err)
		return
	}
	release := f.DefaultRelease()
	if name := q.Get("release"); name != "" {
		if release = f.Release(name); release == nil {
			httpErr(w, http.StatusNotFound, "no such Release")
			return
		}
	}
	sendJSON(w, f.JSON(release, time.Now()))
}

func (s *Server) hAuroraJSON(w http.ResponseWriter, r *http.Request) {
	if p := s.published(w); p != nil {
		sendJSON(w, p.AuroraJSON(s.reportURL(r), time.Now()))
	}
}
