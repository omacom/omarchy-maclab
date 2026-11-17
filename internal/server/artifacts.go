package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

const (
	// artifactChunkBytes is the most written at once.
	artifactChunkBytes = 1 << 20
	// artifactSyncBytes is the most written between syncs: a part's size.
	artifactSyncBytes = 32 << 20
)

type artifactTemp interface {
	io.ReadWriteSeeker
	Name() string
	Fd() uintptr
	Sync() error
	Close() error
}

// Per-call operations let tests exercise I/O failures without global hooks.
type artifactDisk struct {
	create  func(string) (artifactTemp, error)
	drop    func(artifactTemp) error
	rename  func(string, string) error
	syncDir func(string) error
}

func artifactDiskOps() artifactDisk {
	return artifactDisk{
		create: func(dir string) (artifactTemp, error) { return os.CreateTemp(dir, ".upload-*") },
		drop:   dropArtifactCache,
		rename: os.Rename,
		syncDir: func(dir string) error {
			f, err := os.Open(dir)
			if err != nil {
				return err
			}
			return errors.Join(f.Sync(), f.Close())
		},
	}
}

// Write in bounded chunks and sync each artifactSyncBytes rather than leaving
// a whole package in delalloc; the caller syncs what is left. This does not
// repair an unhealthy filesystem: any I/O error refuses upload.
type artifactChunkWriter struct {
	file     artifactTemp
	unsynced int
}

func (w *artifactChunkWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), artifactChunkBytes, artifactSyncBytes-w.unsynced)]
		n, err := w.file.Write(chunk)
		total += n
		w.unsynced += n
		if err == nil && n != len(chunk) {
			err = io.ErrShortWrite
		}
		if err == nil && w.unsynced == artifactSyncBytes {
			err = w.sync()
		}
		if err != nil {
			return total, err
		}
		p = p[n:]
	}
	return total, nil
}

func (w *artifactChunkWriter) sync() error {
	w.unsynced = 0
	return w.file.Sync()
}

// writeVerifiedArtifact closes the file on every path. A hash is returned only
// after synced writes, cache eviction and a complete on-file size/hash readback.
func writeVerifiedArtifact(file artifactTemp, body io.Reader, drop func(artifactTemp) error) (sha string, size int64, err error) {
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			sha, size = "", 0
		}
	}()
	buffer := make([]byte, artifactChunkBytes)
	incoming := sha256.New()
	w := &artifactChunkWriter{file: file}
	size, err = io.CopyBuffer(io.MultiWriter(w, incoming), body, buffer)
	if err != nil {
		return "", 0, fmt.Errorf("artifact write/sync: %w", err)
	}
	if size == 0 || w.unsynced > 0 {
		if err = w.sync(); err != nil {
			return "", 0, fmt.Errorf("artifact sync: %w", err)
		}
	}
	if err = drop(file); err != nil {
		return "", 0, fmt.Errorf("artifact cache eviction: %w", err)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return "", 0, fmt.Errorf("artifact readback seek: %w", err)
	}
	stored := sha256.New()
	var readSize int64
	readSize, err = io.CopyBuffer(stored, file, buffer)
	if err != nil {
		return "", 0, fmt.Errorf("artifact readback: %w", err)
	}
	sha = hex.EncodeToString(incoming.Sum(nil))
	if readSize != size || hex.EncodeToString(stored.Sum(nil)) != sha {
		return "", 0, fmt.Errorf("artifact readback differs from upload: wrote %d, read %d", size, readSize)
	}
	return sha, size, nil
}

func storeArtifactIn(dir string, body io.Reader, disk artifactDisk) (string, int64, error) {
	return storeArtifactAs(dir, body, disk, "")
}

// errNotTheSHA256 is an upload whose bytes are not the SHA-256 it was started with.
var errNotTheSHA256 = errors.New("the bytes are not the SHA-256 the upload named")

// storeArtifactAs stores body only if its SHA-256 is want; "" takes any.
func storeArtifactAs(dir string, body io.Reader, disk artifactDisk, want string) (string, int64, error) {
	file, err := disk.create(dir)
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(file.Name())
	sha, size, err := writeVerifiedArtifact(file, body, disk.drop)
	if err != nil {
		return "", 0, err
	}
	if want != "" && sha != want {
		return "", 0, fmt.Errorf("%w: they are %s", errNotTheSHA256, sha)
	}
	// Replace a same-name CAS object only with the newly verified body; its
	// existence alone is not evidence that the old bytes are correct.
	if err := disk.rename(file.Name(), filepath.Join(dir, sha)); err != nil {
		return "", 0, fmt.Errorf("artifact commit rename: %w", err)
	}
	if err := disk.syncDir(dir); err != nil {
		return "", 0, fmt.Errorf("artifact commit directory sync: %w", err)
	}
	return sha, size, nil
}

// Transfers are the limits uploads work inside the Front door's: the edge
// refuses a request body over 100 MB. The zero value is the lab's own.
type Transfers struct {
	// PartBytes is the size of every part of an upload but its last; 0 is 32 MiB.
	PartBytes int64
	// BodyMax is the largest body labd reads in one request; 0 is 90 MB.
	BodyMax int64
	// AtOnce is how many parts labd tells a client to send at the same time: 4, or 1.
	AtOnce int
}

const (
	defaultPartBytes = 32 << 20
	defaultBodyMax   = 90_000_000
)

func (s *Server) partBytes() int64 {
	if n := s.cfg.Transfers.PartBytes; n > 0 {
		return n
	}
	return defaultPartBytes
}

func (s *Server) bodyMax() int64 {
	if n := s.cfg.Transfers.BodyMax; n > 0 {
		return n
	}
	return defaultBodyMax
}

func (s *Server) tooBig(w http.ResponseWriter) {
	httpErr(w, http.StatusRequestEntityTooLarge, "refused: a request body over %d MB; a larger file goes up in parts", s.bodyMax()/1_000_000)
}

// capBodies refuses a body longer than BodyMax by its Content-Length, before
// reading it, and stops reading one that came without a length at BodyMax.
func (s *Server) capBodies(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > s.bodyMax() {
			s.tooBig(w)
			return
		}
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, s.bodyMax())
		}
		h.ServeHTTP(w, r)
	})
}

// artifactErr answers a failed store: 413 for a body cut at BodyMax, else 500.
func (s *Server) artifactErr(w http.ResponseWriter, err error) {
	if errors.As(err, new(*http.MaxBytesError)) {
		s.tooBig(w)
		return
	}
	httpErr(w, 500, "%v", err)
}

// serveArtifact sends a stored file, honouring a Range. Its bytes never
// change, but who may fetch it does: no shared cache keeps it.
func serveArtifact(w http.ResponseWriter, r *http.Request, path string) {
	w.Header().Set("Cache-Control", "private")
	http.ServeFile(w, r, path)
}

// A partUpload is a file arriving in numbered parts: a folder beside the
// artifacts holding its size, its part size and one file for each part held.
type partUpload struct {
	dir string
	api.UploadState
}

const partUploadMeta = "upload.json"

func (u partUpload) parts() int { return int((u.Size + u.PartBytes - 1) / u.PartBytes) }

func (u partUpload) partLen(n int) int64 {
	return min(u.PartBytes, u.Size-int64(n-1)*u.PartBytes)
}

func (u partUpload) path(n int) string { return filepath.Join(u.dir, strconv.Itoa(n)) }

func (u partUpload) held() []int {
	held := []int{}
	for n := 1; n <= u.parts(); n++ {
		if st, err := os.Stat(u.path(n)); err == nil && st.Size() == u.partLen(n) {
			held = append(held, n)
		}
	}
	return held
}

func loadPartUpload(dir string) (partUpload, error) {
	u := partUpload{dir: dir}
	b, err := os.ReadFile(filepath.Join(dir, partUploadMeta))
	if err != nil {
		return u, err
	}
	if err := json.Unmarshal(b, &u.UploadState); err != nil || u.Size <= 0 || u.PartBytes <= 0 {
		return u, fmt.Errorf("upload %s: unreadable", filepath.Base(dir))
	}
	return u, nil
}

// uploadLocks holds one lock for each upload folder in use. An upload's
// start, the moment a part joins it and its close each take it, so none of
// them sees the folder half made or removes it under another.
var uploadLocks = struct {
	sync.Mutex
	of map[string]*uploadLock
}{of: map[string]*uploadLock{}}

type uploadLock struct {
	sync.Mutex
	users int
}

func lockUpload(dir string) (unlock func()) {
	uploadLocks.Lock()
	l := uploadLocks.of[dir]
	if l == nil {
		l = &uploadLock{}
		uploadLocks.of[dir] = l
	}
	l.users++
	uploadLocks.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		uploadLocks.Lock()
		if l.users--; l.users == 0 {
			delete(uploadLocks.of, dir)
		}
		uploadLocks.Unlock()
	}
}

// errNoUpload is a part or a close whose upload labd no longer holds.
var errNoUpload = errors.New("no such upload")

// beginPartUpload finds the upload in dir again, or starts it, and says which
// parts it holds. One started with another size or part size is not this
// upload: it is dropped.
func beginPartUpload(dir string, want api.UploadState) (partUpload, error) {
	defer lockUpload(dir)()
	u, err := findOrMakePartUpload(dir, want)
	if err == nil {
		u.Held = u.held()
	}
	return u, err
}

func findOrMakePartUpload(dir string, want api.UploadState) (partUpload, error) {
	if u, err := loadPartUpload(dir); err == nil && u.Size == want.Size && u.PartBytes == want.PartBytes {
		now := time.Now()
		return u, os.Chtimes(dir, now, now)
	}
	if err := os.RemoveAll(dir); err != nil {
		return partUpload{}, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return partUpload{}, err
	}
	b, _ := json.Marshal(want)
	return partUpload{dir: dir, UploadState: want}, os.WriteFile(filepath.Join(dir, partUploadMeta), b, 0o600)
}

// put stores part n. The part is held only once all its bytes are on disk,
// and only if the upload it was sent for is still there.
func (u partUpload) put(n int, body io.Reader) error {
	f, err := os.CreateTemp(u.dir, ".part-*")
	if errors.Is(err, os.ErrNotExist) {
		return errNoUpload
	}
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	got, err := io.Copy(f, body)
	if err == nil && got != u.partLen(n) {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	// A folder dropped since, by a close or another start, took this file with it.
	defer lockUpload(u.dir)()
	if err = os.Rename(f.Name(), u.path(n)); errors.Is(err, os.ErrNotExist) {
		return errNoUpload
	}
	return err
}

// partsReader reads an upload's parts one after another, as the file they make.
type partsReader struct {
	u    partUpload
	next int
	cur  *os.File
}

func (p *partsReader) Read(b []byte) (int, error) {
	for {
		if p.cur == nil {
			if p.next > p.u.parts() {
				return 0, io.EOF
			}
			f, err := os.Open(p.u.path(p.next))
			if err != nil {
				return 0, err
			}
			p.cur, p.next = f, p.next+1
		}
		n, err := p.cur.Read(b)
		if err == io.EOF {
			p.cur.Close()
			p.cur, err = nil, nil
		}
		if n > 0 || err != nil {
			return n, err
		}
	}
}

func (p *partsReader) Close() error {
	if p.cur == nil {
		return nil
	}
	return p.cur.Close()
}

// uploadScope is whose upload a request is about. A builder's belongs to
// the build it runs, named by ?build=; a person's to its SHA-256 alone, so
// two people sending the same file fill the same upload.
func (s *Server) uploadScope(w http.ResponseWriter, r *http.Request) (c builderCred, build string, ok bool) {
	c = builderCredOf(r)
	if c.nobody() {
		return c, "", true
	}
	id := r.URL.Query().Get("build")
	s.builds.state.Lock()
	b, err := s.getBuild(id)
	if err == nil {
		err = s.ownsLocked(c, b)
	}
	s.builds.state.Unlock()
	if err != nil {
		httpErr(w, 403, "an upload must name a build this builder is running (?build=): %v", err)
		return c, "", false
	}
	return c, b.ID, true
}

func (s *Server) partUploadDir(build, sha string) string {
	if build != "" {
		build += "-"
	}
	return filepath.Join(s.cfg.DataDir, "artifacts", ".upload-"+build+sha)
}

// hStartUpload opens an upload in parts, or finds it again, and says which
// parts labd holds: that is how an upload resumes.
func (s *Server) hStartUpload(w http.ResponseWriter, r *http.Request) {
	_, build, ok := s.uploadScope(w, r)
	if !ok {
		return
	}
	var req api.UploadStart
	if !decodeBody(w, r, 4<<10, &req) {
		return
	}
	if !reSHA.MatchString(req.SHA256) || req.Size <= 0 {
		httpErr(w, 400, "an upload is started with its sha256 and its size in bytes")
		return
	}
	limit := s.cfg.ArtifactBudget
	if limit <= 0 {
		limit = artifactBudget
	}
	if build != "" {
		limit = min(limit, builderUploadMax)
	}
	if req.Size > limit {
		httpErr(w, 413, "an upload is limited to %d bytes", limit)
		return
	}
	atOnce := 1
	if s.cfg.Transfers.AtOnce == 4 {
		atOnce = 4
	}
	u, err := beginPartUpload(s.partUploadDir(build, req.SHA256), api.UploadState{SHA256: req.SHA256, Size: req.Size, PartBytes: s.partBytes()})
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	u.AtOnce = atOnce
	writeJSON(w, u.UploadState)
}

// openUpload is the upload a part or a close names; 404 when labd holds none.
func (s *Server) openUpload(w http.ResponseWriter, r *http.Request) (u partUpload, c builderCred, build string, ok bool) {
	if c, build, ok = s.uploadScope(w, r); !ok {
		return
	}
	sha := r.PathValue("sha")
	if !reSHA.MatchString(sha) {
		httpErr(w, 400, "bad sha256")
		return u, c, build, false
	}
	u, err := loadPartUpload(s.partUploadDir(build, sha))
	if err != nil {
		noUpload(w, sha)
		return u, c, build, false
	}
	return u, c, build, true
}

func noUpload(w http.ResponseWriter, sha string) {
	httpErr(w, 404, "no upload %s: start it again", short(sha))
}

func (s *Server) hPutPart(w http.ResponseWriter, r *http.Request) {
	u, c, _, ok := s.openUpload(w, r)
	if !ok {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 || n > u.parts() {
		httpErr(w, 404, "upload %s has parts 1 to %d", short(u.SHA256), u.parts())
		return
	}
	switch {
	case r.ContentLength < 0:
		httpErr(w, http.StatusLengthRequired, "a part is sent with its Content-Length")
		return
	case r.ContentLength != u.partLen(n):
		httpErr(w, 400, "part %d of upload %s is %d bytes, not %d", n, short(u.SHA256), u.partLen(n), r.ContentLength)
		return
	}
	defer interruptReadsOnCancel(w, r.Context())()
	if err := u.put(n, ctxReader{r.Context(), r.Body}); err != nil {
		switch {
		case !c.nobody() && r.Context().Err() != nil:
			httpErr(w, 403, "%v", errBuilderRemoved)
		case errors.Is(err, errNoUpload):
			noUpload(w, u.SHA256)
		case errors.Is(err, io.ErrUnexpectedEOF):
			httpErr(w, 400, "part %d was cut short: send it again", n)
		default:
			httpErr(w, 500, "%v", err)
		}
		return
	}
	writeJSON(w, map[string]int{"part": n})
}

// hCloseUpload stores the parts as one artifact if they are the SHA-256 the
// upload was started with; parts that are not are dropped, all of them.
func (s *Server) hCloseUpload(w http.ResponseWriter, r *http.Request) {
	u, c, build, ok := s.openUpload(w, r)
	if !ok {
		return
	}
	defer lockUpload(u.dir)()
	u, err := loadPartUpload(u.dir)
	if err != nil {
		noUpload(w, r.PathValue("sha"))
		return
	}
	if held := u.held(); len(held) != u.parts() {
		var missing []string
		for n := 1; n <= u.parts(); n++ {
			if len(held) == 0 || held[0] != n {
				missing = append(missing, strconv.Itoa(n))
				continue
			}
			held = held[1:]
		}
		httpErr(w, 409, "upload %s is not whole: missing parts %s", short(u.SHA256), strings.Join(missing, ", "))
		return
	}
	parts := &partsReader{u: u, next: 1}
	defer parts.Close()
	sha, n, err := storeArtifactAs(filepath.Join(s.cfg.DataDir, "artifacts"), parts, artifactDiskOps(), u.SHA256)
	switch {
	case errors.Is(err, errNotTheSHA256):
		os.RemoveAll(u.dir)
		httpErr(w, http.StatusUnprocessableEntity, "upload %s: %v; nothing is stored, send the file again", short(u.SHA256), err)
		return
	case err != nil:
		httpErr(w, 500, "%v", err)
		return
	}
	if build != "" {
		if err := s.claimUpload(c, build, sha); err != nil {
			httpErr(w, 403, "not recorded: %v", err)
			return
		}
	}
	os.RemoveAll(u.dir)
	writeJSON(w, api.StoredArtifact{SHA256: sha, Size: n})
}
