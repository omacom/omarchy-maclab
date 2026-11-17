// Package upload sends a file to labd's artifact store through the Front
// door: a file no longer than one part in one request, a longer one in
// numbered parts that a later run resumes.
package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

const (
	// PartBytes is the size of every part but the last.
	PartBytes = 32 << 20
	// Retries is how many times a part is sent again after its first try.
	Retries = 8
	// bodyMax is the largest body labd reads; a part size above it is not believed.
	bodyMax = 90_000_000
)

// Sender sends files to one artifact collection of labd.
type Sender struct {
	Client *http.Client
	// URL is the collection, e.g. https://lab.example/api/artifacts.
	URL   string
	Query url.Values
	Token string
	// PartBytes is the longest file sent in one request; 0 is 32 MiB.
	PartBytes int64
	// Pause waits before a part's retry number n (from 1); nil backs off
	// from one second to thirty.
	Pause func(ctx context.Context, n int) error
	// Note is told when an upload resumes; nil says nothing.
	Note func(format string, args ...any)
	// AtOnce, when 1 or 4, is how many parts go at the same time whatever
	// labd says: the measurement that sets labd's own number needs both.
	AtOnce int
	// Progress is told after each part how many of the upload's parts labd
	// holds; nil is told nothing.
	Progress func(held, total int)
	// Hold, for the Front door's test of a cut upload alone, stops one part
	// halfway once labd holds two, until the upload is canceled or lab is
	// killed: labd then holds part of a part for as long as the test needs.
	Hold bool
}

// holdAt reads the size bytes of f in parts of part bytes, but a read from
// the middle of a part on, once ready says so, waits until ctx ends: the
// first part to get there does, and that part only. No read crosses a
// middle, so the part held has sent its first half and nothing more.
type holdAt struct {
	io.ReaderAt
	ctx        context.Context
	part, size int64
	ready      func() bool
	held       *atomic.Int64
}

func (h holdAt) ReadAt(p []byte, off int64) (int, error) {
	n := off/h.part + 1
	start := (n - 1) * h.part
	mid := start + min(h.part, h.size-start)/2
	if off < mid && off+int64(len(p)) > mid {
		p = p[:mid-off]
	}
	if off >= mid && (h.held.Load() == n || h.ready() && h.held.CompareAndSwap(0, n)) {
		<-h.ctx.Done()
		return 0, h.ctx.Err()
	}
	return h.ReaderAt.ReadAt(p, off)
}

// refused is labd turning a request down: sending it again cannot help.
type refused struct{ msg string }

func (e *refused) Error() string { return e.msg }

// File sends the file at path and returns its SHA-256.
func (s Sender) File(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	return s.Send(ctx, f, st.Size())
}

// Send sends the size bytes of f and returns their SHA-256.
func (s Sender) Send(ctx context.Context, f io.ReaderAt, size int64) (string, error) {
	one := s.PartBytes
	if one <= 0 {
		one = PartBytes
	}
	var out api.StoredArtifact
	if size <= one {
		if err := s.do(ctx, "POST", "", io.NewSectionReader(f, 0, size), size, &out); err != nil {
			return "", fmt.Errorf("upload: %w", err)
		}
		return out.SHA256, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, size)); err != nil {
		return "", err
	}
	sha := hex.EncodeToString(h.Sum(nil))
	start, _ := json.Marshal(api.UploadStart{SHA256: sha, Size: size})
	var state api.UploadState
	if err := s.do(ctx, "POST", "/uploads", bytes.NewReader(start), int64(len(start)), &state); err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	if state.PartBytes <= 0 || state.PartBytes > bodyMax {
		return "", fmt.Errorf("upload: labd asked for parts of %d bytes", state.PartBytes)
	}
	total := int((size + state.PartBytes - 1) / state.PartBytes)
	var missing []int
	for n := 1; n <= total; n++ {
		if !slices.Contains(state.Held, n) {
			missing = append(missing, n)
		}
	}
	if len(missing) < total && s.Note != nil {
		s.Note("resuming: labd holds %d of %d parts", total-len(missing), total)
	}
	workers := 1
	if state.AtOnce == 4 {
		workers = 4
	}
	if s.AtOnce == 1 || s.AtOnce == 4 {
		workers = s.AtOnce
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	queue := make(chan int)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		held  = total - len(missing)
		body  = f
	)
	if s.Hold {
		body = holdAt{ReaderAt: f, ctx: ctx, part: state.PartBytes, size: size, held: new(atomic.Int64), ready: func() bool {
			mu.Lock()
			defer mu.Unlock()
			return held >= 2
		}}
	}
	for range min(workers, len(missing)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range queue {
				off := int64(n-1) * state.PartBytes
				err := s.part(ctx, body, sha, n, off, min(state.PartBytes, size-off))
				mu.Lock()
				if err != nil && first == nil {
					first = err
				}
				if err == nil {
					held++
					if s.Progress != nil {
						s.Progress(held, total)
					}
				}
				mu.Unlock()
				if err != nil {
					cancel()
				}
			}
		}()
	}
feed:
	for _, n := range missing {
		select {
		case queue <- n:
		case <-ctx.Done():
			break feed
		}
	}
	close(queue)
	wg.Wait()
	if first != nil {
		return "", fmt.Errorf("upload: %w", first)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := s.do(ctx, "POST", "/uploads/"+sha+"/close", nil, 0, &out); err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	if out.SHA256 != sha {
		return "", fmt.Errorf("upload: labd stored sha256 %s, but the file is %s", out.SHA256, sha)
	}
	return sha, nil
}

// part sends one part, again after a failure that was not a refusal.
func (s Sender) part(ctx context.Context, f io.ReaderAt, sha string, n int, off, length int64) error {
	var err error
	for try := 0; try <= Retries; try++ {
		if try > 0 {
			if perr := s.pause(ctx, try); perr != nil {
				return perr
			}
		}
		err = s.do(ctx, "PUT", fmt.Sprintf("/uploads/%s/parts/%d", sha, n), io.NewSectionReader(f, off, length), length, nil)
		var no *refused
		if err == nil || errors.As(err, &no) || ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("part %d: %w", n, err)
	}
	return nil
}

func (s Sender) pause(ctx context.Context, n int) error {
	if s.Pause != nil {
		return s.Pause(ctx, n)
	}
	return Backoff(ctx, n)
}

// Backoff waits before retry number n (from 1): one second, doubling to thirty.
func Backoff(ctx context.Context, n int) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(min(time.Second<<min(n-1, 5), 30*time.Second)):
		return nil
	}
}

// do sends one request, its body always with a Content-Length.
func (s Sender) do(ctx context.Context, method, path string, body io.Reader, length int64, out any) error {
	u := s.URL + path
	if len(s.Query) > 0 {
		u += "?" + s.Query.Encode()
	}
	if body == nil {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.ContentLength = length
	if length == 0 {
		req.Body = http.NoBody
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = resp.Status
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
			return &refused{msg}
		}
		return errors.New(msg)
	}
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}
