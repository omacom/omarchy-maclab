package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/upload"
)

var errArtifactIO = errors.New("synthetic artifact I/O failure")

type artifactFaultFile struct {
	file                    *os.File
	fault                   string
	maxWrite, syncs, closes int
}

func (f *artifactFaultFile) Name() string { return f.file.Name() }
func (f *artifactFaultFile) Fd() uintptr  { return f.file.Fd() }
func (f *artifactFaultFile) Read(p []byte) (int, error) {
	if f.fault == "read" {
		return 0, errArtifactIO
	}
	return f.file.Read(p)
}
func (f *artifactFaultFile) Seek(offset int64, whence int) (int64, error) {
	if f.fault == "seek" {
		return 0, errArtifactIO
	}
	return f.file.Seek(offset, whence)
}
func (f *artifactFaultFile) Write(p []byte) (int, error) {
	f.maxWrite = max(f.maxWrite, len(p))
	if f.fault == "write" {
		return 0, errArtifactIO
	}
	if f.fault == "short-write" {
		return f.file.Write(p[:len(p)-1])
	}
	return f.file.Write(p)
}
func (f *artifactFaultFile) Sync() error {
	f.syncs++
	if f.fault == "sync" || (f.fault == "second-sync" && f.syncs == 2) {
		return errArtifactIO
	}
	return f.file.Sync()
}
func (f *artifactFaultFile) Close() error {
	f.closes++
	err := f.file.Close()
	if f.fault == "close" {
		return errors.Join(err, errArtifactIO)
	}
	return err
}

func artifactTestDisk(t *testing.T, fault string) (artifactDisk, **artifactFaultFile) {
	t.Helper()
	disk := artifactDiskOps()
	var file *artifactFaultFile
	disk.create = func(dir string) (artifactTemp, error) {
		f, err := os.CreateTemp(dir, ".upload-*")
		if err != nil {
			return nil, err
		}
		file = &artifactFaultFile{file: f, fault: fault}
		return file, nil
	}
	return disk, &file
}

func artifactSHA(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func TestArtifactBoundedDurableStore(t *testing.T) {
	// A sync on macOS is a full flush of the drive: one for each megabyte
	// made an upload's close take 5 s a GiB on the mini; one for each
	// 32 MiB still leaves no more than a part's bytes unwritten.
	if artifactSyncBytes != 32<<20 {
		t.Fatalf("a sync every %d bytes, not every 32 MiB", artifactSyncBytes)
	}
	for _, size := range []int{0, 19, 2*artifactSyncBytes + 19} {
		dir := t.TempDir()
		body := bytes.Repeat([]byte{0x5a}, size)
		disk, file := artifactTestDisk(t, "")
		// bytes.Reader.WriteTo may pass the entire body in one Write call:
		// bounded allocation must hold even when io.Copy uses that fast path.
		sha, gotSize, err := storeArtifactIn(dir, bytes.NewReader(body), disk)
		if err != nil || sha != artifactSHA(body) || gotSize != int64(size) {
			t.Fatalf("size %d: sha=%s bytes=%d err=%v", size, sha, gotSize, err)
		}
		wantSyncs := max(1, (size+artifactSyncBytes-1)/artifactSyncBytes)
		if (*file).maxWrite > artifactChunkBytes || (*file).syncs != wantSyncs || (*file).closes != 1 {
			t.Fatalf("size %d: file %+v", size, *file)
		}
		stored, err := os.ReadFile(filepath.Join(dir, sha))
		if err != nil || !bytes.Equal(stored, body) {
			t.Fatalf("stored bytes differ: %v", err)
		}
	}
}

func TestArtifactStorageFailuresNeverCommit(t *testing.T) {
	for _, fault := range []string{"write", "short-write", "sync", "second-sync", "close", "seek", "read", "drop", "hash-mismatch", "size-mismatch"} {
		t.Run(fault, func(t *testing.T) {
			dir := t.TempDir()
			body := bytes.Repeat([]byte{0x42}, artifactSyncBytes+7)
			disk, file := artifactTestDisk(t, fault)
			drop := disk.drop
			disk.drop = func(f artifactTemp) error {
				switch fault {
				case "drop":
					return errArtifactIO
				case "hash-mismatch":
					if _, err := (*file).file.WriteAt([]byte{0}, 0); err != nil {
						return err
					}
				case "size-mismatch":
					if err := (*file).file.Truncate(int64(len(body) - 1)); err != nil {
						return err
					}
				}
				if fault == "hash-mismatch" || fault == "size-mismatch" {
					if err := (*file).file.Sync(); err != nil {
						return err
					}
				}
				return drop(f)
			}
			renamed := false
			disk.rename = func(string, string) error { renamed = true; return nil }
			sha, size, err := storeArtifactIn(dir, bytes.NewReader(body), disk)
			if err == nil || sha != "" || size != 0 || renamed || (*file).closes != 1 {
				t.Fatalf("sha=%q bytes=%d err=%v renamed=%t closes=%d", sha, size, err, renamed, (*file).closes)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatalf("failed upload left files: %v", entries)
			}
		})
	}
}

func TestArtifactCommitFailuresNeverReportSuccess(t *testing.T) {
	for _, phase := range []string{"rename", "directory-sync"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			disk, _ := artifactTestDisk(t, "")
			if phase == "rename" {
				disk.rename = func(string, string) error { return errArtifactIO }
			} else {
				disk.syncDir = func(string) error { return errArtifactIO }
			}
			sha, size, err := storeArtifactIn(dir, strings.NewReader("verified body"), disk)
			if !errors.Is(err, errArtifactIO) || sha != "" || size != 0 {
				t.Fatalf("sha=%q bytes=%d err=%v", sha, size, err)
			}
		})
	}
}

func TestArtifactReplacesCorruptSameNameOnlyAfterVerification(t *testing.T) {
	dir := t.TempDir()
	body := []byte("correct body")
	path := filepath.Join(dir, artifactSHA(body))
	if err := os.WriteFile(path, []byte("corrupt old body"), 0o600); err != nil {
		t.Fatal(err)
	}
	disk, _ := artifactTestDisk(t, "sync")
	if _, _, err := storeArtifactIn(dir, bytes.NewReader(body), disk); err == nil {
		t.Fatal("sync failure accepted")
	}
	if stored, _ := os.ReadFile(path); string(stored) != "corrupt old body" {
		t.Fatal("unverified upload replaced existing CAS object")
	}
	if _, _, err := storeArtifactIn(dir, bytes.NewReader(body), artifactDiskOps()); err != nil {
		t.Fatal(err)
	}
	if stored, _ := os.ReadFile(path); !bytes.Equal(stored, body) {
		t.Fatal("verified upload did not replace corrupt object")
	}
}

type artifactBrokenReader struct{}

func (artifactBrokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestArtifactHTTPFailureDoesNotReturnSHA(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{DataDir: dir}}
	for _, handler := range []http.HandlerFunc{s.hPutArtifact} {
		recorder := httptest.NewRecorder()
		handler(recorder, httptest.NewRequest(http.MethodPost, "/api/artifacts", artifactBrokenReader{}))
		if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), `"sha256"`) {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

// transferLab is a labd whose uploads go in parts of 1000 bytes.
func transferLab(t *testing.T, tr Transfers, wrap func(http.Handler) http.Handler) *builderLab {
	if tr.PartBytes == 0 {
		tr.PartBytes = 1000
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b := &builderLab{t: t, ctx: ctx, dir: t.TempDir(), transfers: tr, wrap: wrap}
	b.start()
	return b
}

// sender uploads as the lab command does, as the harness Maintainer.
func (b *builderLab) sender() upload.Sender {
	return upload.Sender{Client: http.DefaultClient, URL: b.url + "/api/artifacts", Token: "lab_mtok", PartBytes: b.transfers.PartBytes,
		Pause: func(context.Context, int) error { return nil }}
}

func (b *builderLab) startUpload(token, path string, data []byte) (api.UploadState, int) {
	var st api.UploadState
	code := b.call("POST", path, token, api.UploadStart{SHA256: artifactSHA(data), Size: int64(len(data))}, &st)
	return st, code
}

func (b *builderLab) stored(sha string) []byte {
	data, _ := os.ReadFile(b.s.artifactPath(sha))
	return data
}

func (b *builderLab) uploadDirs() []string {
	var out []string
	ents, _ := os.ReadDir(filepath.Join(b.dir, "artifacts"))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".upload-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// partsContent is size bytes in which every part differs.
func partsContent(size int) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, "%08d ", i)
	}
	return b.Bytes()[:size]
}

func tempFile(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAnUploadInPartsIsStoredUnderItsSHA256(t *testing.T) {
	b := transferLab(t, Transfers{}, nil)
	data := partsContent(3500)
	sha, err := b.sender().File(b.ctx, tempFile(t, data))
	if err != nil || sha != artifactSHA(data) {
		t.Fatalf("sha %q, %v", sha, err)
	}
	if !bytes.Equal(b.stored(sha), data) {
		t.Fatal("the stored artifact is not the file")
	}
	if left := b.uploadDirs(); len(left) != 0 {
		t.Fatalf("a closed upload left %v", left)
	}
	small := partsContent(1000)
	if sha, err := b.sender().File(b.ctx, tempFile(t, small)); err != nil || !bytes.Equal(b.stored(sha), small) {
		t.Fatalf("one part in one request: %q, %v", sha, err)
	}
}

func TestStartingAnUploadAgainSaysWhichPartsLabdHolds(t *testing.T) {
	b := transferLab(t, Transfers{AtOnce: 4}, nil)
	data := partsContent(3500)
	sha := artifactSHA(data)
	st, code := b.startUpload("lab_mtok", "/api/artifacts/uploads", data)
	if code != 200 || st.PartBytes != 1000 || st.AtOnce != 4 || st.Held == nil || len(st.Held) != 0 || st.SHA256 != sha || st.Size != 3500 {
		t.Fatalf("a new upload: %d %+v", code, st)
	}
	for n, part := range map[int][]byte{1: data[:1000], 4: data[3000:]} {
		if code, body := b.do(b.ctx, "PUT", fmt.Sprintf("/api/artifacts/uploads/%s/parts/%d", sha, n), "lab_mtok", bytes.NewReader(part)); code != 200 {
			t.Fatalf("part %d: %d %s", n, code, body)
		}
	}
	if st, _ := b.startUpload("lab_mtok", "/api/artifacts/uploads", data); !slices.Equal(st.Held, []int{1, 4}) {
		t.Fatalf("held %v, want 1 and 4", st.Held)
	}
	if code, body := b.do(b.ctx, "POST", "/api/artifacts/uploads/"+sha+"/close", "lab_mtok", nil); code != 409 || !strings.Contains(string(body), "2, 3") {
		t.Fatalf("closing with parts missing: %d %s", code, body)
	}
	var notes []string
	s := b.sender()
	s.Note = func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) }
	if got, err := s.File(b.ctx, tempFile(t, data)); err != nil || !bytes.Equal(b.stored(got), data) {
		t.Fatalf("resumed upload: %q, %v", got, err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "2 of 4") {
		t.Fatalf("notes %q", notes)
	}
	if st := transferLab(t, Transfers{}, nil); true {
		if got, _ := st.startUpload("lab_mtok", "/api/artifacts/uploads", data); got.AtOnce != 1 {
			t.Fatalf("parts at once by default: %d, want 1", got.AtOnce)
		}
	}
}

func TestPartsAndUploadsLabdRefuses(t *testing.T) {
	b := transferLab(t, Transfers{}, nil)
	data := partsContent(2500)
	sha := artifactSHA(data)
	other := strings.Repeat("ab", 32)
	if _, code := b.startUpload("lab_mtok", "/api/artifacts/uploads", data); code != 200 {
		t.Fatalf("start: %d", code)
	}
	part := func(sha string, n int, body io.Reader) (int, string) {
		code, out := b.do(b.ctx, "PUT", fmt.Sprintf("/api/artifacts/uploads/%s/parts/%d", sha, n), "lab_mtok", body)
		return code, string(out)
	}
	pr, pw := io.Pipe()
	go func() { pw.Write(data[:1000]); pw.Close() }()
	for _, c := range []struct {
		what string
		sha  string
		n    int
		body io.Reader
		code int
	}{
		{"a part with no Content-Length", sha, 1, pr, 411},
		{"a short part", sha, 1, bytes.NewReader(data[:999]), 400},
		{"a long last part", sha, 3, bytes.NewReader(data[:501]), 400},
		{"part 0", sha, 0, bytes.NewReader(data[:1000]), 404},
		{"a part past the end", sha, 4, bytes.NewReader(data[:1000]), 404},
		{"a part of an upload never started", other, 1, bytes.NewReader(data[:1000]), 404},
	} {
		if code, body := part(c.sha, c.n, c.body); code != c.code {
			t.Errorf("%s: %d %s, want %d", c.what, code, body, c.code)
		}
	}
	if st, _ := b.startUpload("lab_mtok", "/api/artifacts/uploads", data); len(st.Held) != 0 {
		t.Fatalf("refused parts are held: %v", st.Held)
	}
	for what, start := range map[string]api.UploadStart{
		"no sha256": {Size: 2500}, "a short sha256": {SHA256: "abc", Size: 2500}, "no size": {SHA256: other}, "a path for a sha256": {SHA256: "../" + other[3:], Size: 2500},
	} {
		if code := b.call("POST", "/api/artifacts/uploads", "lab_mtok", start, nil); code != 400 {
			t.Errorf("an upload started with %s: %d, want 400", what, code)
		}
	}
	if code := b.call("POST", "/api/artifacts/uploads", "lab_mtok", api.UploadStart{SHA256: other, Size: 20<<30 + 1}, nil); code != 413 {
		t.Errorf("an upload larger than the artifact store: %d, want 413", code)
	}
	for _, token := range []string{"", "bld", "mnt_0000000000000000000000000000000000000000"} {
		if _, code := b.startUpload(token, "/api/artifacts/uploads", data); code != 401 {
			t.Errorf("start as %q: %d, want 401", token, code)
		}
		if code, _ := b.do(b.ctx, "PUT", "/api/artifacts/uploads/"+sha+"/parts/1", token, bytes.NewReader(data[:1000])); code != 401 {
			t.Errorf("a part as %q: %d, want 401", token, code)
		}
		if code, _ := b.do(b.ctx, "POST", "/api/artifacts/uploads/"+sha+"/close", token, nil); code != 401 {
			t.Errorf("close as %q: %d, want 401", token, code)
		}
	}
}

func TestAnUploadWhosePartsAreNotItsSHA256StoresNothing(t *testing.T) {
	b := transferLab(t, Transfers{}, nil)
	said, sent := partsContent(2500), bytes.Repeat([]byte("x"), 2500)
	sha := artifactSHA(said)
	b.startUpload("lab_mtok", "/api/artifacts/uploads", said)
	for n := 1; n <= 3; n++ {
		if code, body := b.do(b.ctx, "PUT", fmt.Sprintf("/api/artifacts/uploads/%s/parts/%d", sha, n), "lab_mtok", bytes.NewReader(sent[(n-1)*1000:min(n*1000, 2500)])); code != 200 {
			t.Fatalf("part %d: %d %s", n, code, body)
		}
	}
	code, body := b.do(b.ctx, "POST", "/api/artifacts/uploads/"+sha+"/close", "lab_mtok", nil)
	if code != 422 || strings.Contains(string(body), `"sha256"`) {
		t.Fatalf("close: %d %s", code, body)
	}
	ents, _ := os.ReadDir(filepath.Join(b.dir, "artifacts"))
	if len(ents) != 0 {
		t.Fatalf("a refused upload left %v", ents)
	}
}

func TestLabdRefusesAnyBodyOver90MB(t *testing.T) {
	called := false
	h := (&Server{}).capBodies(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	for length, want := range map[int64]int{90_000_000: 200, 90_000_001: 413} {
		called = false
		r := httptest.NewRequest("POST", "/api/agent/upload?job=j&name=journal", strings.NewReader("x"))
		r.ContentLength = length
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want || called != (want == 200) {
			t.Errorf("a body of %d bytes: %d, handler called %t", length, w.Code, called)
		}
		if want == 413 && !strings.Contains(w.Body.String(), "90 MB") {
			t.Errorf("the refusal does not name the limit: %q", w.Body.String())
		}
	}
	if uploadMax != 90_000_000 {
		t.Errorf("an agent's job file may be %d bytes, want 90 MB", uploadMax)
	}

	// The length is refused before a byte of the body is read, whoever asks.
	whole := transferLab(t, Transfers{}, nil)
	for _, path := range []string{"/api/artifacts", "/api/agent/upload?job=j&name=journal", "/api/builder/artifacts"} {
		u, _ := url.Parse(whole.url)
		conn, err := net.Dial("tcp", u.Host)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer lab_mtok\r\nContent-Length: 90000001\r\n\r\n", path, u.Host)
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil || resp.StatusCode != 413 {
			t.Errorf("%s with Content-Length over the limit: %v %v", path, resp, err)
		}
		conn.Close()
	}
	// With no Content-Length the limit holds while the body streams.
	b := transferLab(t, Transfers{BodyMax: 2000}, nil)
	builder := b.addBuilder("mini")
	running := b.queue("")
	b.poll(builder, "", 5*time.Second)
	for path, token := range map[string]string{"/api/artifacts": "lab_mtok", "/api/builder/artifacts?build=" + running.ID: builder} {
		pr, pw := io.Pipe()
		go func() { pw.Write(bytes.Repeat([]byte("x"), 4096)); pw.Close() }()
		if code, body := b.do(b.ctx, "POST", path, token, pr); code != 413 || strings.Contains(string(body), `"sha256"`) {
			t.Errorf("%s streaming past the limit: %d %s", path, code, body)
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(b.dir, "artifacts")); len(ents) != 1 {
		t.Fatalf("a refused body left %v beside the running build's input", ents)
	}
	if code, _ := b.do(b.ctx, "POST", "/api/artifacts", "lab_mtok", strings.NewReader("small")); code != 200 {
		t.Fatalf("a body under the limit: %d", code)
	}
}

func TestUnfinishedUploadsAreDeletedAfter24Hours(t *testing.T) {
	b := transferLab(t, Transfers{}, nil)
	old, fresh := partsContent(2500), partsContent(3500)
	for _, data := range [][]byte{old, fresh} {
		b.startUpload("lab_mtok", "/api/artifacts/uploads", data)
		if code, _ := b.do(b.ctx, "PUT", "/api/artifacts/uploads/"+artifactSHA(data)+"/parts/1", "lab_mtok", bytes.NewReader(data[:1000])); code != 200 {
			t.Fatalf("part: %d", code)
		}
	}
	dir := filepath.Join(b.dir, "artifacts")
	age := func(name string, d time.Duration) {
		when := time.Now().Add(-d)
		if err := os.Chtimes(filepath.Join(dir, name), when, when); err != nil {
			t.Fatal(err)
		}
	}
	age(".upload-"+artifactSHA(old), 25*time.Hour)
	age(".upload-"+artifactSHA(fresh), 23*time.Hour)
	b.s.gcArtifacts()
	if left := b.uploadDirs(); !slices.Equal(left, []string{".upload-" + artifactSHA(fresh)}) {
		t.Fatalf("after the collection: %v, want only the upload under 24 hours old", left)
	}
	// A part that arrives, or a resume, counts as the upload's latest hour.
	age(".upload-"+artifactSHA(fresh), 25*time.Hour)
	if st, _ := b.startUpload("lab_mtok", "/api/artifacts/uploads", fresh); !slices.Equal(st.Held, []int{1}) {
		t.Fatalf("held %v", st.Held)
	}
	b.s.gcArtifacts()
	if left := b.uploadDirs(); len(left) != 1 {
		t.Fatalf("a resumed upload was deleted: %v", left)
	}
}

func TestADownloadHonoursARangeAndIsPrivate(t *testing.T) {
	b := transferLab(t, Transfers{}, nil)
	data := partsContent(3000)
	sha, _, err := b.s.storeArtifact(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	// The Builder reads it as the input of the build it runs.
	builder := b.addBuilder("mini")
	if _, err := b.s.queueBuild(&api.Build{ID: "b-range", Key: "key-range", ConfigSHA: sha, State: api.BuildQueued, Created: b.s.now(),
		Source: api.Source{Repo: "https://github.com/labtest/linux", SHA: strings.Repeat("a", 40)}}, true); err != nil {
		t.Fatal(err)
	}
	b.poll(builder, "", 5*time.Second)
	get := func(rng string) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", b.url+"/api/builder/artifacts/"+sha, nil)
		req.Header.Set("Authorization", "Bearer "+builder)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}
	resp, body := get("")
	if resp.StatusCode != 200 || !bytes.Equal(body, data) || resp.Header.Get("Cache-Control") != "private" || resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("whole: %s, Cache-Control %q, Accept-Ranges %q", resp.Status, resp.Header.Get("Cache-Control"), resp.Header.Get("Accept-Ranges"))
	}
	resp, body = get("bytes=1200-")
	if resp.StatusCode != 206 || !bytes.Equal(body, data[1200:]) || resp.Header.Get("Cache-Control") != "private" || resp.Header.Get("Content-Range") != "bytes 1200-2999/3000" {
		t.Fatalf("from 1200: %s, Cache-Control %q, Content-Range %q", resp.Status, resp.Header.Get("Cache-Control"), resp.Header.Get("Content-Range"))
	}
	if resp, _ = get("bytes=3000-"); resp.StatusCode != 416 {
		t.Fatalf("past the end: %s", resp.Status)
	}
}

func TestANamedBuildersPartsBelongToTheBuildItRuns(t *testing.T) {
	b := transferLab(t, Transfers{}, nil)
	a, other := b.addBuilder("a"), b.addBuilder("other")
	k := b.queue("")
	if got, code := b.poll(a, "", 5*time.Second); code != 200 || got.Build.ID != k.ID {
		t.Fatalf("poll: %d %+v", code, got)
	}
	data := partsContent(2500)
	sha := artifactSHA(data)
	if _, code := b.startUpload(a, "/api/builder/artifacts/uploads", data); code != 403 {
		t.Fatalf("an upload that names no build: %d", code)
	}
	if _, code := b.startUpload(other, "/api/builder/artifacts/uploads?build="+k.ID, data); code != 403 {
		t.Fatalf("an upload for another builder's build: %d", code)
	}
	s := upload.Sender{Client: http.DefaultClient, URL: b.url + "/api/builder/artifacts", Query: url.Values{"build": {k.ID}}, Token: a, PartBytes: 1000}
	if got, err := s.File(b.ctx, tempFile(t, data)); err != nil || got != sha || !bytes.Equal(b.stored(sha), data) {
		t.Fatalf("the builder's upload: %q, %v", got, err)
	}
	b.s.builds.state.Lock()
	recorded := b.s.builds.claims[k.ID].uploads[sha]
	b.s.builds.state.Unlock()
	if !recorded {
		t.Fatal("the upload is not recorded for the build")
	}
	if code := b.done(a, k.ID, api.BuildResult{Release: "6.1.0-test", Artifact: sha, Files: []api.BuildFile{{Name: "vmlinuz", SHA256: sha, Size: 2500}}}); code >= 300 {
		t.Fatalf("done naming the upload: %d", code)
	}
	// Parts another builder sends under the same SHA-256 are not this build's.
	k2 := b.queue("")
	if got, code := b.poll(other, "", 5*time.Second); code != 200 || got.Build.ID != k2.ID {
		t.Fatalf("poll: %d %+v", code, got)
	}
	big := partsContent(4500)
	if _, code := b.startUpload(other, "/api/builder/artifacts/uploads?build="+k2.ID, big); code != 200 {
		t.Fatalf("start: %d", code)
	}
	if code, _ := b.do(b.ctx, "PUT", "/api/builder/artifacts/uploads/"+artifactSHA(big)+"/parts/1?build="+k.ID, a, bytes.NewReader(big[:1000])); code == 200 {
		t.Fatal("a builder sent a part for a build it no longer runs")
	}
}

// cutPart cuts the connection in the middle of the body of every PUT of one
// part, while on; it counts the PUTs labd is sent.
type cutPart struct {
	mu   sync.Mutex
	part string
	on   bool
	cuts int
	puts int
}

type cutBody struct {
	io.ReadCloser
	left int64
	cut  func()
}

func (c *cutBody) Read(p []byte) (int, error) {
	if c.left <= 0 {
		c.cut()
		return 0, io.ErrUnexpectedEOF
	}
	n, err := c.ReadCloser.Read(p[:min(int64(len(p)), c.left)])
	c.left -= int64(n)
	return n, err
}

func (c *cutPart) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			c.mu.Lock()
			c.puts++
			cut := c.on && strings.HasSuffix(r.URL.Path, "/parts/"+c.part)
			if cut {
				c.cuts++
			}
			c.mu.Unlock()
			if cut {
				r.Body = &cutBody{ReadCloser: r.Body, left: r.ContentLength / 2, cut: func() {
					if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
						conn.Close()
					}
				}}
			}
		}
		h.ServeHTTP(w, r)
	})
}

// cutAndResume uploads a file of parts parts: the first run is cut in the
// middle of one part until it gives up, the second sends what is missing.
func cutAndResume(t *testing.T, tr Transfers, path, sha string, parts, cutAt int) {
	cut := &cutPart{part: fmt.Sprint(cutAt), on: true}
	b := transferLab(t, tr, cut.wrap)
	s := b.sender()
	if got, err := s.File(b.ctx, path); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("part %d", cutAt)) {
		t.Fatalf("the cut upload: %q, %v", got, err)
	}
	if cut.cuts != 1+upload.Retries || cut.puts != cutAt-1+cut.cuts {
		t.Fatalf("%d PUTs, %d of them cut: want part %d cut %d times after %d whole parts", cut.puts, cut.cuts, cutAt, 1+upload.Retries, cutAt-1)
	}
	if _, err := os.Stat(b.s.artifactPath(sha)); err == nil {
		t.Fatal("a cut upload was stored")
	}
	cut.mu.Lock()
	cut.on, cut.puts = false, 0
	cut.mu.Unlock()
	var notes []string
	s.Note = func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) }
	got, err := s.File(b.ctx, path)
	if err != nil || got != sha {
		t.Fatalf("the resumed upload: %q, %v", got, err)
	}
	if cut.puts != parts-cutAt+1 || len(notes) != 1 || !strings.Contains(notes[0], fmt.Sprintf("%d of %d", cutAt-1, parts)) {
		t.Fatalf("the resume sent %d parts (notes %q), want %d", cut.puts, notes, parts-cutAt+1)
	}
	f, err := os.Open(b.s.artifactPath(sha))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil || hex.EncodeToString(h.Sum(nil)) != sha {
		t.Fatalf("the stored artifact's checksum is not the file's: %v", err)
	}
}

func TestAnUploadCutInTheMiddleOfAPartResumes(t *testing.T) {
	data := partsContent(7500)
	cutAndResume(t, Transfers{}, tempFile(t, data), artifactSHA(data), 8, 4)
}

func TestAOneGBUploadCutInTheMiddleOfAPartResumesAndItsChecksumMatches(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 3 GB to the temporary folder")
	}
	const size, block = 1 << 30, 1 << 20
	path := filepath.Join(t.TempDir(), "one-gb")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	buf := bytes.Repeat([]byte{0xa5}, block)
	for i := 0; i < size/block; i++ {
		copy(buf, fmt.Sprintf("block %08d", i))
		if _, err := io.MultiWriter(f, h).Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	cutAndResume(t, Transfers{PartBytes: 32 << 20}, path, hex.EncodeToString(h.Sum(nil)), 32, 17)
}

// Removing a builder ends a part it is sending, even one whose sender has
// stalled mid-body, and the part is not held.
func TestRemovingABuilderMidPart(t *testing.T) {
	b := transferLab(t, Transfers{}, nil)
	a := b.addBuilder("a")
	k := b.queue("")
	b.poll(a, "", 5*time.Second)
	data := partsContent(2500)
	sha := artifactSHA(data)
	if _, code := b.startUpload(a, "/api/builder/artifacts/uploads?build="+k.ID, data); code != 200 {
		t.Fatalf("start: %d", code)
	}
	pr, pw := io.Pipe()
	defer pw.Close()
	answered := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(b.ctx, "PUT", b.url+"/api/builder/artifacts/uploads/"+sha+"/parts/1?build="+k.ID, pr)
		req.ContentLength = 1000
		req.Header.Set("Authorization", "Bearer "+a)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			answered <- 0
			return
		}
		resp.Body.Close()
		answered <- resp.StatusCode
	}()
	pw.Write(data[:500])
	time.Sleep(300 * time.Millisecond) // labd has read that and waits for more
	if code := b.call("DELETE", "/api/builders/a", "lab_mtok", nil, nil); code != 200 {
		t.Fatalf("remove: %d", code)
	}
	select {
	case code := <-answered:
		if code == 200 {
			t.Fatal("a removed builder's part was accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled part outlived its builder")
	}
	go pw.Write(data[500:1000])
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(b.s.partUploadDir(k.ID, sha), "1")); err == nil {
		t.Fatal("labd holds a part its builder was removed while sending")
	}
}

// One upload's starts and parts take their turns: starting it again, by
// anyone and at any moment, never drops a part labd has answered for.
func TestStartsOfOneUploadAtOnceLoseNoPart(t *testing.T) {
	b := transferLab(t, Transfers{}, nil)
	for round := 0; round < 40 && !t.Failed(); round++ {
		data := append(partsContent(3999), byte(round))
		path := "/api/artifacts/uploads/" + artifactSHA(data)
		var wg sync.WaitGroup
		for n := 1; n <= 4; n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, code := b.startUpload("lab_mtok", "/api/artifacts/uploads", data); code != 200 {
					t.Errorf("round %d: start: %d", round, code)
					return
				}
				if code, msg := b.do(b.ctx, "PUT", fmt.Sprintf("%s/parts/%d", path, n), "lab_mtok", bytes.NewReader(data[(n-1)*1000:n*1000])); code != 200 {
					t.Errorf("round %d: part %d: %d %s", round, n, code, msg)
					return
				}
				if st, code := b.startUpload("lab_mtok", "/api/artifacts/uploads", data); code != 200 || !slices.Contains(st.Held, n) {
					t.Errorf("round %d: part %d was answered for, and a start then says labd holds %v (%d)", round, n, st.Held, code)
				}
			}()
		}
		wg.Wait()
		var stored api.StoredArtifact
		if code := b.call("POST", path+"/close", "lab_mtok", nil, &stored); !t.Failed() && (code != 200 || !bytes.Equal(b.stored(stored.SHA256), data)) {
			t.Errorf("round %d: close: %d", round, code)
		}
	}
}

// A part still arriving when its upload is closed finds no upload: it is
// not joined to a folder that is gone.
func TestAPartThatArrivesAfterItsUploadIsClosedIsToldToStartAgain(t *testing.T) {
	b := transferLab(t, Transfers{}, nil)
	data := partsContent(2500)
	path := "/api/artifacts/uploads/" + artifactSHA(data)
	b.startUpload("lab_mtok", "/api/artifacts/uploads", data)
	for n := 1; n <= 3; n++ {
		if code, msg := b.do(b.ctx, "PUT", fmt.Sprintf("%s/parts/%d", path, n), "lab_mtok", bytes.NewReader(data[(n-1)*1000:min(n*1000, 2500)])); code != 200 {
			t.Fatalf("part %d: %d %s", n, code, msg)
		}
	}
	pr, pw := io.Pipe()
	defer pw.Close()
	type answer struct {
		code int
		msg  string
	}
	answered := make(chan answer, 1)
	go func() {
		req, _ := http.NewRequestWithContext(b.ctx, "PUT", b.url+path+"/parts/1", pr)
		req.ContentLength = 1000
		req.Header.Set("Authorization", "Bearer lab_mtok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			answered <- answer{0, err.Error()}
			return
		}
		defer resp.Body.Close()
		msg, _ := io.ReadAll(resp.Body)
		answered <- answer{resp.StatusCode, string(msg)}
	}()
	pw.Write(data[:500])
	time.Sleep(300 * time.Millisecond) // labd has read that and waits for more
	if code := b.call("POST", path+"/close", "lab_mtok", nil, nil); code != 200 {
		t.Fatalf("close: %d", code)
	}
	pw.Write(data[500:1000])
	if a := <-answered; a.code != 404 || !strings.Contains(a.msg, "start it again") {
		t.Fatalf("the late part: %d %s", a.code, a.msg)
	}
	if left := b.uploadDirs(); len(left) != 0 {
		t.Fatalf("a closed upload left %v", left)
	}
}
