package window

import (
	"testing"
	"time"
	_ "time/tzdata" // the zones below, wherever the tests run
)

// The tests' Mac lives in New York. 2026-10-26 is a Monday; New York leaves
// daylight saving time at 02:00 on Sunday 2026-11-01.
const macZone = "America/New_York"

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// at is a wall-clock time in the Mac's zone, "2026-10-27 06:00".
func at(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.ParseInLocation("2006-01-02 15:04", s, zone(t, macZone))
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func rules(t *testing.T, ss ...string) []Rule {
	t.Helper()
	var rs []Rule
	for _, s := range ss {
		r, err := ParseRule(s)
		if err != nil {
			t.Fatalf("ParseRule(%q): %v", s, err)
		}
		rs = append(rs, r)
	}
	return rs
}

func TestParseRuleReadsWeeklyWindows(t *testing.T) {
	for _, s := range []string{
		"mon-fri 19:00-07:00", // crosses midnight
		"sat-sun 09:00-17:00", // a day range
		"wed 12:00-13:00",     // one day
		"fri-mon 22:00-23:00", // a day range across the week's end
		"sun 00:00-04:00",
	} {
		r, err := ParseRule(s)
		if err != nil {
			t.Errorf("ParseRule(%q): %v", s, err)
			continue
		}
		if r.String() != s {
			t.Errorf("ParseRule(%q).String() = %q", s, r.String())
		}
	}
}

func TestParseRuleRefusesBadSyntax(t *testing.T) {
	for _, s := range []string{
		"",
		"mon-fri",
		"19:00-07:00",
		"mon-fri 19:00",
		"mon-fri 19:00-",
		"funday 19:00-07:00",
		"mon-funday 19:00-07:00",
		"mon--fri 19:00-07:00",
		"mon-fri 25:00-07:00",
		"mon-fri 19:60-07:00",
		"mon-fri 7pm-7am",
		"mon-fri 19:00-19:00", // a window of no length
		"mon-fri 19:00-07:00 sat",
	} {
		if _, err := ParseRule(s); err == nil {
			t.Errorf("ParseRule(%q) accepted bad syntax", s)
		}
	}
}

func TestEvaluateAnswersOpenAttendedEndAndNext(t *testing.T) {
	never := time.Time{}
	cases := []struct {
		name    string
		rules   []string
		zone    string // the Mac's zone; "" is macZone
		paused  string // PausedUntil, in the Mac's zone
		open    string // OpenUntil, in the Mac's zone
		attend  bool
		now     time.Time
		want    bool // open
		attnd   bool
		end     time.Time
		next    time.Time
		nextNil bool
	}{
		{name: "inside a window that crosses midnight", rules: []string{"mon-fri 19:00-07:00"},
			now: at(t, "2026-10-27 06:00"), want: true, end: at(t, "2026-10-27 07:00"), next: at(t, "2026-10-27 19:00")},
		{name: "a window opens at its start", rules: []string{"mon-fri 19:00-07:00"},
			now: at(t, "2026-10-26 19:00"), want: true, end: at(t, "2026-10-27 07:00"), next: at(t, "2026-10-27 19:00")},
		{name: "a window is closed at its end", rules: []string{"mon-fri 19:00-07:00"},
			now: at(t, "2026-10-27 07:00"), next: at(t, "2026-10-27 19:00")},
		{name: "a crossing window belongs to the day it starts: Monday morning is Sunday night", rules: []string{"mon-fri 19:00-07:00"},
			now: at(t, "2026-10-26 06:00"), next: at(t, "2026-10-26 19:00")},
		{name: "Friday's window runs into Saturday morning", rules: []string{"mon-fri 19:00-07:00"},
			now: at(t, "2026-10-31 06:00"), want: true, end: at(t, "2026-10-31 07:00"), next: at(t, "2026-11-02 19:00")},
		{name: "closed over the weekend", rules: []string{"mon-fri 19:00-07:00"},
			now: at(t, "2026-10-31 12:00"), next: at(t, "2026-11-02 19:00")},
		{name: "a day range", rules: []string{"sat-sun 09:00-17:00"},
			now: at(t, "2026-11-01 10:00"), want: true, end: at(t, "2026-11-01 17:00"), next: at(t, "2026-11-07 09:00")},
		{name: "a day range across the week's end covers Sunday", rules: []string{"fri-mon 22:00-23:00"},
			now: at(t, "2026-11-01 22:30"), want: true, end: at(t, "2026-11-01 23:00"), next: at(t, "2026-11-02 22:00")},
		{name: "a day range across the week's end leaves Tuesday out", rules: []string{"fri-mon 22:00-23:00"},
			now: at(t, "2026-10-27 22:30"), next: at(t, "2026-10-30 22:00")},
		{name: "one day a week", rules: []string{"wed 12:00-13:00"},
			now: at(t, "2026-10-28 13:30"), next: at(t, "2026-11-04 12:00")},
		{name: "overlapping windows end with the later one", rules: []string{"mon 18:00-20:00", "mon 19:00-22:00"},
			now: at(t, "2026-10-26 19:30"), want: true, end: at(t, "2026-10-26 22:00"), next: at(t, "2026-11-02 18:00")},
		{name: "back-to-back windows are one window", rules: []string{"mon 18:00-20:00", "mon 20:00-22:00"},
			now: at(t, "2026-10-26 19:00"), want: true, end: at(t, "2026-10-26 22:00"), next: at(t, "2026-11-02 18:00")},
		{name: "the next window is the earliest of several", rules: []string{"sat 10:00-12:00", "wed 12:00-13:00"},
			now: at(t, "2026-10-27 09:00"), next: at(t, "2026-10-28 12:00")},
		{name: "no windows: closed, and nothing next",
			now: at(t, "2026-10-27 06:00"), nextNil: true},
		{name: "the schedule is in the Mac's time zone, whatever zone now is in", rules: []string{"mon-fri 19:00-07:00"},
			now: time.Date(2026, 10, 27, 10, 30, 0, 0, time.UTC), want: true, end: time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC), next: at(t, "2026-10-27 19:00")},
		{name: "the same moment on a Mac in UTC is outside the window", rules: []string{"mon-fri 19:00-07:00"}, zone: "UTC",
			now: time.Date(2026, 10, 27, 10, 30, 0, 0, time.UTC), next: time.Date(2026, 10, 27, 19, 0, 0, 0, time.UTC)},
		{name: "a window over the end of daylight saving time ends at its local time", rules: []string{"sun 00:00-04:00"},
			now: at(t, "2026-11-01 00:30"), want: true, end: at(t, "2026-11-01 04:00"), next: at(t, "2026-11-08 00:00")},
		{name: "Pause closes a window until it ends", rules: []string{"mon-fri 19:00-07:00"}, paused: "2026-10-26 23:00",
			now: at(t, "2026-10-26 20:00"), next: at(t, "2026-10-26 23:00")},
		{name: "a window reopens when its Pause ends", rules: []string{"mon-fri 19:00-07:00"}, paused: "2026-10-26 23:00",
			now: at(t, "2026-10-26 23:30"), want: true, end: at(t, "2026-10-27 07:00"), next: at(t, "2026-10-27 19:00")},
		{name: "Pause until a date skips every window before it", rules: []string{"mon-fri 19:00-07:00"}, paused: "2026-10-29 00:00",
			now: at(t, "2026-10-27 20:00"), next: at(t, "2026-10-29 00:00")},
		{name: "a Pause that has ended changes nothing", rules: []string{"mon-fri 19:00-07:00"}, paused: "2026-10-26 08:00",
			now: at(t, "2026-10-27 06:00"), want: true, end: at(t, "2026-10-27 07:00"), next: at(t, "2026-10-27 19:00")},
		{name: "open now opens the Mac outside its schedule", rules: []string{"mon-fri 19:00-07:00"}, open: "2026-10-31 14:00",
			now: at(t, "2026-10-31 12:00"), want: true, end: at(t, "2026-10-31 14:00"), next: at(t, "2026-11-02 19:00")},
		{name: "open now that runs into a window ends with the window", rules: []string{"mon-fri 19:00-07:00"}, open: "2026-10-26 20:00",
			now: at(t, "2026-10-26 17:00"), want: true, end: at(t, "2026-10-27 07:00"), next: at(t, "2026-10-27 19:00")},
		{name: "open now with no schedule", open: "2026-10-31 14:00",
			now: at(t, "2026-10-31 12:00"), want: true, end: at(t, "2026-10-31 14:00"), nextNil: true},
		{name: "open now has ended", rules: []string{"mon-fri 19:00-07:00"}, open: "2026-10-31 11:00",
			now: at(t, "2026-10-31 12:00"), next: at(t, "2026-11-02 19:00")},
		{name: "an Attended open now is attended", rules: []string{"mon-fri 19:00-07:00"}, open: "2026-10-31 14:00", attend: true,
			now: at(t, "2026-10-31 12:00"), want: true, attnd: true, end: at(t, "2026-10-31 14:00"), next: at(t, "2026-11-02 19:00")},
		{name: "a weekly window is not attended", rules: []string{"mon-fri 19:00-07:00"}, open: "2026-10-27 05:00", attend: true,
			now: at(t, "2026-10-27 06:00"), want: true, end: at(t, "2026-10-27 07:00"), next: at(t, "2026-10-27 19:00")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Settings{Rules: rules(t, c.rules...), Zone: c.zone, Attended: c.attend}
			if s.Zone == "" {
				s.Zone = macZone
			}
			if c.paused != "" {
				s.PausedUntil = at(t, c.paused)
			}
			if c.open != "" {
				s.OpenUntil = at(t, c.open)
			}
			got := Evaluate(s, c.now)
			if got.Open != c.want || got.Attended != c.attnd {
				t.Fatalf("at %s: open %v attended %v, want open %v attended %v", c.now, got.Open, got.Attended, c.want, c.attnd)
			}
			if c.want && !got.End.Equal(c.end) {
				t.Errorf("at %s: window ends %s, want %s", c.now, got.End, c.end)
			}
			if !c.want && !got.End.IsZero() {
				t.Errorf("at %s: closed, but a window end of %s", c.now, got.End)
			}
			want := c.next
			if c.nextNil {
				want = never
			}
			if !got.Next.Equal(want) {
				t.Errorf("at %s: next window %s, want %s", c.now, got.Next, want)
			}
		})
	}
}

// A Pause taken on the lab's site closes the Mac whatever the Mac says, an
// Open now included, and it is over at its time.
func TestAPauseFromTheSiteClosesTheMacWhateverItsOwnSettingsSay(t *testing.T) {
	now := at(t, "2026-10-19 20:00") // a Monday
	s := Settings{Rules: rules(t, "mon-fri 19:00-23:00"), Zone: macZone, OpenUntil: now.Add(6 * time.Hour)}
	if st := Evaluate(s, now); !st.Open {
		t.Fatalf("the Mac is not open by its own settings: %+v", st)
	}
	if st := Paused(s, time.Time{}, now); st.Open || !st.Next.IsZero() {
		t.Errorf("Paused until Resume: %+v, want closed with no opening ahead", st)
	}
	until := now.Add(2 * time.Hour)
	if st := Paused(s, until, now); st.Open || !st.Next.Equal(until) {
		t.Errorf("Paused for two hours inside a window: %+v, want closed, opening at %s", st, until)
	}
	// A Pause that ends between windows: the Mac opens with its next window.
	s.OpenUntil = time.Time{}
	until = at(t, "2026-10-20 03:00")
	if st := Paused(s, until, now); st.Open || !st.Next.Equal(at(t, "2026-10-20 19:00")) {
		t.Errorf("Paused past the window's end: %+v, want closed until Tuesday 19:00", st)
	}
	if st := Paused(s, now.Add(-time.Minute), now); !st.Open {
		t.Errorf("a Pause whose time has passed still closes the Mac: %+v", st)
	}
}

func TestScheduleReadsAsTheVolunteerTypedIt(t *testing.T) {
	s := Settings{Rules: rules(t, "mon-fri 19:00-07:00", "sat 10:00-12:00")}
	if got := s.Schedule(); got != "mon-fri 19:00-07:00, sat 10:00-12:00" || !s.Scheduled() {
		t.Errorf("schedule %q, scheduled %v", got, s.Scheduled())
	}
	if (Settings{}).Schedule() != "" || (Settings{}).Scheduled() {
		t.Error("a Mac with no weekly window has a schedule")
	}
}

// "always" is every hour of every day, and 24:00 ends a window at midnight:
// a whole day is DAYS 00:00-24:00, and a whole week of them reads as always.
func TestParseRuleReadsAlwaysAndWholeDays(t *testing.T) {
	for s, want := range map[string]string{
		"always":              "always",
		"mon-sun 00:00-24:00": "always",
		"sun-sat 00:00-24:00": "always",
		"wed-tue 00:00-24:00": "always",
		"mon-fri 00:00-24:00": "mon-fri 00:00-24:00",
		"sat 00:00-24:00":     "sat 00:00-24:00",
		"fri 19:00-24:00":     "fri 19:00-24:00",
	} {
		r, err := ParseRule(s)
		if err != nil {
			t.Errorf("ParseRule(%q): %v", s, err)
			continue
		}
		if r.String() != want {
			t.Errorf("ParseRule(%q).String() = %q, want %q", s, r.String(), want)
		}
		back, err := ParseRule(r.String())
		if err != nil || back != r {
			t.Errorf("ParseRule(%q) does not read back: %v, %v", r.String(), back, err)
		}
	}
	if rs := rules(t, "always", "mon-sun 00:00-24:00"); rs[0] != rs[1] {
		t.Errorf("always %v and mon-sun 00:00-24:00 %v are not the same window", rs[0], rs[1])
	}
	for _, s := range []string{"always mon", "Always", "mon always", "mon 24:00-07:00", "mon 00:00-24:01", "mon 24:00-24:00", "mon 00:00-25:00"} {
		if _, err := ParseRule(s); err == nil {
			t.Errorf("ParseRule(%q) accepted bad syntax", s)
		}
	}
}

// A window of whole days has no gap at midnight, and always never closes:
// open across each day's end, the week's end and the end of daylight saving
// time, with no next window.
func TestWholeDaysAndAlwaysHaveNoGapAtMidnight(t *testing.T) {
	always := Settings{Rules: rules(t, "always"), Zone: macZone}
	for _, now := range []time.Time{
		at(t, "2026-10-26 00:00"), at(t, "2026-10-26 23:59"), at(t, "2026-10-27 00:00"),
		at(t, "2026-11-01 00:00"), at(t, "2026-11-01 01:30"), at(t, "2026-11-01 23:59").Add(59 * time.Second),
	} {
		st := Evaluate(always, now)
		if !st.Open || !st.Always || !st.Next.IsZero() || st.End.Sub(now) < 7*24*time.Hour {
			t.Errorf("always at %s: %+v, want open with no end", now, st)
		}
	}
	weekdays := Settings{Rules: rules(t, "mon-fri 00:00-24:00"), Zone: macZone}
	st := Evaluate(weekdays, at(t, "2026-10-27 23:59").Add(59*time.Second))
	if !st.Open || st.Always || !st.End.Equal(at(t, "2026-10-31 00:00")) || !st.Next.Equal(at(t, "2026-11-02 00:00")) {
		t.Errorf("mon-fri 00:00-24:00 on Tuesday night: %+v, want open until Saturday 00:00", st)
	}
	if st := Evaluate(weekdays, at(t, "2026-10-31 00:00")); st.Open {
		t.Errorf("mon-fri 00:00-24:00 is open on Saturday: %+v", st)
	}
	split := Settings{Rules: rules(t, "mon-sun 00:00-12:00", "mon-sun 12:00-24:00"), Zone: macZone}
	if st := Evaluate(split, at(t, "2026-10-28 09:00")); !st.Open || !st.Always {
		t.Errorf("two halves of every day: %+v, want always open", st)
	}
	// A Pause on an always Mac closes it until then, and it opens for good after.
	always.PausedUntil = at(t, "2026-10-29 00:00")
	if st := Evaluate(always, at(t, "2026-10-27 12:00")); st.Open || !st.Next.Equal(always.PausedUntil) {
		t.Errorf("a Paused always Mac: %+v, want closed until the Pause ends", st)
	}
	// One whole day a week is not always, on that day or any other.
	sunday := Settings{Rules: rules(t, "sun 00:00-24:00"), Zone: macZone}
	if st := Evaluate(sunday, at(t, "2026-11-01 12:00")); !st.Open || st.Always || !st.End.Equal(at(t, "2026-11-02 00:00")) {
		t.Errorf("sun 00:00-24:00 on Sunday: %+v", st)
	}
}
