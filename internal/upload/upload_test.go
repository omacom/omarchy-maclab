package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

const testPart = 1000

// fakeLab speaks labd's side of an upload and counts what it is sent.
type fakeLab struct {
	t  *testing.T
	mu sync.Mutex

	atOnce         int
	held           map[int][]byte
	whole          [][]byte       // bodies of one-request uploads
	requests       []string       // "METHOD path" in arrival order
	tries          map[int]int    // PUTs seen per part
	failPart       map[int]int    // answer 503 to this many PUTs of a part
	refuse         map[int]string // answer 403 with this line
	closeSHA       string         // answer this at the close instead of the truth
	inFlight, peak int
	hold           time.Duration // a PUT takes this long
	waitFor        int           // a PUT waits until this many are in flight
	started        api.UploadStart
}

func newFakeLab(t *testing.T) (*fakeLab, *httptest.Server) {
	f := &fakeLab{t: t, atOnce: 1, held: map[int][]byte{}, tries: map[int]int{}, failPart: map[int]int{}, refuse: map[int]string{}}
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	return f, ts
}

func (f *fakeLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && (r.ContentLength < 0 || len(r.TransferEncoding) > 0) {
		f.t.Errorf("%s %s came without Content-Length (length %d, transfer encoding %v)", r.Method, r.URL.Path, r.ContentLength, r.TransferEncoding)
	}
	if r.Header.Get("Authorization") != "Bearer secret" {
		http.Error(w, "no token", 401)
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	rest := strings.TrimPrefix(r.URL.Path, "/api/artifacts")
	switch {
	case rest == "" && r.Method == "POST":
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.whole = append(f.whole, b)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(api.StoredArtifact{SHA256: sum(b), Size: int64(len(b))})
	case rest == "/uploads" && r.Method == "POST":
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewDecoder(r.Body).Decode(&f.started)
		st := api.UploadState{SHA256: f.started.SHA256, Size: f.started.Size, PartBytes: testPart, AtOnce: f.atOnce, Held: []int{}}
		for n := range f.held {
			st.Held = append(st.Held, n)
		}
		slices.Sort(st.Held)
		json.NewEncoder(w).Encode(st)
	case strings.HasSuffix(rest, "/close") && r.Method == "POST":
		f.mu.Lock()
		defer f.mu.Unlock()
		var all []byte
		for n := 1; n <= len(f.held); n++ {
			all = append(all, f.held[n]...)
		}
		sha := sum(all)
		if f.closeSHA != "" {
			sha = f.closeSHA
		}
		json.NewEncoder(w).Encode(api.StoredArtifact{SHA256: sha, Size: int64(len(all))})
	case strings.Contains(rest, "/parts/") && r.Method == "PUT":
		n, _ := strconv.Atoi(rest[strings.LastIndex(rest, "/")+1:])
		f.mu.Lock()
		f.tries[n]++
		f.inFlight++
		f.peak = max(f.peak, f.inFlight)
		fail, refuse := f.failPart[n] > 0, f.refuse[n]
		if fail {
			f.failPart[n]--
		}
		f.mu.Unlock()
		time.Sleep(f.hold)
		for end := time.Now().Add(5 * time.Second); f.waitFor > 1 && time.Now().Before(end); time.Sleep(time.Millisecond) {
			f.mu.Lock()
			enough := f.peak >= f.waitFor
			f.mu.Unlock()
			if enough {
				break
			}
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.inFlight--
		switch {
		case refuse != "":
			http.Error(w, refuse, 403)
		case fail:
			http.Error(w, "try again", 503)
		default:
			f.held[n] = b
		}
		f.mu.Unlock()
	default:
		http.Error(w, "no such route", 404)
	}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// content is size bytes in which every part differs.
func content(size int) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, "%08d ", i)
	}
	return b.Bytes()[:size]
}

func fileOf(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func sender(ts *httptest.Server) Sender {
	return Sender{Client: ts.Client(), URL: ts.URL + "/api/artifacts", Token: "secret", PartBytes: testPart,
		Pause: func(context.Context, int) error { return nil }}
}

func TestAnythingUpToOnePartIsOneRequest(t *testing.T) {
	for _, size := range []int{0, 1, testPart - 1, testPart} {
		f, ts := newFakeLab(t)
		data := content(size)
		sha, err := sender(ts).File(context.Background(), fileOf(t, data))
		if err != nil || sha != sum(data) {
			t.Fatalf("%d bytes: sha %q, %v", size, sha, err)
		}
		if len(f.requests) != 1 || f.requests[0] != "POST /api/artifacts" || !bytes.Equal(f.whole[0], data) {
			t.Fatalf("%d bytes: requests %v", size, f.requests)
		}
	}
}

func TestALargerFileGoesInNumberedPartsAndASHA256ClosesIt(t *testing.T) {
	f, ts := newFakeLab(t)
	data := content(3*testPart + 500)
	sha, err := sender(ts).File(context.Background(), fileOf(t, data))
	if err != nil || sha != sum(data) {
		t.Fatalf("sha %q, %v", sha, err)
	}
	if f.started != (api.UploadStart{SHA256: sum(data), Size: int64(len(data))}) {
		t.Fatalf("started as %+v", f.started)
	}
	id := "/api/artifacts/uploads/" + sum(data)
	want := []string{"POST /api/artifacts/uploads", "PUT " + id + "/parts/1", "PUT " + id + "/parts/2", "PUT " + id + "/parts/3", "PUT " + id + "/parts/4", "POST " + id + "/close"}
	if !slices.Equal(f.requests, want) {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(f.requests, "\n"), strings.Join(want, "\n"))
	}
	for n, wantLen := range map[int]int{1: testPart, 2: testPart, 3: testPart, 4: 500} {
		if !bytes.Equal(f.held[n], data[(n-1)*testPart:(n-1)*testPart+wantLen]) {
			t.Errorf("part %d is not bytes %d..%d of the file", n, (n-1)*testPart, (n-1)*testPart+wantLen)
		}
	}
}

func TestResumeAsksWhichPartsLabdHoldsAndSendsTheRest(t *testing.T) {
	f, ts := newFakeLab(t)
	data := content(4 * testPart)
	f.held[1], f.held[3] = data[:testPart], data[2*testPart:3*testPart]
	var notes []string
	s := sender(ts)
	s.Note = func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) }
	sha, err := s.File(context.Background(), fileOf(t, data))
	if err != nil || sha != sum(data) {
		t.Fatalf("sha %q, %v", sha, err)
	}
	if f.tries[1] != 0 || f.tries[3] != 0 || f.tries[2] != 1 || f.tries[4] != 1 {
		t.Fatalf("parts sent: %v, want 2 and 4 once each", f.tries)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "2 of 4 parts") {
		t.Fatalf("notes: %q", notes)
	}
}

// The Front door's test 4 cuts an upload once labd holds some parts: it is
// told after each part how many labd holds, the ones a resume found there
// counted in.
func TestProgressSaysHowManyPartsLabdHoldsAfterEachPart(t *testing.T) {
	f, ts := newFakeLab(t)
	data := content(5 * testPart)
	f.held[2] = data[testPart : 2*testPart]
	var held []int
	s := sender(ts)
	s.Progress = func(n, total int) {
		if total != 5 {
			t.Errorf("total %d", total)
		}
		held = append(held, n)
	}
	if sha, err := s.File(context.Background(), fileOf(t, data)); err != nil || sha != sum(data) {
		t.Fatalf("sha %q, %v", sha, err)
	}
	if fmt.Sprint(held) != "[2 3 4 5]" {
		t.Errorf("progress said %v, want 2 3 4 5", held)
	}
}

func TestAPartIsRetriedUpToEightTimes(t *testing.T) {
	data := content(3 * testPart)
	f, ts := newFakeLab(t)
	f.failPart[2] = 8
	if sha, err := sender(ts).File(context.Background(), fileOf(t, data)); err != nil || sha != sum(data) {
		t.Fatalf("eight failures then success: sha %q, %v", sha, err)
	}
	if f.tries[2] != 9 || f.tries[1] != 1 {
		t.Fatalf("tries: %v, want part 2 sent 9 times", f.tries)
	}

	f, ts = newFakeLab(t)
	f.failPart[2] = 100
	_, err := sender(ts).File(context.Background(), fileOf(t, data))
	if err == nil || !strings.Contains(err.Error(), "part 2") || f.tries[2] != 9 {
		t.Fatalf("a part that never arrives: err %v, tries %v", err, f.tries)
	}
	if slices.Contains(f.requests, "POST /api/artifacts/uploads/"+sum(data)+"/close") {
		t.Fatal("an upload with a part missing was closed")
	}
}

func TestARefusedPartIsNotSentAgain(t *testing.T) {
	f, ts := newFakeLab(t)
	f.refuse[1] = "not yours"
	_, err := sender(ts).File(context.Background(), fileOf(t, content(2*testPart)))
	if err == nil || !strings.Contains(err.Error(), "not yours") || f.tries[1] != 1 {
		t.Fatalf("err %v, tries %v", err, f.tries)
	}
}

func TestPartsGoOneAtATimeOrFourAsLabdSays(t *testing.T) {
	for said, want := range map[int]int{0: 1, 1: 1, 3: 1, 4: 4, 9: 1} {
		f, ts := newFakeLab(t)
		f.atOnce = said
		f.hold, f.waitFor = 20*time.Millisecond, want
		data := content(8 * testPart)
		if sha, err := sender(ts).File(context.Background(), fileOf(t, data)); err != nil || sha != sum(data) {
			t.Fatalf("at_once %d: sha %q, %v", said, sha, err)
		}
		if f.peak != want {
			t.Errorf("labd said %d at once: %d went at once, want %d", said, f.peak, want)
		}
	}
}

// The measurement that sets labd's --parts-at-once sends the same kind of
// file both ways from one machine, whatever labd says: AtOnce 1 or 4 is
// obeyed, anything else leaves labd's word standing.
func TestTheMeasurementSendsOneOrFourAtOnceWhateverLabdSays(t *testing.T) {
	for _, c := range []struct{ labd, asked, want int }{{1, 4, 4}, {4, 1, 1}, {4, 0, 4}, {1, 3, 1}, {1, 16, 1}} {
		f, ts := newFakeLab(t)
		f.atOnce, f.waitFor = c.labd, c.want
		s := sender(ts)
		s.AtOnce = c.asked
		if sha, err := s.File(context.Background(), fileOf(t, content(8*testPart))); err != nil || sha == "" {
			t.Fatalf("labd said %d, asked %d: sha %q, %v", c.labd, c.asked, sha, err)
		}
		if f.peak != c.want {
			t.Errorf("labd said %d, asked %d: %d went at once, want %d", c.labd, c.asked, f.peak, c.want)
		}
	}
}

func TestACloseThatNamesAnotherSHA256IsAnError(t *testing.T) {
	f, ts := newFakeLab(t)
	f.closeSHA = strings.Repeat("0", 64)
	if sha, err := sender(ts).File(context.Background(), fileOf(t, content(2*testPart))); err == nil || sha != "" {
		t.Fatalf("sha %q, %v", sha, err)
	}
}

// Hold has one part stop halfway once labd holds two, until the upload is
// canceled: the Front door's test 4 then finds labd holding part of a part
// however fast the other parts went.
func TestHoldStopsOnePartHalfwayOnceLabdHoldsTwo(t *testing.T) {
	f, _ := newFakeLab(t)
	half := make(chan int, 1)
	rest := make(chan int, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/parts/3") {
			f.ServeHTTP(w, r)
			return
		}
		b := make([]byte, testPart)
		n, _ := io.ReadFull(r.Body, b[:testPart/2])
		half <- n
		more, _ := io.ReadAll(r.Body)
		rest <- len(more)
	}))
	t.Cleanup(ts.Close)
	s := sender(ts)
	s.Hold = true
	var said []int
	s.Progress = func(n, _ int) { said = append(said, n) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.File(ctx, fileOf(t, content(5*testPart)))
		done <- err
	}()
	if n := <-half; n != testPart/2 {
		t.Fatalf("part 3 sent %d bytes before its middle, want %d", n, testPart/2)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("the held upload ended without an error")
	}
	if n := <-rest; n != 0 {
		t.Errorf("part 3 sent %d bytes past its middle", n)
	}
	if fmt.Sprint(said) != "[1 2]" || f.tries[4] != 0 {
		t.Errorf("progress %v, part 4 tried %d times: want two parts held and nothing after the held one", said, f.tries[4])
	}
}

func TestHoldHoldsWithProgressOff(t *testing.T) {
	f, _ := newFakeLab(t)
	half := make(chan int, 1)
	rest := make(chan int, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/parts/3") {
			f.ServeHTTP(w, r)
			return
		}
		b := make([]byte, testPart)
		n, _ := io.ReadFull(r.Body, b[:testPart/2])
		half <- n
		more, _ := io.ReadAll(r.Body)
		rest <- len(more)
	}))
	t.Cleanup(ts.Close)
	s := sender(ts)
	s.Hold = true
	s.Progress = nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.File(ctx, fileOf(t, content(5*testPart)))
		done <- err
	}()
	<-half
	select {
	case n := <-rest:
		t.Fatalf("with progress off part 3 went on past its middle (%d more bytes): the hold did not hold", n)
	case <-time.After(500 * time.Millisecond):
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("the held upload ended without an error")
	}
	if f.tries[4] != 0 {
		t.Errorf("part 4 tried %d times after the held one", f.tries[4])
	}
}
