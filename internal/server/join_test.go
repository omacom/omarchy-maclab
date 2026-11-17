package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/web"
)

// joinLab is a labd at https://chonkbase.net/maclab with a lab-agent to hand
// out, driven by requests straight to its Handler, and its log.
type joinLab struct {
	t    *testing.T
	s    *Server
	h    http.Handler
	logs *strings.Builder
	mu   sync.Mutex
}

func newJoinLab(t *testing.T) *joinLab {
	t.Helper()
	agentBin := filepath.Join(t.TempDir(), "lab-agent-linux-arm64")
	os.WriteFile(agentBin, []byte("\x7fELF lab-agent for the test"), 0o755)
	pub, _ := ParsePublicURL("https://chonkbase.net/maclab")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	j := &joinLab{t: t, logs: &strings.Builder{}}
	s, err := newWith("ada", "lab_adm_secret", ctx, Config{DataDir: t.TempDir(),
		PublicURL: pub, AgentBinary: agentBin}, log.New(lockedWriter{j}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	j.s, j.h = s, s.Handler()
	return j
}

type lockedWriter struct{ j *joinLab }

func (w lockedWriter) Write(b []byte) (int, error) {
	w.j.mu.Lock()
	defer w.j.mu.Unlock()
	return w.j.logs.Write(b)
}

func (j *joinLab) do(method, path, bearer, body string) (int, string) {
	r := httptest.NewRequest(method, "http://chonkbase.net/maclab"+path, strings.NewReader(body))
	r.RemoteAddr = viaProxy
	r = r.WithContext(onTunnel(r.Context()))
	r.Header.Set("X-Forwarded-Proto", "https")
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	j.h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// invite has a Maintainer make an enrollment token for name, as lab enroll does.
func (j *joinLab) invite(name string) api.EnrollTokenResponse {
	j.t.Helper()
	code, body := j.do("POST", "/api/enroll-tokens", "lab_adm_secret", `{"name":"`+name+`"}`)
	var t api.EnrollTokenResponse
	if json.Unmarshal([]byte(body), &t); code != 200 || t.Token == "" {
		j.t.Fatalf("invite %s: %d %s", name, code, body)
	}
	return t
}

func (j *joinLab) enroll(token, key, terms string) (int, api.EnrollResponse) {
	b, _ := json.Marshal(api.EnrollRequest{Token: token, ActivationKey: key, TermsSHA256: terms})
	code, body := j.do("POST", "/api/agent/enroll", "", string(b))
	var e api.EnrollResponse
	json.Unmarshal([]byte(body), &e)
	return code, e
}

const keyA, keyB = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// The token is good for one activation. The Mac that used it may enroll again
// with its own activation key until its first check-in (the answer never
// reached it); a copy of the token on another Mac is worth nothing.
func TestEnrollmentTokenActivatesOneMacOnce(t *testing.T) {
	j := newJoinLab(t)
	tok := j.invite("fresh").Token
	first, a := j.enroll(tok, keyA, termsSum())
	if first != 200 || a.DeviceID == "" {
		t.Fatalf("first activation: %d", first)
	}
	for name, key := range map[string]string{"another Mac's key": keyB, "no key (an older lab-agent)": ""} {
		if code, _ := j.enroll(tok, key, termsSum()); code != 403 {
			t.Errorf("the used token with %s: %d, want 403", name, code)
		}
	}
	code, again := j.enroll(tok, keyA, termsSum())
	if code != 200 || again.DeviceID != a.DeviceID || again.Secret == a.Secret {
		t.Fatalf("the same activation again: %d %+v (first %+v)", code, again, a)
	}
	// The first secret was replaced; the new one checks in, which spends the token.
	ci, _ := json.Marshal(api.Checkin{BootID: "b1", Kernel: "7.2"})
	if code, _ := j.do("POST", "/api/agent/checkin", a.Secret, string(ci)); code != 401 {
		t.Errorf("the replaced secret checks in: %d", code)
	}
	if code, body := j.do("POST", "/api/agent/checkin", again.Secret, string(ci)); code != 200 {
		t.Fatalf("check-in: %d %s", code, body)
	}
	if code, _ := j.enroll(tok, keyA, termsSum()); code != 403 {
		t.Errorf("the same activation after the first check-in: %d, want 403", code)
	}
	// A token for a name taken by a Mac that never checked in is refused up
	// front, with what to do.
	j.invite("pending")
	if code, _ := j.enroll(j.invite("pending2").Token, keyA, termsSum()); code != 200 {
		t.Fatal("pending2")
	}
	if code, body := j.do("POST", "/api/enroll-tokens", "lab_adm_secret", `{"name":"pending2"}`); code != 409 || !strings.Contains(body, "lab forget pending2") {
		t.Errorf("a new token for a Mac that never checked in: %d %s", code, body)
	}
}

// Two Macs racing with one token: exactly one gets it.
func TestOneOfTwoRacingEnrollmentsWins(t *testing.T) {
	j := newJoinLab(t)
	tok := j.invite("race").Token
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = j.enroll(tok, strings.Repeat(string(rune('c'+i)), 64), termsSum())
		}()
	}
	wg.Wait()
	won := 0
	for _, c := range codes {
		if c == 200 {
			won++
		} else if c != 403 && c != 409 {
			t.Errorf("a losing enrollment got %d", c)
		}
	}
	if won != 1 {
		t.Errorf("%d enrollments won: %v", won, codes)
	}
}

// labd takes only an enrollment whose owner accepted its current terms, and a
// refused one leaves the token unused.
func TestEnrollmentNeedsTheCurrentTerms(t *testing.T) {
	j := newJoinLab(t)
	tok := j.invite("terms").Token
	for _, bad := range []string{"", strings.Repeat("0", 64), strings.ToUpper(termsSum())} {
		if code, _ := j.enroll(tok, keyA, bad); code != 409 {
			t.Errorf("terms %q: %d, want 409", bad, code)
		}
	}
	code, e := j.enroll(tok, keyA, termsSum())
	if code != 200 {
		t.Fatalf("with the terms: %d", code)
	}
	_, body := j.do("GET", "/api/devices/terms", "lab_adm_secret", "")
	var d api.Device
	json.Unmarshal([]byte(body), &d)
	if d.ID != e.DeviceID || d.TermsSHA256 != termsSum() || d.TermsAccepted.IsZero() {
		t.Errorf("the Mac's record of the terms: %+v", d)
	}
	// The terms are the text shipped with the Lab release, the one /terms
	// serves: its sha256 is the version a Mac accepts.
	if text, sum := j.s.terms(); text != web.Terms().Body || sum != web.Terms().SHA256 || len(sum) != 64 || !strings.HasPrefix(text, "OMARCHY MAC LAB: LENDING A MAC\n") {
		t.Errorf("the terms a Mac accepts are not the shipped text: %s", sum)
	}
}

// The join routes answer to an unexpired enrollment token in the
// Authorization header, and to nothing else: not a Lab token, not a
// device secret, not a token in the URL.
func TestJoinRoutesTakeOnlyAnEnrollmentTokenInTheHeader(t *testing.T) {
	j := newJoinLab(t)
	tok := j.invite("joiner").Token
	code, body := j.do("GET", "/api/join/terms", tok, "")
	var terms api.JoinTerms
	if json.Unmarshal([]byte(body), &terms); code != 200 || terms.SHA256 != termsSum() || terms.Text != web.Terms().Body || terms.Date != web.Terms().Date || terms.Date == "" {
		t.Fatalf("terms: %d %s", code, body)
	}
	if code, body := j.do("GET", "/api/join/lab-agent", tok, ""); code != 200 || !strings.HasPrefix(body, "\x7fELF") {
		t.Fatalf("lab-agent: %d", code)
	}
	_, e := j.enroll(j.invite("enrolled").Token, keyA, termsSum())
	for name, bearer := range map[string]string{"no token": "", "a Maintainer's token": "lab_adm_secret", "a device secret": e.Secret, "a made-up token": "enr_00000000000000000000000000000000"} {
		for _, p := range []string{"/api/join/terms", "/api/join/lab-agent"} {
			if code, _ := j.do("GET", p, bearer, ""); code != 401 {
				t.Errorf("%s on %s: %d, want 401", name, p, code)
			}
		}
	}
	if code, _ := j.do("GET", "/api/join/terms?token="+tok, "", ""); code != 401 {
		t.Errorf("a token in the URL: %d, want 401", code)
	}
	// The enrollment token reaches nothing a Maintainer or a Mac does.
	for _, p := range []string{"/api/devices", "/api/jobs", "/api/agent/enrollment"} {
		if code, _ := j.do("GET", p, tok, ""); code != 401 {
			t.Errorf("an enrollment token on %s: %d, want 401", p, code)
		}
	}
}

// A Maintainer revokes an invite: unused, it stops working, and so do the
// join routes.
func TestRevokedInviteIsRefused(t *testing.T) {
	j := newJoinLab(t)
	tok := j.invite("revoked").Token
	if code, body := j.do("DELETE", "/api/enroll-tokens/revoked", "lab_adm_secret", ""); code != 200 || !strings.Contains(body, `"revoked":1`) {
		t.Fatalf("revoke: %d %s", code, body)
	}
	if code, _ := j.enroll(tok, keyA, termsSum()); code != 403 {
		t.Errorf("enrolling with a revoked token: %d", code)
	}
	if code, _ := j.do("GET", "/api/join/terms", tok, ""); code != 401 {
		t.Errorf("terms with a revoked token: %d", code)
	}
	if code, _ := j.do("DELETE", "/api/enroll-tokens/revoked", tok, ""); code != 401 {
		t.Errorf("an enrollment token revoking: %d", code)
	}
}

// An enrollment is the one body anyone may send before labd knows who they
// are: one that trickles in is cut off at its deadline, not held open.
func TestASlowEnrollmentBodyIsCutOff(t *testing.T) {
	old := enrollBodyTimeout
	enrollBodyTimeout = 300 * time.Millisecond
	defer func() { enrollBodyTimeout = old }()
	j := newJoinLab(t)
	srv := httptest.NewServer(j.h)
	defer srv.Close()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	fmt.Fprintf(c, "POST /maclab/api/agent/enroll HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{", srv.Listener.Addr())
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	b, _ := io.ReadAll(c)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the slow body held the connection %s", took)
	}
	if len(b) > 0 && !strings.Contains(string(b), " 400 ") && !strings.Contains(string(b), " 408 ") {
		t.Errorf("answer to a slow body: %q", b)
	}
}

// A Join command refused at its test for the cap or the name is not bound to
// the Mac that asked: setup forgets its key on a refusal, and the same line
// run again once the cap is raised or the name is free enrolls that Mac.
func TestAJoinCommandRefusedAtItsTestRunsAgainOnceTheCapIsRaised(t *testing.T) {
	l, _ := lendLab(t)
	ravi := l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "edith", "as", "invite")
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	edith, grace := l.signedIn(edithID), l.signedIn(graceID)

	_, token := edith.makeJoin("edith-6")
	for i := 1; i <= lendCap; i++ {
		l.addMac(fmt.Sprintf("edith-%d", i), edithID)
	}
	if w := l.joinCheck(token, keyA); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Cap reached") {
		t.Fatalf("at the cap: %d %s", w.Code, w.Body)
	}
	ravi.did(fmt.Sprintf("/people/%d/cap", edithID), "macs", "7")
	if w := l.joinCheck(token, keyB); w.Code != http.StatusOK {
		t.Fatalf("the same Join command after the cap was raised: %d %s", w.Code, w.Body)
	}
	if w := l.enrollWith(token, keyB); w.Code != http.StatusOK {
		t.Fatalf("enrolling after the cap was raised: %d %s", w.Code, w.Body)
	}

	_, token = grace.makeJoin("taken")
	l.addMac("taken", 0)
	if w := l.joinCheck(token, keyA); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Name taken") {
		t.Fatalf("a name taken: %d %s", w.Code, w.Body)
	}
	// The Mac called taken leaves, which frees its name.
	l.s.mu.Lock()
	delete(l.s.devs, "taken")
	l.s.mu.Unlock()
	if w := l.joinCheck(token, keyB); w.Code != http.StatusOK {
		t.Fatalf("the same Join command once the name is free: %d %s", w.Code, w.Body)
	}
}
