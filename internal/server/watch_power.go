package server

import (
	"fmt"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// powerHold is how long a Lab-owned Mac must stay on battery, draining on
// underpowered AC, or low without charging before "Agents" is told.
const powerHold = 5 * time.Minute

// lookPower watches each live Lab-owned Mac's AC and battery telemetry,
// telling "Agents" once when one stays on battery, drains on underpowered AC,
// or sits at a low charge without charging. Offline Macs are skipped so an
// existing power alert stays latched until the Mac returns on healthy power.
func (s *Server) lookPower(now time.Time) {
	for _, v := range s.allDevs() {
		v.mu.Lock()
		d := v.d
		alive := v.aliveLocked()
		v.mu.Unlock()
		if d.IsVolunteer() || d.State == api.StateLeft || !alive {
			continue
		}
		bad, detail := d.Health.PowerTrouble()
		key := "power:" + d.ID
		rec := d.Name + " is on AC power again."
		if sum := d.Health.PowerSummary(); sum != "" {
			rec = fmt.Sprintf("%s is on AC power again: %s.", d.Name, sum)
		}
		s.judge(now, key, bad, powerHold,
			fixed(fmt.Sprintf("%s is %s.", d.Name, detail)),
			fixed(rec))
	}
}
