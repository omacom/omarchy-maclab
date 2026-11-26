package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// lab agent-update <mac> has a Mac take the lab's lab-agent; what labd
// refuses it prints as labd said it.
func TestAgentUpdateSaysWhatChangedAndPrintsARefusalAsItIs(t *testing.T) {
	const volunteers = "vol-air is a Volunteer's Mac, and the lab's lab-agent is still on trial on Lab-owned Macs: it reaches a Volunteer's Mac once it has run well there"
	var asked []string
	c := labAt(t, func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.RequestURI())
		switch r.URL.Path {
		case "/api/devices/m1-pro/agent-update":
			json.NewEncoder(w).Encode(api.AgentUpdate{From: "0.10.0", To: "0.11.0", LabRelease: "v0.2.1", Changed: true})
		case "/api/devices/m2-max/agent-update":
			json.NewEncoder(w).Encode(api.AgentUpdate{From: "0.11.0", To: "0.11.0", LabRelease: "v0.2.1"})
		case "/api/devices/vol-air/agent-update":
			http.Error(w, volunteers, http.StatusConflict)
		default:
			http.NotFound(w, r)
		}
	})
	out, err := printed(t, c, "agent-update", "m1-pro")
	if err != nil || out != "m1-pro: lab-agent 0.10.0 -> 0.11.0 (Lab release v0.2.1), its signature checked on the Mac\n" {
		t.Fatalf("lab agent-update m1-pro: %q %v", out, err)
	}
	if out, err := printed(t, c, "agent-update", "m2-max"); err != nil || out != "m2-max: lab-agent 0.11.0 (Lab release v0.2.1) is already the lab's\n" {
		t.Fatalf("lab agent-update of an up-to-date Mac: %q %v", out, err)
	}
	if out, err := printed(t, c, "agent-update", "vol-air"); err == nil || err.Error() != volunteers || out != "" {
		t.Fatalf("lab agent-update of a Volunteer's Mac: %q %v", out, err)
	}
	if _, err := printed(t, c, "agent-update"); err == nil || !strings.Contains(err.Error(), "lab agent-update <mac>") {
		t.Fatalf("lab agent-update with no Mac: %v", err)
	}
	if got := strings.Join(asked, ", "); got != "POST /api/devices/m1-pro/agent-update, POST /api/devices/m2-max/agent-update, POST /api/devices/vol-air/agent-update" {
		t.Errorf("asked: %s", got)
	}
	if !strings.Contains(usage, "lab agent-update <mac>") {
		t.Error("lab's help does not name agent-update")
	}
}

// lab status shows an agent older than the lab's, and adds one line when
// the lab runs a newer release than this lab.
func TestStatusShowsAnOlderAgentAndOneLineWhenTheLabRunsANewerRelease(t *testing.T) {
	devices := `[{"name":"m1-pro","state":"ready","kind":"lab_owned","facts":{"agent_version":"0.10.0"},"lab_agent":"0.11.0"},
{"name":"m2-max","state":"ready","kind":"lab_owned","facts":{"agent_version":"0.11.0"}},
{"name":"vol-air","state":"ready","kind":"volunteer","volunteer":true,"facts":{"agent_version":"0.9.0"},"lab_agent":"0.11.0"}]`
	release := "v0.3.0"
	c := labAt(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Lab-Release", release)
		switch r.URL.Path {
		case "/api/devices":
			io.WriteString(w, devices)
		case "/api/devices/m1-pro":
			io.WriteString(w, `{"name":"m1-pro","state":"ready","kind":"lab_owned","facts":{"agent_version":"0.10.0"},"lab_agent":"0.11.0"}`)
		case "/api/devices/vol-air":
			io.WriteString(w, `{"name":"vol-air","state":"ready","kind":"volunteer","volunteer":true,"facts":{"agent_version":"0.9.0"},"lab_agent":"0.11.0"}`)
		case "/api/devices/m2-max":
			io.WriteString(w, `{"name":"m2-max","state":"ready","kind":"lab_owned","facts":{"agent_version":"0.11.0"}}`)
		default:
			http.NotFound(w, r)
		}
	})
	was := version
	t.Cleanup(func() { version = was })
	version = "v0.2.0"
	newer := "Omarchy Mac Lab runs a newer release, v0.3.0. Update lab: curl -fsSL " + c.URL + "/install/lab | bash\n"

	out, err := printed(t, c, "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"m1-pro runs lab-agent 0.10.0, older than the lab's 0.11.0: lab agent-update m1-pro\n",
		"vol-air runs lab-agent 0.9.0, older than the lab's 0.11.0: its Volunteer updates it at the Mac\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lab status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "m2-max runs lab-agent") {
		t.Errorf("lab status says something of an agent that is the lab's:\n%s", out)
	}
	if strings.Count(out, newer) != 1 || !strings.HasSuffix(out, newer) {
		t.Errorf("lab status does not end with the one line about the newer release:\n%s", out)
	}

	out, err = printed(t, c, "status", "m1-pro")
	if err != nil || !strings.Contains(out, "  lab-agent:   0.10.0, older than the lab's 0.11.0 (lab agent-update m1-pro)\n") || strings.Count(out, newer) != 1 {
		t.Errorf("lab status m1-pro: %v\n%s", err, out)
	}
	if out, _ = printed(t, c, "status", "vol-air"); !strings.Contains(out, "  lab-agent:   0.9.0, older than the lab's 0.11.0 (its Volunteer updates it at the Mac)\n") {
		t.Errorf("lab status vol-air:\n%s", out)
	}
	if out, _ = printed(t, c, "status", "m2-max"); !strings.Contains(out, "  lab-agent:   0.11.0\n") || strings.Contains(out, "older") {
		t.Errorf("lab status m2-max:\n%s", out)
	}

	// The same release, a newer lab, a Release candidate of the lab's
	// release: no line.
	for _, mine := range []string{"v0.3.0", "v0.4.0", "v0.3.0-rc.2", "dev"} {
		version = mine
		if out, _ := printed(t, c, "status"); strings.Contains(out, "newer release") {
			t.Errorf("lab %s against a lab on %s:\n%s", mine, release, out)
		}
	}
}
