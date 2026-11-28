package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/client"
)

func TestOOBAndStatusShowCandidateHintAndDuplicateHostnameWarning(t *testing.T) {
	var buf bytes.Buffer
	showCable(&buf, &api.OOBStatus{
		Device:            "m2-max",
		Controller:        api.ControllerHealthy,
		Cable:             &api.Cable{State: api.CableNotSighted, NoHardReset: "the lab has no Sighting of m2-max"},
		CandidatePort:     "left",
		CandidateBoard:    "j416c",
		DuplicateHostname: []string{"m4pro14"},
		Ports: []api.OOBPort{
			{Port: "left", Connected: true, ProductID: 29453, PlugCount: 55},
		},
	})
	out := buf.String()
	if !strings.Contains(out, "  hint:        the left port matches m2-max's board (j416c): `lab reset m2-max --prove left`\n") {
		t.Errorf("showCable lacks candidate hint:\n%s", out)
	}
	if !strings.Contains(out, "  hostname:    shared with m4pro14; duplicate hostnames block Thunderbolt Sighting") {
		t.Errorf("showCable lacks duplicate hostname line:\n%s", out)
	}

	fleet := []client.Device{
		{Device: api.Device{Name: "m1-pro", State: api.StateReady, Enrollment: api.Enrollment{Kind: api.KindLabOwned}, Facts: api.Facts{Hostname: "maralc-apple-macbookpro"}}},
		{Device: api.Device{Name: "m4pro14", State: api.StateReady, Enrollment: api.Enrollment{Kind: api.KindLabOwned}, Facts: api.Facts{Hostname: "maralc-apple-macbookpro"}}},
		{Device: api.Device{Name: "m2-max", State: api.StateReady, Enrollment: api.Enrollment{Kind: api.KindLabOwned}, Facts: api.Facts{Hostname: "omarchy-m2-max"}}},
	}
	c := labAt(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/devices" {
			json.NewEncoder(w).Encode(fleet)
			return
		}
		http.NotFound(w, r)
	})
	statusOut, err := printed(t, c, "status")
	if err != nil {
		t.Fatal(err)
	}
	want := `m1-pro, m4pro14 share hostname "maralc-apple-macbookpro": duplicate hostnames block Thunderbolt Sighting (rename with hostnamectl set-hostname)`
	if !strings.Contains(statusOut, want) {
		t.Errorf("lab status lacks duplicate hostname warning:\n%s", statusOut)
	}
}
