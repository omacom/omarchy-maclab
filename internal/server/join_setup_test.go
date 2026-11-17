package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/api"
)

// yesTTY is the terminal of a person who types what they were told to.
type yesTTY struct {
	in  *strings.Reader
	out bytes.Buffer
}

func (y *yesTTY) Read(p []byte) (int, error)  { return y.in.Read(p) }
func (y *yesTTY) Write(p []byte) (int, error) { return y.out.Write(p) }

// macsDoor is the lab as a Mac reaches it: through the tunnel, by the Lab
// address, with the edge's headers.
func macsDoor(t *testing.T, l *signInLab) string {
	t.Helper()
	return throughTunnel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host, r.RemoteAddr = labHost, "@"
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("CF-Connecting-IP", "203.0.113.9")
		l.h.ServeHTTP(w, r)
	}))
}

// The Join command, run on the Mac of the person who made it: `lab-agent
// setup` shows the lab's shipped terms, takes the typed yes and enrolls. The
// lab records that acceptance with the person, the Mac, the time and the
// sha256 of the text, and the Mac arrives at Qualification with publishing
// off.
func TestTheTypedYesIsRecordedWithThePersonTheMacTheTimeAndTheTerms(t *testing.T) {
	l, _ := lendLab(t)
	l.signedIn(maintainerID).did("/people/mark", "handle", "grace", "as", "invite")
	_, token := l.signedIn(graceID).makeJoin("shelf-mac")
	door := macsDoor(t, l)

	sim := agent.NewSim(t.TempDir())
	var said []string
	setup := func(answers string) (*yesTTY, error) {
		tty := &yesTTY{in: strings.NewReader(answers)}
		err := agent.Setup(context.Background(), agent.SetupOptions{Server: door, Token: token, Mac: sim, TTY: tty,
			Log: func(f string, a ...any) { said = append(said, fmt.Sprintf(f, a...)) }})
		return tty, err
	}

	// Declined: the lab has no Mac, and the Join command is still unused.
	if _, err := setup("no\n"); err == nil || !strings.Contains(err.Error(), "not accepted") {
		t.Fatalf("a declined prompt: %v", err)
	}
	if l.s.dev("shelf-mac") != nil || sim.SetUps() != 0 {
		t.Fatal("a Mac whose owner declined the terms is in the lab")
	}
	if n := l.count(`SELECT count(*) FROM join_commands WHERE bound_to != ''`); n != 0 {
		t.Errorf("a declined prompt bound the Join command to the Mac (%d)", n)
	}

	tty, err := setup("yes\n")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, strings.Join(said, "\n"))
	}
	terms := termsSum()
	if shown := tty.out.String(); !strings.Contains(shown, "Lend only a test machine") || !strings.Contains(shown, terms) {
		t.Errorf("the terminal did not show the lab's terms with their sha256:\n%.400s", shown)
	}
	v := l.s.dev("shelf-mac")
	if v == nil {
		t.Fatal("the Mac is not in the lab")
	}
	d := v.snapshot()
	if d.VolunteerID != graceID || d.Name != "shelf-mac" || d.TermsSHA256 != terms || !d.TermsAccepted.Equal(l.clock.Now()) {
		t.Errorf("the acceptance on record: person %d, Mac %s, terms %s at %s", d.VolunteerID, d.Name, d.TermsSHA256, d.TermsAccepted)
	}
	if d.Kind != api.KindVolunteer || d.AccessLevel != api.AccessQualification || d.Publish {
		t.Errorf("the Mac arrived as %s at %q, publishing %v", d.Kind, d.AccessLevel, d.Publish)
	}
	// The same line on the same Mac, once it is set up: already in this lab.
	if _, err := setup("yes\n"); err == nil || !strings.Contains(err.Error(), "already in this lab, as shelf-mac") {
		t.Errorf("the Join command run again: %v", err)
	}
}
