// Package oob is labd's side of out-of-band control: Hard reset through the
// one Out-of-band controller on the mini, which drives a port, not a Mac.
package oob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"time"
)

// RIDs are the mini's three rear USB-C ports as the reset tool numbers them,
// left to right facing the back.
var RIDs = []int{1, 0, 2}

// PortName is how people name the port with this RID: left, middle or
// right, facing the back of the mini.
func PortName(rid int) string {
	switch rid {
	case 1:
		return "left"
	case 0:
		return "middle"
	case 2:
		return "right"
	}
	return fmt.Sprintf("RID %d", rid)
}

// RIDOf is the RID of the port people call name.
func RIDOf(name string) (int, bool) {
	for _, rid := range RIDs {
		if PortName(rid) == name {
			return rid, true
		}
	}
	return 0, false
}

// Port is what the Out-of-band controller sees on one of its ports.
type Port struct {
	RID       int  `json:"rid"`
	Connected bool `json:"connected"`
	// Name is the name the Mac's Linux shows over a Thunderbolt link; empty
	// with no link, a Mac that is hung or off, or a USB 2 cable.
	Name string `json:"name,omitempty"`
	// ProductID is the partner's USB-PD product id: the same for two Macs of one model.
	ProductID int `json:"product_id,omitempty"`
	// PlugCount goes up each time something is plugged in; it starts again
	// when the mini restarts.
	PlugCount int `json:"plug_count"`
	// NoMac is a port connected on CC alone, with no USB2 or CIO and no
	// USB-PD product id: a display or power, which the reset tool cannot
	// reach and the lab leaves alone.
	NoMac bool `json:"no_mac,omitempty"`
	// Tool is the reset tool's answer on this port, when it was asked for.
	Tool       string `json:"tool,omitempty"`
	ToolFailed bool   `json:"tool_failed,omitempty"`
}

// Seen is the state of a port at a Sighting. A reset that carries it goes
// out only while the port still shows it.
type Seen struct {
	ProductID int `json:"product_id"`
	PlugCount int `json:"plug_count"`
}

// Changed is the controller's refusal of a reset whose port is no longer as
// it was seen: the Sighting behind it is to be dropped.
type Changed struct{ Reason string }

func (e *Changed) Error() string { return e.Reason }

// Client talks to the Out-of-band controller on this machine.
type Client struct {
	// URL is the controller's loopback address, such as http://127.0.0.1:7780.
	URL string
	// TokenFile holds the controller's token, in a file only labd's account
	// reads. It is read for every request, so a token made after labd
	// started needs no restart.
	TokenFile string
}

// NewClient checks that addr is on this machine: the controller never
// listens anywhere else, and its token goes nowhere else.
func NewClient(addr, tokenFile string) (*Client, error) {
	u, err := url.Parse(addr)
	if err != nil || u.Scheme != "http" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.User != nil {
		return nil, fmt.Errorf("the Out-of-band controller's address %q is not http://127.0.0.1:PORT", addr)
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() || u.Port() == "" {
		return nil, fmt.Errorf("the Out-of-band controller's address %q is not a loopback address with a port: it listens on this machine only", addr)
	}
	if tokenFile == "" {
		return nil, errors.New("the Out-of-band controller's token has no file")
	}
	return &Client{URL: "http://" + u.Host, TokenFile: tokenFile}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, timeout time.Duration) ([]byte, error) {
	token, err := ReadToken(c.TokenFile)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("the Out-of-band controller does not answer: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	text := strings.TrimSpace(string(b))
	switch resp.StatusCode {
	case http.StatusOK:
		return b, nil
	case http.StatusConflict:
		return nil, &Changed{Reason: text}
	}
	return nil, fmt.Errorf("the Out-of-band controller refused (%s): %s", resp.Status, text)
}

func (c *Client) status(ctx context.Context, rid int, query string, timeout time.Duration) (Port, error) {
	var p Port
	b, err := c.do(ctx, "GET", fmt.Sprintf("/v1/ports/%d/status%s", rid, query), nil, timeout)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("the Out-of-band controller's answer for RID %d: %w", rid, err)
	}
	if p.RID != rid {
		return p, fmt.Errorf("the Out-of-band controller answered for RID %d when asked for RID %d", p.RID, rid)
	}
	return p, nil
}

// Port asks what the controller sees on a port. No tool runs and nothing is reset.
func (c *Client) Port(ctx context.Context, rid int) (Port, error) {
	return c.status(ctx, rid, "", 30*time.Second)
}

// Probe is Port with the reset tool's own answer on that port. It waits its
// turn behind a reset and resets no Mac. The tool does take the mini's own
// port controller into its debug mode for any command, this one included,
// which is why Port asks for none.
func (c *Client) Probe(ctx context.Context, rid int) (Port, error) {
	return c.status(ctx, rid, "?probe=1", 2*time.Minute)
}

// Reset sends a Hard reset to a port. With seen, the controller sends it only
// while the port still shows that state, and answers *Changed otherwise.
// With nil the port alone is named, and nothing is compared.
//
// ctx bounds the wait for the controller's turn. Once the controller says
// the reset is going out (102), its answer is waited for whatever ctx does:
// a reset that went out is never reported as not sent. A failure after
// that, or a connection lost after the request reached the controller and
// before any answer, is *MaybeSent.
func (c *Client) Reset(ctx context.Context, rid int, seen *Seen) error {
	var body []byte
	if seen != nil {
		body, _ = json.Marshal(seen)
	}
	onSending, _ := ctx.Value(sendingKey{}).(func())
	run, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	var mu sync.Mutex
	sending, wrote, answered := false, false, false
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		if !sending {
			cancel()
		}
	})
	defer stop()
	run = httptrace.WithClientTrace(run, &httptrace.ClientTrace{
		WroteRequest: func(i httptrace.WroteRequestInfo) {
			mu.Lock()
			wrote = wrote || i.Err == nil
			mu.Unlock()
		},
		GotFirstResponseByte: func() {
			mu.Lock()
			answered = true
			mu.Unlock()
		},
		Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
			if code == http.StatusProcessing {
				mu.Lock()
				first := !sending
				sending = true
				mu.Unlock()
				if first && onSending != nil {
					onSending()
				}
			}
			return nil
		}})
	_, err := c.do(run, "POST", fmt.Sprintf("/v1/ports/%d/reset", rid), body, 2*time.Minute)
	mu.Lock()
	sent, asked, heard := sending, wrote, answered
	mu.Unlock()
	switch {
	case err == nil:
		return nil
	case sent:
		return &MaybeSent{Err: err}
	case ctx.Err() != nil:
		return &NotSent{Err: ctx.Err()}
	case asked && !heard:
		return &MaybeSent{Err: err}
	}
	return err
}

type sendingKey struct{}

// OnSending has Reset call f as the controller says the reset is going out
// (its 102), before the tool's answer: the moment of the reset, for a caller
// that must know the Mac as it was then.
func OnSending(ctx context.Context, f func()) context.Context {
	return context.WithValue(ctx, sendingKey{}, f)
}

// MaybeSent is a reset that may have gone out although no success came
// back: the controller said it was going out and then failed, or the
// connection was lost after the request had reached the controller. A
// caller takes it as sent.
type MaybeSent struct{ Err error }

func (e *MaybeSent) Error() string { return "it may have gone out: " + e.Err.Error() }

func (e *MaybeSent) Unwrap() error { return e.Err }

// NotSent is a reset given up before the controller said it was going out:
// its caller stopped waiting for its turn. The controller asks for its
// caller when the turn comes, so it sends nothing then; only in the moment
// the turn comes as the caller gives up may it still begin the reset, which
// is why labd watches the Mac after a NotSent as after a reset.
type NotSent struct{ Err error }

func (e *NotSent) Error() string {
	if errors.Is(e.Err, context.DeadlineExceeded) {
		return "its turn at the Out-of-band controller did not come in time"
	}
	return "it was called off while it waited for the Out-of-band controller"
}

func (e *NotSent) Unwrap() error { return e.Err }
