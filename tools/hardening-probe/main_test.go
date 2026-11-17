package main

import (
	"bytes"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/testhome"
)

func TestMain(m *testing.M) { testhome.Main(m) }

// What the probe prints of an answer never carries a credential, whatever
// the lab put in it: a builder's token, a Lab token, a Join command's.
func TestThePrintedAnswerCarriesNoCredential(t *testing.T) {
	var buf bytes.Buffer
	out = &buf
	defer func() { out = os.Stdout }()
	for _, body := range []string{
		`{"name":"b1","token":"bldr_0123456789abcdef0123456789abcdef01234567"}`,
		`Your token: lab_0123456789abcdef0123456789abcdef01234567`,
		`curl -fsSL https://lab.example/join | sudo bash -s -- enr_0123456789abcdef0123456789abcdef`,
	} {
		buf.Reset()
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Location": {"/x?t=" + body}}}
		if err := sayAnswer(resp, []byte(body), ""); err != nil {
			t.Fatal(err)
		}
		for _, prefix := range []string{"bldr_0", "lab_0", "enr_0"} {
			if strings.Contains(buf.String(), prefix) {
				t.Errorf("the probe printed a credential:\n%s", buf.String())
			}
		}
		if !strings.Contains(buf.String(), "(a credential, not shown)") {
			t.Errorf("the probe does not say what it left out:\n%s", buf.String())
		}
	}
	buf.Reset()
	sayAnswer(&http.Response{StatusCode: 401, Header: http.Header{}}, []byte("no lab_session cookie"), "")
	if !strings.Contains(buf.String(), "no lab_session cookie") {
		t.Errorf("a name that is no credential was hidden:\n%s", buf.String())
	}
}

// A browser's request, a page or a form, gives up at its deadline: a lab
// that never answers does not hold the test for ever.
func TestABrowserRequestHasADeadline(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer srv.Close()
	defer close(stop)
	t.Setenv("LAB_ADDRESS", srv.URL)
	session := filepath.Join(t.TempDir(), "session")
	if err := os.WriteFile(session, []byte("s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	was := browserTimeout
	browserTimeout = 300 * time.Millisecond
	defer func() { browserTimeout = was }()
	var buf bytes.Buffer
	out = &buf
	defer func() { out = os.Stdout }()
	for name, call := range map[string]func() error{
		"page": func() error {
			return pageCommand(flag.NewFlagSet("page", flag.ContinueOnError), []string{"--session", session, "/me"})
		},
		"form": func() error {
			return formCommand(flag.NewFlagSet("form", flag.ContinueOnError), []string{"--session", session, "/me/tokens", "name=x"})
		},
	} {
		done := make(chan error, 1)
		go func() { done <- call() }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: a lab that never answered gave no error", name)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: no deadline", name)
		}
	}
}

// --rows reads a key inside an object (a build's source.input) as well as
// one at the top.
func TestRowsReadAKeyInsideAnObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":"b1","source":{"input":"https://github.com/o/linux-aurora"}},{"id":"b2","source":{}},{"id":"b3"}]`))
	}))
	defer srv.Close()
	t.Setenv("LAB_ADDRESS", srv.URL)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("lab_x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	out = &buf
	defer func() { out = os.Stdout }()
	if err := askCommand(flag.NewFlagSet("ask", flag.ContinueOnError), []string{"--token-file", token, "--rows", "id,source.input,id.x", "GET", "/api/builds"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"row=b1|https://github.com/o/linux-aurora|\n", "row=b2||\n", "row=b3||\n"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the rows lack %q:\n%s", want, buf.String())
		}
	}
}
