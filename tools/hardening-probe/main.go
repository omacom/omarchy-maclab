// Command hardening-probe is what Hardening's tests (scripts/cutover/hardening/)
// cannot do with lab and curl alone: send one request under the saved login,
// a Lab token kept in a file or a browser's session, hold a stream and say how
// it ended, act as a browser on the lab's own pages, and count who connects
// to a port.
//
// It prints key=value lines and never a token, a session or a page. Under
// the saved login it is the Go client the lab command is: the login is read
// by internal/client, which sends a saved Lab token only to the address it
// was saved for.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/client"
	"github.com/omacom/omarchy-maclab/internal/labhttp"
)

const usage = `usage: hardening-probe <command>

  ask [--token-file F | --session F] [--json BODY] [--header "K: V"]... [--out FILE] [--rows K,K...] [--timeout 3m] METHOD PATH
      one request: its status and what it said in one line. With no
      --token-file or --session it goes under the saved login. --rows prints
      one row= line for each object of a JSON array, the values of K,K...
      (a.b is the b inside a)
  hold [--token-file F | --session F] [--for SEC] PATH
      hold a stream (GET): opened=STATUS as soon as it answers, then
      ended=closed|timeout|error and seconds=N
  sign-in --session F --as ACCOUNT [--return PATH]
      sign in as a browser does, at a lab whose GitHub is a stand-in that
      signs in any account it knows (tools/fake-github): the session goes
      into F; landed= is where the browser was sent afterwards
  form --session F [--page PATH] [--origin URL | --no-origin] [--fetch-site V] [--out FILE] PATH [NAME=VALUE]...
      post a form as the lab's own page does: the CSRF token is read from
      --page (default /me). --origin names another page's origin
  page --session F [--find REGEX] [--out FILE] PATH
      fetch a page as the browser of the session: its status, and each
      first group of REGEX found= in the page's order, once (never the page)
  listen
      take connections on a loopback port: port=N, then connection=N for
      each one, until stopped

The lab's address is the saved login's, or LAB_ADDRESS.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	var err error
	switch os.Args[1] {
	case "ask":
		err = askCommand(fs, os.Args[2:])
	case "hold":
		err = holdCommand(fs, os.Args[2:])
	case "sign-in":
		err = signInCommand(fs, os.Args[2:])
	case "form":
		err = formCommand(fs, os.Args[2:])
	case "page":
		err = pageCommand(fs, os.Args[2:])
	case "listen":
		err = listenCommand()
	default:
		fs.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hardening-probe:", err)
		os.Exit(1)
	}
}

var out io.Writer = os.Stdout

func say(key string, value any) { fmt.Fprintf(out, "%s=%v\n", key, value) }

// credential is a token the lab hands out: a builder's, a Lab token, a Join
// command's.
var credential = regexp.MustCompile(`\b(bldr|lab|enr)_[0-9a-f]{16,}`)

// plain is what a caller said, fit for a record: one line of plain
// characters, never a page and never a credential.
func plain(b []byte) string {
	head := strings.ToLower(string(b[:min(len(b), 512)]))
	if strings.Contains(head, "<!doctype") || strings.Contains(head, "<html") || strings.Contains(head, "<body") {
		return "a page, not shown"
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	line = credential.ReplaceAllString(line, "(a credential, not shown)")
	var o strings.Builder
	for _, r := range line {
		if r < ' ' || r > '~' || r == '<' || r == '>' {
			r = '?'
		}
		o.WriteRune(r)
		if o.Len() >= 300 {
			break
		}
	}
	return o.String()
}

// who is how a request is let in: the saved login, a Lab token in a file,
// or a browser's session in a file.
type who struct {
	base    string
	token   string
	session string
	agent   string
	http    *http.Client
}

const browserAgent = "Mozilla/5.0 (hardening probe)"

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if v == "" || strings.ContainsAny(v, "\n\r ") {
		return "", fmt.Errorf("%s does not hold one line", path)
	}
	return v, nil
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func address() (string, error) {
	a := strings.TrimRight(os.Getenv("LAB_ADDRESS"), "/")
	if a == "" {
		return "", errors.New("no LAB_ADDRESS")
	}
	if u, err := url.Parse(a); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("LAB_ADDRESS %q is not a lab's address", a)
	}
	return a, nil
}

func newWho(tokenFile, sessionFile string) (*who, error) {
	w := &who{agent: "lab/" + strings.TrimPrefix(client.Release, "v") + " (hardening probe)"}
	var err error
	switch {
	case tokenFile != "" && sessionFile != "":
		return nil, errors.New("--token-file or --session, not both")
	case tokenFile != "":
		if w.token, err = readSecret(tokenFile); err != nil {
			return nil, err
		}
		if w.base, err = address(); err != nil {
			return nil, err
		}
	case sessionFile != "":
		if b, e := os.ReadFile(sessionFile); e == nil && strings.TrimSpace(string(b)) == "" {
			w.session = "" // signed out: a browser with no session
		} else if w.session, err = readSecret(sessionFile); err != nil {
			return nil, err
		}
		if w.base, err = address(); err != nil {
			return nil, err
		}
		w.agent = browserAgent
	default:
		c, err := client.Load()
		if err != nil {
			return nil, err
		}
		w.base, w.token = strings.TrimRight(c.URL, "/"), c.Token
		p := labhttp.Policy{AllowPlainHTTP: c.AllowPlainHTTP, UserAgent: w.agent}
		if c.CAFile != "" {
			if p.CAPEM, err = labhttp.ReadCAFile(c.CAFile); err != nil {
				return nil, err
			}
		}
		if w.http, err = p.Client(0); err != nil {
			return nil, err
		}
		w.http.CheckRedirect = noRedirect
		return w, nil
	}
	w.http = &http.Client{CheckRedirect: noRedirect}
	return w, nil
}

// cookieName is the session cookie's name at the lab's address.
func (w *who) cookieName() string {
	if strings.HasPrefix(w.base, "https://") {
		return "__Host-lab_session"
	}
	return "lab_session"
}

func (w *who) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, w.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", w.agent)
	if w.token != "" {
		req.Header.Set("Authorization", "Bearer "+w.token)
	}
	if w.session != "" {
		req.AddCookie(&http.Cookie{Name: w.cookieName(), Value: w.session})
	}
	return req, nil
}

type headers []string

func (h *headers) String() string     { return strings.Join(*h, ", ") }
func (h *headers) Set(v string) error { *h = append(*h, v); return nil }

func sayAnswer(resp *http.Response, b []byte, outFile string) error {
	say("status", resp.StatusCode)
	say("said", plain(b))
	if loc := resp.Header.Get("Location"); loc != "" {
		say("location", plain([]byte(loc)))
	}
	if outFile != "" {
		return os.WriteFile(outFile, b, 0o600)
	}
	return nil
}

func askCommand(fs *flag.FlagSet, args []string) error {
	tokenFile := fs.String("token-file", "", "a file that holds a Lab token")
	sessionFile := fs.String("session", "", "a file that holds a browser's session")
	body := fs.String("json", "", "a JSON body")
	outFile := fs.String("out", "", "write the answer's body here")
	rows := fs.String("rows", "", "for a JSON array: one row= line for each object, these keys")
	timeout := fs.Duration("timeout", 3*time.Minute, "how long to wait for the answer")
	var hs headers
	fs.Var(&hs, "header", `a header, "Name: value"; repeat for more`)
	fs.Parse(args)
	if fs.NArg() != 2 {
		fs.Usage()
		return errors.New("ask: METHOD PATH")
	}
	w, err := newWho(*tokenFile, *sessionFile)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var rd io.Reader
	if *body != "" {
		rd = strings.NewReader(*body)
	}
	req, err := w.request(ctx, fs.Arg(0), fs.Arg(1), rd)
	if err != nil {
		return err
	}
	if *body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, h := range hs {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return fmt.Errorf("--header %q is not Name: value", h)
		}
		req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	resp, err := w.http.Do(req)
	if err != nil {
		say("status", 0)
		say("said", plain([]byte(err.Error())))
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err := sayAnswer(resp, b, *outFile); err != nil {
		return err
	}
	if *rows != "" {
		var list []map[string]any
		if json.Unmarshal(b, &list) != nil {
			return nil
		}
		keys := strings.Split(*rows, ",")
		for _, o := range list {
			vals := make([]string, len(keys))
			for i, k := range keys {
				if v := field(o, k); v != nil {
					vals[i] = plain([]byte(fmt.Sprint(v)))
				}
			}
			say("row", strings.Join(vals, "|"))
		}
	}
	return nil
}

// field is o's value at key, where a.b is b inside a; nil when it has none.
func field(o map[string]any, key string) any {
	var v any = o
	for _, k := range strings.Split(key, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func holdCommand(fs *flag.FlagSet, args []string) error {
	tokenFile := fs.String("token-file", "", "a file that holds a Lab token")
	sessionFile := fs.String("session", "", "a file that holds a browser's session")
	holdFor := fs.Float64("for", 60, "how long to hold it at most, in seconds")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("hold: PATH")
	}
	w, err := newWho(*tokenFile, *sessionFile)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*holdFor*float64(time.Second)))
	defer cancel()
	req, err := w.request(ctx, "GET", fs.Arg(0), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	start := time.Now()
	resp, err := w.http.Do(req)
	if err != nil {
		say("opened", 0)
		say("ended", "error")
		say("said", plain([]byte(err.Error())))
		return nil
	}
	defer resp.Body.Close()
	say("opened", resp.StatusCode)
	if f, ok := out.(*os.File); ok {
		f.Sync()
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		say("said", plain(b))
		say("ended", "refused")
		return nil
	}
	_, err = io.Copy(io.Discard, resp.Body)
	say("seconds", fmt.Sprintf("%.1f", time.Since(start).Seconds()))
	switch {
	case ctx.Err() != nil:
		say("ended", "timeout")
	case err != nil:
		say("ended", "error")
	default:
		say("ended", "closed")
	}
	return nil
}

func saveSession(path, value string) error {
	return os.WriteFile(path, []byte(value+"\n"), 0o600)
}

func signInCommand(fs *flag.FlagSet, args []string) error {
	sessionFile := fs.String("session", "", "where the session goes")
	as := fs.String("as", "", "the account id at the stand-in for GitHub")
	back := fs.String("return", "", "the return address sign-in is asked for")
	fs.Parse(args)
	if *sessionFile == "" || *as == "" || fs.NArg() != 0 {
		fs.Usage()
		return errors.New("sign-in: --session F --as ACCOUNT")
	}
	base, err := address()
	if err != nil {
		return err
	}
	hc := &http.Client{CheckRedirect: noRedirect, Timeout: time.Minute}
	get := func(u string, cookies []*http.Cookie) (*http.Response, error) {
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", browserAgent)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		resp, err := hc.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		return resp, err
	}
	start := base + "/sign-in"
	if *back != "" {
		start += "?return=" + url.QueryEscape(*back)
	}
	resp, err := get(start, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusFound {
		return fmt.Errorf("/sign-in answered %d", resp.StatusCode)
	}
	state := resp.Cookies()
	authorize, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		return err
	}
	q := authorize.Query()
	q.Set("as", *as)
	authorize.RawQuery = q.Encode()
	if resp, err = get(authorize.String(), nil); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusFound {
		return fmt.Errorf("the stand-in for GitHub answered %d: is account %s one it knows?", resp.StatusCode, *as)
	}
	if resp, err = get(resp.Header.Get("Location"), state); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusSeeOther {
		say("status", resp.StatusCode)
		return fmt.Errorf("the lab did not sign account %s in: %d", *as, resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if strings.HasSuffix(c.Name, "lab_session") && c.Value != "" {
			if err := saveSession(*sessionFile, c.Value); err != nil {
				return err
			}
			say("status", resp.StatusCode)
			say("landed", plain([]byte(resp.Header.Get("Location"))))
			return nil
		}
	}
	return errors.New("the lab set no session")
}

// browserTimeout is how long a browser's request, a page or a form, may
// take.
var browserTimeout = 2 * time.Minute

var csrfField = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func (w *who) fetch(path string) (*http.Response, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), browserTimeout)
	defer cancel()
	req, err := w.request(ctx, "GET", path, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp, b, nil
}

func formCommand(fs *flag.FlagSet, args []string) error {
	sessionFile := fs.String("session", "", "a file that holds a browser's session")
	page := fs.String("page", "/me", "the page whose CSRF token the form carries")
	origin := fs.String("origin", "", "the Origin to send, in place of the lab's own")
	noOrigin := fs.Bool("no-origin", false, "send no Origin")
	fetchSite := fs.String("fetch-site", "", "Sec-Fetch-Site to send")
	outFile := fs.String("out", "", "write the answer's body here")
	fs.Parse(args)
	if *sessionFile == "" || fs.NArg() < 1 {
		fs.Usage()
		return errors.New("form: --session F PATH [NAME=VALUE]...")
	}
	w, err := newWho("", *sessionFile)
	if err != nil {
		return err
	}
	form := url.Values{}
	if w.session != "" {
		_, b, err := w.fetch(*page)
		if err != nil {
			return err
		}
		m := csrfField.FindSubmatch(b)
		if m == nil {
			return fmt.Errorf("%s has no form for this session", *page)
		}
		form.Set("csrf_token", string(m[1]))
	}
	for _, kv := range fs.Args()[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("%q is not NAME=VALUE", kv)
		}
		form.Add(k, v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), browserTimeout)
	defer cancel()
	req, err := w.request(ctx, "POST", fs.Arg(0), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	switch {
	case *noOrigin:
	case *origin != "":
		req.Header.Set("Origin", *origin)
	default:
		req.Header.Set("Origin", w.base)
	}
	if *fetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", *fetchSite)
	}
	resp, err := w.http.Do(req)
	if err != nil {
		say("status", 0)
		say("said", plain([]byte(err.Error())))
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 400 {
		if m := regexp.MustCompile(`(?s)<main[^>]*>(.*?)</main>`).FindSubmatch(b); m != nil {
			say("said_page", plain([]byte(strings.Join(strings.Fields(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(string(m[1]), " ")), " "))))
		}
	}
	return sayAnswer(resp, b, *outFile)
}

func pageCommand(fs *flag.FlagSet, args []string) error {
	sessionFile := fs.String("session", "", "a file that holds a browser's session")
	find := fs.String("find", "", "a regular expression whose first group is said for each match")
	outFile := fs.String("out", "", "write the page here")
	fs.Parse(args)
	if *sessionFile == "" || fs.NArg() != 1 {
		fs.Usage()
		return errors.New("page: --session F PATH")
	}
	w, err := newWho("", *sessionFile)
	if err != nil {
		return err
	}
	resp, b, err := w.fetch(fs.Arg(0))
	if err != nil {
		return err
	}
	say("status", resp.StatusCode)
	if loc := resp.Header.Get("Location"); loc != "" {
		say("location", plain([]byte(loc)))
	}
	if *find != "" {
		re, err := regexp.Compile(*find)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		var found []string
		for _, m := range re.FindAllSubmatch(b, -1) {
			if len(m) > 1 && !seen[string(m[1])] {
				seen[string(m[1])] = true
				found = append(found, string(m[1]))
			}
		}
		for _, f := range found {
			say("found", plain([]byte(f)))
		}
	}
	if *outFile != "" {
		return os.WriteFile(*outFile, b, 0o600)
	}
	return nil
}

func listenCommand() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(os.Stdout)
	fmt.Fprintf(bw, "port=%d\n", ln.Addr().(*net.TCPAddr).Port)
	bw.Flush()
	n := 0
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		n++
		fmt.Fprintf(bw, "connection=%d\n", n)
		bw.Flush()
		go func() {
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			buf := make([]byte, 4096)
			c.Read(buf)
			if bytes.HasPrefix(buf, []byte("CONNECT ")) || bytes.Contains(buf[:16], []byte("HTTP")) {
				c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
			}
		}()
	}
}
