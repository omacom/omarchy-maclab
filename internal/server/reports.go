package server

// Reports from omarchy-m-test: the upload, and the two links every upload is
// answered with, which work as the old site's did with only the host
// changed. /reports/<public_id> is the report's page;
// /reports/<public_id>/deletion?token=<secret> opens a confirmation, and the
// report is deleted only when that is sent.

import (
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"time"

	"github.com/omacom/omarchy-maclab/catalogue"
	"github.com/omacom/omarchy-maclab/internal/reports"
	"github.com/omacom/omarchy-maclab/internal/web"
)

// oldSite is the live site until the lab opens.
const oldSite = "omarchy-m-testing.org"

func refuseJSON(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// hUploadReport is POST /api/v1/reports. The address its limit counts on is
// the caller's: CF-Connecting-IP on the tunnel's socket and nowhere else.
func (s *Server) hUploadReport(w http.ResponseWriter, r *http.Request) {
	switch {
	case s.Preview():
		refuseJSON(w, http.StatusForbidden, "Omarchy Mac Lab is not open yet and takes no reports. Until it opens, "+oldSite+" is the live site: upload there.")
	case s.uploads == nil:
		refuseJSON(w, http.StatusServiceUnavailable, "This lab takes no reports yet: it was given no machine-id key. The report is saved on your Mac.")
	default:
		if err := s.uploads.Handle(w, r, callerAddr(r), s.publicBase(r)); err != nil {
			s.log.Printf("report upload: %v", err)
		}
	}
}

// hReportPage is a published report's page. A Hidden report is not found,
// but by a Maintainer. It reads the report and what its page says of it,
// not every report.
func (s *Server) hReportPage(w http.ResponseWriter, r *http.Request) {
	around := s.reports.Around
	if s.maintains(r) {
		around = s.reports.AroundAny
	}
	p, run, err := around(s.catalogue, r.PathValue("id"))
	if err != nil {
		s.pageFailed(w, err)
		return
	}
	if run == nil {
		s.notFound(w, r)
		return
	}
	page := s.reportPage(r, p, run)
	s.render(w, r, "reports/show", web.Page{Title: page.Title, Nav: "reports", Public: true, Data: page})
}

func (s *Server) pageFailed(w http.ResponseWriter, err error) {
	s.log.Printf("report page: %v", err)
	httpErr(w, http.StatusInternalServerError, "the lab could not show this page")
}

// deletionAsked finds the report a deletion link names: the report with that
// public id whose secret is token. Anything else is not found, whichever of
// the two is wrong.
func (s *Server) deletionAsked(w http.ResponseWriter, r *http.Request, token string) *reports.Report {
	// The answer to a deletion link is never kept and names no page it came from.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	report, err := s.reports.Report(r.PathValue("id"))
	if errors.Is(err, reports.ErrNoReport) || (err == nil && !report.DeletionTokenMatches(token)) {
		http.NotFound(w, r)
		return nil
	}
	if err != nil {
		s.pageFailed(w, err)
		return nil
	}
	return report
}

func (s *Server) hReportDeletion(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	report := s.deletionAsked(w, r, token)
	if report == nil {
		return
	}
	model, _ := member(report.Document(), "machine", "model").(string)
	s.render(w, r, "reports/deletion", web.Page{Title: "Delete report", Nav: "reports", Public: true,
		Data: struct{ Model, Uploaded, Action, Token, CSRF string }{
			model, uploadedAt(report), s.basePath() + "/reports/" + report.PublicID, token, CSRFToken(r)}})
}

// hReportDelete is the confirmation, sent: the form of the deletion page,
// which carries the link's secret in its body.
func (s *Server) hReportDelete(w http.ResponseWriter, r *http.Request) {
	// Anyone may send this form: it must come whole soon, as a signed-in
	// browser's must.
	readBodyBy(w, r.Context(), time.Now().Add(formWait))
	r.Body = http.MaxBytesReader(w, r.Body, formMax)
	if err := r.ParseForm(); err != nil || r.PostForm.Get("_method") != "delete" {
		http.NotFound(w, r)
		return
	}
	token := r.PostForm.Get("token")
	report := s.deletionAsked(w, r, token)
	if report == nil {
		return
	}
	switch err := s.reports.DeleteByLink(report.PublicID, token); {
	case errors.Is(err, reports.ErrNoReport):
		http.NotFound(w, r)
	case err != nil:
		s.pageFailed(w, err)
	default:
		// The link is the credential: whoever sent it is not asked who they are.
		s.reportDeleted(report, "whoever holds its deletion link", 0)
		s.render(w, r, "reports/deleted", web.Page{Title: "Report deleted", Nav: "reports", Public: true})
	}
}

func uploadedAt(report *reports.Report) string {
	return report.CreatedAt.UTC().Format("2006-01-02 15:04 UTC")
}

// member follows names into a report's document; nil where it ends early.
func member(value any, path ...string) any {
	for _, key := range path {
		object, _ := value.(*reports.Object)
		if object == nil {
			return nil
		}
		value, _ = object.Get(key)
	}
	return value
}

// words is a report's value as its page shows it.
func words(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case *big.Int:
		return value.String()
	}
	return ""
}

// loadCatalogue is the feature catalogue the pages word results with.
func loadCatalogue() (*reports.Catalogue, error) {
	return reports.LoadCatalogue(catalogue.JSON)
}
