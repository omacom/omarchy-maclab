package server

import (
	"fmt"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// markEverSightedPrefix persists in line_marks whether an active Lab-owned
// enrollment (by device ID) has ever held a Sighting, so a restart of labd
// before partHold never forgets a lost Sighting.
const markEverSightedPrefix = "ops:ever-sighted:"

func (s *Server) recordEverSightedID(devID string) {
	if s.lines == nil || devID == "" {
		return
	}
	s.lines.markOnce(markEverSightedPrefix+devID, "1")
}

func (s *Server) recordEverSightedName(name string) {
	if s.lines == nil {
		return
	}
	s.mu.Lock()
	v := s.devs[name]
	s.mu.Unlock()
	if v == nil {
		return
	}
	v.mu.Lock()
	id, vol := v.d.ID, v.d.IsVolunteer()
	v.mu.Unlock()
	if !vol {
		s.recordEverSightedID(id)
	}
}

func (s *Server) hasEverBeenSighted(devID string) bool {
	if s.lines == nil || devID == "" {
		return false
	}
	_, ok := s.lines.mark(markEverSightedPrefix + devID)
	return ok
}

// lookSightings watches Lab-owned Macs that have held a Sighting and tells
// "Agents" once when one stays unsighted past partHold while the Out-of-band
// controller itself is healthy.
func (s *Server) lookSightings(now time.Time) {
	if s.oob.Empty() {
		return
	}
	s.cables.mu.Lock()
	down := s.cables.down != ""
	s.cables.mu.Unlock()
	if down {
		return
	}
	for _, v := range s.allDevs() {
		v.mu.Lock()
		d := v.d
		v.mu.Unlock()
		if d.IsVolunteer() || d.State == api.StateLeft {
			continue
		}
		s.cables.mu.Lock()
		sg, sighted := s.cables.held[d.Name]
		lost := s.cables.lost[d.Name]
		s.cables.mu.Unlock()
		key := "sighting:" + d.ID
		if sighted {
			s.recordEverSightedID(d.ID)
			port := s.oob.PortName(sg.rid)
			s.judge(now, key, false, partHold,
				fixed(""),
				fixed(fmt.Sprintf("%s is sighted again on the %s port.", d.Name, port)))
			continue
		}
		if !s.hasEverBeenSighted(d.ID) {
			continue
		}
		detail := ""
		if lost != "" {
			detail = " (" + lost + ")"
		}
		s.judge(now, key, true, partHold,
			fixed(fmt.Sprintf("%s lost its Sighting%s. It has no Hard reset until sighted again or proved.", d.Name, detail)),
			fixed(d.Name+" is sighted again."))
	}
}
