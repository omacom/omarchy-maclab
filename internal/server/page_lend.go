package server

// "Lend a Mac", for anyone signed in, and what People adds for it. The page
// opens with the Test machine rule, then the table of what the lab may and
// may not do, then the terms. Under them is where the person stands: a
// Request to lend to send, one waiting, one declined, or, with leave to
// lend, the form that names a Mac and makes its Join command.

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/omacom/omarchy-maclab/internal/web"
)

func init() {
	addPage("GET /lend", web.SignedIn, (*Server).pLend)
	addPage("POST /lend/request", web.SignedIn, (*Server).pRequestToLend)
	addPage("POST /lend/request/withdraw", web.SignedIn, (*Server).pWithdrawRequest)
	addPage("POST /lend/join-command", web.SignedIn, (*Server).pMakeJoinCommand)
	addPage("POST /lend/join-command/cancel", web.SignedIn, (*Server).pCancelJoinCommand)
	addPage("POST /people/{id}/request/approve", web.Maintainers, onPerson(func(s *Server, r *http.Request, k knownPerson) error {
		return s.answerRequest(r, k, true, "")
	}))
	addPage("POST /people/{id}/request/decline", web.Maintainers, onPerson(func(s *Server, r *http.Request, k knownPerson) error {
		return s.answerRequest(r, k, false, r.PostFormValue("reason"))
	}))
	addPage("POST /people/{id}/cap", web.Maintainers, onPerson(func(s *Server, r *http.Request, k knownPerson) error {
		macs, err := strconv.Atoi(r.PostFormValue("macs"))
		if err != nil {
			return refuse(http.StatusUnprocessableEntity, "A cap is a number of Macs.")
		}
		return s.raiseCap(r, k, macs)
	}))
}

// lendAsked is a Request to lend as its own person sees it.
type lendAsked struct {
	When, Model, Reason string
}

type lendCommand struct {
	Name, Line, Expires string
}

type lendPage struct {
	Terms   web.Text
	Address string // the Lab address, as the page names it
	Refused string
	// Leave is whether the person has leave to lend. Without it, Waiting is
	// their request no Maintainer has answered and Declined the one a
	// Maintainer declined; with neither the page asks for one.
	Leave    bool
	Waiting  *lendAsked
	Declined *lendAsked
	Models   []lendModel
	Model    string // the model chosen, in the form
	Name     string // the name in the form: typed, or the lab's suggestion
	// Lends counts toward Cap, which is 0 for a Maintainer; AtCap leaves no
	// room for another Mac.
	Lends, Cap int
	AtCap      bool
	// Unused is the person's unused Join command, without its line, and
	// Made the one just made, the one time its line is shown.
	Unused *lendCommand
	Made   *lendCommand
}

func (s *Server) pLend(w http.ResponseWriter, r *http.Request) {
	s.showLend(w, r, http.StatusOK, nil)
}

// showLend renders "Lend a Mac" for the person signed in on r; change sets
// what an action adds to it.
func (s *Server) showLend(w http.ResponseWriter, r *http.Request, status int, change func(*lendPage)) {
	p := PersonOf(r)
	s.expireJoinCommands()
	page := &lendPage{Terms: web.Terms(), Address: s.publicBase(r), Leave: p.MayLend(), Models: lendModels(), Model: r.FormValue("model"), Name: r.FormValue("name")}
	if page.Leave {
		l, err := s.lendsOf(*p)
		if err != nil {
			s.pageFailed(w, err)
			return
		}
		page.Lends, page.Cap = l.Count(), l.Cap
		page.AtCap = l.Cap != 0 && len(l.Macs) >= l.Cap
		if l.Unused != nil {
			page.Unused = &lendCommand{Name: l.Unused.Name, Expires: utcMinute(l.Unused.Expires)}
		}
	} else {
		switch q, err := s.store.lastRequest(p.AccountID); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			s.pageFailed(w, err)
			return
		case q.State == requestWaiting:
			page.Waiting = &lendAsked{When: utcMinute(q.Asked), Model: q.ModelName()}
		case q.State == requestDeclined:
			page.Declined = &lendAsked{When: utcMinute(q.Answered), Model: q.ModelName(), Reason: q.Reason}
			if page.Model == "" {
				page.Model = q.Model
			}
		}
	}
	if change != nil {
		change(page)
	}
	if m, ok := lendModelByID(page.Model); ok && page.Leave && page.Name == "" {
		page.Name = s.suggestedName(m)
	}
	s.render(w, r, "lend", web.Page{Title: "Lend a Mac", Nav: "lend", Status: status, Data: page})
}

// lendDone answers an action on "Lend a Mac": back to the page, or the page
// itself saying why the lab refused.
func (s *Server) lendDone(w http.ResponseWriter, r *http.Request, err error) {
	var no *refusal
	switch {
	case err == nil:
		http.Redirect(w, r, s.basePath()+"/lend", http.StatusSeeOther)
	case errors.As(err, &no):
		s.showLend(w, r, no.code, func(page *lendPage) { page.Refused = no.msg })
	default:
		s.log.Printf("Lend a Mac, %s: %v", r.URL.Path, err)
		s.showLend(w, r, http.StatusInternalServerError, func(page *lendPage) { page.Refused = "The lab couldn't do that." })
	}
}

func (s *Server) pRequestToLend(w http.ResponseWriter, r *http.Request) {
	s.lendDone(w, r, s.requestToLend(r, r.PostFormValue("model"), r.PostFormValue("note"), r.PostFormValue("contact")))
}

func (s *Server) pWithdrawRequest(w http.ResponseWriter, r *http.Request) {
	if !s.sure(w, r, "Withdraw: your Request to lend is taken back", "/lend") {
		return
	}
	s.lendDone(w, r, s.withdrawRequest(r))
}

// pMakeJoinCommand answers with the page that shows the Join command's
// line, once: the lab keeps only the hash of its token.
func (s *Server) pMakeJoinCommand(w http.ResponseWriter, r *http.Request) {
	made, err := s.makeJoinCommand(r, r.PostFormValue("model"), r.PostFormValue("name"))
	if errors.Is(err, errAskerCut) {
		// The answer a signed-out browser gets on this route.
		w.Header().Set("Location", "/sign-in")
		w.WriteHeader(http.StatusSeeOther)
		return
	}
	if err != nil {
		s.lendDone(w, r, err)
		return
	}
	s.showLend(w, r, http.StatusOK, func(page *lendPage) {
		page.Made = &lendCommand{Name: made.Name, Line: made.Line, Expires: utcMinute(made.Expires)}
		page.Unused, page.Name, page.Model = nil, "", ""
	})
}

// pCancelJoinCommand is Cancel beside an unused Join command, on "Lend a
// Mac" and above the cards of My Macs; it answers on the page it was
// pressed on.
func (s *Server) pCancelJoinCommand(w http.ResponseWriter, r *http.Request) {
	back := "/lend"
	if r.PostFormValue("back") == "my-macs" {
		back = myMacsPath(leaser(r))
	}
	if !s.sure(w, r, "Cancel: this Join command stops working now", back) {
		return
	}
	err := s.cancelOwnJoinCommand(r)
	if err == nil && back != "/lend" {
		http.Redirect(w, r, s.basePath()+back, http.StatusSeeOther)
		return
	}
	s.lendDone(w, r, err)
}
