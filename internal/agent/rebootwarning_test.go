package agent

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// slowNotice is a Sim whose notice takes a minute of the clock to come up.
type slowNotice struct {
	*Sim
	clock *FakeClock
	took  time.Duration
}

func (s slowNotice) NotifyWithPause(ctx context.Context, text string, onPause func()) bool {
	s.clock.Advance(s.took)
	return s.Sim.NotifyWithPause(ctx, text, onPause)
}

func TestANoticeThatTakesAMinuteToShowStillGivesTheFullWarning(t *testing.T) {
	const warning = 5 * time.Minute
	t0 := time.Date(2026, 11, 16, 10, 0, 0, 0, time.Local)
	clock := NewFakeClock(t0)
	dir := t.TempDir()
	sys := slowNotice{Sim: NewSim(dir), clock: clock, took: time.Minute}
	a := New(&Config{Server: "http://127.0.0.1:1", Secret: "test", WorkDir: dir, Clock: clock}, sys, log.New(io.Discard, "", 0))
	a.kind = api.KindVolunteer

	done := make(chan error, 1)
	go func() {
		done <- a.warnBeforeReboot(context.Background(), api.Command{ID: "c1", JobID: "j1"}, api.BootOnceArgs{RebootWarningSec: int(warning / time.Second)})
	}()
	var end time.Time
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if at, ok := clock.Next(); ok {
			end = at
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the warning ended before waiting: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the warning never began its wait")
		}
	}
	up := t0.Add(time.Minute)
	if want := up.Add(warning); !end.Equal(want) {
		t.Fatalf("the reboot waits until %s, %s after the notice came up at %s: want the full %s from then", end.Format("15:04:05"), end.Sub(up), up.Format("15:04:05"), warning)
	}
	ns := sys.Notices()
	if len(ns) != 1 || !strings.Contains(ns[0], "j1") {
		t.Fatalf("notices %q", ns)
	}
	entries, err := readAudit(dir, 0, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("Audit log %v, %v", entries, err)
	}
	if b, _ := json.Marshal(entries[0]); !strings.Contains(entries[0].Action, "10:06") {
		t.Errorf("the Audit line does not name the reboot's time, 10:06: %s", b)
	}
	clock.Advance(warning - time.Second)
	select {
	case err := <-done:
		t.Fatalf("the warning ended a second short of its full time: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	clock.Advance(time.Second)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the warning did not end at its full time")
	}
}
