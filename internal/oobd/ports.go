package oobd

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/oob"
)

// SystemPorts reads the ports with what macOS has: ioreg for the port
// controllers, system_profiler for the name a Mac shows over Thunderbolt.
// Neither needs root and neither touches the port.
type SystemPorts struct {
	Ioreg    string
	Profiler string
}

func DefaultPorts() SystemPorts {
	return SystemPorts{Ioreg: "/usr/sbin/ioreg", Profiler: "/usr/sbin/system_profiler"}
}

func (sp SystemPorts) Read(ctx context.Context, rid int) (oob.Port, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	p, receptacle, err := sp.look(ctx, rid)
	if err != nil || !p.Connected {
		return p, err
	}
	// A port with no name is still a port: a Mac that is hung, off or on a
	// USB 2 link shows none.
	if out, err := exec.CommandContext(ctx, sp.Profiler, "SPThunderboltDataType", "-json").Output(); err == nil {
		if names, err := peerNames(out); err == nil {
			p.Name = names[receptacle]
		}
	}
	return p, nil
}

func (sp SystemPorts) Look(ctx context.Context, rid int) (oob.Port, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	p, _, err := sp.look(ctx, rid)
	return p, err
}

// look is the one read of ioreg: the port, and the receptacle its name
// would be found on.
func (sp SystemPorts) look(ctx context.Context, rid int) (oob.Port, int, error) {
	out, err := exec.CommandContext(ctx, sp.Ioreg, "-l", "-w0", "-r", "-c", "AppleHPM").Output()
	if err != nil {
		return oob.Port{}, 0, fmt.Errorf("reading the ports with %s: %v", sp.Ioreg, err)
	}
	h, ok := parsePorts(string(out))[rid]
	if !ok {
		return oob.Port{}, 0, fmt.Errorf("this machine has no port with RID %d", rid)
	}
	p := oob.Port{RID: rid, Connected: h.connected, ProductID: h.productID, PlugCount: h.plugCount}
	if !h.connected {
		p.ProductID = 0
	}
	p.NoMac = h.connected && !h.macLink && p.ProductID == 0
	return p, h.receptacle, nil
}

// hpmPort is one USB-C port controller as ioreg shows it.
type hpmPort struct {
	rid        int
	receptacle int
	connected  bool
	// macLink is USB2 or CIO among the port's active transports.
	macLink   bool
	productID int
	plugCount int
}

var (
	nodeRE = regexp.MustCompile(`^([ |]*)\+-o (.+?)  <class ([A-Za-z0-9_]+)[,>]`)
	propRE = regexp.MustCompile(`^[ |]*"([^"]+)" = (.*)$`)
)

// parsePorts reads `ioreg -l -w0 -r -c AppleHPM`: one tree for each port
// controller, found by the RID the reset tool matches. Under it the port's
// own entry says whether something is connected and how often something was
// plugged in, and the entry of its USB-PD partner carries the product id.
// Entries further down (a cable, a hub's devices) have ids of their own and
// are not read.
func parsePorts(text string) map[int]hpmPort {
	ports := map[int]hpmPort{}
	var cur hpmPort
	inTree, haveRID := false, false
	class := ""
	flush := func() {
		if inTree && haveRID {
			ports[cur.rid] = cur
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if m := nodeRE.FindStringSubmatch(line); m != nil {
			if m[1] == "" {
				flush()
				cur, inTree, haveRID = hpmPort{}, true, false
			}
			class = m[3]
			continue
		}
		m := propRE.FindStringSubmatch(line)
		if m == nil || !inTree {
			continue
		}
		key, value := m[1], strings.TrimSpace(m[2])
		n, numErr := strconv.Atoi(value)
		switch {
		case key == "RID" && !haveRID && numErr == nil:
			cur.rid, haveRID = n, true
		case strings.HasPrefix(class, "AppleHPMInterface"):
			switch key {
			case "ConnectionActive":
				cur.connected = value == "Yes"
			case "Plug Event Count":
				cur.plugCount = n
			case "PortNumber":
				cur.receptacle = n
			case "TransportsActive":
				cur.macLink = strings.Contains(value, `"USB2"`) || strings.Contains(value, `"CIO"`)
			}
		case class == "IOPortTransportComponentCCUSBPDSOP" && key == "Product ID" && numErr == nil:
			cur.productID = n
		}
	}
	flush()
	return ports
}

// peerNames reads `system_profiler SPThunderboltDataType -json`: for each of
// this machine's Thunderbolt buses, the receptacle it belongs to and the
// name of the one machine on it. A receptacle with several devices behind
// it has no single Mac and gets no name.
func peerNames(profile []byte) (map[int]string, error) {
	var doc struct {
		Buses []struct {
			Receptacle struct {
				ID string `json:"receptacle_id_key"`
			} `json:"receptacle_1_tag"`
			Items []struct {
				Name       string `json:"_name"`
				DeviceName string `json:"device_name_key"`
			} `json:"_items"`
		} `json:"SPThunderboltDataType"`
	}
	if err := json.Unmarshal(profile, &doc); err != nil {
		return nil, err
	}
	names := map[int]string{}
	for _, bus := range doc.Buses {
		receptacle, err := strconv.Atoi(bus.Receptacle.ID)
		if err != nil || len(bus.Items) != 1 {
			continue
		}
		name := bus.Items[0].DeviceName
		if name == "" {
			name = bus.Items[0].Name
		}
		if name != "" {
			names[receptacle] = name
		}
	}
	return names, nil
}
