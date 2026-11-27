package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// Scope is what a request's caller may see and do. A Maintainer sees the
// whole fleet. A person labd knows without a Maintainer's token sees
// only the Macs they volunteered, or a Maintainer linked to their login,
// while those stay enrolled. A Mac sees only
// itself, through its own routes (see agent). A Volunteer's Mac must never
// reach another Mac, a Volunteer's or Lab-owned, and this is where that
// holds. What a caller may do to a Mac (mayDo) keeps a Volunteer's Mac's
// Access level, Availability window and held rules in one place too.
//
// Every route says who may reach it (reach), and labd registers no route
// that doesn't, so a new route can't forget to ask.

// caller is who made a request on a route people may reach.
type caller struct {
	maintainer string // a Maintainer's name, by their token
	login      string // a person's login, without a token
	team       string // an M team member's handle: they read the whole fleet
	account    int64  // the GitHub account id of a signed-in person who is no Maintainer
	mac        string // a Mac's name, by its device secret
	labd       bool   // labd itself (see labdItself)
}

type (
	personKey  struct{}
	teamKey    struct{}
	accountKey struct{}
)

// reads reports whether c reads every Mac and every job: a Maintainer, or
// an M team member, who sees the fleet and whose each Mac is.
func (c caller) reads() bool { return c.maintainer != "" || c.team != "" }

// callerOf is who made r.
func callerOf(r *http.Request) caller { return callerOfCtx(r.Context()) }

// callerOfCtx is who a request's context acts for; the zero caller when
// nobody is named, which Scope refuses everything (see labdItself).
func callerOfCtx(ctx context.Context) caller {
	c := caller{maintainer: maintainerOf(ctx)}
	c.login, _ = ctx.Value(personKey{}).(string)
	c.team, _ = ctx.Value(teamKey{}).(string)
	c.account, _ = ctx.Value(accountKey{}).(int64)
	if v, ok := ctx.Value(ctxKey{}).(*dev); ok {
		c.mac = v.d.Name
	}
	return c
}

// sees reports whether c may see the Mac called name, its jobs and its
// events. name "" is the fleet's own: builds and the like, Maintainers' only.
func (s *Server) sees(c caller, name string) bool {
	if c.maintainer != "" {
		return true
	}
	if c.team != "" {
		return name != ""
	}
	if c.login == "" || name == "" {
		return false
	}
	v := s.dev(name)
	if v == nil {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return s.seesLocked(c, v)
}

// seesLocked is sees for a person and a Mac whose v.mu the caller holds:
// the Mac they volunteered, or one a Maintainer linked to their login. A
// signed-in person is known by account id, and sees the Macs they lend.
func (s *Server) seesLocked(c caller, v *dev) bool {
	if c.login == "" || v.forgotten || v.d.State == api.StateLeft {
		return false
	}
	if c.account != 0 {
		return v.d.VolunteerID == c.account
	}
	return v.volunteeredBy != "" && strings.EqualFold(v.volunteeredBy, c.login) ||
		v.d.LinkedLogin != "" && strings.EqualFold(v.d.LinkedLogin, c.login)
}

// reLogin is a tailnet login: user@github, user@passkey, or an email.
var reLogin = regexp.MustCompile(`^[a-z0-9][a-z0-9._%+-]{0,63}@[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// reSecretLike is what labd's and Tailscale's secrets look like: a typed
// prefix (enr_, adm_, dev_, ...) or a long run of hex.
var reSecretLike = regexp.MustCompile(`^(enr|adm|dev|bld|mnt)_|^tskey-|[0-9a-f]{24}`)

// volunteerLogin is raw as a login a Maintainer may link to a Mac,
// lowercased, or why it isn't one. Linking shows the Mac's screens and jobs
// to whoever holds that login, so a typo or a pasted secret must not pass.
func volunteerLogin(raw string) (string, error) {
	l := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case reSecretLike.MatchString(l):
		return "", fmt.Errorf("%q looks like a secret, not a tailnet login: give the Volunteer's login, like ada@github or ada@example.org", raw)
	case len(l) > 254 || !reLogin.MatchString(l):
		return "", fmt.Errorf("%q isn't a tailnet login: want one like ada@github or ada@example.org (tailscale whois shows it)", raw)
	}
	return l, nil
}

// seesJob reports whether c may read job j: a Maintainer may read any; a
// person only one their Mac ran in its current enrollment. A forgotten Mac's
// name can be taken again, by anyone, and the old enrollment's jobs stay with
// the Maintainers.
func (s *Server) seesJob(c caller, j *api.Job) bool {
	if comparePrivate(j) {
		return c.maintainer != ""
	}
	if c.reads() {
		return true
	}
	if !s.sees(c, j.Spec.Device) {
		return false
	}
	v := s.dev(j.Spec.Device)
	if v == nil {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return j.DeviceID != "" && j.DeviceID == v.d.ID
}

// seenEnrollments is the enrollments (Device.ID) of the Macs c sees, or of
// the one Mac named if name isn't "".
func (s *Server) seenEnrollments(c caller, name string) []string {
	var ids []string
	for _, v := range s.allDevs() {
		if (name == "" || v.d.Name == name) && s.sees(c, v.d.Name) {
			v.mu.Lock()
			ids = append(ids, v.d.ID)
			v.mu.Unlock()
		}
	}
	return ids
}

// seenMacs names the Macs c sees.
func (s *Server) seenMacs(c caller) []string {
	var names []string
	for _, v := range s.allDevs() {
		if s.sees(c, v.d.Name) {
			names = append(names, v.d.Name)
		}
	}
	return names
}

// reach is who may reach a route.
type reach int

const (
	unset       reach = iota // refused at registration
	anyone                   // no credential: /api/auth, enrolling with a token
	ownMac                   // a Mac, by its device secret; it only touches itself
	builder                  // the builder, by its token
	invited                  // someone joining with an enrollment token (enr_...), see joinToken
	maintainers              // a Maintainer's token: the fleet's own, and every change
	team                     // a Maintainer or an M team member: a Lease
	teamBuilds               // a Maintainer, or an M team member by the Builder's rules (builders.go)
	people                   // Maintainers, and any person labd knows: ./volunteer
	macNamed                 // the Mac in {name}: whoever sees it
	jobNamed                 // the job in {id}: whoever sees its Mac
	scoped                   // a list or the stream: the handler keeps to what the caller sees
)

// letsPeopleIn reports whether a person without a Maintainer's token may
// reach a route at all: the reaches from people on, in the order above.
func (r reach) letsPeopleIn() bool { return r >= people }

type route struct {
	pattern string
	reach   reach
	h       http.HandlerFunc
}

// register adds rt to m behind what its reach asks for.
func (s *Server) register(m *http.ServeMux, rt route) {
	h := rt.h
	switch rt.reach {
	case anyone:
	case ownMac:
		h = s.agent(h)
	case builder:
		h = s.builderAuth(h)
	case invited:
		h = s.joinToken(h)
	case maintainers:
		h = s.maintainer(h)
	case team:
		h = s.teamUp(h)
	case teamBuilds:
		h = s.buildsTeam(h)
	case people:
		h = s.viewer(notSignedIn(h))
	case scoped:
		h = s.viewer(h)
	case macNamed:
		h = s.viewer(s.macInScope(h))
	case jobNamed:
		h = s.viewer(s.jobInScope(h))
	default:
		panic(fmt.Sprintf("route %s says nothing about who may reach it", rt.pattern))
	}
	m.HandleFunc(rt.pattern, s.labCall(rt, h))
}

// macInScope answers 404 for a Mac the caller doesn't see, as if it weren't
// there: names of other Macs stay unknown.
func (s *Server) macInScope(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, name := callerOf(r), r.PathValue("name")
		if !s.sees(c, name) {
			if s.dev(name) != nil {
				s.log.Printf("%s asked for %s %s, a Mac they don't see", c.login, r.Method, r.URL.Path)
			}
			httpErr(w, 404, "no device %q", name)
			return
		}
		h(w, r)
	}
}

// jobInScope answers 404 for a job the caller may not read (seesJob).
func (s *Server) jobInScope(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, id := callerOf(r), r.PathValue("id")
		if c.maintainer == "" {
			if j, err := s.store.job(id); err != nil || !s.seesJob(c, j) {
				s.log.Printf("%s asked for %s %s, a job they don't see", c.login, r.Method, r.URL.Path)
				httpErr(w, 404, "no job %s", id)
				return
			}
		}
		h(w, r)
	}
}

// volunteersOwn are the fields of a Mac's view that are for its Volunteer
// and the Maintainers: how to reach the Volunteer, and the topic the Mac's
// notices go to. The M team sees whose Mac it is and no more of them.
var volunteersOwn = []string{"contact", "ntfy_topic", "ntfy_own_url", "notice_topic_refused", "linked_login"}

// viewFor is the Mac as c may see it.
func (s *Server) viewFor(c caller, v *dev) DeviceView {
	d := s.view(v)
	if d.VolunteerID != 0 {
		if k, err := s.store.knownPerson(d.VolunteerID); err == nil {
			d.VolunteerHandle = k.Handle
		}
	}
	if c.team != "" {
		d.Contact, d.NtfyTopic, d.NtfyOwnURL, d.NoticeTopicRefused, d.LinkedLogin = "", "", "", "", ""
	}
	if c.maintainer != "" && !d.IsVolunteer() {
		d.SignIn = s.signInGap(v.snapshot().MachineID)
	}
	return d
}

// signInGap is why a Lab-owned Mac's runs are Community runs: its Machine key
// is not signed in, or is signed in as someone off the M team. "" when its
// runs are Verified runs, or before its first report gave the lab its key.
func (s *Server) signInGap(machineID string) string {
	if machineID == "" || s.reports == nil {
		return ""
	}
	signIn, err := s.reports.SignInOf(machineID)
	switch {
	case err != nil:
		s.log.Printf("the sign-in of a Mac: %v", err)
		return ""
	case signIn == nil:
		return "not signed in"
	case !s.onMTeam(signIn.AccountID):
		return "signed in as @" + signIn.Handle + ", who is not on the M team"
	}
	return ""
}

// forTeam is a Mac's view as the stream carries it, without what is its
// Volunteer's own.
func forTeam(view []byte) []byte {
	var d map[string]json.RawMessage
	if json.Unmarshal(view, &d) != nil {
		return []byte("{}")
	}
	for _, field := range volunteersOwn {
		delete(d, field)
	}
	out, _ := json.Marshal(d)
	return out
}

// asSignedIn serves r for the person whose browser session or Lab token it
// came with, who is no Maintainer, under key: an M team member (teamKey) or
// an External volunteer (personKey). It ends when their role goes, or the
// session or the token.
func (s *Server) asSignedIn(w http.ResponseWriter, r *http.Request, key any, h http.HandlerFunc) {
	p := credentialOf(r)
	noteCaller(r, "person:"+p.Handle)
	ctx := context.WithValue(context.WithValue(r.Context(), key, p.Handle), accountKey{}, p.AccountID)
	ctx, cancel := context.WithCancel(ctx)
	defer s.conns.add(cancel, connKeysOf(r)...)()
	h(w, r.WithContext(ctx))
}

// notSignedIn keeps a route of the retired ./volunteer from a person who
// signed in on the site: a Mac is lent from "Lend a Mac" there.
func notSignedIn(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c := callerOf(r); c.account != 0 {
			httpErr(w, 403, "a Mac is lent from the site: sign in and open Lend a Mac")
			return
		}
		h(w, r)
	}
}

// asPerson serves r for a person's login.
func asPerson(w http.ResponseWriter, r *http.Request, login string, h http.HandlerFunc) {
	noteCaller(r, "person:"+login)
	h(w, r.WithContext(context.WithValue(r.Context(), personKey{}, login)))
}

// errNotSeen is what a caller gets for a Mac they don't see: as if it
// weren't there.
var errNotSeen = errors.New("no such Mac")

// labdItself is labd acting on its own, for a job no Maintainer submitted.
var labdItself = caller{labd: true}

// callerFor is the caller a job acts as: its Maintainer, or labd itself.
func callerFor(maintainer string) caller {
	if maintainer == "" {
		return labdItself
	}
	return caller{maintainer: maintainer}
}

// action is something a caller asks to do on a Mac.
type action struct {
	kind actionKind
	what string // named in a refusal
}

type actionKind int

const (
	kindFull   actionKind = iota + 1 // needs a Volunteer's Mac at Access level Full
	kindWatch                        // its screen or live journal
	kindLease                        // holding it for a Maintainer
	kindUpload                       // a file added to one of its jobs
)

var (
	actExec     = action{kindFull, "lab exec"}
	actMesa     = action{kindFull, "staged Mesa packages"}
	actRecovery = action{kindFull, "recovery-policy overrides"}
	actWatch    = action{kindWatch, "its screen or live journal"}
	actLease    = action{kindLease, "a lease"}
	actUpload   = action{kindUpload, "files added to its jobs"}
)

// actScript is running a Maintainer's script on a Mac; why says which (see
// needsFull).
func actScript(why string) action { return action{kindFull, "Maintainer's script (" + why + ")"} }

// mayDo is why c may not do a on v, or nil when it may. A Mac c doesn't see
// is errNotSeen. A person may only watch their own Mac; a Mac may only add
// files to its own jobs; Maintainers and labd do the rest, and an M team
// member on a Mac they lease, as far as a Volunteer's Mac's Access level
// and Availability window allow.
func (s *Server) mayDo(c caller, v *dev, a action) error {
	if v == nil {
		return errNotSeen
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return s.mayDoLocked(c, v, a)
}

// mayDoLocked is mayDo for callers already holding v.mu.
func (s *Server) mayDoLocked(c caller, v *dev, a action) error {
	if v == nil {
		return errNotSeen
	}
	d := &v.d
	switch {
	case c.maintainer != "" || c.labd:
	case c.mac != "":
		if a.kind == kindUpload && c.mac == d.Name {
			return nil
		}
		return fmt.Errorf("a Mac may not ask for %s on %s", a.what, d.Name)
	case c.team != "":
		switch l := v.liveLeaseLocked(); {
		case l == nil || l.AccountID == 0 || l.AccountID != c.account:
			return fmt.Errorf("an M team member needs their own Lease on %s for %s: `lab lease %s`", d.Name, a.what, d.Name)
		case l.Extended:
			return errors.New(extendedOnly(l, d.Name, a.what))
		}
	case c.login != "":
		if !s.seesLocked(c, v) {
			return errNotSeen
		}
		if a.kind != kindWatch {
			return fmt.Errorf("only a Maintainer may ask for %s on %s", a.what, d.Name)
		}
	default:
		return errNotSeen
	}
	switch a.kind {
	case kindFull:
		return refuseBelowFull(d, a)
	case kindWatch:
		if d.IsVolunteer() && (!v.heldLocked() || v.openUntilLocked().IsZero()) {
			return fmt.Errorf("%s is a Volunteer's Mac: at any Access level its screen and live journal are shown only while a job or lease holds it inside its Availability window", d.Name)
		}
	case kindLease:
		if d.IsVolunteer() && v.openUntilLocked().IsZero() {
			return fmt.Errorf("%s's Availability window is closed: a lease on a Volunteer's Mac lasts only inside it", d.Name)
		}
	case kindUpload:
		return fmt.Errorf("only a Mac adds files to its own jobs")
	}
	return nil
}

// refuseBelowFull is why a, which only Access level Full allows, can't run
// on a Volunteer's Mac at Qualification, or nil. validateMesa asks it of a
// job's copy of the Mac, whoever submitted the job.
func refuseBelowFull(d *api.Device, a action) error {
	if d.AllowsAnything() {
		return nil
	}
	if d.Limited && d.AccessLevel == api.AccessFull {
		return fmt.Errorf("%s is a Volunteer's Mac its Volunteer limited to Access level Qualification, which allows no %s; only its Volunteer can remove the limit, on My Macs", d.Name, a.what)
	}
	return fmt.Errorf("%s is a Volunteer's Mac at Access level Qualification, which allows no %s; only its Volunteer can raise it (sudo lab-agent access full)", d.Name, a.what)
}
