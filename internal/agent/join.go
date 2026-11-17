package agent

// Joining the lab with a Join command, as `lab-agent setup` runs it on the
// Mac: the pre-checks, which read the Mac and reach no lab; the terms, which
// the Mac's owner accepts by typing yes; the check that binds the Join
// command's token to this Mac; leaving another lab; the enrollment; and the
// one question for the weekly Availability window. Nothing on the Mac
// changes, and nothing is posted to the lab, before the terms are accepted.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/labhttp"
)

// Activation is what an enrollment carries besides its token: the key that
// binds the token to this Mac, and the terms its owner accepted.
type Activation struct {
	Key         string
	TermsSHA256 string
}

// FetchTerms reads labd's participation terms with the enrollment token,
// and checks that the text is the one its sha256 names.
func FetchTerms(ctx context.Context, server, token string, p labhttp.Policy) (*api.JoinTerms, error) {
	if err := labhttp.CheckServer(server, p.AllowPlainHTTP); err != nil {
		return nil, err
	}
	c, err := p.Client(0)
	if err != nil {
		return nil, err
	}
	var t api.JoinTerms
	if err := getJSON(ctx, c, strings.TrimRight(server, "/")+"/api/join/terms", token, &t); err != nil {
		return nil, fmt.Errorf("labd's participation terms: %w", err)
	}
	sum := sha256.Sum256([]byte(t.Text))
	if t.Text == "" || hex.EncodeToString(sum[:]) != t.SHA256 {
		return nil, errors.New("labd's participation terms don't match their sha256")
	}
	return &t, nil
}

// errTermsDeclined is the owner not accepting the terms; setup stops.
var errTermsDeclined = errors.New("the terms were not accepted, so nothing was changed and nothing was sent to the lab")

// terminal is where setup asks the Mac's owner its questions: leaving
// another lab, the terms, the Availability window. One reader serves them
// all, so that no answer is lost in the buffer of the question before it.
type terminal struct {
	in  *bufio.Reader
	out io.Writer
}

func newTerminal(rw io.ReadWriter) *terminal {
	if rw == nil {
		return nil
	}
	return &terminal{in: bufio.NewReader(rw), out: rw}
}

// ask shows prompt and reads one line; ok is false when the terminal closed
// with no answer.
func (t *terminal) ask(prompt string) (answer string, ok bool) {
	fmt.Fprint(t.out, prompt)
	line, err := t.in.ReadString('\n')
	if err != nil && line == "" {
		return "", false
	}
	return strings.TrimSpace(line), true
}

// acceptTerms shows t to the Mac's owner on tty and returns their acceptance:
// they must type yes. accept, when set, is acceptance given beforehand, for
// a run without a terminal: it must be the sha256 of these very terms. With
// neither, nothing is accepted.
func acceptTerms(t *api.JoinTerms, server, accept string, tty *terminal) error {
	if accept != "" {
		if !strings.EqualFold(accept, t.SHA256) {
			return fmt.Errorf("--accept-terms %s is not the lab's current terms (%s): read them again at %s/terms", accept, t.SHA256, strings.TrimRight(server, "/"))
		}
		return nil
	}
	if tty == nil {
		return fmt.Errorf("accepting the lab's terms needs a terminal: run setup in one, or pass --accept-terms %s after reading them", t.SHA256)
	}
	version := t.SHA256
	if t.Date != "" {
		version = t.Date + ", " + t.SHA256
	}
	answer, _ := tty.ask(fmt.Sprintf("\n%s\n(terms %s)\n\nType yes to accept these terms and lend this Mac to the lab at %s; anything else stops: ", strings.TrimRight(t.Text, "\n"), version, server))
	if answer != "yes" {
		return errTermsDeclined
	}
	return nil
}

// openTTY is the controlling terminal, nil without one. Setup reads the
// owner's answer there, not on stdin, which may carry the token.
func openTTY() io.ReadWriteCloser {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil
	}
	return f
}

// activationFile is, under the work directory, the activation key of an
// enrollment in progress, kept until setup has saved the Mac's config: setup
// run again after an interruption enrolls with the same key, which labd
// accepts as the same activation.
const activationFile = "activation.json"

type activation struct {
	TokenSHA256 string `json:"token_sha256"` // which token it is for
	Key         string `json:"key"`
}

// activationKey is the key for enrolling with token: the one an earlier,
// interrupted setup kept for the same token, else a new one, kept before it
// is used.
func activationKey(workDir, token string) (string, error) {
	h := sha256.Sum256([]byte(token))
	th := hex.EncodeToString(h[:])
	p := filepath.Join(workDir, activationFile)
	if b, err := os.ReadFile(p); err == nil {
		var a activation
		if json.Unmarshal(b, &a) == nil && a.TokenSHA256 == th && len(a.Key) >= 32 {
			return a.Key, nil
		}
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return "", err
	}
	a := activation{TokenSHA256: th, Key: hex.EncodeToString(k)}
	b, _ := json.Marshal(a)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", err
	}
	return a.Key, os.Rename(tmp, p)
}

// activationDone forgets the activation key once the Mac is set up.
func activationDone(workDir string) {
	os.Remove(filepath.Join(workDir, activationFile))
}

// NewActivationKey is a key for an enrollment that never runs again: a
// simulated Mac's, or a test's.
func NewActivationKey() string {
	k := make([]byte, 32)
	rand.Read(k)
	return hex.EncodeToString(k)
}

// ReadToken reads an enrollment token from r, the first line: setup
// --token-stdin, so that the token is in no command line.
func ReadToken(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, 4096)).ReadString('\n')
	tok := strings.TrimSpace(line)
	if tok == "" {
		if err == nil {
			err = errors.New("empty")
		}
		return "", fmt.Errorf("no enrollment token on stdin: %w", err)
	}
	return tok, nil
}

// resuming is whether workDir holds the activation key of a setup with this
// very token that never finished: the same line run again, not a Mac that is
// in the lab already.
func resuming(workDir, token string) bool {
	if token == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(workDir, activationFile))
	if err != nil {
		return false
	}
	h := sha256.Sum256([]byte(token))
	var a activation
	return json.Unmarshal(b, &a) == nil && a.TokenSHA256 == hex.EncodeToString(h[:]) && len(a.Key) >= 32
}

// omarchyPackages are the packages one of which an Omarchy system has
// installed: omarchy or omarchy-mac on a converged image, omarchy-dev on the
// omarchy-mx-mac fork, omarchy-mac-keyring on a legacy omarchy-mac install.
var omarchyPackages = []string{"omarchy", "omarchy-mac", "omarchy-dev", "omarchy-mac-keyring"}

// notOmarchy says why the system under root is not Omarchy, "" when it is.
// The rule is omarchy-m-test's (cli/omarchy_m_test/system.py, detect): one
// of omarchyPackages is installed, or, for a legacy install made from a
// checkout, Omarchy's own omarchy-version is there. The test tool asks
// pacman and the PATH; the Join command reads the same facts from files,
// pacman's records of installed packages (one folder each, named
// <package>-<version>-<release>) and the checkout in the home of whoever ran
// it, because root has no such PATH and the pre-checks run nothing.
func notOmarchy(root, home string) string {
	local := filepath.Join(root, "var/lib/pacman/local")
	ents, err := os.ReadDir(local)
	for _, e := range ents {
		f := strings.Split(e.Name(), "-")
		if e.IsDir() && len(f) > 2 && slices.Contains(omarchyPackages, strings.Join(f[:len(f)-2], "-")) {
			return ""
		}
	}
	checkouts := []string{filepath.Join(root, "usr/share/omarchy")}
	if home != "" {
		checkouts = append(checkouts, filepath.Join(home, ".local/share/omarchy"))
	}
	for _, c := range checkouts {
		if st, err := os.Stat(filepath.Join(c, "bin/omarchy-version")); err == nil && st.Mode().IsRegular() {
			return ""
		}
	}
	if err != nil {
		return "it has no pacman records of installed packages, and no Omarchy checkout"
	}
	return "none of the packages " + strings.Join(omarchyPackages, ", ") + " is installed, and there is no Omarchy checkout"
}

// sameLab is whether two addresses name one lab.
func sameLab(a, b string) bool {
	norm := func(s string) string {
		u, err := url.Parse(strings.TrimSpace(s))
		if err != nil {
			return strings.TrimRight(s, "/")
		}
		return strings.ToLower(u.Scheme+"://"+u.Host) + strings.TrimRight(u.Path, "/")
	}
	return norm(a) == norm(b)
}

// prechecked is what the pre-checks made of a Mac: why it cannot join, what
// its owner should know, and the other lab it is in, to leave on a yes.
type prechecked struct {
	Stops    []string
	Warnings []string
	Other    *Config
}

// precheck judges a Mac before the terms are shown, before the Join
// command's token is used and before anything changes. The first two rows
// stop alone: on a system that is not Omarchy on a Mac, or without root, the
// rest was not read or means nothing.
func precheck(f JoinFacts, server string) prechecked {
	const tests = ": the lab tests Omarchy on Apple Silicon Macs"
	apple := slices.ContainsFunc(f.Compatible, func(c string) bool { return strings.HasPrefix(c, "apple,") })
	switch {
	case f.OS != "linux":
		return prechecked{Stops: []string{"this system is " + f.OS + ", not Linux" + tests}}
	case !apple:
		return prechecked{Stops: []string{"this is not an Apple Silicon Mac (its device tree names no Apple machine)" + tests}}
	case f.NotOmarchy != "":
		return prechecked{Stops: []string{"this system is not Omarchy (" + f.NotOmarchy + ")" + tests}}
	case f.EUID != 0:
		return prechecked{Stops: []string{"setup needs root: run the Join command as it is, and it asks for sudo itself"}}
	case f.SudoUID == "" || f.SudoUID == "0":
		return prechecked{Stops: []string{"run the Join command as yourself, not as root: it asks for sudo itself, and the Mac is lent by the person who runs it"}}
	}
	var p prechecked
	stop := func(format string, a ...any) { p.Stops = append(p.Stops, fmt.Sprintf(format, a...)) }
	base := strings.TrimRight(server, "/")
	if in := f.Enrolled; in != nil && in.Secret != "" {
		switch {
		case sameLab(in.Server, server) && !f.Resuming:
			stop("this Mac is already in this lab, as %s: My Macs shows it (%s/my-macs)", in.Name, base)
		case sameLab(in.Server, server):
		case !in.Enrollment.IsVolunteer():
			stop("this Mac is Lab-owned in another lab, %s, as %s, and cannot leave it by itself: ask that lab's Maintainer to end its enrollment, then run the Join command again", in.Server, in.Name)
		default:
			p.Other = in
		}
	}
	if f.Installed != "" && !api.VersionAtLeast(Version, f.Installed) {
		stop("lab-agent %s is installed on this Mac, and this one is %s, older: an older lab-agent is never installed over a newer one. Ask the lab's Maintainers to bring the lab's lab-agent up to date", f.Installed, Version)
	}
	if tag := jobTag(f.Cmdline); tag != "" {
		stop("this Mac is running a lab kernel (job %s): restart into your own kernel first", tag)
	}
	if f.Passphrase != "" {
		stop("the disk waits for a passphrase at every start (%s), and the lab restarts the Mac with nobody at it. Two ways out: reinstall Omarchy without encryption; "+
			"or, on an install that is for the lab only, turn on lab-only-autounlock (scripts/lab-only-autounlock in the lab's repository): whoever holds the Mac can then read that disk", f.Passphrase)
	}
	for _, why := range f.Bootloader {
		stop("the bootloader cannot restart this Mac unattended: %s", why)
	}
	p.Stops = append(p.Stops, f.Missing...)
	if f.Space != "" {
		stop("%s", f.Space)
	}
	if f.Snapshot != nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("this system cannot be snapshotted (%v): no Installed runs on this Mac", f.Snapshot))
	}
	if f.OnBattery {
		p.Warnings = append(p.Warnings, "this Mac is on battery: jobs start only on AC power")
	}
	return p
}

// askLeaveOther names the other lab this Mac is in and asks, once, whether
// to leave it. It contacts nobody.
func askLeaveOther(tty *terminal, other *Config, server string) error {
	if tty == nil {
		return fmt.Errorf("this Mac is in another lab, %s, as %s. Leaving it is asked on a terminal: run the Join command in one, or leave that lab first with `sudo lab-agent leave`. Nothing was changed", other.Server, other.Name)
	}
	answer, _ := tty.ask(fmt.Sprintf("\nThis Mac is in another lab: %s, as %s. A Mac is in one lab at a time.\nLeave it and join the lab at %s? Leaving happens once you have accepted the terms below. [y/N] ", other.Server, other.Name, server))
	if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
		return fmt.Errorf("this Mac is still in the lab at %s: nothing was changed, and neither lab was told anything", other.Server)
	}
	return nil
}

// leaveOtherLab leaves the lab cfg names in an order no failure spoils: that
// lab is told first, with the Mac's device secret there, and only once it
// has heard is anything removed from the Mac.
func leaveOtherLab(ctx context.Context, cfg *Config, remove func() error) error {
	rec := &left{Server: cfg.Server, ServerCA: cfg.ServerCA, AllowPlainHTTP: cfg.AllowPlainHTTP, Secret: cfg.Secret}
	if !rec.tellLabd(ctx) {
		return fmt.Errorf("the lab at %s did not answer, so this Mac is still in it, as %s, and was not changed. Run the Join command again when it answers; if that lab is gone for good, leave it first with `sudo lab-agent leave`", cfg.Server, cfg.Name)
	}
	return remove()
}

// checkJoin has the lab test the Join command's token and bind it to this
// Mac's activation key. What would refuse the enrollment is said here.
func checkJoin(ctx context.Context, server, token, key string, p labhttp.Policy) (*api.JoinCheck, error) {
	c, err := p.Client(0)
	if err != nil {
		return nil, err
	}
	var check api.JoinCheck
	req, _ := json.Marshal(api.JoinCheckRequest{ActivationKey: key})
	r, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(server, "/")+"/api/join/check", strings.NewReader(string(req)))
	r.Header.Set("Content-Type", "application/json")
	return &check, doJSON(c, r, token, &check)
}

// joinRefused words a lab's refusal of a Join command's token.
func joinRefused(err error, server string) error {
	var se *statusError
	if errors.As(err, &se) && se.Code == http.StatusUnauthorized {
		return fmt.Errorf("this Join command has expired or was used: make a new one on Lend a Mac (%s/lend). Nothing was changed", strings.TrimRight(server, "/"))
	}
	return fmt.Errorf("the lab did not take this Mac: %w. Nothing was changed", err)
}

// join is the Join command on the Mac, up to and including the enrollment.
// f is what the pre-checks read.
func join(ctx context.Context, o SetupOptions, m SetupMac, f JoinFacts, policy labhttp.Policy, tty *terminal) (*Config, error) {
	say := o.Log
	pre := precheck(f, o.Server)
	for _, w := range pre.Warnings {
		say("warning: %s", w)
	}
	if len(pre.Stops) > 0 {
		return nil, fmt.Errorf("this Mac cannot join the lab yet:\n  - %s\nNothing was changed, and the lab was told nothing", strings.Join(pre.Stops, "\n  - "))
	}
	if pre.Other != nil {
		if err := askLeaveOther(tty, pre.Other, o.Server); err != nil {
			return nil, err
		}
	}
	terms, err := FetchTerms(ctx, o.Server, o.Token, policy)
	if err != nil {
		return nil, joinRefused(err, o.Server)
	}
	if err := acceptTerms(terms, o.Server, o.AcceptTerms, tty); err != nil {
		return nil, err
	}
	say("terms %s accepted", short(terms.SHA256))

	// From here the lab hears of the Mac. The key is kept before it is used,
	// so that a setup cut off at any point runs again as the same Mac.
	_, work := m.Home()
	hadWork, hadKey := exists(work), exists(filepath.Join(work, activationFile))
	key, err := activationKey(work, o.Token)
	if err != nil {
		return nil, err
	}
	check, err := checkJoin(ctx, o.Server, o.Token, key, policy)
	if err != nil {
		// Refused by the lab: the Mac is left as it was found. With no
		// answer the key stays, since the lab may have bound it.
		var se *statusError
		if errors.As(err, &se) && se.Code >= 400 && se.Code < 500 {
			if !hadKey {
				os.Remove(filepath.Join(work, activationFile))
			}
			if !hadWork {
				os.Remove(work)
			}
		}
		return nil, joinRefused(err, o.Server)
	}
	if check.TermsSHA256 != terms.SHA256 {
		return nil, errors.New("the lab's terms changed while you read them: run the Join command again. Nothing was changed")
	}
	if check.Kind == api.KindVolunteer {
		if err := refuseOnVolunteer(o.Autologin, o.StayAwake); err != nil {
			return nil, err
		}
	}
	if pre.Other != nil {
		if err := m.LeaveOther(ctx, pre.Other); err != nil {
			return nil, err
		}
		say("left the lab at %s, which knew this Mac as %s", pre.Other.Server, pre.Other.Name)
	}
	// A Mac that left a lab while that lab could not be reached: setup is
	// about to drop the record lab-agent would have told it from.
	if rec, _ := readLeft(work); rec != nil && !rec.Told {
		if rec.tellLabd(ctx) {
			say("told the lab at %s that this Mac, %s there, left it", rec.Server, rec.Name)
		} else {
			say("the lab at %s has still not heard that this Mac, %s there, left it: end that enrollment from its side", rec.Server, rec.Name)
		}
	}
	facts := m.Facts()
	if facts.AgentVersion == "" {
		facts.AgentVersion, facts.LabRelease = Version, LabRelease
	}
	resp, err := EnrollWith(ctx, o.Server, o.Token, facts, policy, Activation{Key: key, TermsSHA256: terms.SHA256})
	if err != nil {
		return nil, fmt.Errorf("enroll: %w", err)
	}
	cfg := &Config{Server: o.Server, DeviceID: resp.DeviceID, Name: resp.Name, Secret: resp.Secret, Enrollment: resp.AgentEnrollment}
	say("enrolled as %s: %s", resp.Name, kindName(resp.Enrollment))
	if err := arrive(work, cfg, time.Now()); err != nil {
		return nil, err
	}
	if cfg.Enrollment.IsVolunteer() {
		say("this Mac arrives at Qualification, and publishing is off: `sudo lab-agent access full` and `sudo lab-agent publish on` change that, here only")
	}
	return cfg, nil
}

// arrive puts what an earlier enrollment left in the work directory in the
// state a Mac arrives in. A Volunteer's Mac is at Qualification with
// publishing off, whatever it had; its window is kept. The Audit log of the
// earlier enrollment is set aside, still the owner's to read: lab-agent
// copies the Audit log to the lab, and no history follows a Mac.
func arrive(work string, cfg *Config, now time.Time) error {
	if cfg.Enrollment.IsVolunteer() {
		if err := os.Remove(filepath.Join(work, accessFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	old := filepath.Join(work, auditFile)
	if err := os.Rename(old, old+".before-"+now.UTC().Format("20060102T150405Z")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// sayWindow ends the Join of a Volunteer's Mac: it asks nothing about the
// weekly Availability window, which its Volunteer sets on My Macs. A Mac
// with none takes nothing from the lab; one it had is kept.
func sayWindow(cfg *Config, say func(string, ...any)) {
	myMacs := strings.TrimRight(cfg.Server, "/") + "/my-macs"
	if s, _, err := loadWindow(cfg); err == nil && len(s.Rules) > 0 {
		say("this Mac keeps the Availability window it had: %s (`sudo lab-agent window show`)", s.Schedule())
		say("Change when the lab may use this Mac on My Macs: %s", myMacs)
		return
	}
	say("This Mac takes nothing from the lab until it has an Availability window. Set when the lab may use this Mac on My Macs: %s", myMacs)
}
