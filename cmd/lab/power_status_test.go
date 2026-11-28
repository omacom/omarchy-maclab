package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/client"
)

func TestStatusShowsPowerTelemetryAndDrainWarning(t *testing.T) {
	pct5 := 5
	drainUW := int64(-2122000)
	pct88 := 88
	chargeUW := int64(42654000)
	fleet := []client.Device{
		{Device: api.Device{
			Name:       "m2-max",
			State:      api.StateReady,
			Enrollment: api.Enrollment{Kind: api.KindLabOwned},
			Health: api.Health{
				SystemState: "running",
				Power: &api.Power{
					BatteryPercent:    &pct5,
					BatteryPowerUW:    &drainUW,
					InputPowerLimitUW: 7500000,
					Draining:          true,
				},
			},
		}},
		{Device: api.Device{
			Name:       "m1-pro",
			State:      api.StateReady,
			Enrollment: api.Enrollment{Kind: api.KindLabOwned},
			Health: api.Health{
				SystemState: "running",
				Power: &api.Power{
					BatteryPercent:    &pct88,
					BatteryPowerUW:    &chargeUW,
					InputPowerLimitUW: 139720000,
				},
			},
		}},
		{Device: api.Device{
			Name:       "studio",
			State:      api.StateReady,
			Enrollment: api.Enrollment{Kind: api.KindLabOwned},
			Health:     api.Health{SystemState: "running"},
		}},
	}
	c := labAt(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/devices":
			json.NewEncoder(w).Encode(fleet)
		case "/api/devices/m2-max":
			json.NewEncoder(w).Encode(fleet[0])
		case "/api/devices/m1-pro":
			json.NewEncoder(w).Encode(fleet[1])
		case "/api/devices/studio":
			json.NewEncoder(w).Encode(fleet[2])
		case "/api/devices/m2-max/leases", "/api/devices/m1-pro/leases", "/api/devices/studio/leases":
			json.NewEncoder(w).Encode([]api.LeaseEvent{})
		default:
			http.NotFound(w, r)
		}
	})

	out, err := printed(t, c, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "m2-max is losing charge on underpowered AC (5%, -2.1 W, 7.5 W limit): plug in its charger") {
		t.Errorf("lab status lacks drain warning:\n%s", out)
	}
	if strings.Contains(out, "m1-pro is losing charge") {
		t.Errorf("lab status warned on healthy AC:\n%s", out)
	}

	out, err = printed(t, c, "status", "m2-max")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "  power:       underpowered AC (7.5 W limit), battery 5%, -2.1 W, draining\n") {
		t.Errorf("lab status m2-max lacks power line:\n%s", out)
	}

	out, err = printed(t, c, "status", "m1-pro")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "  power:       AC (140 W limit), battery 88%, +42.7 W\n") {
		t.Errorf("lab status m1-pro lacks power line:\n%s", out)
	}

	out, err = printed(t, c, "status", "studio")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "  power:") {
		t.Errorf("lab status studio printed power line without telemetry:\n%s", out)
	}
}
