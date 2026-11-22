package server

// "Set up macOS" on a card of My Macs: the Mac's Volunteer gets the macOS
// runner's one-time line for their own Mac, the same line `lab macos
// enroll <mac>` gives a Maintainer, shown once on the card. The lab keeps
// only the hash of its token.

import (
	"net/http"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/web"
)

func init() {
	addPage("POST /my-macs/{name}/macos", web.SignedIn, (*Server).pSetUpMacOS)
}

// macOSLine is the one-time line a card shows once.
type macOSLine struct{ Line, Expires string }

// macOSWords is a card's macOS fact: whether the runner is enrolled, and
// whether a macOS run needs a person at the Mac.
func macOSWords(side api.MacOSSide) string {
	switch {
	case !side.Enrolled:
		return "not set up"
	case side.Attendance == api.MacOSUnattended:
		return "Unattended"
	}
	return "Attended"
}

func (s *Server) pSetUpMacOS(w http.ResponseWriter, r *http.Request) {
	v := s.myMac(w, r)
	if v == nil {
		return
	}
	t, err := s.macOSEnrollLine(r, v)
	if err != nil {
		s.controlDone(w, r, v, err)
		return
	}
	s.log.Printf("the Volunteer of %s made its macOS runner's line", t.Mac)
	s.renderMyMacsWith(w, r, nil, map[string]*macOSLine{t.Mac: {Line: t.Line, Expires: utcMinute(t.Expires)}})
}
