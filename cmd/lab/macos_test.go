package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/omacom/omarchy-maclab/internal/api"
)

// lab macos at-mac <mac> says a person is at the Mac whose macOS run waits
// for one, and prints when macOS boots; a refusal is printed as labd said
// it. Its tool says the same.
func TestMacOSAtMacSaysWhenMacOSBoots(t *testing.T) {
	const nobodyWaits = "m2-max: no macOS run of this Mac waits for a person"
	var asked []string
	c := labAt(t, func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.Path)
		run := api.MacOSRun{ID: "mos-1", Set: "bench-1", Version: "27.2.1", Build: "25C56", State: api.MacOSQueued}
		switch r.URL.Path {
		case "/api/devices/m1-pro/macos/at-mac":
			run.Job = "j1"
			json.NewEncoder(w).Encode(run)
		case "/api/devices/m1-air/macos/at-mac":
			json.NewEncoder(w).Encode(run)
		case "/api/devices/m2-max/macos/at-mac":
			http.Error(w, nobodyWaits, http.StatusConflict)
		default:
			http.NotFound(w, r)
		}
	})
	out, err := printed(t, c, "macos", "at-mac", "m1-pro")
	if err != nil || out != "m1-pro: macOS run mos-1, bench-1's Full list at macOS 27.2.1 (25C56): job j1 boots macOS now. Type its password and log in when it asks.\n" {
		t.Fatalf("lab macos at-mac m1-pro: %q %v", out, err)
	}
	if out, err := printed(t, c, "macos", "at-mac", "m1-air"); err != nil || !strings.Contains(out, "macOS boots at the Mac's next slot") {
		t.Fatalf("lab macos at-mac of a Mac not idle yet: %q %v", out, err)
	}
	if out, err := printed(t, c, "macos", "at-mac", "m2-max"); err == nil || err.Error() != nobodyWaits || out != "" {
		t.Fatalf("lab macos at-mac with no run waiting: %q %v", out, err)
	}
	if _, err := printed(t, c, "macos", "at-mac"); err == nil || !strings.Contains(err.Error(), "lab macos at-mac <mac>") {
		t.Fatalf("lab macos at-mac with no Mac: %v", err)
	}
	if got := strings.Join(asked, ", "); got != "POST /api/devices/m1-pro/macos/at-mac, POST /api/devices/m1-air/macos/at-mac, POST /api/devices/m2-max/macos/at-mac" {
		t.Errorf("asked: %s", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := newMCP(c).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "lab_macos_at_mac", Arguments: map[string]any{"device": "m1-pro"}})
	if err != nil {
		t.Fatal(err)
	}
	var said string
	for _, part := range res.Content {
		if text, ok := part.(*mcp.TextContent); ok {
			said += text.Text
		}
	}
	if res.IsError || !strings.Contains(said, "job j1 boots macOS now") {
		t.Errorf("lab_macos_at_mac answered (error %v): %s", res.IsError, said)
	}
	if res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "lab_macos_at_mac", Arguments: map[string]any{"device": "m2-max"}}); err == nil && !res.IsError {
		t.Error("lab_macos_at_mac with no run waiting did not fail")
	}
}
