package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/client"
)

// enrollTokenLab is a labd that only answers POST /api/enroll-tokens, with a
// Join command, and remembers what the Maintainer asked for.
func enrollTokenLab(t *testing.T) (*client.Client, *[]map[string]any) {
	return joinCommandLab(t, " curl -fsSL https://lab.example/join | LAB_JOIN=enr_fixture_token bash", nil)
}

// joinCommandLab is enrollTokenLab answering with line as the Join command
// ("" is a labd with no signed lab-agent to hand out); revoked counts the
// Join commands canceled.
func joinCommandLab(t *testing.T, line string, revoked *int) (*client.Client, *[]map[string]any) {
	t.Helper()
	var asked []map[string]any
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" && r.URL.Path == "/api/enroll-tokens/rack-mac" && revoked != nil {
			*revoked++
			io.WriteString(w, `{"revoked":1}`)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/api/enroll-tokens" {
			http.NotFound(w, r)
			return
		}
		var m map[string]any
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &m)
		asked = append(asked, m)
		json.NewEncoder(w).Encode(map[string]any{"token": "enr_fixture_token", "expires": time.Now().Add(time.Hour), "join_command": line})
	}))
	t.Cleanup(hs.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MACLAB_URL", hs.URL)
	t.Setenv("MACLAB_TOKEN", "tok")
	c, err := client.Load()
	if err != nil {
		t.Fatal(err)
	}
	return c, &asked
}

// runLab runs `lab <cmd> <args>` and returns what it printed.
func runLab(t *testing.T, c *client.Client, cmd string, args ...string) (string, error) {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := dispatch(context.Background(), c, cmd, args)
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), err
}

// lab enroll is for Lab-owned Macs only: it has no flag that names a
// Volunteer, a contact or a login, and lab device changes a Mac's label and
// model, not whose it is.
func TestEnrollAndDeviceNameNoVolunteer(t *testing.T) {
	enroll := flag.NewFlagSet("enroll", flag.ContinueOnError)
	enrollFlags(enroll)
	device := flag.NewFlagSet("device", flag.ContinueOnError)
	deviceFlags(device)
	for _, gone := range []string{"volunteer", "contact", "login", "unlink", "oob-url", "oob-token", "no-oob"} {
		for _, fs := range []*flag.FlagSet{enroll, device} {
			if fs.Lookup(gone) != nil {
				t.Errorf("lab %s still takes --%s", fs.Name(), gone)
			}
		}
	}
	for fs, kept := range map[*flag.FlagSet][]string{enroll: {"label", "model", "revoke"}, device: {"label", "model"}} {
		var has []string
		fs.VisitAll(func(f *flag.Flag) { has = append(has, f.Name) })
		if !slices.Equal(has, kept) {
			t.Errorf("lab %s takes %v, want %v", fs.Name(), has, kept)
		}
	}
	for _, line := range strings.Split(usage, "\n") {
		if strings.Contains(line, "--volunteer ") || strings.Contains(line, "--contact") || strings.Contains(line, "--login") || strings.Contains(line, "--unlink") || strings.Contains(line, "--oob-") || strings.Contains(line, "--no-oob") {
			t.Errorf("lab's usage still offers: %s", strings.TrimSpace(line))
		}
	}
}

func TestEnrollMakesAJoinCommandForALabOwnedMac(t *testing.T) {
	c, asked := enrollTokenLab(t)
	out, err := runLab(t, c, "enroll", "rack-mac", "--label", "left desk", "--model", "desktop")
	if err != nil {
		t.Fatal(err)
	}
	if len(*asked) != 1 {
		t.Fatalf("labd was asked %d times", len(*asked))
	}
	got := (*asked)[0]
	if got["name"] != "rack-mac" || got["label"] != "left desk" || got["model"] != "desktop" {
		t.Fatalf("request: %v", got)
	}
	for _, field := range []string{"volunteer", "volunteer_name", "contact", "volunteer_login", "volunteer_id"} {
		if _, sent := got[field]; sent {
			t.Errorf("lab enroll sent %s: %v", field, got)
		}
	}
	if !strings.Contains(out, "enr_fixture_token") || !strings.Contains(out, "a Lab-owned Mac") {
		t.Fatalf("the token or the Mac's kind is not printed:\n%s", out)
	}
}

// With no signed lab-agent to hand out labd makes no Join command, and lab
// enroll prints no line to copy an agent and run it unchecked: the token
// alone, and why there is no line.
func TestEnrollPrintsNoUncheckedLineWhenLabdHasNoSignedAgent(t *testing.T) {
	c, _ := joinCommandLab(t, "", nil)
	out, err := runLab(t, c, "enroll", "rack-mac")
	if err != nil || !strings.Contains(out, "no signed lab-agent") || strings.Contains(out, "lab-agent setup") || strings.Contains(out, "copy lab-agent") || !strings.Contains(out, "enr_fixture_token") {
		t.Fatalf("lab enroll with no Join command: %v\n%s", err, out)
	}
}

func TestEnrollNeedsAMacName(t *testing.T) {
	c, asked := enrollTokenLab(t)
	if _, err := runLab(t, c, "enroll"); err == nil {
		t.Fatal("no Mac name was accepted")
	}
	if len(*asked) != 0 {
		t.Fatal("labd was asked anyway")
	}
}
