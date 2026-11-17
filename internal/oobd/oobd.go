// Package oobd is the Out-of-band controller: one process on the mini that
// sends a Hard reset to a Lab-owned Mac through one of the mini's rear ports.
// Every request names its port, and one reset tool run happens at a time.
package oobd

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/oob"
)

// MiniRIDs are the mini's three rear USB-C ports as the reset tool numbers
// them: middle 0, left 1, right 2, facing the back.
var MiniRIDs = []int{0, 1, 2}

// Tool is the reset tool. The real one is a program (ExecTool).
type Tool interface {
	// Run runs one command against the port with this RID and returns what
	// the tool printed.
	Run(ctx context.Context, rid int, command string) (string, error)
}

// Ports reads what this machine sees on a port, touching nothing.
type Ports interface {
	Read(ctx context.Context, rid int) (oob.Port, error)
	// Look is Read without the Mac's name, which is slow to read: what a
	// reset compares the moment before it is sent.
	Look(ctx context.Context, rid int) (oob.Port, error)
}

type Config struct {
	// Token is what labd presents. A controller with none is not made.
	Token string
	// RIDs are the ports of this machine; none means the mini's.
	RIDs  []int
	Tool  Tool
	Ports Ports
}

type Server struct {
	cfg Config
	log *log.Logger

	// tool is the tool's one turn, held for every tool run and across a
	// reset's last look at its port, so that one run happens at a time over
	// all ports. A channel and not a mutex: a reset whose caller leaves
	// while it waits stops waiting.
	tool chan struct{}
}

// takeTool waits for the tool's turn until ctx ends, and says whether it
// got it; dropTool gives the turn back.
func (s *Server) takeTool(ctx context.Context) bool {
	select {
	case s.tool <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Server) dropTool() { <-s.tool }

func New(cfg Config, logger *log.Logger) (*Server, error) {
	if cfg.Token == "" {
		return nil, errors.New("no token: anyone on this machine could reset a Mac")
	}
	if cfg.Tool == nil || cfg.Ports == nil {
		return nil, errors.New("no reset tool or no port reader")
	}
	if len(cfg.RIDs) == 0 {
		cfg.RIDs = MiniRIDs
	}
	return &Server{cfg: cfg, log: logger, tool: make(chan struct{}, 1)}, nil
}

// Loopback reports whether addr is a loopback address and a port, which is
// the only place the controller listens.
func Loopback(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return fmt.Errorf("%q is not ADDRESS:PORT", addr)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%q is not a loopback address: the Out-of-band controller listens on this machine only", addr)
	}
	return nil
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/ports/{rid}/status", s.port(s.hStatus))
	m.HandleFunc("POST /v1/ports/{rid}/reset", s.port(s.hReset))
	return m
}

func (s *Server) rids() string {
	list := make([]string, len(s.cfg.RIDs))
	for i, r := range s.cfg.RIDs {
		list[i] = strconv.Itoa(r)
	}
	return strings.Join(list, ", ")
}

// port lets a request through when it comes from this machine, carries the
// token and names one of this machine's ports, written as the plain number.
func (s *Server) port(h func(http.ResponseWriter, *http.Request, int)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			http.Error(w, "the Out-of-band controller answers this machine only", http.StatusForbidden)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		name := r.PathValue("rid")
		for _, rid := range s.cfg.RIDs {
			if name == strconv.Itoa(rid) {
				h(w, r, rid)
				return
			}
		}
		s.log.Printf("refused RID %q", name)
		http.Error(w, fmt.Sprintf("no port with RID %q: this machine's are %s", name, s.rids()), http.StatusBadRequest)
	}
}

// run is one tool run. A run that has started is left to finish even when
// its caller goes away: a tool killed halfway leaves the port in the mode it
// was working in.
func (s *Server) run(ctx context.Context, rid int, command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	out, err := s.cfg.Tool.Run(ctx, rid, command)
	s.log.Printf("RID %d %s: err=%v\n%s", rid, command, err, strings.TrimSpace(out))
	return out, err
}

// noConnection is the reset tool's answer on a port with no Mac at its far end.
const noConnection = "No connection detected"

func lastLine(s string) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	return l[len(l)-1]
}

func (s *Server) hStatus(w http.ResponseWriter, r *http.Request, rid int) {
	p, err := s.cfg.Ports.Read(r.Context(), rid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	p.RID = rid
	if r.URL.Query().Get("probe") == "1" {
		s.tool <- struct{}{}
		out, err := s.run(r.Context(), rid, "nop")
		s.dropTool()
		p.Tool, p.ToolFailed = strings.TrimSpace(out), err != nil
		// On a display or power the tool finds no Mac, which is its answer.
		if p.NoMac && strings.Contains(out, noConnection) {
			p.ToolFailed = false
		}
		if p.Tool == "" && err != nil {
			p.Tool = err.Error()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(p)
}

// seen reads the port state a reset carries: nothing at all, or both the
// product id and the plug count of the Sighting.
func seen(r *http.Request) (*oob.Seen, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	var want oob.Seen
	if err := json.Unmarshal(b, &fields); err != nil {
		return nil, errors.New("the body is not a JSON object")
	}
	if len(fields) != 2 || fields["product_id"] == nil || fields["plug_count"] == nil {
		return nil, errors.New("a reset carries product_id and plug_count, both, or no body")
	}
	if err := json.Unmarshal(b, &want); err != nil || want.ProductID <= 0 || want.PlugCount <= 0 {
		return nil, errors.New("product_id and plug_count are numbers above 0")
	}
	return &want, nil
}

func (s *Server) hReset(w http.ResponseWriter, r *http.Request, rid int) {
	want, err := seen(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The turn can be taken in the moment its caller left: look once more.
	if !s.takeTool(r.Context()) {
		s.log.Printf("RID %d reset: its caller left while it waited; nothing was sent", rid)
		return
	}
	defer s.dropTool()
	if r.Context().Err() != nil {
		s.log.Printf("RID %d reset: its caller left while it waited; nothing was sent", rid)
		return
	}
	if want != nil {
		p, err := s.cfg.Ports.Look(r.Context(), rid)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		var why string
		switch {
		case !p.Connected:
			why = "is not connected"
		case p.ProductID != want.ProductID:
			why = fmt.Sprintf("shows product id %d, not the %d it was seen with", p.ProductID, want.ProductID)
		case p.PlugCount != want.PlugCount:
			why = fmt.Sprintf("has plug count %d, not the %d it was seen with", p.PlugCount, want.PlugCount)
		}
		if why != "" {
			s.log.Printf("RID %d reset refused: the port %s", rid, why)
			http.Error(w, fmt.Sprintf("the port with RID %d %s: no reset was sent", rid, why), http.StatusConflict)
			return
		}
	}
	// 102 tells labd the reset is going out: from here it waits for the
	// answer however long it was willing to wait for its turn.
	w.WriteHeader(http.StatusProcessing)
	if out, err := s.run(r.Context(), rid, "reboot"); err != nil {
		http.Error(w, fmt.Sprintf("the reset tool failed on RID %d: %v: %s", rid, err, lastLine(out)), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "{\"rid\":%d}\n", rid)
}

// ExecTool is the pinned reset tool, omacom/macvdmtool, whose --rid is
// required and strictly parsed. It is run as `<Path> --rid <n> <command>`
// with the two commands the controller has a use for.
type ExecTool struct{ Path string }

func (t ExecTool) Run(ctx context.Context, rid int, command string) (string, error) {
	if command != "reboot" && command != "nop" {
		return "", fmt.Errorf("the controller does not run %q of the reset tool", command)
	}
	if rid < 0 {
		return "", fmt.Errorf("RID %d", rid)
	}
	if _, err := os.Stat(t.Path); err != nil {
		return "", fmt.Errorf("the reset tool is not installed at %s", t.Path)
	}
	cmd := exec.CommandContext(ctx, t.Path, "--rid", strconv.Itoa(rid), command)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%v: %s", err, lastLine(string(out)))
	}
	return string(out), nil
}
