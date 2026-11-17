package server

// Which Lab-owned Mac is on which of the lab server's cables. The Out-of-band
// controller resets a port, not a Mac, and no port is written in a Mac's
// record: labd learns the port by a Sighting, the Mac's name read there, or
// by a Maintainer's Proving reset, and sends a Hard reset only where it is
// sure. Sightings are kept in labd's data: after a restart labd keeps each
// one whose port is still connected with the same product id, at its first
// look at the ports, and Macs that are up and show their name are sighted
// again by themselves.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/oob"
)

// sightEvery is how often labd asks the controller what its ports show.
const sightEvery = 15 * time.Second

// sighting is the lab knowing a Mac's port, and the port as it was then.
type sighting struct {
	rid  int
	seen oob.Seen
	at   time.Time
	// proved: placed by a Proving reset, the port showing no name.
	proved bool
	// boot is the Mac's boot id when the port last showed seen. A Mac's
	// own restart moves its port's plug count; a new boot id says that is
	// what moved it.
	boot string
	// rose is when labd first saw the plug count above seen with the Mac
	// still on boot, as far as labd has heard: zero while it is not.
	rose time.Time
}

type cables struct {
	mu sync.Mutex
	// none: labd was given no controller.
	none bool
	// ports are the controller's ports as it last showed them.
	ports map[int]oob.Port
	held  map[string]sighting
	// kept are the Sightings labd's data held as it started, until its
	// first look at the ports keeps or drops each; nil after that.
	kept map[string]sighting
	// saved is what labd's data holds now; saveMu is held to write it.
	saved  map[string]sighting
	saveMu sync.Mutex
	// lost is why a Mac has no Sighting, where there is something to say.
	lost map[string]string
	// down is why the controller is not healthy; "" while it answers.
	down string
	// downs counts the times it was found not answering, so that a look
	// that began before one does not undo it.
	downs uint64
	// proving is held for the length of a Proving reset: one at a time.
	proving sync.Mutex
	// looking is held for the length of a look at the ports, from the
	// first read to the Sightings made of them: a look that read the ports
	// earlier never lands after one that read them later.
	looking sync.Mutex
}

func newCables(c *oob.Client) *cables {
	return &cables{none: c == nil, ports: map[int]oob.Port{}, held: map[string]sighting{}, lost: map[string]string{}}
}

// sightings are the Sightings labd's data holds.
func (st *store) sightings() (map[string]sighting, error) {
	rows, err := st.db.Query(`SELECT mac, rid, product_id, plug_count, boot, proved, at FROM sightings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]sighting{}
	for rows.Next() {
		var name string
		var sg sighting
		var at int64
		if err := rows.Scan(&name, &sg.rid, &sg.seen.ProductID, &sg.seen.PlugCount, &sg.boot, &sg.proved, &at); err != nil {
			return nil, err
		}
		sg.at = time.UnixMicro(at)
		out[name] = sg
	}
	return out, rows.Err()
}

// saveSightings makes labd's data hold these Sightings and no others.
func (st *store) saveSightings(held map[string]sighting) error {
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM sightings`); err != nil {
		return err
	}
	for name, sg := range held {
		if _, err := tx.Exec(`INSERT INTO sightings (mac, rid, product_id, plug_count, boot, proved, at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			name, sg.rid, sg.seen.ProductID, sg.seen.PlugCount, sg.boot, sg.proved, sg.at.UnixMicro()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// saveSightings writes the Sightings labd holds to its data, when they
// changed. Before labd's first look at the ports its data keeps what it
// held.
func (s *Server) saveSightings() {
	c := s.cables
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.mu.Lock()
	if c.kept != nil || c.none {
		c.mu.Unlock()
		return
	}
	held := map[string]sighting{}
	for name, sg := range c.held {
		sg.rose = time.Time{}
		held[name] = sg
	}
	c.mu.Unlock()
	if maps.EqualFunc(held, c.saved, func(a, b sighting) bool {
		return a.rid == b.rid && a.seen == b.seen && a.proved == b.proved && a.boot == b.boot && a.at.Equal(b.at)
	}) {
		return
	}
	if err := s.store.saveSightings(held); err != nil {
		s.log.Printf("sighting: the Sightings were not written to labd's data: %v", err)
		return
	}
	c.saved = held
}

// restoreLocked keeps or drops each Sighting labd's data held as it
// started, at its first look at the ports: one is kept while its port is
// still connected with the same product id, from the port as it is then,
// whose plug count the mini's own restart starts again. It returns the
// log's lines. The caller holds c.mu.
func (c *cables) restoreLocked(ports map[int]oob.Port, macs []labOwned) (said []string) {
	if c.kept == nil {
		return nil
	}
	known := map[string]bool{}
	for _, m := range macs {
		known[m.name] = true
	}
	names := slices.Sorted(maps.Keys(c.kept))
	for _, name := range names {
		sg := c.kept[name]
		p := ports[sg.rid]
		port := "the " + oob.PortName(sg.rid) + " port"
		why := ""
		switch {
		case !known[name]:
			why = "it is no Lab-owned Mac of the lab any more"
		case !p.Connected:
			why = port + " is no longer connected"
		case p.ProductID != sg.seen.ProductID:
			why = fmt.Sprintf("%s shows product id %d, not the %d it was seen with", port, p.ProductID, sg.seen.ProductID)
		}
		where := name + " on " + oob.PortName(sg.rid)
		if why != "" {
			if known[name] {
				c.lost[name] = "its Sighting was not kept across a restart of labd: " + why
			}
			said = append(said, "sighting not kept across the restart: "+where+": "+why)
			continue
		}
		sg.seen.PlugCount, sg.rose = p.PlugCount, time.Time{}
		c.held[name] = sg
		said = append(said, "sighting kept across the restart: "+where)
	}
	c.kept = nil
	return said
}

// of is the Sighting of the Mac named name.
func (c *cables) of(name string) (sighting, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sg, ok := c.held[name]
	return sg, ok
}

// saw records what a port showed at a look outside the regular ones.
func (c *cables) saw(p oob.Port) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ports[p.RID] = p
}

// keep stores sg as the Sighting of name, if name is still sighted on
// sg's port.
func (c *cables) keep(name string, sg sighting) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if held, ok := c.held[name]; ok && held.rid == sg.rid {
		c.held[name] = sg
	}
}

// drop ends a Mac's Sighting, saying why.
func (c *cables) drop(name, why string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(name, why, now)
}

func (c *cables) dropLocked(name, why string, now time.Time) {
	if _, ok := c.held[name]; ok {
		delete(c.held, name)
		why = "its Sighting was dropped at " + when(now) + ": " + why
	}
	c.lost[name] = why
}

// onPortLocked is the Mac sighted on a port, "" for none.
func (c *cables) onPortLocked(rid int) string {
	for name, sg := range c.held {
		if sg.rid == rid {
			return name
		}
	}
	return ""
}

// freeLocked are the connected ports nothing is sighted on, as people name
// them.
func (c *cables) freeLocked() []string {
	var out []string
	for _, rid := range oob.RIDs {
		if p, ok := c.ports[rid]; ok && p.Connected && c.onPortLocked(rid) == "" {
			out = append(out, oob.PortName(rid))
		}
	}
	return out
}

const noController = "labd was given no Out-of-band controller"

// cable is what the lab knows of a Lab-owned Mac's cable.
func (c *cables) cable(name string) api.Cable {
	if c == nil {
		return api.Cable{State: api.CableNone, NoHardReset: noController}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if sg, ok := c.held[name]; ok {
		return api.Cable{State: api.CableSighted, Port: oob.PortName(sg.rid), Proved: sg.proved, Since: sg.at, NoHardReset: c.down}
	}
	if c.none {
		return api.Cable{State: api.CableNone, NoHardReset: noController}
	}
	free := c.freeLocked()
	lost := c.lost[name]
	if lost != "" {
		lost = " (" + lost + ")"
	}
	switch {
	case c.down != "":
		return api.Cable{State: api.CableNotSighted, NoHardReset: c.down}
	case len(free) > 0:
		return api.Cable{State: api.CableNotSighted, NoHardReset: fmt.Sprintf("the lab has no Sighting of %s%s. A Maintainer can prove its port: `lab reset %s --prove %s`",
			name, lost, name, strings.Join(free, "|"))}
	}
	return api.Cable{State: api.CableNone, NoHardReset: fmt.Sprintf("no cable can be the one of %s%s: every connected port of the lab server is another Mac's, or none is connected", name, lost)}
}

// status is the status a refused Hard reset is answered with: the
// controller's trouble is the lab's, anything else is the Mac's state.
func (c *cables) status() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down != "" || c.none {
		return http.StatusServiceUnavailable
	}
	return http.StatusConflict
}

// reach is why no Hard reset can reach the Mac named name now, "" when one
// can.
func (c *cables) reach(name string) string { return c.cable(name).NoHardReset }

// cableLocked is the Cable of v's Mac, nil for a Volunteer's. The caller
// holds v.mu.
func (v *dev) cableLocked() *api.Cable {
	if v.d.IsVolunteer() {
		return nil
	}
	c := v.s.cables.cable(v.d.Name)
	if c.NoHardReset == "" && v.networkOnlyLocked() {
		c.NoHardReset = "network-only recovery is selected for it"
	}
	return &c
}

// noHardResetLocked is why the lab sends v's Mac no Hard reset now, "" when
// it can send one. The caller holds v.mu.
func (v *dev) noHardResetLocked() string {
	c := v.cableLocked()
	if c == nil {
		return neverAVolunteers(v.d.Name)
	}
	return c.NoHardReset
}

func (v *dev) noHardReset() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.noHardResetLocked()
}

// neverAVolunteers is the lab's refusal to Hard reset a Volunteer's Mac.
func neverAVolunteers(name string) string {
	return name + " is a Volunteer's Mac, and the lab never sends a Volunteer's Mac a Hard reset: it recovers by network means and its own watchdog, then its Volunteer is asked"
}

// labOwned is a Lab-owned Mac's record as a look at the ports needs it.
type labOwned struct {
	name, hostname string
	// boot is its boot id, and heard when labd last heard the Mac itself.
	boot  string
	heard time.Time
}

// labOwnedMacs are the Lab-owned Macs the lab has a record of.
func (s *Server) labOwnedMacs() []labOwned {
	var out []labOwned
	for _, v := range s.allDevs() {
		if m, ok := v.labOwned(); ok {
			out = append(out, m)
		}
	}
	return out
}

// labOwned is v's Mac as a look at the ports needs it; false for a
// Volunteer's.
func (v *dev) labOwned() (labOwned, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.d.IsVolunteer() {
		return labOwned{}, false
	}
	return labOwned{v.d.Name, v.d.Facts.Hostname, v.d.BootID, v.heard}, true
}

// sightLoop keeps the Sightings: it asks the controller what its ports show
// now and then, for as long as labd runs.
func (s *Server) sightLoop() {
	if s.cfg.OOB == nil {
		return
	}
	every := s.cfg.SightEvery
	if every <= 0 {
		every = sightEvery
	}
	s.sight(s.ctx)
	t := s.clock().NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C():
			s.sight(s.ctx)
		}
	}
}

// sight is one look at the controller's ports: Sightings whose port changed
// are dropped, and a name that belongs to exactly one Lab-owned Mac's record
// makes one.
func (s *Server) sight(ctx context.Context) {
	c := s.cfg.OOB
	if c == nil {
		return
	}
	s.cables.looking.Lock()
	defer s.cables.looking.Unlock()
	s.cables.mu.Lock()
	downs := s.cables.downs
	s.cables.mu.Unlock()
	ports := map[int]oob.Port{}
	for _, rid := range oob.RIDs {
		actx, cancel := context.WithTimeout(ctx, 20*time.Second)
		p, err := c.Port(actx, rid)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				s.controllerDown(err)
			}
			return
		}
		ports[rid] = p
	}
	if s.lookedAt(ports, downs) {
		s.saveSightings()
	}
}

// lookedAt brings the Sightings in line with ports, read in a look begun
// when the controller had been found not answering downs times; false when
// that look is older news.
func (s *Server) lookedAt(ports map[int]oob.Port, downs uint64) bool {
	macs := s.labOwnedMacs()
	now := s.now()
	s.cables.mu.Lock()
	defer s.cables.mu.Unlock()
	if s.cables.downs != downs {
		// found not answering while this look was under way: its ports are older news
		return false
	}
	if s.cables.down != "" {
		s.cables.down = ""
		s.log.Printf("the Out-of-band controller answers again")
	}
	for _, line := range s.cables.restoreLocked(ports, macs) {
		s.log.Printf("%s", line)
	}
	for _, line := range s.cables.apply(ports, macs, now) {
		s.log.Printf("sighting: %s", line)
	}
	return true
}

// portNow is how a port compares with what a Sighting saw on it.
type portNow int

const (
	// portSame: as it was seen.
	portSame portNow = iota
	// portRestarted: the plug count rose and the Mac is on a new boot: its
	// own restart moved it, and the Sighting holds at the new count.
	portRestarted
	// portRising: the plug count rose and the Mac has not been heard since
	// on the boot it had: it may be restarting, or hung after a restart.
	// The Sighting holds.
	portRising
	// portChanged: the cable is not the one that was seen; the Sighting
	// ends.
	portChanged
)

// against is how port p compares with what sg saw on it, for the Mac m,
// and why for portChanged and portRising. A Mac's own restart moves its
// port's plug count as a pull does; what tells them apart is the Mac: on a
// restart it comes back on a new boot or is not heard again, and with a
// cable pulled while it stayed up it is heard again on the boot it had.
func (sg sighting) against(p oob.Port, m labOwned) (portNow, string) {
	port := "the " + oob.PortName(p.RID) + " port"
	moved := fmt.Sprintf("%s has plug count %d, not the %d it was seen with", port, p.PlugCount, sg.seen.PlugCount)
	switch {
	case !p.Connected:
		return portChanged, port + " is no longer connected"
	case p.ProductID != sg.seen.ProductID:
		return portChanged, fmt.Sprintf("%s shows product id %d, not the %d it was seen with", port, p.ProductID, sg.seen.ProductID)
	case p.PlugCount < sg.seen.PlugCount:
		return portChanged, moved + ": the port's count started again"
	case p.PlugCount == sg.seen.PlugCount:
		return portSame, ""
	case sg.boot != "" && m.boot != sg.boot:
		return portRestarted, ""
	case !sg.rose.IsZero() && m.heard.After(sg.rose):
		return portChanged, moved + ", and " + m.name + " was heard since on the boot it had: its cable was pulled and something plugged in"
	}
	return portRising, moved + ", and " + m.name + " has not been heard on a new boot since"
}

// placeholderNames are what a Mac's Thunderbolt link shows before its
// hostname is set, early in boot or hung there: the Linux kernel's default
// hostname "(none)", systemd's fallbacks, and the mini's own placeholder for
// a partner whose name it has not read. None is evidence of any Mac.
var placeholderNames = []string{"unknown_xd_device", "(none)", "localhost", "localhost.localdomain", "archlinux"}

// realName is the name a port shows, or "" when it is a placeholder.
func realName(name string) string {
	name = strings.TrimSpace(name)
	for _, p := range placeholderNames {
		if strings.EqualFold(name, p) {
			return ""
		}
	}
	return name
}

// recordsNamed are the Macs a name read on a port belongs to.
func recordsNamed(name string, macs []labOwned) []string {
	var out []string
	for _, m := range macs {
		if strings.EqualFold(name, m.name) || m.hostname != "" && strings.EqualFold(name, m.hostname) {
			out = append(out, m.name)
		}
	}
	return out
}

// apply brings the Sightings in line with what the ports show, and returns
// what changed, for the log. The caller holds c.mu.
func (c *cables) apply(ports map[int]oob.Port, macs []labOwned, now time.Time) (said []string) {
	c.ports = ports
	drop := func(name, why string) {
		if _, ok := c.held[name]; ok {
			said = append(said, name+" is no longer sighted: "+why)
		}
		c.dropLocked(name, why, now)
	}
	known := map[string]labOwned{}
	for _, m := range macs {
		known[m.name] = m
	}
	for name, sg := range c.held {
		m, ok := known[name]
		if !ok {
			delete(c.held, name)
			delete(c.lost, name)
			said = append(said, name+" is no longer sighted: it is no Lab-owned Mac of the lab any more")
			continue
		}
		p := ports[sg.rid]
		switch st, why := sg.against(p, m); st {
		case portChanged:
			drop(name, why)
		case portRestarted:
			said = append(said, fmt.Sprintf("%s is still sighted on the %s port: it restarted, and the plug count went from %d to %d", name, oob.PortName(sg.rid), sg.seen.PlugCount, p.PlugCount))
			sg.seen.PlugCount, sg.boot, sg.rose = p.PlugCount, m.boot, time.Time{}
			c.held[name] = sg
		case portRising:
			if sg.rose.IsZero() {
				sg.rose = now
				c.held[name] = sg
				said = append(said, name+" is still sighted: "+why+"; it may be restarting")
			}
		case portSame:
			if sg.boot != m.boot {
				sg.boot = m.boot
				c.held[name] = sg
			}
		}
	}
	for name := range c.lost {
		if _, ok := known[name]; !ok {
			delete(c.lost, name)
		}
	}
	// Each name a port shows, and the one record it belongs to.
	want := map[int]string{}
	on := map[string][]int{}
	conflict := map[string]string{}
	for _, rid := range oob.RIDs {
		p := ports[rid]
		shown := realName(p.Name)
		if !p.Connected || shown == "" {
			continue
		}
		port := "the " + oob.PortName(rid) + " port"
		owners := recordsNamed(shown, macs)
		switch {
		case len(owners) > 1:
			for _, name := range owners {
				conflict[name] = fmt.Sprintf("the name %q on %s belongs to %d Lab-owned Macs' records (%s)", p.Name, port, len(owners), strings.Join(owners, ", "))
			}
			continue
		case len(owners) == 0:
			// A name of nothing the lab owns. Whatever was placed there is
			// not what is there now.
			if held := c.onPortLocked(rid); held != "" {
				conflict[held] = fmt.Sprintf("%s shows the name %q, which is not its own", port, p.Name)
			}
			continue
		}
		name := owners[0]
		want[rid] = name
		on[name] = append(on[name], rid)
		if held := c.onPortLocked(rid); held != "" && held != name {
			conflict[held] = fmt.Sprintf("%s shows the name of %s", port, name)
			conflict[name] = fmt.Sprintf("its name is on %s, where %s was sighted", port, held)
		}
	}
	for name, rids := range on {
		if len(rids) > 1 {
			var ps []string
			for _, rid := range rids {
				ps = append(ps, oob.PortName(rid))
			}
			conflict[name] = "its name is on " + strings.Join(ps, " and ") + " ports at once"
			continue
		}
		if sg, ok := c.held[name]; ok && sg.rid != rids[0] {
			conflict[name] = fmt.Sprintf("its name is on the %s port, and it was sighted on the %s port", oob.PortName(rids[0]), oob.PortName(sg.rid))
		}
	}
	names := make([]string, 0, len(conflict))
	for name := range conflict {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := c.held[name]; !ok {
			said = append(said, name+" is not sighted: "+conflict[name])
		}
		drop(name, conflict[name])
	}
	for _, rid := range oob.RIDs {
		name, ok := want[rid]
		if !ok || conflict[name] != "" {
			continue
		}
		p := ports[rid]
		if p.ProductID <= 0 || p.PlugCount <= 0 {
			c.lost[name] = fmt.Sprintf("the %s port shows its name and no product id or plug count to remember it by", oob.PortName(rid))
			continue
		}
		seen := oob.Seen{ProductID: p.ProductID, PlugCount: p.PlugCount}
		if sg, ok := c.held[name]; ok && sg.rid == rid && sg.seen == seen {
			if sg.proved {
				sg.proved = false
				c.held[name] = sg
			}
			continue
		}
		c.held[name] = sighting{rid: rid, seen: seen, at: now, boot: known[name].boot}
		delete(c.lost, name)
		said = append(said, fmt.Sprintf("%s is sighted on the %s port (product id %d, plug count %d)", name, oob.PortName(rid), p.ProductID, p.PlugCount))
	}
	return said
}

// controllerDown records that the controller is not healthy, and why: until
// it answers again no Mac has a Hard reset. "Agents" is told by labd's watch
// of the lab's own parts, once the trouble has lasted (watch.go).
func (s *Server) controllerDown(err error) {
	why := "the Out-of-band controller is not healthy: " + err.Error()
	if _, lerr := os.Lstat(s.cfg.OOB.TokenFile); errors.Is(lerr, os.ErrNotExist) {
		why = "the Out-of-band controller is not set up: its token is not at " + s.cfg.OOB.TokenFile + " yet"
	}
	s.cables.mu.Lock()
	was := s.cables.down
	s.cables.down = why
	s.cables.downs++
	s.cables.mu.Unlock()
	if was != why {
		s.log.Printf("no Hard reset: %s", why)
	}
}

// hardReset sends a Hard reset to v's Mac through the port it is sighted
// on, or says why it sends none. It goes out only while the port is still
// connected with the product id of the Sighting, and with its plug count or
// one the Mac's own restart moved (see against): labd looks, and the
// controller compares that once more under its lock. A port whose count
// moved while the Mac is still heard on the boot it had gets no reset until
// the lab knows which it was; anything else drops the Sighting.
func (s *Server) hardReset(ctx context.Context, v *dev) (port string, err error) {
	name := v.snapshot().Name
	if why := v.noHardReset(); why != "" {
		return "", refuse(s.cables.status(), "no Hard reset was sent: %s", why)
	}
	sg, ok := s.cables.of(name)
	if !ok {
		return "", refuse(http.StatusConflict, "no Hard reset was sent: %s", s.cables.reach(name))
	}
	port = oob.PortName(sg.rid)
	p, err := s.cfg.OOB.Port(ctx, sg.rid)
	if err != nil {
		if ctx.Err() == nil {
			s.controllerDown(err)
		}
		return port, refuse(http.StatusServiceUnavailable, "no Hard reset was sent: the Out-of-band controller is not healthy: %v", err)
	}
	s.cables.saw(p)
	m, _ := v.labOwned()
	switch st, why := sg.against(p, m); st {
	case portChanged:
		s.cables.drop(name, why, s.now())
		s.saveSightings()
		s.log.Printf("sighting: %s is no longer sighted: %s", name, why)
		return port, refuse(http.StatusConflict, "no Hard reset was sent: %s. The Sighting of %s is dropped", why, name)
	case portRestarted:
		sg.seen.PlugCount, sg.boot, sg.rose = p.PlugCount, m.boot, time.Time{}
		s.cables.keep(name, sg)
		s.saveSightings()
	case portRising:
		if sg.rose.IsZero() {
			sg.rose = s.now()
			s.cables.keep(name, sg)
		}
		v.mu.Lock()
		up := v.aliveLocked()
		v.mu.Unlock()
		if up {
			return port, refuse(http.StatusConflict, "no Hard reset was sent: the %s port has plug count %d, not the %d it was seen with, and %s is up on the boot it had: the lab resets that port once it hears %s on a new boot, or not at all", port, p.PlugCount, sg.seen.PlugCount, name, name)
		}
	}
	seen := oob.Seen{ProductID: p.ProductID, PlugCount: p.PlugCount}
	err = s.cfg.OOB.Reset(ctx, sg.rid, &seen)
	var moved *oob.Changed
	var unsent *oob.NotSent
	var maybe *oob.MaybeSent
	switch {
	case errors.As(err, &unsent):
		return port, mayHaveGone{refuse(http.StatusServiceUnavailable, "no Hard reset was sent: %v", unsent)}
	case errors.As(err, &maybe):
		return port, mayHaveGone{refuse(http.StatusBadGateway, "the Hard reset of %s may have gone out: %v", name, maybe.Err)}
	case errors.As(err, &moved):
		s.cables.drop(name, moved.Reason, s.now())
		s.saveSightings()
		s.log.Printf("sighting: %s is no longer sighted: %s", name, moved.Reason)
		return port, refuse(http.StatusConflict, "no Hard reset was sent: %s. The Sighting of %s is dropped", moved.Reason, name)
	case err != nil:
		return port, refuse(http.StatusBadGateway, "no Hard reset reached %s: %v", name, err)
	}
	s.log.Printf("%s: Hard reset sent through the %s port", name, port)
	return port, nil
}

// provingResetOOB sends a Proving reset of rid as resetOOB sends a Hard
// reset: one controller call of v's at a time, called off by a retirement
// (or a policy transition) while it waits, and never for a Mac leaving the
// lab.
func (v *dev) provingResetOOB(ctx context.Context, rid int, seen *oob.Seen) error {
	v.oobCallMu.Lock()
	defer v.oobCallMu.Unlock()
	v.mu.Lock()
	if v.forgotten {
		name := v.d.Name
		v.mu.Unlock()
		return refuse(http.StatusNotFound, "no Proving reset was sent: %s is leaving the lab", name)
	}
	op, cancel := context.WithCancel(ctx)
	v.oobCallCancel = cancel
	v.mu.Unlock()
	defer func() { cancel(); v.mu.Lock(); v.oobCallCancel = nil; v.mu.Unlock() }()
	return v.s.cfg.OOB.Reset(op, rid, seen)
}

// leavingLab is whether v's enrollment is ending or has ended.
func (v *dev) leavingLab() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.forgotten || v.goneWhy != ""
}

// prove is a Proving reset: a Maintainer names the port of a Mac that
// shows no name, the lab resets that port, and the Mac is placed there only
// if it returns on a new boot and every other Lab-owned Mac keeps its boot
// id.
func (s *Server) prove(v *dev, rid int, by string) (*api.Proving, error) {
	d := v.snapshot()
	name, port := d.Name, "the "+oob.PortName(rid)+" port"
	switch {
	case d.IsVolunteer():
		return nil, refuse(http.StatusForbidden, "no Proving reset was sent: %s", neverAVolunteers(name))
	case s.cfg.OOB == nil:
		return nil, refuse(http.StatusServiceUnavailable, "no Proving reset was sent: %s", noController)
	case d.State == api.StateBusy || d.State == api.StateRecovering:
		return nil, refuse(http.StatusConflict, "%s is busy; cancel its job first", name)
	}
	if !s.cables.proving.TryLock() {
		return nil, refuse(http.StatusConflict, "no Proving reset was sent: another one is under way, and two at once prove nothing")
	}
	defer s.cables.proving.Unlock()
	// Admitted under v.mu, which a job's dispatch holds: from here no job is
	// given the Mac until the Proving reset has ended, however it ends.
	v.mu.Lock()
	d = v.d
	busy := d.State == api.StateBusy || d.State == api.StateRecovering || d.ActiveJob != "" || v.resetting
	if !busy {
		v.proving = true
	}
	v.mu.Unlock()
	if busy {
		return nil, refuse(http.StatusConflict, "%s is busy; cancel its job first", name)
	}
	defer func() {
		v.mu.Lock()
		v.proving = false
		v.bump()
		v.mu.Unlock()
	}()
	ctx := s.ctx
	s.sight(ctx)
	s.cables.mu.Lock()
	sg, sighted := s.cables.held[name]
	owner, down := s.cables.onPortLocked(rid), s.cables.down
	s.cables.mu.Unlock()
	switch {
	case down != "":
		return nil, refuse(http.StatusServiceUnavailable, "no Proving reset was sent: %s", down)
	case sighted:
		return nil, refuse(http.StatusConflict, "no Proving reset was sent: %s is sighted on the %s port, and a Proving reset is for a Mac the lab has no Sighting of", name, oob.PortName(sg.rid))
	case owner != "":
		return nil, refuse(http.StatusConflict, "no Proving reset was sent: %s is the cable of %s", port, owner)
	}
	p, err := s.cfg.OOB.Port(ctx, rid)
	switch {
	case err != nil:
		return nil, refuse(http.StatusServiceUnavailable, "no Proving reset was sent: the Out-of-band controller is not healthy: %v", err)
	case !p.Connected:
		return nil, refuse(http.StatusConflict, "no Proving reset was sent: nothing is connected to %s", port)
	case realName(p.Name) != "":
		return nil, refuse(http.StatusConflict, "no Proving reset was sent: %s shows the name %q, and a Proving reset is for a Mac that shows none", port, p.Name)
	case p.ProductID <= 0 || p.PlugCount <= 0:
		return nil, refuse(http.StatusConflict, "no Proving reset was sent: %s shows no product id or plug count, and the reset goes out only while the port is as it was read", port)
	}
	start := oob.Seen{ProductID: p.ProductID, PlugCount: p.PlugCount}
	others := map[string]string{}
	heard := map[string]bool{}
	for _, o := range s.allDevs() {
		o.mu.Lock()
		if o != v && !o.d.IsVolunteer() {
			others[o.d.Name] = o.d.BootID
			heard[o.d.Name] = o.aliveLocked()
		}
		o.mu.Unlock()
	}
	if d.State == api.StateReady || d.State == api.StateNew {
		v.setState(api.StateRecovering, "Proving reset of "+port+" by "+by)
		defer v.setState(d.State, "")
	}
	fail := func(format string, args ...any) (*api.Proving, error) {
		why := fmt.Sprintf(format, args...)
		s.cables.mu.Lock()
		s.cables.lost[name] = "the Proving reset of " + port + " at " + when(s.now()) + " did not count: " + why
		s.cables.mu.Unlock()
		s.log.Printf("%s: Proving reset of %s by %s did not count: %s", name, port, by, why)
		return nil, refuse(http.StatusConflict, "the Proving reset of %s did not count: %s. %s is not placed", port, why, name)
	}
	sent := s.now()
	s.log.Printf("%s: Proving reset of %s by %s", name, port, by)
	err = v.provingResetOOB(ctx, rid, &start)
	var moved *oob.Changed
	switch {
	case err != nil && v.leavingLab():
		return nil, refuse(http.StatusConflict, "no Proving reset was sent: %s left the lab while it waited at the controller", name)
	case errors.As(err, &moved):
		return nil, refuse(http.StatusConflict, "no Proving reset was sent: %s", moved.Reason)
	case err != nil:
		return nil, refuse(http.StatusBadGateway, "no Proving reset reached %s: %v", port, err)
	}
	restarted := func() []string {
		var out []string
		for _, o := range s.allDevs() {
			o.mu.Lock()
			if boot, ok := others[o.d.Name]; ok && o.d.BootID != boot {
				out = append(out, o.d.Name)
			}
			o.mu.Unlock()
		}
		return out
	}
	if v.waitFor(ctx, s.cfg.BootTimeout, func() bool { return v.d.BootID != d.BootID && v.d.LastSeen.After(sent) }) != nil {
		if also := restarted(); len(also) > 0 {
			return fail("%s did not return on a new boot within %s, and %s restarted: %s is its cable, or it restarted by itself", name, s.cfg.BootTimeout, strings.Join(also, ", "), port)
		}
		return fail("%s did not return on a new boot within %s", name, s.cfg.BootTimeout)
	}
	// A Mac the reset hit instead is down for a while: every other Mac that
	// was up is heard again, on the boot it had, before the result counts.
	back := s.now()
	for _, o := range s.allDevs() {
		o.mu.Lock()
		boot, other := others[o.d.Name]
		o.mu.Unlock()
		if !other || !heard[o.d.Name] {
			continue
		}
		if o.waitFor(ctx, 2*s.aliveWindow(), func() bool { return o.d.BootID != boot || o.heard.After(back) }) != nil {
			return fail("%s has not been heard since the reset: it may be the Mac on %s", o.d.Name, port)
		}
	}
	if also := restarted(); len(also) > 0 {
		return fail("%s restarted too, and a Proving reset counts only if every other Mac keeps its boot id", strings.Join(also, ", "))
	}
	p, err = s.cfg.OOB.Port(ctx, rid)
	switch {
	case err != nil:
		return fail("the Out-of-band controller did not say what %s shows now: %v", port, err)
	case !p.Connected || p.ProductID <= 0 || p.PlugCount <= 0:
		return fail("%s shows no connection, product id or plug count to remember %s by", port, name)
	}
	boot := v.snapshot().BootID
	s.cables.mu.Lock()
	if owner := s.cables.onPortLocked(rid); owner != "" && owner != name {
		s.cables.mu.Unlock()
		return fail("%s was sighted on %s meanwhile", owner, port)
	}
	s.cables.held[name] = sighting{rid: rid, seen: oob.Seen{ProductID: p.ProductID, PlugCount: p.PlugCount}, at: s.now(), proved: true, boot: boot}
	delete(s.cables.lost, name)
	s.cables.mu.Unlock()
	s.saveSightings()
	s.log.Printf("sighting: %s is placed on %s by the Proving reset of %s (product id %d, plug count %d)", name, port, by, p.ProductID, p.PlugCount)
	return &api.Proving{Device: name, Port: oob.PortName(rid), Cable: s.cables.cable(name)}, nil
}

// hOOBStatus is what the lab knows of a Mac's cable, and what the
// controller's ports show.
func (s *Server) hOOBStatus(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	v.mu.Lock()
	out := api.OOBStatus{Device: v.d.Name, Cable: v.cableLocked()}
	v.mu.Unlock()
	if out.Cable == nil || !callerOf(r).reads() {
		writeJSON(w, out)
		return
	}
	s.cables.mu.Lock()
	out.Controller = orStr(s.cables.down, api.ControllerHealthy)
	if s.cables.none {
		out.Controller = noController
	}
	for _, rid := range oob.RIDs {
		if p, ok := s.cables.ports[rid]; ok {
			out.Ports = append(out.Ports, api.OOBPort{Port: oob.PortName(rid), Connected: p.Connected, Name: p.Name, ProductID: p.ProductID, PlugCount: p.PlugCount, Mac: s.cables.onPortLocked(rid)})
		}
	}
	s.cables.mu.Unlock()
	writeJSON(w, out)
}

// recoverAfterStart runs recovery on every Lab-owned Mac that was up when
// labd stopped and did not return after it started, and writes the result
// for "Agents". A Mac gets its own watchdog and a boot first.
func (s *Server) recoverAfterStart(up []*dev) {
	if len(up) == 0 {
		return
	}
	select {
	case <-s.ctx.Done():
		return
	case <-s.clock().After(s.cfg.WatchdogIdleReset + s.cfg.BootTimeout):
	}
	var results []string
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, v := range up {
		v.mu.Lock()
		returned := v.heard.After(s.start)
		skip := returned || v.forgotten || v.d.State == api.StateLeft || v.d.State == api.StateBusy || v.d.State == api.StateRecovering || v.d.State == api.StateNeedsHands
		name := v.d.Name
		v.mu.Unlock()
		if skip {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			steps, ok := v.ladder(s.ctx, "did not return after the lab started again", s.cfg.BootTimeout, false)
			s.log.Printf("%s: recovery after the start: ok=%v steps=%v", name, ok, steps)
			line := name + " is back after a Hard reset."
			if !ok {
				why := v.snapshot().StateReason
				if a := v.snapshot().Action; a != nil {
					why = a.Why
				}
				line = name + " needs hands: " + strings.TrimSuffix(why, ".") + "."
			}
			mu.Lock()
			results = append(results, line)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(results) == 0 || s.ctx.Err() != nil {
		return
	}
	sort.Strings(results)
	s.agents.RecoveryAfterStart(results)
}

// RecoveryAfterStart is the result of the recovery labd ran, after it
// started, on the Lab-owned Macs that did not return.
func (a agentsLines) RecoveryAfterStart(results []string) {
	macs := "1 Lab-owned Mac"
	if len(results) != 1 {
		macs = fmt.Sprintf("%d Lab-owned Macs", len(results))
	}
	a.l.post(lineAgents, "After the lab started again "+macs+" did not return, and the lab ran recovery. "+strings.Join(results, " "))
}
