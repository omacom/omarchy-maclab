package server

// How labd listens: on this machine only, never the LAN or the tailnet.
//
//   - A Unix socket only the account labd runs as can open. cloudflared
//     dials it, so it is the tunnel's: the only place CF-Connecting-IP and
//     what the edge forwards are believed (see edge.go).
//   - A loopback port for callers on the same machine (the Builder, the
//     Out-of-band controller's client side, break-glass), which believes none
//     of that: any local process could forge it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ListenConfig is where labd serves.
type ListenConfig struct {
	// Addr is the loopback port, host:port.
	Addr string
	// Socket is the path of the tunnel's Unix socket; empty serves none.
	Socket string
}

// Check is why labd must not serve as lc says, or nil.
func (lc ListenConfig) Check() error {
	host, _, err := net.SplitHostPort(lc.Addr)
	if err != nil {
		return fmt.Errorf("--listen %q: %v", lc.Addr, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("--listen %s is beyond this machine: labd listens on loopback and on the tunnel's socket (--socket), never on the LAN or the tailnet", lc.Addr)
	}
	if lc.Socket != "" && !filepath.IsAbs(lc.Socket) {
		return fmt.Errorf("--socket %q: give the socket's full path", lc.Socket)
	}
	return nil
}

// Server limits. A request's headers must arrive within headerTimeout, and an
// idle keep-alive connection closes after idleTimeout, which stays above the
// 90 s cloudflared keeps a connection in its pool. There is no overall read
// or write timeout: long polls, the event stream, the console, an exec of up
// to an hour and downloads of large artifacts all run longer.
const (
	headerTimeout = 10 * time.Second
	idleTimeout   = 2 * time.Minute
	maxHeaderSize = 64 << 10
)

// Server is an http.Server for h as lc says. It knows which requests came
// through the tunnel's socket.
func (lc ListenConfig) Server(h http.Handler) (*http.Server, error) {
	if err := lc.Check(); err != nil {
		return nil, err
	}
	return &http.Server{Handler: h, ReadHeaderTimeout: headerTimeout, IdleTimeout: idleTimeout, MaxHeaderBytes: maxHeaderSize,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if _, ok := c.(*net.UnixConn); ok {
				return onTunnel(ctx)
			}
			return ctx
		}}, nil
}

// Listen opens lc's listeners: the loopback port first, then the socket if
// lc names one. The socket is made for its owner alone (0600); one left by a
// labd that died is replaced, and anything else at its path stops labd.
func (lc ListenConfig) Listen() ([]net.Listener, error) {
	if err := lc.Check(); err != nil {
		return nil, err
	}
	tcp, err := net.Listen("tcp", lc.Addr)
	if err != nil {
		return nil, err
	}
	if lc.Socket == "" {
		return []net.Listener{tcp}, nil
	}
	unix, err := listenSocket("--socket", lc.Socket)
	if err != nil {
		tcp.Close()
		return nil, err
	}
	return []net.Listener{tcp, unix}, nil
}

// listenSocket opens the Unix socket the flag named flag gives.
func listenSocket(flag, path string) (net.Listener, error) {
	switch st, err := os.Lstat(path); {
	case err == nil && st.Mode()&os.ModeSocket == 0:
		return nil, fmt.Errorf("%s %s: something that is not a socket is there", flag, path)
	case err == nil:
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s %s: another labd is serving on it", flag, path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	// The socket is born closed to everyone else: a chmod after a bind at
	// its path would leave a moment when it is not. It is bound in a folder
	// beside its path that only its owner can enter, and moved to its path
	// once its mode is set. The umask would do it in one step, but the umask
	// is the whole process's: whatever else makes a file at that moment gets
	// it too, and two sockets opened at once leave it changed for good.
	born, err := os.MkdirTemp(filepath.Dir(path), ".s")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(born)
	first := filepath.Join(born, "s")
	ln, err := net.Listen("unix", first)
	if err != nil {
		return nil, err
	}
	unix := ln.(*net.UnixListener)
	unix.SetUnlinkOnClose(false)
	if err = os.Chmod(first, 0o600); err == nil {
		err = os.Rename(first, path)
	}
	if err != nil {
		unix.Close()
		return nil, err
	}
	return &socket{UnixListener: unix, path: path}, nil
}

// socket is a listening Unix socket that was moved to path after its bind.
// Closing it removes it from there.
type socket struct {
	*net.UnixListener
	path string
	once sync.Once
}

func (s *socket) Close() error {
	s.once.Do(func() { os.Remove(s.path) })
	return s.UnixListener.Close()
}

// Serve serves hs on every listener and returns when one of them stops.
func Serve(hs *http.Server, lns ...net.Listener) error {
	errs := make(chan error, len(lns))
	for _, ln := range lns {
		go func() { errs <- hs.Serve(ln) }()
	}
	return <-errs
}

type tunnelKey struct{}

// onTunnel marks a connection's context as the tunnel's socket's.
func onTunnel(ctx context.Context) context.Context {
	return context.WithValue(ctx, tunnelKey{}, true)
}

// viaTunnel reports whether r came in on the tunnel's socket.
func viaTunnel(r *http.Request) bool {
	ok, _ := r.Context().Value(tunnelKey{}).(bool)
	return ok
}

// bodyWait is how long labd waits for the next bytes of a request's body,
// its first ones included; tests shorten it.
var bodyWait = 30 * time.Second

// bodyDeadlines gives every request with a body a read deadline before
// anything reads it: bodyWait from now, and from each read again, so a body
// that keeps coming, however long, is never cut. A body that stops, or that
// nothing reads, is cut at the deadline, Go's own drain of an unread body
// after the answer included: so nobody holds a connection open with a body
// labd never asked for. A handler that wants its body sooner says so with
// readBodyBy. Once the body has come whole the deadline is lifted, so a held
// answer is not cut by a read the server makes after it.
func bodyDeadlines(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			h.ServeHTTP(w, r)
			return
		}
		c := &bodyClock{rc: http.NewResponseController(w)}
		c.wait()
		r.Body = timedBody{r.Body, c}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), bodyClockKey{}, c)))
	})
}

type bodyClockKey struct{}

// bodyClock keeps a request's read deadline.
type bodyClock struct {
	rc   *http.ResponseController
	mu   sync.Mutex
	by   time.Time // the handler's own deadline; zero for none
	came bool      // the body has come whole
}

// wait sets the deadline for the next bytes.
func (c *bodyClock) wait() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.came {
		return
	}
	d := time.Now().Add(bodyWait)
	if !c.by.IsZero() && c.by.Before(d) {
		d = c.by
	}
	c.rc.SetReadDeadline(d)
}

func (c *bodyClock) whole() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.came = true
	c.rc.SetReadDeadline(time.Time{})
}

// timedBody is a body read under its bodyClock.
type timedBody struct {
	io.ReadCloser
	c *bodyClock
}

func (b timedBody) Read(p []byte) (int, error) {
	b.c.wait()
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.c.whole()
	}
	return n, err
}

// readBodyBy has the body of the request whose context is ctx come whole by
// t: no read waits past it. Time now ends a read waiting at once.
func readBodyBy(w http.ResponseWriter, ctx context.Context, t time.Time) {
	c, ok := ctx.Value(bodyClockKey{}).(*bodyClock)
	if !ok {
		http.NewResponseController(w).SetReadDeadline(t)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.by = t
	if !c.came {
		c.rc.SetReadDeadline(t)
	}
}
