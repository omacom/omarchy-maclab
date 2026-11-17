// Package web is the lab's interface: one header and one footer around
// every page, the files those pages load, and the two texts of /terms and
// /privacy. It is compiled into labd whole and knows nothing of labd's
// data: the server says who is looking and hands each page its facts.
//
// A page is one file under templates/ that defines "main" and, if it wants
// something in the document's head, "head"; it is named by its path there
// without ".html", such as "reports/show". Files whose name starts with "_"
// are shared pieces, parsed into every page.
package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
)

//go:embed all:templates
var templateFiles embed.FS

// Kind is who is looking at a page, in the order of the interface's table.
type Kind int

const (
	SignedOut Kind = iota
	NoRole
	External
	MTeam
	Maintainer
	Admin
)

// Kinds are the six viewers.
var Kinds = []Kind{SignedOut, NoRole, External, MTeam, Maintainer, Admin}

func (k Kind) String() string {
	return [...]string{"Signed out", "Signed in, no role", "External volunteer", "M team member", "Maintainer", "Admin"}[k]
}

// Badge is the kind as the header's outline badge says it.
func (k Kind) Badge() string {
	return [...]string{"", "no role", "External volunteer", "M team", "Maintainer", "Admin"}[k]
}

// BadgeClass colours the badge.
func (k Kind) BadgeClass() string {
	return [...]string{"", "kind-none", "kind-external", "kind-mteam", "kind-maintainer", "kind-admin"}[k]
}

// Access is the set of kinds that may open a page.
type Access uint8

func only(kinds ...Kind) Access {
	var a Access
	for _, k := range kinds {
		a |= 1 << k
	}
	return a
}

var (
	Anyone      = only(Kinds...)
	SignedIn    = only(NoRole, External, MTeam, Maintainer, Admin)
	NoRoleOnly  = only(NoRole)
	Volunteers  = only(External)
	MTeamUp     = only(MTeam, Maintainer, Admin)
	Maintainers = only(Maintainer, Admin)
	Admins      = only(Admin)
)

// Allows reports whether a viewer of kind k may open the page.
func (a Access) Allows(k Kind) bool { return a&(1<<k) != 0 }

// NavItem is one link of the header.
type NavItem struct {
	ID, Label, Path string
	// Count is the number shown beside the label, when it is not zero.
	Count int
}

// PublicNav is row 1, the same for every viewer.
func PublicNav() []NavItem {
	return []NavItem{
		{ID: "matrix", Label: "Matrix", Path: "/matrix"},
		{ID: "models", Label: "Models", Path: "/models"},
		{ID: "features", Label: "Features", Path: "/features"},
		{ID: "gaps", Label: "Kernel gaps", Path: "/gaps"},
		{ID: "aurora", Label: "Aurora", Path: "/aurora"},
		{ID: "benchmarks", Label: "Benchmarks", Path: "/benchmarks"},
		{ID: "candidates", Label: "Candidates", Path: "/candidates"},
		{ID: "reports", Label: "Reports", Path: "/reports"},
		{ID: "data", Label: "Data", Path: "/data"},
	}
}

var (
	navLend     = NavItem{ID: "lend", Label: "Lend a Mac", Path: "/lend"}
	navMyMacs   = NavItem{ID: "my-macs", Label: "My Macs", Path: "/my-macs"}
	navMacs     = NavItem{ID: "macs", Label: "Macs", Path: "/macs"}
	navJobs     = NavItem{ID: "jobs", Label: "Jobs", Path: "/jobs"}
	navBuilds   = NavItem{ID: "builds", Label: "Builds", Path: "/builds"}
	navReleases = NavItem{ID: "releases", Label: "Releases", Path: "/releases"}
	navPeople   = NavItem{ID: "people", Label: "People", Path: "/people"}
)

// OwnPage is the path of a person's own page, which their handle links to.
const OwnPage = "/me"

// LabNav is row 2 for a viewer of kind k: the lab pages that kind may open.
func LabNav(k Kind) []NavItem {
	switch k {
	case NoRole:
		return []NavItem{navLend}
	case External:
		return []NavItem{navMyMacs}
	case MTeam:
		return []NavItem{navMacs, navJobs, navReleases}
	case Maintainer, Admin:
		return []NavItem{navMacs, navJobs, navBuilds, navReleases, navPeople}
	}
	return nil
}

// Fleet is the fleet status at the end of row 2, for the M team and up.
type Fleet struct {
	Macs, Free, JobsRunning, NeedsHands int
}

// Viewer is who a page is rendered for.
type Viewer struct {
	Kind   Kind
	Handle string
	// CSRF is the token the viewer's forms carry; "" signed out.
	CSRF string
}

// Request is what the server knows of one request for a page.
type Request struct {
	Viewer Viewer
	// Path is this page's own path and query under the Lab address, where
	// a visitor who signs in from it returns.
	Path string
	// Fleet is shown to the M team and up; nil shows nothing.
	Fleet *Fleet
	// PendingRequests is the count People carries in the navigation.
	PendingRequests int
	// Preview is the lab before it opens.
	Preview bool
}

// Page is one page's own part.
type Page struct {
	// Title names the page in the browser's tab; "" on the home page.
	Title string
	// Nav is the ID of the header's link this page belongs under, "" for none.
	Nav string
	// Status is the HTTP status; 0 is 200.
	Status int
	// Public marks a page anyone may open: in Preview it carries the banner
	// naming the live site.
	Public bool
	// Data is whatever the page's template reads as .Data.
	Data any
}

// LiveSite is the site that serves the public until the lab opens.
const LiveSite = "omarchy-m-testing.org"

// Site is the interface as one labd serves it, under its prefix.
type Site struct {
	base   string
	pages  map[string]*template.Template
	assets map[string]*asset // by plain name
	served map[string]*asset // by fingerprinted name
}

// New builds the interface for a labd served under base, "" for none.
func New(base string) (*Site, error) {
	s := &Site{base: base, pages: map[string]*template.Template{}}
	if err := s.loadAssets(); err != nil {
		return nil, err
	}
	funcs := template.FuncMap{"href": s.Href, "asset": s.Asset, "copy": copyOf}
	shared := template.New("shared").Funcs(funcs)
	var pages []string
	err := fs.WalkDir(templateFiles, "templates", func(f string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(f) != ".html" {
			return err
		}
		if name := path.Base(f); f == "templates/layout.html" || strings.HasPrefix(name, "_") {
			_, err = shared.ParseFS(templateFiles, f)
		} else {
			pages = append(pages, f)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	for _, f := range pages {
		t, err := shared.Clone()
		if err == nil {
			_, err = t.ParseFS(templateFiles, f)
		}
		if err != nil {
			return nil, err
		}
		if t.Lookup("main") == nil {
			return nil, fmt.Errorf("%s defines no \"main\"", f)
		}
		s.pages[strings.TrimSuffix(strings.TrimPrefix(f, "templates/"), ".html")] = t
	}
	return s, nil
}

// Href is a path of the lab as a page links to it: under labd's prefix.
func (s *Site) Href(p string) string { return s.base + p }

// view is what a template sees.
type view struct {
	Page
	Request
	site *Site
}

func (v view) Row1() []NavItem { return PublicNav() }

func (v view) Row2() []NavItem {
	items := LabNav(v.Viewer.Kind)
	for i := range items {
		if items[i].ID == navPeople.ID {
			items[i].Count = v.PendingRequests
		}
	}
	return items
}

func (v view) SignedIn() bool { return v.Viewer.Kind != SignedOut }

// ShowFleet is the fleet status, for the M team and up.
func (v view) ShowFleet() *Fleet {
	if MTeamUp.Allows(v.Viewer.Kind) {
		return v.Fleet
	}
	return nil
}

// Banner reports whether the page carries Preview's banner.
func (v view) Banner() bool { return v.Preview && v.Public }

func (v view) LiveSite() string { return LiveSite }

func (v view) OwnPage() string { return v.site.Href(OwnPage) }

// SignInURL is the Sign in link: it comes back to this page.
func (v view) SignInURL() string {
	u := v.site.Href("/sign-in")
	if v.Path != "" {
		u += "?return=" + url.QueryEscape(v.Path)
	}
	return u
}

// Take is one action that takes something away, for the "take" template: a
// button that asks twice in place. The first click turns Label into
// Sentence, which states the effect; the second posts to Action.
type Take struct {
	Action, Label, Sentence, CSRF string
	Fields                        []Field
}

// Field is one more value a form carries.
type Field struct{ Name, Value string }

// Take is the button for {{template "take" (.Take "/path" "Remove" "Remove:
// 1 Lease ends now")}}; fields are further name, value pairs of its form.
func (v view) Take(action, label, sentence string, fields ...string) Take {
	t := Take{Action: v.site.Href(action), Label: label, Sentence: sentence, CSRF: v.Viewer.CSRF}
	for i := 0; i+1 < len(fields); i += 2 {
		t.Fields = append(t.Fields, Field{fields[i], fields[i+1]})
	}
	return t
}

// With is the view with data as its page's Data: for a shared piece that two
// pages show, each handing it its own part of what it reads.
func (v view) With(data any) view {
	v.Data = data
	return v
}

// Copyable is a value a person is meant to copy, what the pieces "copy",
// "copy-block" and "copy-title" of layout.html read: the text, what its Copy
// button names ("the Join command"), and an id for a value a link points at.
// The button is rendered hidden and app.js shows it, so a browser that runs
// no script shows the text alone, still selectable.
type Copyable struct {
	What, Text, ID string
}

// copyOf is the template function copy: copy WHAT TEXT [ID].
func copyOf(what string, text any, id ...string) Copyable {
	c := Copyable{What: what, Text: fmt.Sprint(text)}
	if len(id) > 0 {
		c.ID = id[0]
	}
	return c
}

// Asking is what the page "ask_twice" reads: the server asking the second
// time itself, for a browser that ran no script.
type Asking struct {
	Action, Sentence, Back string
	Fields                 []Field
}

// CSP is the content security policy of every page: the lab's own files
// only, so a page carries no inline script and no inline style.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// Render writes the page named name, inside the header and footer. A page
// rendered for someone signed in is theirs alone and is never stored.
func (s *Site) Render(w http.ResponseWriter, name string, req Request, p Page) error {
	t := s.pages[name]
	if t == nil {
		return fmt.Errorf("web: no page %q in templates/", name)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", view{Page: p, Request: req, site: s}); err != nil {
		return fmt.Errorf("web: page %q: %v", name, err)
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", CSP)
	if req.Viewer.Kind != SignedOut {
		h.Set("Cache-Control", "private, no-store")
	}
	if p.Status != 0 {
		w.WriteHeader(p.Status)
	}
	_, err := w.Write(buf.Bytes())
	return err
}
