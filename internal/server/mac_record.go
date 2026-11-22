package server

// The Mac's record: one line for each event of lending a Mac, as it
// happens. It Joined, its first baseline, a change of its weekly window,
// Pause, Open now, Resume, a change of its Access level or of publishing,
// and its leaving. A line names nobody: its Volunteer reads it on My Macs,
// Maintainers and the M team on the Mac's page. Each of these events is
// also a line for the Lab group, which names the Mac only.
//
// A Mac that leaves is Retired: its record moves out of the fleet into
// retired_macs, where Maintainers keep it with the Mac's record, its jobs
// and labd's copy of its Audit log.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// The events of the Mac's record.
const (
	macJoined        = "joined"
	macFirstBaseline = "first-baseline"
	macWindow        = "window"
	macPaused        = "paused"
	macOpenNow       = "open-now"
	macResumed       = "resumed"
	macAccessLevel   = "access-level"
	macLimited       = "limited"
	macLimitRemoved  = "limit-removed"
	macPublishing    = "publishing"
	macPerformance   = "performance"
	macLeft          = "left"
)

// macEvent is one line of a Mac's record.
type macEvent struct {
	Time  time.Time `json:"time"`
	Mac   string    `json:"mac"`
	Event string    `json:"event"`
	Line  string    `json:"line"`
}

func (st *store) addMacEvent(deviceID string, e macEvent) error {
	_, err := st.db.Exec(`INSERT INTO mac_record (at, mac, device_id, event, line) VALUES (?, ?, ?, ?, ?)`,
		e.Time.UnixMicro(), e.Mac, deviceID, e.Event, e.Line)
	return err
}

// macRecord is the newest lines of the record of one enrollment of a Mac,
// newest first.
func (st *store) macRecord(deviceID string, limit int) ([]macEvent, error) {
	rows, err := st.db.Query(`SELECT at, mac, event, line FROM mac_record WHERE device_id = ? ORDER BY id DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []macEvent{}
	for rows.Next() {
		var e macEvent
		var at int64
		if err := rows.Scan(&at, &e.Mac, &e.Event, &e.Line); err != nil {
			return nil, err
		}
		e.Time = time.UnixMicro(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// said writes one line of the Mac's record. A line that cannot be written
// is logged: the record never holds an event up. Caller holds v.mu.
func (v *dev) said(event, line string) {
	v.s.macSaid(v.d.Name, v.d.ID, event, line)
}

func (s *Server) macSaid(name, id, event, line string) {
	if err := s.store.addMacEvent(id, macEvent{Time: s.now(), Mac: name, Event: event, Line: line}); err != nil {
		s.log.Printf("%s's record, %q: %v", name, line, err)
	}
}

// macRecord is the Mac's record as its page and My Macs show it, newest
// first.
func (s *Server) macRecord(d DeviceView) []recordShownLine {
	events, err := s.store.macRecord(d.ID, macRecordsShown)
	if err != nil {
		s.log.Printf("%s's record: %v", d.Name, err)
	}
	var out []recordShownLine
	for _, e := range events {
		out = append(out, recordShownLine{whenDay(e.Time), e.Line})
	}
	return out
}

// ---- Retired Macs ----

// How a Mac left the lab.
const (
	leftByVolunteer  = "volunteer"
	leftByMaintainer = "maintainer"
	leftBlocked      = "blocked"
	// A record of the old lab, Retired when its data was carried in (oldlab.go).
	retiredAtImport = "import"
)

// RetiredMac is the record of a Mac whose enrollment ended, kept for
// Maintainers: the Mac as it stood, when and how it left, and its record.
type RetiredMac struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Retired     time.Time  `json:"retired"`
	How         string     `json:"how"`
	VolunteerID int64      `json:"volunteer_id,omitempty"`
	Device      api.Device `json:"device"`
	Record      []macEvent `json:"record"`
}

// retire moves a Mac's record out of the fleet: its row in devices goes,
// which frees its name, and the record is kept as Retired. Neither happens
// without the other.
func (st *store) retire(d *api.Device, how string, at time.Time, ended *endedRow) error {
	kept := *d
	// A Retired Mac holds nothing and nobody reaches it.
	kept.Lease, kept.OOB, kept.ActiveJob, kept.Action, kept.SitePause = nil, nil, "", nil, nil
	b, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO retired_macs (id, name, retired, how, volunteer_id, data) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET retired = excluded.retired, how = excluded.how, data = excluded.data`,
		d.ID, d.Name, at.UnixMicro(), how, d.VolunteerID, string(b)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM devices WHERE id = ?`, d.ID); err != nil {
		return err
	}
	if ended != nil {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO ended(secret_hash,name,maintainer,at) VALUES(?,?,?,?)`, ended.secretHash, ended.name, ended.by, at.Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// retiredMacs are the newest Retired Macs, newest first.
func (st *store) retiredMacs(limit int) ([]RetiredMac, error) {
	rows, err := st.db.Query(`SELECT id, name, retired, how, volunteer_id, data FROM retired_macs ORDER BY retired DESC, id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RetiredMac{}
	for rows.Next() {
		var m RetiredMac
		var at int64
		var data string
		if err := rows.Scan(&m.ID, &m.Name, &at, &m.How, &m.VolunteerID, &data); err != nil {
			return nil, err
		}
		m.Retired = time.UnixMicro(at)
		if err := json.Unmarshal([]byte(data), &m.Device); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// retiredShown is how many Retired Macs a request gets.
const retiredShown = 200

// hRetired is the Retired Macs with their records, for Maintainers.
func (s *Server) hRetired(w http.ResponseWriter, r *http.Request) {
	macs, err := s.store.retiredMacs(retiredShown)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	for i := range macs {
		if macs[i].Record, err = s.store.macRecord(macs[i].ID, macRecordsShown); err != nil {
			httpErr(w, 500, "%v", err)
			return
		}
	}
	writeJSON(w, macs)
}
