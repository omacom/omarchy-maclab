package server

// The edge is what labd does with every request before any route: which
// name it was asked by, the caller log's line, the prefix labd lives under,
// and the headers every answer carries.
//
//   - Config.PublicURL is the Lab address, the one URL people and Macs use,
//     such as https://lab.omarchy-mac.org. Every link labd builds names it,
//     whichever listener a request came in on. Its path, if it has one, is
//     labd's prefix: labd serves everything under it, and a request outside
//     it gets 404. Without a Lab address labd is a lab under development: it
//     takes each request's own origin.
//   - The host check accepts exactly two names on the tunnel's socket: the
//     Lab address, which is served, and Config.Alias, which only leads to
//     it. On the Alias a page request gets a 308 to the same path and query
//     at the Lab address, and anything under /api/ is refused in one plain
//     line naming the Lab address; no route is reached, so no cookie is set
//     and no credential read. At the loopback port labd answers to this
//     machine's own names (localhost, a loopback address). Any other name
//     gets 421.
//   - What the edge says of a request (CF-Connecting-IP, CF-Ray,
//     X-Forwarded-Proto) is believed on the tunnel's socket only (see
//     listen.go). Nothing forwarded is ever an identity.
//   - The edge marks a request that arrived as TLS early data, which can be
//     replayed: under /api/ it gets 425.
//   - Every answer says nosniff and no-referrer, and no page may frame
//     labd. API answers, which include files Macs uploaded, are sandboxed,
//     so an uploaded page can't run script with labd's origin. Over https
//     the browser is told to stay on https (HSTS). In Preview every answer
//     tells crawlers not to index it.
//   - Every answer states a cache policy (statePolicy): no-store, unless its
//     handler states its own or the edge may keep it.

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
)

// The cache policy. A public page or an export, asked for with no session,
// may be kept at the edge: fresh for a minute, then served stale while the
// edge fetches it again, for up to a day, and for a day while labd fails.
// The edge's lifetime goes in the header only Cloudflare reads, which it
// takes in place of Cache-Control and does not pass on; s-maxage,
// must-revalidate or no-cache in a header the edge reads would end stale
// serving. What the browser gets makes it ask again every time.
// docs/acceptance/cache-policy.md has the pair and what it rests on.
const (
	cacheNever      = "no-store"
	cacheSession    = "private, no-store"
	cacheBrowser    = "public, max-age=0"
	cacheEdgeHeader = "Cloudflare-CDN-Cache-Control"
	cacheEdge       = "max-age=60, stale-while-revalidate=86400, stale-if-error=86400"
)

// keptPaths and keptUnder are the paths whose answer the edge may keep,
// under labd's prefix: the public pages, /install and the exports. The
// first cache rule of scripts/cloud/edge.json names the same ones, and a
// path ending in neverKeptEnd is none of them. A public page that must
// never be kept, as /status, stays out of both.
var (
	keptPaths = []string{
		"/", "/matrix", "/models", "/features", "/gaps", "/aurora", "/performance", "/candidates", "/reports", "/data", "/install", "/install/lab", "/terms", "/privacy",
		"/api/v1/reports", "/api/v1/reports.json", "/api/v1/reports.csv", "/api/v1/checks", "/api/v1/checks.csv",
		"/api/v1/matrix", "/api/v1/matrix.json", "/api/v1/benchmarks", "/api/v1/benchmarks.json", "/api/v1/aurora", "/api/v1/aurora.json", "/api/v1/performance.json",
	}
	keptUnder = []string{"/models/", "/features/", "/performance/", "/candidates/", "/reports/"}
)

const neverKeptEnd = "/deletion"

// sessionCookieText is how the edge's rule knows a request that carries the
// session cookie, by either of its names.
const sessionCookieText = "lab_session="

func edgeKeeps(path string) bool {
	if strings.HasSuffix(path, neverKeptEnd) {
		return false
	}
	return slices.Contains(keptPaths, path) || slices.ContainsFunc(keptUnder, func(under string) bool { return strings.HasPrefix(path, under) })
}

// carriesSession reports whether r carries the session cookie, as the
// edge's rule reads it: known to labd or not.
func carriesSession(r *http.Request) bool {
	return slices.ContainsFunc(r.Header.Values("Cookie"), func(c string) bool { return strings.Contains(c, sessionCookieText) })
}

// statePolicy states the cache policy of the answer to r, whose path under
// labd's prefix is path, as its headers go. A handler that stated its own
// keeps it: the interface's files, /install, an artifact, a page rendered
// for a session. Anything else asked for with the session cookie is that
// session's alone. The edge may keep only a good answer to a read of a
// path it keeps, asked for with no credential and setting no cookie.
// Everything else is no-store: any error, so that the edge goes on serving
// the copy it has.
func statePolicy(hdr http.Header, r *http.Request, path string, status int) {
	switch hdr.Get("Cache-Control") {
	case "":
		// net/http takes the header off a file it could not serve.
		hdr.Set("Cache-Control", cacheNever)
	case cacheNever:
	default:
		return
	}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case carriesSession(r):
		hdr.Set("Cache-Control", cacheSession)
	case read && status == http.StatusOK && r.Header.Get("Authorization") == "" && len(hdr.Values("Set-Cookie")) == 0 && edgeKeeps(path):
		hdr.Set("Cache-Control", cacheBrowser)
		hdr.Set(cacheEdgeHeader, cacheEdge)
	}
}

// rePublicPath is a prefix's path: segments of safe characters.
var rePublicPath = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)*$`)

// ParsePublicURL checks a --public-url: https (or http) scheme://host[:port]
// and an optional path prefix, nothing else.
func ParsePublicURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("--public-url %q: %v", raw, err)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	switch {
	case u.Scheme != "https" && u.Scheme != "http":
		return nil, fmt.Errorf("--public-url %q: want https://host[/prefix]", raw)
	case u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		return nil, fmt.Errorf("--public-url %q: give scheme://host[:port][/prefix] only", raw)
	case !rePublicPath.MatchString(u.Path) || strings.Contains(u.Path, "/.") || u.RawPath != "":
		return nil, fmt.Errorf("--public-url %q: the prefix must be plain path segments, such as /maclab", raw)
	}
	u.Host = canonHost(u.Scheme, u.Host)
	return &url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}, nil
}

// ParseAlias checks an --alias: a host name alone, which the Alias is.
func ParseAlias(raw string) (string, error) {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if !reHostName.MatchString(h) || !strings.Contains(h, ".") {
		return "", fmt.Errorf("--alias %q: give a host name alone, such as maclab.omarchy.org", raw)
	}
	return h, nil
}

// canonHost is host[:port] in lower case, without the scheme's default port.
func canonHost(scheme, host string) string {
	host = strings.ToLower(host)
	if h, p, err := net.SplitHostPort(host); err == nil && p == map[string]string{"http": "80", "https": "443"}[scheme] {
		if strings.Contains(h, ":") {
			return "[" + h + "]"
		}
		return h
	}
	return host
}

// basePath is labd's prefix, "" when it serves at the root.
func (s *Server) basePath() string {
	if s.cfg.PublicURL == nil {
		return ""
	}
	return s.cfg.PublicURL.Path
}

// labAddress is the Lab address, "" when labd was given none.
func (s *Server) labAddress() string {
	if s.cfg.PublicURL == nil {
		return ""
	}
	return s.cfg.PublicURL.String()
}

// pageAddress is the address of a page of the lab, as a line for a Signal
// group gives it; "" when labd was given no Lab address.
func (s *Server) pageAddress(path string) string {
	if s.cfg.PublicURL == nil {
		return ""
	}
	return strings.TrimSuffix(s.cfg.PublicURL.String(), "/") + path
}

// labName is the name labd goes by in what it writes: the Lab address's
// host, or the name r asked for when labd was given no Lab address.
func (s *Server) labName(r *http.Request) string {
	if s.cfg.PublicURL != nil {
		return s.cfg.PublicURL.Host
	}
	return r.Host
}

// Preview reports whether the lab is in Preview: answering at the Lab
// address, closed, before it opens. It is the one switch for everything
// Preview changes. Once the lab has opened (opening.go) it is never in Preview
// again, whatever labd was started with.
func (s *Server) Preview() bool { return s.cfg.Preview && s.openedAt.Load() == 0 }

// asked is which of labd's names a request asked for.
type asked int

const (
	otherName asked = iota // refused
	labItself              // the Lab address, or this machine at the loopback port
	aliasName              // the Alias: forwarded or refused, never served
)

// externalOrigin is the origin r came to, scheme://host[:port] as the caller
// saw it, and which of labd's names that is. Through the tunnel it is the
// Lab address's, when r names it and the edge says nothing else of the
// scheme; the Alias leads to the Lab address whatever the scheme. At the loopback port it is the connection's own scheme and the
// Host header, which must name this machine once labd has a Lab address.
func (s *Server) externalOrigin(r *http.Request) (origin string, name asked) {
	pub := s.cfg.PublicURL
	if viaTunnel(r) {
		if pub == nil {
			return "", otherName
		}
		switch host := canonHost(pub.Scheme, r.Host); {
		case s.cfg.Alias != "" && host == s.cfg.Alias:
			return pub.Scheme + "://" + host, aliasName
		case host != pub.Host:
			return "", otherName
		}
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" && !strings.EqualFold(proto, pub.Scheme) {
			return "", otherName
		}
		return pub.Scheme + "://" + pub.Host, labItself
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if pub != nil && !loopbackName(r.Host) {
		return "", otherName
	}
	return scheme + "://" + canonHost(scheme, r.Host), labItself
}

// sameOriginAs reports whether a browser's Origin header names origin.
func sameOriginAs(o, origin string) bool {
	u, err := url.Parse(o)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" {
		return false
	}
	return u.Scheme+"://"+canonHost(u.Scheme, u.Host) == origin
}

// edge wraps the API and UI: the caller log, the name check, the Alias, the
// prefix, the browser check and the headers every answer carries.
func (s *Server) edge(h http.Handler) http.Handler {
	base := s.basePath()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.logged(w, r, func(a *answer, r *http.Request) {
			var w http.ResponseWriter = a
			hdr := w.Header()
			hdr.Set("X-Content-Type-Options", "nosniff")
			hdr.Set("Referrer-Policy", "no-referrer")
			hdr.Set("Cache-Control", cacheNever)
			if s.Preview() {
				hdr.Set("X-Robots-Tag", "noindex")
			}
			origin, name := s.externalOrigin(r)
			api := strings.HasPrefix(r.URL.Path, base+"/api/")
			switch {
			case name == otherName:
				hdr.Set("Content-Security-Policy", "sandbox; default-src 'none'")
				if lab := s.labAddress(); lab != "" {
					httpErr(w, http.StatusMisdirectedRequest, "refused: Omarchy Mac Lab is at %s and does not answer as %s", lab, hostName(r.Host))
				} else {
					httpErr(w, http.StatusMisdirectedRequest, "refused: this labd was given no Lab address (--public-url), so it serves nothing through the tunnel's socket")
				}
				return
			case name == aliasName && api:
				hdr.Set("Content-Security-Policy", "sandbox; default-src 'none'")
				httpErr(w, http.StatusMisdirectedRequest, "Omarchy Mac Lab is at %s: use that address", s.labAddress())
				return
			case name == aliasName:
				to := s.cfg.PublicURL.Scheme + "://" + s.cfg.PublicURL.Host + r.URL.EscapedPath()
				if r.URL.RawQuery != "" {
					to += "?" + r.URL.RawQuery
				}
				hdr.Set("Content-Security-Policy", "sandbox; default-src 'none'")
				hdr.Set("Location", to)
				w.WriteHeader(http.StatusPermanentRedirect)
				return
			}
			if strings.HasPrefix(origin, "https://") {
				hdr.Set("Strict-Transport-Security", "max-age=31536000")
			}
			if api && r.Header.Get("Early-Data") == "1" {
				hdr.Set("Content-Security-Policy", "sandbox; default-src 'none'")
				httpErr(w, http.StatusTooEarly, "sent as TLS early data, which can be replayed: send it again once the connection is up")
				return
			}
			if base != "" {
				rest, found := strings.CutPrefix(r.URL.Path, base)
				// A path the router would clean (//, /./, /../) gets 404, not
				// its redirect: that would name a path outside the prefix.
				if !found || (rest != "" && rest[0] != '/') || (rest != "" && path.Clean(rest) != strings.TrimSuffix(rest, "/") && path.Clean(rest) != rest) {
					hdr.Set("Content-Security-Policy", "sandbox; default-src 'none'")
					http.NotFound(w, r)
					return
				}
				r = stripPrefix(r, base, rest)
				w = &prefixedRedirects{ResponseWriter: w, base: base}
			}
			if api {
				hdr.Set("Content-Security-Policy", "sandbox; default-src 'none'; frame-ancestors 'none'")
			} else {
				hdr.Set("Content-Security-Policy", "frame-ancestors 'none'")
			}
			asked := r
			a.before = func(status int) { statePolicy(hdr, asked, asked.URL.Path, status) }
			if !s.sameOriginOK(w, r, origin) {
				return
			}
			r, ok := s.browserSession(w, r, origin)
			if !ok {
				return
			}
			r = s.labTokenRequest(r)
			h.ServeHTTP(w, r)
		})
	})
}

// prefixedRedirects puts labd's prefix back in front of the path a redirect
// names: the router and file server below it know only the stripped path,
// and a redirect to /ui/ would otherwise leave labd for its neighbours.
type prefixedRedirects struct {
	http.ResponseWriter
	base string
}

func (p *prefixedRedirects) WriteHeader(code int) {
	if code >= 300 && code < 400 {
		if loc := p.Header().Get("Location"); strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") {
			p.Header().Set("Location", p.base+loc)
		}
	}
	p.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the writer below (flushing a
// stream, its deadlines).
func (p *prefixedRedirects) Unwrap() http.ResponseWriter { return p.ResponseWriter }

// Flush flushes, for the handlers that ask the writer itself.
func (p *prefixedRedirects) Flush() {
	if f, ok := p.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// stripPrefix is r with base taken off its path once; rest is what follows
// it, "" for the prefix itself, which serves the UI.
func stripPrefix(r *http.Request, base, rest string) *http.Request {
	r2 := new(http.Request)
	*r2 = *r
	r2.URL = new(url.URL)
	*r2.URL = *r.URL
	if rest == "" {
		rest = "/"
	}
	r2.URL.Path = rest
	r2.URL.RawPath = ""
	if r.URL.RawPath != "" {
		if rp, found := strings.CutPrefix(r.URL.RawPath, base); found {
			r2.URL.RawPath = rp
		}
	}
	return r2
}
