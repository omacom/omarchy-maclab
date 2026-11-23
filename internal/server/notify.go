package server

// Two kinds of message leave labd here. A Notification of the Maintainers'
// (a Lab-owned Mac needs hands, a job's result) goes to the hooks, the log
// and --notify-cmd on the mini: it never leaves the mini. A Notice goes to
// a Volunteer's phone through the notice server, on the Mac's own topic: the
// Mac's name, one of six fixed wordings and a link to the Mac on My Macs,
// with the title "Omarchy Mac Lab". Nothing else is ever sent to the notice
// server: no handle, no label, no log line, no kernel, no source, and never a
// request for a password or a command.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

type Notification struct {
	Device   string
	Title    string
	Body     string
	Priority string // urgent, high, default, low
	// Volunteer makes this a Notice: it is posted to MacTopic alone, never
	// to --notify-cmd, which is the Maintainers'. Only s.notice makes one.
	// A Volunteer's Mac without a topic gets none.
	MacTopic  string
	Volunteer bool
	// Link is the Mac on My Macs, which the Notice opens.
	Link string
	// Own is whether MacTopic is the Volunteer's own topic, which labd
	// checks before it sends; the lab's own topics are on the notice server
	// the owner configured.
	Own bool
}

// noticeTitle is the title of every Notice.
const noticeTitle = "Omarchy Mac Lab"

// noticeKind is one of the five fixed wordings.
type noticeKind int

const (
	noticeRestart noticeKind = iota // a restart is needed, with the physical steps
	noticeEnded                     // the Mac's enrollment was ended
	noticeJob                       // a job is starting or is done
	noticeTerms                     // the lab's terms changed
	noticeUpdate                    // an update is waiting
	noticeMacOS                     // an Attended Mac's macOS result is due
)

// noticeKinds are all of them, for the test that holds every wording to the
// rules.
var noticeKinds = []noticeKind{noticeRestart, noticeEnded, noticeJob, noticeTerms, noticeUpdate, noticeMacOS}

const (
	updateNotice = "An update is waiting for this Mac. Open My Macs."
	termsNotice  = "The lab's terms changed. This Mac is waiting for you: open My Macs."
	endedNotice  = "This Mac's enrollment in Omarchy Mac Lab was ended. Open My Macs."
	macOSNotice  = "The lab would like this Mac's macOS result. It waits a day for someone at the Mac: if that is you, open My Macs and say you are at it."
)

// restartSteps are the physical steps of the restart Notice, by the Mac's
// kind: what a person does with their hands, never a command.
var restartSteps = map[string]string{
	api.ModelDesktop: "It needs a restart: unplug its power cable for 10 seconds, then plug it back in; if it does not start, press the power button on the back once. Open My Macs.",
	api.ModelLaptop:  "It needs a restart: hold its power button (the Touch ID key, top right of the keyboard) for 10 seconds, until the screen stays black, then wait 5 seconds and press it once to turn it back on. Open My Macs.",
}

// jobWording is the job Notice: starting with its id and longest duration,
// or done with its id and how it ended (one of the lab's outcomes).
type jobWording struct {
	id      string
	minutes int    // starting: the longest it takes
	outcome string // done
}

// noticeText is the fixed wording of a Notice of kind k about the Mac d.
func noticeText(d api.Device, k noticeKind, job jobWording) string {
	var said string
	switch k {
	case noticeRestart:
		said = restartSteps[d.Model]
		if said == "" {
			said = restartSteps[api.ModelLaptop]
		}
	case noticeEnded:
		said = endedNotice
	case noticeJob:
		if job.outcome == "" {
			said = fmt.Sprintf("Job %s is starting. The Mac will reboot for it, and the job takes at most about %d min. Please do not reboot or shut it down until the done Notice. Open My Macs.", job.id, max(job.minutes, 1))
		} else {
			said = fmt.Sprintf("Job %s is done (%s). The lab is finished with this Mac for now: you can reboot it or use it. Open My Macs.", job.id, job.outcome)
		}
	case noticeTerms:
		said = termsNotice
	case noticeUpdate:
		said = updateNotice
	case noticeMacOS:
		said = macOSNotice
	}
	return d.Name + ": " + said
}

// notice is the Notice of kind k about the Volunteer's Mac d, to its topic.
func (s *Server) notice(d api.Device, k noticeKind, job jobWording) Notification {
	priority := "default"
	switch k {
	case noticeRestart:
		priority = "urgent"
	case noticeEnded:
		priority = "high"
	}
	return Notification{Device: d.Name, Volunteer: true, Title: noticeTitle, Body: noticeText(d, k, job), Priority: priority,
		MacTopic: s.macTopicURL(d.Enrollment), Own: d.NtfyOwnURL != "", Link: s.myMacsLink(d.Name)}
}

// myMacsLink is the address of the Mac called name on My Macs: at the Lab
// address, or the path alone when labd was given none.
func (s *Server) myMacsLink(name string) string {
	return s.myMacLink(strings.TrimSuffix(s.labAddress(), "/"), name)
}

// CheckNoticeServer checks labd's --ntfy: the notice server's address
// alone, scheme and host. A topic there is a secret, which no command line
// carries.
func CheckNoticeServer(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("--ntfy %q: give the notice server's address, like https://ntfy.sh", raw)
	}
	if strings.Trim(u.Path, "/") != "" {
		return fmt.Errorf("--ntfy names a topic (%s): give the notice server's address alone, like %s://%s. Each Volunteer's Mac has its own topic, which labd keeps; labd posts nothing to a topic of its own", u.Path, u.Scheme, u.Host)
	}
	return nil
}

// ntfyPriorities is how a Notice's priority reads to ntfy; default sends neither.
var ntfyPriorities = map[string]struct{ priority, tag string }{
	"urgent": {"urgent", "rotating_light"},
	"high":   {"high", "warning"},
	"low":    {"low", "information_source"},
}

func (s *Server) notify(n Notification) {
	if n.Volunteer {
		s.log.Printf("NOTICE %s: %s", n.Device, n.Body)
	} else {
		s.log.Printf("NOTIFY [%s] %s: %s", n.Priority, n.Title, strings.ReplaceAll(n.Body, "\n", " | "))
	}
	s.mu.Lock()
	for _, f := range s.notifyHooks {
		f(n)
	}
	s.mu.Unlock()
	if n.Volunteer {
		if n.MacTopic != "" {
			go s.postNotice(n)
		}
		return
	}
	if s.cfg.NotifyCmd != "" {
		go func() {
			cmd := exec.Command("sh", "-c", s.cfg.NotifyCmd)
			cmd.Env = append(os.Environ(), "MACLAB_TITLE="+n.Title, "MACLAB_BODY="+n.Body, "MACLAB_PRIORITY="+n.Priority, "MACLAB_DEVICE="+n.Device)
			if out, err := cmd.CombinedOutput(); err != nil {
				s.log.Printf("notify-cmd: %v: %s", err, out)
			}
		}()
	}
}

// postNotice sends a Notice once. A Volunteer's own topic is checked first,
// and the address labd connects to is judged again as it connects; a
// refused topic is kept on the Mac's record for My Macs, and the Notice is
// dropped. What the notice server refuses is logged and dropped: no cap,
// queue or retry.
func (s *Server) postNotice(n Notification) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	body := n.Body
	if n.Link != "" {
		body += "\n" + n.Link
	}
	req, err := http.NewRequestWithContext(ctx, "POST", n.MacTopic, strings.NewReader(body))
	if err != nil {
		s.log.Printf("notice for %s dropped: its topic is not an address", n.Device)
		return
	}
	req.Header.Set("Title", n.Title)
	if strings.HasPrefix(n.Link, "https://") || strings.HasPrefix(n.Link, "http://") {
		req.Header.Set("Click", n.Link)
	}
	if p, ok := ntfyPriorities[n.Priority]; ok {
		req.Header.Set("Priority", p.priority)
		req.Header.Set("Tags", p.tag)
	}
	client := s.noticeClient(false)
	if n.Own {
		if err := s.checkNoticeTopic(ctx, n.MacTopic); err != nil {
			s.topicRefused(n.Device, n.MacTopic, err)
			return
		}
		client = s.noticeClient(true)
	}
	resp, err := client.Do(req)
	if err != nil {
		var refused *topicRefusal
		if n.Own && errors.As(err, &refused) {
			s.topicRefused(n.Device, n.MacTopic, refused)
			return
		}
		s.log.Printf("notice for %s dropped: %s", n.Device, sendFailure(err))
		return
	}
	resp.Body.Close()
	switch {
	case n.Own && resp.StatusCode >= 300 && resp.StatusCode < 400:
		s.topicRefused(n.Device, n.MacTopic, &topicRefusal{"it answers with a redirect, which labd does not follow"})
	case resp.StatusCode >= 300:
		s.log.Printf("notice for %s dropped: the notice server answered %s", n.Device, resp.Status)
	case n.Own:
		s.topicRefused(n.Device, n.MacTopic, nil)
	}
}

// sendFailure is what kind of failure err is, in words that never carry
// the topic: the error itself names the whole URL, and a topic is its own
// secret.
func sendFailure(err error) string {
	var (
		cert    *tls.CertificateVerificationError
		unknown x509.UnknownAuthorityError
		host    x509.HostnameError
		invalid x509.CertificateInvalidError
		dns     *net.DNSError
		op      *net.OpError
		netErr  net.Error
	)
	switch {
	case errors.As(err, &cert), errors.As(err, &unknown), errors.As(err, &host), errors.As(err, &invalid):
		return "the notice server's certificate is not trusted"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "the notice server did not answer in time"
	case errors.As(err, &dns):
		return "the notice server's host does not resolve"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "the notice server refused the connection"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "the notice server closed the connection"
	case errors.As(err, &op):
		return "labd could not reach the notice server (" + op.Op + ")"
	}
	return "labd could not reach the notice server"
}

// noticeClient is the client a Notice goes out with. It follows no
// redirect and goes through no proxy. For a Volunteer's own topic (own) it
// connects only to a public address, judged on the address it actually
// connects to.
func (s *Server) noticeClient(own bool) *http.Client {
	t := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 10 * time.Second}
	if s.cfg.noticeTLS != nil {
		t.TLSClientConfig = s.cfg.noticeTLS.Clone()
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	if own {
		d.Control = func(network, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return &topicRefusal{"labd could not tell the address it reached: " + address}
			}
			if !s.publicAddr(ap) {
				return &topicRefusal{fmt.Sprintf("it is at %s, not a public address", ap.Addr().Unmap())}
			}
			return nil
		}
	}
	t.DialContext = d.DialContext
	return &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// topicRefusal is why labd will not send to a Volunteer's own topic.
type topicRefusal struct{ why string }

func (e *topicRefusal) Error() string { return e.why }

// checkNoticeTopic is the check of a Volunteer's own topic: an https address
// whose host is at public addresses only. postNotice judges again the
// address it connects to.
func (s *Server) checkNoticeTopic(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return &topicRefusal{"it is not an address"}
	case u.Scheme != "https":
		return &topicRefusal{"it is not an https address"}
	case u.Hostname() == "" || u.User != nil:
		return &topicRefusal{"it names no host, or carries a name and password"}
	}
	port := uint16(443)
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return &topicRefusal{"its port is not a port"}
		}
		port = uint16(n)
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if err != nil || len(addrs) == 0 {
		return &topicRefusal{"its host " + u.Hostname() + " does not resolve"}
	}
	for _, a := range addrs {
		if !s.publicAddr(netip.AddrPortFrom(a, port)) {
			return &topicRefusal{fmt.Sprintf("its host %s is at %s, not a public address", u.Hostname(), a.Unmap())}
		}
	}
	return nil
}

// notPublic are the ranges that are not the public internet beyond what
// netip names: this network, the tailnet's and carrier NAT's, the IETF's,
// the documentation and benchmarking ranges, the reserved block, NAT64,
// Teredo and 6to4.
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
}

// publicAddr is whether labd may send a Volunteer's Notice to ap.
func (s *Server) publicAddr(ap netip.AddrPort) bool {
	if s.cfg.noticeAllow != nil && s.cfg.noticeAllow(ap) {
		return true
	}
	return isPublic(ap.Addr())
}

func isPublic(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// topicRefused keeps why the Mac called name's own topic was refused, for
// My Macs, or clears it (why nil) once the topic passed.
func (s *Server) topicRefused(name, topic string, why error) {
	if why != nil {
		s.log.Printf("notice for %s dropped: its Volunteer's own topic is refused: %v", name, why)
	}
	v := s.dev(name)
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.d.NtfyOwnURL != topic {
		return // the Volunteer chose another meanwhile
	}
	said := ""
	if why != nil {
		said = why.Error()
	}
	if v.d.NoticeTopicRefused == said {
		return
	}
	v.d.NoticeTopicRefused = said
	v.saveLocked()
	v.publishLocked()
}

// checkReportedTopic checks the Volunteer's own topic a check-in carried.
func (s *Server) checkReportedTopic(name, topic string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.topicRefused(name, topic, s.checkNoticeTopic(ctx, topic))
}

// tellVolunteerJobStarting tells a Volunteer, on their Mac's own topic, that a
// job is about to take their Mac: its id and how long it can take at most.
// Only when the Volunteer asked (`notify --jobs on`); a Lab-owned Mac has no
// Volunteer to tell.
func (r *jobRun) tellVolunteerJobStarting() {
	d := r.v.snapshot()
	if !d.IsVolunteer() || !d.JobNotices {
		return
	}
	minutes := int(math.Ceil(r.expectedLength().Minutes()))
	r.s.notify(r.s.notice(d, noticeJob, jobWording{id: r.j.ID, minutes: minutes}))
	r.volunteerTold = true
}

// volunteerDoneNotice is the notice that ends the heads-up: how the job ended
// and that the Mac is free.
func (s *Server) volunteerDoneNotice(d api.Device, id, outcome string) Notification {
	return s.notice(d, noticeJob, jobWording{id: id, outcome: outcome})
}

// tellVolunteerJobDone sends the done notice after a start notice. While the
// Mac still needs hands or is being recovered it would not be true, so it waits
// (dev.owedDone) and goes when the Mac is back.
func (r *jobRun) tellVolunteerJobDone(outcome string) {
	if !r.volunteerTold {
		return
	}
	d := r.v.snapshot()
	n := r.s.volunteerDoneNotice(d, r.j.ID, outcome)
	if d.State == api.StateNeedsHands || d.State == api.StateRecovering {
		r.v.mu.Lock()
		r.v.owedDone = &n
		r.v.mu.Unlock()
		return
	}
	r.s.notify(n)
}

// sendOwedDoneLocked sends a done notice that waited for the Mac to be back.
func (v *dev) sendOwedDoneLocked() {
	if v.owedDone == nil || v.d.State == api.StateNeedsHands || v.d.State == api.StateRecovering || v.d.State == api.StateOffline {
		return
	}
	n := *v.owedDone
	v.owedDone = nil
	v.s.notify(n)
}

// tellVolunteerJobEndedByRestart closes the heads-up for a job labd lost by
// restarting while it held a Volunteer's Mac: the start notice went, and no
// job is left that could send the done one. A job that had not yet reached the
// Mac (queued, building) never sent a start notice.
func (s *Server) tellVolunteerJobEndedByRestart(j *api.Job, outcome string) {
	switch j.State {
	case api.JobStaging, api.JobBooting, api.JobTesting, api.JobCollecting, api.JobRestoring:
	default:
		return
	}
	v := s.devs[j.Spec.Device]
	if v == nil {
		return
	}
	d := v.snapshot()
	if !d.IsVolunteer() || !d.JobNotices {
		return
	}
	n := s.volunteerDoneNotice(d, j.ID, outcome)
	// labd keeps a Mac that needs hands as it was: the all-clear waits for
	// the Mac to be back, as tellVolunteerJobDone does.
	if d.State == api.StateNeedsHands || d.State == api.StateRecovering {
		v.mu.Lock()
		v.owedDone = &n
		v.mu.Unlock()
		return
	}
	s.notify(n)
}
