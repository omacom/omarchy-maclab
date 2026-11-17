package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/api"
)

// ntfyPost is one POST that reached a fake ntfy server.
type ntfyPost struct {
	path, title, body, priority, click string
}

// ntfyCatcher is a fake ntfy server recording every post to it.
type ntfyCatcher struct {
	*httptest.Server
	mu    sync.Mutex
	posts []ntfyPost
	// answer, when set, is what the server answers each post with.
	answer func(w http.ResponseWriter, r *http.Request)
}

func newNtfyCatcher(t *testing.T) *ntfyCatcher { return newCatcher(t, false) }

// newNtfyCatcherTLS is a fake notice server on https, for a Volunteer's
// own topic.
func newNtfyCatcherTLS(t *testing.T) *ntfyCatcher { return newCatcher(t, true) }

func newCatcher(t *testing.T, tls bool) *ntfyCatcher {
	c := &ntfyCatcher{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := new(strings.Builder)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		c.mu.Lock()
		c.posts = append(c.posts, ntfyPost{r.URL.Path, r.Header.Get("Title"), b.String(), r.Header.Get("Priority"), r.Header.Get("Click")})
		answer := c.answer
		c.mu.Unlock()
		if answer != nil {
			answer(w, r)
		}
	})
	if tls {
		c.Server = httptest.NewTLSServer(h)
	} else {
		c.Server = httptest.NewServer(h)
	}
	t.Cleanup(c.Close)
	return c
}

func (c *ntfyCatcher) answering(f func(w http.ResponseWriter, r *http.Request)) {
	c.mu.Lock()
	c.answer = f
	c.mu.Unlock()
}

// addr is where the fake listens.
func (c *ntfyCatcher) addr() netip.AddrPort {
	return netip.MustParseAddrPort(c.Listener.Addr().String())
}

// trusting has labd trust these fake https notice servers as if they were
// on the public internet: their certificates, and their exact addresses
// (nothing else on loopback).
func trusting(cs ...*ntfyCatcher) func(*Config) {
	return func(cfg *Config) {
		pool := x509.NewCertPool()
		allowed := map[netip.AddrPort]bool{}
		for _, c := range cs {
			pool.AddCert(c.Certificate())
			allowed[c.addr()] = true
		}
		cfg.noticeTLS = &tls.Config{RootCAs: pool}
		cfg.noticeAllow = func(ap netip.AddrPort) bool { return allowed[netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())] }
	}
}

func (c *ntfyCatcher) all() []ntfyPost {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ntfyPost(nil), c.posts...)
}

// at is the posts to one topic path.
func (c *ntfyCatcher) at(path string) []ntfyPost {
	var out []ntfyPost
	for _, p := range c.all() {
		if p.path == path {
			out = append(out, p)
		}
	}
	return out
}

// text is a post's title and body, lower-cased, for matching on.
func (p ntfyPost) text() string { return strings.ToLower(p.title + "\n" + p.body) }

// asksForHands is whether a post is a needs-hands notice: it names the power
// button steps.
func (p ntfyPost) asksForHands() bool {
	t := p.text()
	return strings.Contains(t, "hold") && strings.Contains(t, "10 s")
}

// asksForDiskPassword is whether a post is the "waiting for your disk
// password" notice.
func (p ntfyPost) asksForDiskPassword() bool {
	return strings.Contains(p.text(), "disk password")
}

// endedEnrollment is whether a post is the "enrollment ended" notice.
func (p ntfyPost) endedEnrollment() bool {
	t := p.text()
	return strings.Contains(t, "enrollment") && strings.Contains(t, "ended")
}

// noticeLab is a Volunteer lab on a fake clock (running at wall speed unless a
// test jumps it) whose labd has a fake notice server.
type noticeLab struct {
	*lab
	clock *agent.FakeClock
	ntfy  *ntfyCatcher
	cmds  string // the file --notify-cmd appends to
}

func newNoticeLab(t *testing.T, c labConfig) *noticeLab {
	t.Helper()
	n := &noticeLab{ntfy: newNtfyCatcher(t), cmds: notifyCmdLog(t)}
	n.clock = agent.NewFakeClock(time.Now())
	c.clock = n.clock
	prev := c.config
	c.config = func(cfg *Config) {
		if prev != nil {
			prev(cfg)
		}
		cfg.NtfyURL = n.ntfy.URL
		cfg.NotifyCmd = `printf '%s | %s | %s\n' "$MACLAB_DEVICE" "$MACLAB_TITLE" "$(printf %s "$MACLAB_BODY" | tr '\n' ' ')" >> ` + n.cmds
	}
	n.lab = newLabWith(t, c)
	t.Cleanup(pumpClock(n.clock))
	if c.volunteer {
		n.openWindowForNotices()
	}
	n.baseline()
	return n
}

// notifyCmdLog is the file --notify-cmd appends to. labd does not wait for the
// commands it starts, and one sent as the test ends is still running while the
// test's folders are removed: it made the file again in a folder being
// emptied, and the removal failed. So the file's folder is renamed first: a
// late command finds no folder to write in, and nothing is added to the one
// being removed.
func notifyCmdLog(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "live")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Rename(dir, filepath.Join(tmp, "gone")); err != nil {
			t.Error(err)
		}
	})
	return filepath.Join(dir, "notify-cmd.log")
}

// openWindowForNotices opens the Volunteer Mac's Availability window for the
// next 12 hours of lab time, as a Volunteer would with `open --until`: a
// Volunteer Mac with no window is closed. Until #26 lands, "open" not being a
// Volunteer command is tolerated; any other error fails the test.
func (n *noticeLab) openWindowForNotices() {
	n.t.Helper()
	until := n.clock.Now().Add(12 * time.Hour).Format("15:04")
	if out, err := n.volunteer("open", "--until", until); err != nil && !strings.Contains(err.Error(), `"open" is not a Volunteer command`) {
		n.t.Fatalf("open --until %s: %v\n%s", until, err, out)
	}
}

// cmdLines is what --notify-cmd was run with, one line per run.
func (n *noticeLab) cmdLines() []string {
	b, _ := os.ReadFile(n.cmds)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

var subscribeLine = regexp.MustCompile(`(?m)subscribe to:\s+(\S+)`)

// macTopic is the path of the Mac's own topic on the lab's ntfy server, as
// `status` tells the Volunteer.
func (n *noticeLab) macTopic() string {
	n.t.Helper()
	out, err := n.volunteer("status")
	if err != nil {
		n.t.Fatalf("status: %v", err)
	}
	m := subscribeLine.FindStringSubmatch(out)
	if m == nil || !strings.HasPrefix(m[1], n.ntfy.URL+"/") {
		n.t.Fatalf("status does not show the Mac's topic on the lab's ntfy server %s:\n%s", n.ntfy.URL, out)
	}
	return strings.TrimPrefix(m[1], n.ntfy.URL)
}

// hang submits a job whose Lab boot hangs for good: nothing on the Mac can
// bring it back, so only a human can. It returns once the Mac has left for
// that boot: until then labd is waiting for the Mac's answers, and a test
// that steps the clock while a slow one is on its way has labd count the Mac
// as lost and end the job before it hangs.
func (n *noticeLab) hang() string {
	n.t.Helper()
	id := n.submitJob(api.JobSpec{Kernel: n.artifact("krel=7.2-hang behavior=hang")})
	n.waitJob(id, func(j *api.Job) bool {
		for _, e := range j.Events {
			if strings.HasPrefix(e.Msg, "one-shot armed") {
				return true
			}
		}
		return false
	})
	return id
}

// step moves the clock on in steps of by until cond holds, giving labd time
// to look at each (keep by under the harness's 3s quietGrace: a bigger jump can
// land between a command and its result, which labd counts as lost), and
// returns how far past start it got. It gives up after max.
func (n *noticeLab) step(start time.Time, by, max time.Duration, cond func() bool) time.Duration {
	n.t.Helper()
	for n.clock.Now().Sub(start) < max {
		if cond() {
			return n.clock.Now().Sub(start)
		}
		n.clock.Advance(by)
		time.Sleep(20 * time.Millisecond)
	}
	n.t.Fatalf("not true after %s of lab time", max)
	return 0
}

func (n *noticeLab) state() api.DeviceState { return n.s.dev(n.name).snapshot().State }

// waitPosts waits up to wall time d for cond to hold of the posts to path.
func (n *noticeLab) waitPosts(path string, d time.Duration, cond func([]ntfyPost) bool) bool {
	deadline := time.Now().Add(d)
	for {
		if cond(n.ntfy.at(path)) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func anyPost(f func(ntfyPost) bool) func([]ntfyPost) bool {
	return func(ps []ntfyPost) bool {
		for _, p := range ps {
			if f(p) {
				return true
			}
		}
		return false
	}
}

// needsHandsAfter hangs a Lab boot and returns how far into the job the
// Volunteer was asked for hands (a needs-hands post on topic), with the
// check that nothing asked sooner.
func (n *noticeLab) needsHandsAfter(topic string) time.Duration {
	n.t.Helper()
	start := n.clock.Now()
	n.hang()
	return n.step(start, 2*time.Second, time.Hour, func() bool {
		return anyPost(ntfyPost.asksForHands)(n.ntfy.at(topic))
	})
}

// finish is the Volunteer power cycling their Mac; the job then ends.
func (n *noticeLab) finish() {
	n.t.Helper()
	before := n.waitDevice(func(d api.Device) bool { return d.State == api.StateNeedsHands })
	n.sim.PowerCycle()
	n.waitDevice(func(d api.Device) bool { return d.BootID != before.BootID && d.Kernel == agent.SimKnownGood })
}

// A job is at least its boot timeout, the time labd gives a Lab boot to come
// back by itself (twice that) and five minutes: its hard ceiling is no
// earlier. labd's boot timeout in the harness is 4 s.
const earliestCeiling = 4*time.Second + 2*4*time.Second + 5*time.Minute

func TestNeedsHandsOnAVolunteerMacOnlyAfterTheHardCeilingPlusABootTimeout(t *testing.T) {
	n := newNoticeLab(t, labConfig{volunteer: true})
	topic := n.macTopic()
	start := n.clock.Now()
	n.hang()
	var asked time.Duration
	asked = n.step(start, 2*time.Second, time.Hour, func() bool {
		if anyPost(ntfyPost.asksForHands)(n.ntfy.at(topic)) {
			return true
		}
		if n.state() == api.StateNeedsHands && n.clock.Now().Sub(start) < earliestCeiling+4*time.Second {
			t.Fatalf("the Mac was marked needs hands %s into the job, before the ceiling plus a boot timeout", n.clock.Now().Sub(start))
		}
		return false
	})
	if min := earliestCeiling + 4*time.Second; asked < min {
		t.Fatalf("asked for hands %s into the job: the hard ceiling is at least %s away, plus a 4s boot timeout", asked, min)
	}
	n.finish()
}

func TestNeedsHandsStateAndNoticeComeTogetherOnAVolunteerMac(t *testing.T) {
	n := newNoticeLab(t, labConfig{volunteer: true})
	topic := n.macTopic()
	n.needsHandsAfter(topic)
	d := n.waitDevice(func(d api.Device) bool { return d.State == api.StateNeedsHands })
	if d.Action == nil {
		t.Fatalf("needs hands with no action: %+v", d)
	}
	n.finish()
	// A Volunteer's Mac is told to its Volunteer, not to "Agents".
	if lines := n.agentsLines(); has(lines, "needs hands") {
		t.Errorf("Agents got a line about a Volunteer's Mac: %q", lines)
	}
}

func TestNeedsHandsNoticeGivesExactStepsHoldPowerTenSecondsThenPowerOn(t *testing.T) {
	n := newNoticeLab(t, labConfig{volunteer: true})
	topic := n.macTopic()
	n.needsHandsAfter(topic)
	var post ntfyPost
	for _, p := range n.ntfy.at(topic) {
		if p.asksForHands() {
			post = p
		}
	}
	t.Logf("notice: %q / %q", post.title, post.body)
	text := post.text()
	hold := strings.Index(text, "hold")
	ten := strings.Index(text, "10 s")
	if hold < 0 || ten < 0 || !strings.Contains(text, "power") {
		t.Fatalf("no 'hold the power button for 10 seconds' step: %q", post.body)
	}
	if on := strings.LastIndex(text, "on"); !(strings.Contains(text, "turn it back on") || strings.Contains(text, "power on")) || on < ten {
		t.Fatalf("no 'power on' step after the hold: %q", post.body)
	}
	if post.priority != "urgent" {
		t.Fatalf("needs hands is %q priority on ntfy, want urgent", post.priority)
	}
	n.finish()
}

func TestNeedsHandsOnAVolunteerMacGoesToItsOwnTopicAlone(t *testing.T) {
	n := newNoticeLab(t, labConfig{volunteer: true})
	topic := n.macTopic()
	n.needsHandsAfter(topic)
	n.finish()
	time.Sleep(300 * time.Millisecond)
	if got := len(n.ntfy.at(topic)); got != 1 {
		t.Fatalf("%d posts to the Mac's topic, want exactly the one needs-hands: %+v", got, n.ntfy.at(topic))
	}
	for _, p := range n.ntfy.all() {
		if p.path != topic {
			t.Fatalf("a post to a topic that is not the Mac's: %+v", p)
		}
	}
}

func TestMacTopicCarriesOnlyNeedsHandsNotJobResultsOrBackOrAsleepNotices(t *testing.T) {
	n := newNoticeLab(t, labConfig{volunteer: true})
	topic := n.macTopic()
	// a passing job, a failing test, a lost-contact job, a hang with its
	// "back" notice: routine traffic for Maintainers, none for the Volunteer
	n.expect(n.jobAPI(n.submitJob(api.JobSpec{Kernel: n.artifact("krel=7.2-ok behavior=ok")}), 60*time.Second), api.OutcomePass)
	n.waitDevice(func(d api.Device) bool { return d.State == api.StateReady })
	n.expect(n.jobAPI(n.submitJob(api.JobSpec{Kernel: n.artifact("krel=7.2-nn behavior=no-network")}), 60*time.Second), api.OutcomeLostContact)
	n.waitDevice(func(d api.Device) bool { return d.State == api.StateReady })
	n.needsHandsAfter(topic)
	n.finish()
	time.Sleep(500 * time.Millisecond)
	posts := n.ntfy.at(topic)
	if len(posts) != 1 || !posts[0].asksForHands() {
		t.Fatalf("the Mac's topic got %+v, want only the one needs-hands", posts)
	}
	// and Maintainers still get the job results, on the mini alone
	if !n.waitCmd(5*time.Second, func(l string) bool { return strings.Contains(l, "job ") }) {
		t.Fatalf("no job result reached --notify-cmd: %q", n.cmdLines())
	}
}

func TestNotifyTopicSendsNeedsHandsToTheVolunteersOwnTopic(t *testing.T) {
	own := newNtfyCatcherTLS(t)
	n := newNoticeLab(t, labConfig{volunteer: true, config: trusting(own)})
	generated := n.macTopic()
	if out, err := n.volunteer("notify", "--topic", own.URL+"/my-own-topic"); err != nil {
		t.Fatalf("notify --topic: %v\n%s", err, out)
	}
	time.Sleep(time.Second) // it syncs to labd at the next check-in
	start := n.clock.Now()
	n.hang()
	n.step(start, 2*time.Second, time.Hour, func() bool { return len(own.at("/my-own-topic")) > 0 })
	n.finish()
	if p := own.at("/my-own-topic"); len(p) != 1 || !p[0].asksForHands() {
		t.Fatalf("the Volunteer's own topic got %+v, want the one needs-hands", p)
	}
	if got := n.ntfy.at(generated); len(got) != 0 {
		t.Fatalf("the lab's generated topic still got %+v after notify --topic", got)
	}
}

func TestNotifyTopicRefusesABadURLAndKeepsTheMacsTopic(t *testing.T) {
	n := newNoticeLab(t, labConfig{volunteer: true})
	topic := n.macTopic()
	for _, bad := range []string{"", "not a url", "ftp://ntfy.example.org/t", "http://", "http://ntfy.example.org", "/just/a/path"} {
		args := []string{"notify", "--topic"}
		if bad != "" {
			args = append(args, bad)
		}
		if out, err := n.volunteer(args...); err == nil {
			t.Fatalf("notify --topic %q was accepted: %s", bad, out)
		}
	}
	if _, err := n.volunteer("notify", "--topic"); err == nil {
		t.Fatal("notify --topic with no URL was accepted")
	}
	time.Sleep(time.Second)
	if again := n.macTopic(); again != topic {
		t.Fatalf("a refused notify --topic changed the Mac's topic from %s to %s", topic, again)
	}
	n.needsHandsAfter(topic)
	n.finish()
}

func TestNotifyCmdIsRunForMaintainersNoticesOnAVolunteerMacButNotNeedsHands(t *testing.T) {
	n := newNoticeLab(t, labConfig{volunteer: true})
	topic := n.macTopic()
	n.expect(n.jobAPI(n.submitJob(api.JobSpec{Kernel: n.artifact("krel=7.2-ok behavior=ok")}), 60*time.Second), api.OutcomePass)
	n.needsHandsAfter(topic)
	n.finish()
	// --notify-cmd runs in the background, a shell each time: the hung job's
	// notice and "is back" both come after the needs hands, and once they
	// are in, a needs hands run for the Volunteer would be too.
	var lines []string
	var jobResult, back bool
	for deadline := time.Now().Add(20 * time.Second); !(jobResult && back) && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		lines = n.cmdLines()
		for _, l := range lines {
			jobResult = jobResult || strings.Contains(l, "job ")
			back = back || strings.Contains(l, " is back")
		}
	}
	for _, l := range lines {
		low := strings.ToLower(l)
		if strings.Contains(low, "hold") && strings.Contains(low, "10 s") || strings.Contains(low, "power cycle") {
			t.Fatalf("--notify-cmd was run for the Volunteer's needs hands: %q", l)
		}
	}
	if !jobResult || !back {
		t.Fatalf("--notify-cmd no longer runs for Maintainers' job notices on a Volunteer Mac: %q", lines)
	}
}

func TestMaintainersNoticesAboutAVolunteerMacStayOnTheMiniAndNeverReachTheNoticeServer(t *testing.T) {
	n := newNoticeLab(t, labConfig{volunteer: true})
	topic := n.macTopic()
	id := n.submitJob(api.JobSpec{Kernel: n.artifact("krel=7.2-nn behavior=no-network")})
	n.lostBoot("7.2-nn")
	n.clock.Advance(2 * 4 * time.Second)
	n.expect(n.jobAPI(id, 60*time.Second), api.OutcomeLostContact)
	if !n.waitCmd(5*time.Second, func(l string) bool { return strings.Contains(l, id) && strings.Contains(l, api.OutcomeLostContact) }) {
		t.Fatalf("no failed-job notice for %s on --notify-cmd: %q", id, n.cmdLines())
	}
	for _, p := range n.ntfy.all() {
		if p.path != topic || strings.Contains(p.text(), api.OutcomeLostContact) {
			t.Fatalf("a Maintainers' notice reached the notice server: %+v", p)
		}
	}
}

func TestLabOwnedMacNeedsHandsGoesToNotifyCmdAndNothingToTheNoticeServer(t *testing.T) {
	n := newNoticeLab(t, labConfig{})
	start := n.clock.Now()
	n.hang()
	// The ladder runs on the lab's clock: moved by the test, not left to the
	// harness's wall-speed pump, which load slows.
	n.step(start, 2*time.Second, time.Hour, func() bool { return n.state() == api.StateNeedsHands })
	if !n.waitCmd(10*time.Second, func(l string) bool {
		low := strings.ToLower(l)
		return strings.Contains(low, "hold") && strings.Contains(low, "10 s")
	}) {
		t.Fatalf("--notify-cmd not run for a Lab-owned Mac's needs hands: %q", n.cmdLines())
	}
	time.Sleep(300 * time.Millisecond)
	if p := n.ntfy.all(); len(p) != 0 {
		t.Fatalf("a Lab-owned Mac posted to the notice server: %+v", p)
	}
	n.sim.PowerCycle()
}

func TestLabOwnedMacIsAskedForHandsAfterTheBootTimeoutNotTheCeiling(t *testing.T) {
	n := newNoticeLab(t, labConfig{})
	start := n.clock.Now()
	n.hang()
	asked := n.step(start, 2*time.Second, time.Hour, func() bool { return n.lab.notified(func(m Notification) bool { return m.Action() }) })
	if asked >= earliestCeiling {
		t.Fatalf("a Lab-owned Mac waited %s for hands: its ladder is unchanged and does not wait for the ceiling", asked)
	}
	n.sim.PowerCycle()
}

// waitCmd waits up to wall time d for a line --notify-cmd was run with.
func (n *noticeLab) waitCmd(d time.Duration, cond func(string) bool) bool {
	for deadline := time.Now().Add(d); ; time.Sleep(50 * time.Millisecond) {
		for _, l := range n.cmdLines() {
			if cond(l) {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// notified is whether a notification labd made so far is one cond holds of.
func (l *lab) notified(cond func(Notification) bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.notes {
		if cond(m) {
			return true
		}
	}
	return false
}

// Action is whether a notification asks for hands.
func (m Notification) Action() bool {
	low := strings.ToLower(m.Body)
	return strings.Contains(low, "hold") && strings.Contains(low, "10 s")
}

// ---- the five wordings, and what a Notice never carries ----

// noticeWordings are the only bodies a Notice may have, after the Mac's
// name: the five fixed wordings (spec 13), the restart one in its laptop
// and desktop steps, the job one starting and done.
var noticeWordings = []*regexp.Regexp{
	regexp.MustCompile(`^It needs a restart: hold its power button \(the Touch ID key, top right of the keyboard\) for 10 seconds, until the screen stays black, then wait 5 seconds and press it once to turn it back on\. Open My Macs\.$`),
	regexp.MustCompile(`^It needs a restart: unplug its power cable for 10 seconds, then plug it back in; if it does not start, press the power button on the back once\. Open My Macs\.$`),
	regexp.MustCompile(`^This Mac's enrollment in Omarchy Mac Lab was ended\. Open My Macs\.$`),
	regexp.MustCompile(`^Job [a-z0-9-]+ is starting\. The Mac will reboot for it, and the job takes at most about \d+ min\. Please do not reboot or shut it down until the done Notice\. Open My Macs\.$`),
	regexp.MustCompile(`^Job [a-z0-9-]+ is done \([a-z_-]+\)\. The lab is finished with this Mac for now: you can reboot it or use it\. Open My Macs\.$`),
	regexp.MustCompile(`^The lab's terms changed\. This Mac is waiting for you: open My Macs\.$`),
	regexp.MustCompile(`^An update is waiting for this Mac\. Open My Macs\.$`),
}

// asksForAPasswordOrACommand are words a Notice never says (spec 14,
// decision 8): it never asks for a password or for a command to be run.
var asksForAPasswordOrACommand = []string{"password", "passphrase", "sudo", "lab-agent", "command", "terminal", "`", "$ ", " run ", "type ", "enter "}

// checkNotice holds one post to the rules: the title, the Mac's name, one
// of the fixed wordings, the link to the Mac on My Macs, and none of what
// must never be in it.
func checkNotice(t *testing.T, p ntfyPost, mac string, never ...string) {
	t.Helper()
	if p.title != noticeTitle {
		t.Errorf("a Notice titled %q", p.title)
	}
	first, link, _ := strings.Cut(p.body, "\n")
	said, ok := strings.CutPrefix(first, mac+": ")
	if !ok {
		t.Errorf("a Notice that does not start with the Mac's name: %q", p.body)
	}
	known := false
	for _, w := range noticeWordings {
		known = known || w.MatchString(said)
	}
	if !known {
		t.Errorf("a Notice in no fixed wording: %q", said)
	}
	if !strings.HasSuffix(link, "/my-macs/"+mac) || strings.Contains(link, "\n") {
		t.Errorf("a Notice whose link is %q, not the Mac on My Macs", link)
	}
	low := strings.ToLower(p.title + "\n" + p.body)
	for _, w := range append(asksForAPasswordOrACommand, never...) {
		if w != "" && strings.Contains(low, strings.ToLower(w)) {
			t.Errorf("a Notice says %q: %q", w, p.body)
		}
	}
}

func TestOnlyTheFiveWordingsExistAndNoneAsksForAPasswordOrACommand(t *testing.T) {
	if len(noticeKinds) != 5 {
		t.Fatalf("%d kinds of Notice, want the five", len(noticeKinds))
	}
	pub, _ := ParsePublicURL("https://lab.example.org")
	s := &Server{cfg: Config{PublicURL: pub}}
	seen := map[string]bool{}
	for _, model := range []string{api.ModelLaptop, api.ModelDesktop} {
		d := api.Device{Name: "vol-air", Label: "Ada's MacBook Air, the shelf", Model: model, VolunteerName: "Ada Lovelace", Contact: "ada@example.org",
			Kernel: "7.2.0-rc1-asahi", LinkedLogin: "ada"}
		d.Kind = api.KindVolunteer
		for _, k := range noticeKinds {
			for _, job := range []jobWording{{id: "j-7f3a", minutes: 37}, {id: "j-7f3a", outcome: api.OutcomePass}} {
				n := s.notice(d, k, job)
				if !n.Volunteer || n.Link != "https://lab.example.org/my-macs/vol-air" {
					t.Errorf("kind %d: %+v", k, n)
				}
				checkNotice(t, ntfyPost{title: n.Title, body: n.Body + "\n" + n.Link}, "vol-air", "Ada", "shelf", "asahi", "7.2", "ada@", "linus", "@")
				seen[strings.TrimPrefix(n.Body, "vol-air: ")] = true
			}
		}
	}
	// two restarts, one ending, a job starting and done, terms, an update
	if len(seen) != 7 {
		t.Errorf("%d bodies, want 7: %v", len(seen), seen)
	}
}

// No Notice is made but by s.notice: nothing else in labd marks a message
// for a Volunteer's phone.
func TestOnlyTheNoticeMakerMakesANotice(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "notify.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if regexp.MustCompile(`Notification\{[^}]*(Volunteer|MacTopic)|\.MacTopic\s*=|n\.Volunteer\s*=`).Match(b) {
			t.Errorf("%s makes a Notice of its own: use s.notice", f)
		}
	}
}

// Every Notice a Volunteer's Mac gets over a job, a restart and its ending
// carries the Mac's name, a fixed wording and the link, and never a handle,
// the Mac's label, a log line, a kernel or a source.
func TestANoticeCarriesNeverAHandleALabelALogLineAKernelOrASource(t *testing.T) {
	n, grace := leavingLab(t, labConfig{})
	topic := n.macTopic()
	label := "Ada's MacBook on the shelf"
	v := n.s.dev(n.name)
	v.mu.Lock()
	v.d.Label = label
	v.saveLocked()
	v.mu.Unlock()
	if out, err := n.volunteer("notify", "--jobs", "on"); err != nil {
		t.Fatalf("notify --jobs on: %v\n%s", err, out)
	}
	n.waitDevice(func(d api.Device) bool { return d.JobNotices })
	src := "https://github.com/labtest/linux-aurora"
	id := n.submitJob(api.JobSpec{Kernel: n.artifact("krel=7.2-ok behavior=ok")})
	n.expect(n.jobAPI(id, 60*time.Second), api.OutcomePass)
	n.waitDevice(func(d api.Device) bool { return d.State == api.StateReady })
	n.needsHandsAfter(topic)
	lastLog := n.waitDevice(func(d api.Device) bool { return d.State == api.StateNeedsHands }).Action.LastLog
	n.finish()
	n.leavingMustEnd(grace, n.name)
	if !n.waitPosts(topic, 10*time.Second, anyPost(ntfyPost.endedEnrollment)) {
		t.Fatalf("no ending Notice: %+v", n.ntfy.at(topic))
	}
	posts := n.ntfy.all()
	if len(posts) < 4 {
		t.Fatalf("%d Notices, want a job starting and done, a restart and an ending: %+v", len(posts), posts)
	}
	never := []string{harnessMaintainer + " ", "@" + harnessMaintainer, leavingMaintainer, label, "shelf", "Ada", "7.2-ok", "7.2-hang", "krel", src, "github"}
	if lastLog != "" {
		never = append(never, lastLog)
	}
	for _, p := range posts {
		if p.path != topic {
			t.Errorf("a post to %s, not the Mac's topic", p.path)
		}
		checkNotice(t, p, n.name, never...)
	}
}

// ---- a Volunteer's own topic ----

// ownTopicLab is a Volunteer lab with fake https notice servers labd trusts
// as public, the first of which the Volunteer names.
func ownTopicLab(t *testing.T, more ...*ntfyCatcher) (*noticeLab, *ntfyCatcher) {
	t.Helper()
	own := newNtfyCatcherTLS(t)
	n := newNoticeLab(t, labConfig{volunteer: true, config: trusting(append([]*ntfyCatcher{own}, more...)...)})
	return n, own
}

// chooseTopic is the Volunteer's `notify --topic`, once labd has it.
func (n *noticeLab) chooseTopic(topic string) {
	n.t.Helper()
	if out, err := n.volunteer("notify", "--topic", topic); err != nil {
		n.t.Fatalf("notify --topic %s: %v\n%s", topic, err, out)
	}
	n.waitDevice(func(d api.Device) bool { return d.NtfyOwnURL == topic })
}

// refusedTopic waits for labd to refuse the Volunteer's own topic, saying why.
func (n *noticeLab) refusedTopic(why string) {
	n.t.Helper()
	d := n.waitDevice(func(d api.Device) bool { return d.NoticeTopicRefused != "" })
	if !strings.Contains(d.NoticeTopicRefused, why) {
		n.t.Fatalf("the topic is refused for %q, want %q", d.NoticeTopicRefused, why)
	}
}

// tell sends the Mac a Notice now.
func (n *noticeLab) tell() {
	n.s.notify(n.s.notice(n.s.dev(n.name).snapshot(), noticeTerms, jobWording{}))
	time.Sleep(300 * time.Millisecond)
}

func TestAVolunteersOwnTopicMustBeHTTPSAtAPublicAddressWithNoRedirect(t *testing.T) {
	target := newNtfyCatcherTLS(t)
	n, own := ownTopicLab(t, target)
	generated := n.macTopic()

	// Plain http is refused when the Mac reports it, and at the send.
	plain := newNtfyCatcher(t)
	n.chooseTopic(plain.URL + "/mine")
	n.refusedTopic("not an https address")
	n.tell()
	if len(plain.all()) != 0 || len(n.ntfy.at(generated)) != 0 {
		t.Fatalf("a Notice went to a refused topic or fell back to the lab's: %+v %+v", plain.all(), n.ntfy.all())
	}

	// https at a public address (as the fake stands in for) passes, and the
	// Notice goes.
	n.chooseTopic(own.URL + "/mine")
	n.waitDevice(func(d api.Device) bool { return d.NoticeTopicRefused == "" })
	n.tell()
	if p := own.at("/mine"); len(p) != 1 {
		t.Fatalf("the Volunteer's own topic got %+v", p)
	}

	// A private address: loopback that is not one of the trusted fakes.
	private := newNtfyCatcherTLS(t)
	n.chooseTopic(private.URL + "/mine")
	n.refusedTopic("not a public address")
	n.tell()
	if len(private.all()) != 0 {
		t.Fatalf("a Notice went to a private address: %+v", private.all())
	}

	// A redirect is not followed, even to a public address, and refuses the
	// topic at the send.
	own.answering(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/elsewhere", http.StatusTemporaryRedirect)
	})
	n.chooseTopic(own.URL + "/moved")
	n.waitDevice(func(d api.Device) bool { return d.NoticeTopicRefused == "" })
	n.tell()
	n.refusedTopic("redirect")
	if len(target.all()) != 0 {
		t.Fatalf("labd followed a redirect: %+v", target.all())
	}

	// The Mac's own status says so too.
	out, err := n.volunteer("status")
	if err != nil || !strings.Contains(out, "refused by the lab") {
		t.Errorf("status: %v\n%s", err, out)
	}
}

// The address labd judges is the one it connects to: a client for a
// Volunteer's own topic will not connect to a private address, whatever a
// check before it said.
func TestTheNoticeClientConnectsToNoPrivateAddress(t *testing.T) {
	private := newNtfyCatcherTLS(t)
	s := &Server{}
	trusting()(&s.cfg)
	s.cfg.noticeTLS.RootCAs.AddCert(private.Certificate())
	_, err := s.noticeClient(true).Post(private.URL+"/t", "text/plain", strings.NewReader("x"))
	var refused *topicRefusal
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "not a public address") || len(private.all()) != 0 {
		t.Fatalf("the client reached a private address: %v, %+v", err, private.all())
	}
}

func TestPublicAddresses(t *testing.T) {
	for addr, public := range map[string]bool{
		"8.8.8.8": true, "1.1.1.1": true, "2606:4700::1111": true,
		"10.0.0.1": false, "172.16.0.1": false, "192.168.1.20": false, "127.0.0.1": false, "169.254.1.1": false,
		"100.100.1.2": false, "0.0.0.0": false, "224.0.0.1": false, "255.255.255.255": false, "198.18.0.1": false,
		"::1": false, "fe80::1": false, "fd7a:115c:a1e0::1": false, "::ffff:192.168.0.1": false, "::ffff:8.8.8.8": true,
		"64:ff9b::a00:1": false, "2002:c0a8:1::1": false, "::": false,
	} {
		if got := isPublic(netip.MustParseAddr(addr)); got != public {
			t.Errorf("%s public: %v, want %v", addr, got, public)
		}
	}
}

// ntfy.sh's limit: a Notice the notice server refuses is logged and dropped,
// with no cap, queue or retry.
func TestANoticeTheNoticeServerRefusesIsLoggedAndDropped(t *testing.T) {
	n := newNoticeLab(t, labConfig{volunteer: true})
	topic := n.macTopic()
	n.ntfy.answering(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "daily quota reached", http.StatusTooManyRequests)
	})
	n.tell()
	time.Sleep(time.Second)
	if got := n.ntfy.at(topic); len(got) != 1 {
		t.Fatalf("%d posts of one refused Notice, want 1 and no retry", len(got))
	}
	if !strings.Contains(n.logs.String(), "notice for "+n.name+" dropped: the notice server answered 429") {
		t.Errorf("the refusal is not in labd's log:\n%s", n.logs.String())
	}
	n.ntfy.answering(nil)
	n.tell()
	if got := n.ntfy.at(topic); len(got) != 2 {
		t.Fatalf("the next Notice after a refused one: %d posts", len(got))
	}
}

func TestTheNoticeServerIsAnAddressWithNoTopic(t *testing.T) {
	for raw, ok := range map[string]bool{"https://ntfy.sh": true, "https://ntfy.sh/": true, "http://127.0.0.1:8080": true,
		"https://ntfy.sh/secret-topic": false, "ntfy.sh": false, "https://u:p@ntfy.sh": false, "https://ntfy.sh?x=1": false} {
		if err := CheckNoticeServer(raw); (err == nil) != ok {
			t.Errorf("--ntfy %s: %v", raw, err)
		}
	}
}

// A topic is its only secret: a send that fails before any answer (here a
// certificate labd does not trust) logs the Mac and what went wrong, never
// the topic.
func TestASendThatFailsLogsNoTopic(t *testing.T) {
	bad := newNtfyCatcherTLS(t)
	var logs strings.Builder
	s := &Server{log: log.New(&logs, "", 0)}
	trusting()(&s.cfg)
	s.cfg.noticeAllow = func(netip.AddrPort) bool { return true }
	for _, own := range []bool{true, false} {
		logs.Reset()
		topic := bad.URL + "/s3cret-topic-name"
		s.postNotice(Notification{Device: "mac-1", Title: "t", Body: "b", MacTopic: topic, Volunteer: true, Own: own})
		got := logs.String()
		if strings.Contains(got, "s3cret") || strings.Contains(got, bad.Listener.Addr().String()) {
			t.Errorf("own %v: the log names the topic:\n%s", own, got)
		}
		if !strings.Contains(got, "notice for mac-1 dropped") || !strings.Contains(got, "certificate") {
			t.Errorf("own %v: the log does not say which Mac and why:\n%s", own, got)
		}
	}
	logs.Reset()
	s.postNotice(Notification{Device: "mac-1", MacTopic: "https://ntfy.example/s3cret\x7f", Volunteer: true})
	if got := logs.String(); strings.Contains(got, "s3cret") || !strings.Contains(got, "notice for mac-1 dropped") {
		t.Errorf("a topic that is no address: %s", got)
	}
}

// Each Notice has its own transport: one that kept its connection open
// would keep a socket and its goroutines per Notice.
func TestANoticeKeepsNoConnectionOpen(t *testing.T) {
	s := &Server{}
	for _, own := range []bool{true, false} {
		if tr, ok := s.noticeClient(own).Transport.(*http.Transport); !ok || !tr.DisableKeepAlives {
			t.Errorf("own %v: the notice transport keeps connections alive", own)
		}
	}
}

func TestTeredoIsNotPublic(t *testing.T) {
	if isPublic(netip.MustParseAddr("2001:0:4136:e378:8000:63bf:3fff:fdd2")) {
		t.Error("a Teredo address is taken as public")
	}
}
