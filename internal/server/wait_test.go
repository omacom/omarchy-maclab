package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/api"
)

// waiting starts a GET as ada and gives its answer when it comes.
func waiting(s *Server, target string) <-chan *httptest.ResponseRecorder {
	got := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest("GET", target, nil)
		req.RemoteAddr = direct
		req.Host = loopback
		req.Header.Set("Authorization", "Bearer lab_adm_secret")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		got <- rec
	}()
	return got
}

// advanceUntil moves the clock on in half seconds until an answer comes or
// the clock has moved by limit, and says how far it moved.
func advanceUntil(clock *agent.FakeClock, limit time.Duration, got <-chan *httptest.ResponseRecorder) (time.Duration, *httptest.ResponseRecorder) {
	var moved time.Duration
	for moved < limit {
		clock.Advance(500 * time.Millisecond)
		moved += 500 * time.Millisecond
		select {
		case rec := <-got:
			return moved, rec
		case <-time.After(20 * time.Millisecond):
		}
	}
	return moved, nil
}

// One job wait lasts 50 s at most, whatever the caller asks for.
func TestAJobWaitEndsBy50s(t *testing.T) {
	s, clock := heldServer(t)
	s.store.saveJob(&api.Job{ID: "j-slow", Spec: api.JobSpec{Device: "m1"}, State: api.JobTesting, Created: s.now()})
	for _, timeout := range []string{"3600", "0", ""} {
		got := waiting(s, "/api/jobs/j-slow/wait?timeout="+timeout)
		if moved, rec := advanceUntil(clock, 49*time.Second, got); rec != nil {
			t.Fatalf("timeout=%q: the wait ended after %s: %d %s", timeout, moved, rec.Code, rec.Body)
		}
		moved, rec := advanceUntil(clock, 30*time.Second, got)
		if rec == nil {
			t.Fatalf("timeout=%q: the wait was still held %s after 50 s", timeout, moved-time.Second)
		}
		var j api.Job
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &j) != nil || j.ID != "j-slow" || j.State != api.JobTesting {
			t.Fatalf("timeout=%q: the wait answered %d %s", timeout, rec.Code, rec.Body)
		}
	}

	// A shorter wait is kept.
	got := waiting(s, "/api/jobs/j-slow/wait?timeout=5")
	if moved, rec := advanceUntil(clock, 4*time.Second, got); rec != nil {
		t.Fatalf("timeout=5: the wait ended after %s", moved)
	}
	if _, rec := advanceUntil(clock, 10*time.Second, got); rec == nil {
		t.Fatal("timeout=5: the wait was still held at 14 s")
	}
}

// A wait that names how many events its caller has seen answers as soon as
// the job has more, so lab wait shows them as they happen.
func TestAJobWaitAnswersWhenTheJobSaysMore(t *testing.T) {
	s, clock := heldServer(t)
	j := &api.Job{ID: "j-chatty", Spec: api.JobSpec{Device: "m1"}, State: api.JobTesting, Created: s.now(), Events: []api.JobEvent{{Msg: "queued"}}}
	s.store.saveJob(j)

	select {
	case rec := <-waiting(s, "/api/jobs/j-chatty/wait?events=0"):
		if !strings.Contains(rec.Body.String(), "queued") {
			t.Fatalf("an event the caller has not seen: %d %s", rec.Code, rec.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait held back an event its caller has not seen")
	}

	got := waiting(s, "/api/jobs/j-chatty/wait?events=1")
	if moved, rec := advanceUntil(clock, 5*time.Second, got); rec != nil {
		t.Fatalf("nothing new, yet the wait ended after %s: %s", moved, rec.Body)
	}
	j.Events = append(j.Events, api.JobEvent{Msg: "booted"})
	s.store.saveJob(j)
	if _, rec := advanceUntil(clock, 5*time.Second, got); rec == nil || !strings.Contains(rec.Body.String(), "booted") {
		t.Fatalf("the wait did not answer with the new event: %v", rec)
	}
}

// Cutting a waiter does nothing to its job.
func TestCuttingAWaiterLeavesItsJobUntouched(t *testing.T) {
	wallClock(t)
	held := make(chan struct{}, 1)
	l := newLabWith(t, labConfig{wrap: func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/wait") {
				held <- struct{}{}
			}
			h.ServeHTTP(w, r)
		})
	}})
	l.waitDevice(func(d api.Device) bool { return d.BootID != "" })
	l.sim.RebootDelay = 2 * time.Second // the job outlives its waiter
	j, _, err := l.s.Submit(api.JobSpec{Device: l.name, Baseline: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cut := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, "GET", l.url+"/api/jobs/"+j.ID+"/wait?timeout=50", nil)
		req.Header.Set("Authorization", "Bearer "+harnessToken)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		cut <- err
	}()
	<-held
	time.Sleep(300 * time.Millisecond) // labd is holding the wait
	cancel()
	if err := <-cut; err == nil {
		t.Fatal("the job ended before its waiter could be cut")
	}
	time.Sleep(300 * time.Millisecond) // labd has seen the cut
	if now, _ := l.s.store.job(j.ID); now.State == api.JobDone {
		t.Fatalf("the job ended with its waiter: %s: %s", now.Outcome, now.Summary)
	}
	done := l.wait(j.ID, 60*time.Second)
	if done.Outcome != api.OutcomePass || done.Canceled {
		t.Fatalf("the job after its waiter was cut: %s canceled=%v: %s", done.Outcome, done.Canceled, done.Summary)
	}
}
