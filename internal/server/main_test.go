package server

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/testhome"
)

// The package's tests wait on real clocks: one after another they take over
// twenty minutes, most of it asleep. Run with no filter, the test binary
// hands them to copies of itself and runs a share here.
// LAB_TEST_SHARDS=1 turns that off; so does any -run, -skip, -list, -bench,
// -fuzz, -shuffle, cover or profile flag.
// named sends the tests' requests under a name of their own. Go's name is
// what a lab built from maclab sends, and labd refuses it on a lab's routes
// (see labcli.go).
type named struct{}

func (named) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("User-Agent") == "" {
		r = r.Clone(r.Context())
		r.Header.Set("User-Agent", "labd-tests")
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestMain(m *testing.M) {
	// No test reads or writes the home of whoever runs the tests. The copies
	// of this binary share the home the first one made.
	exit := os.Exit
	if os.Getenv("LAB_TEST_SHARD") == "" {
		exit = testhome.Set()
	}
	http.DefaultClient.Transport = named{}
	// macOS keeps its temporary folder behind a symlink (/var), and the code
	// under test refuses paths with a symlink among their ancestors.
	if dir, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		os.Setenv("TMPDIR", dir)
	}
	// The tests' git finds https://github.com/labtest/<name> in a folder of
	// this machine: a source has to be a public GitHub repository, and no
	// test reaches GitHub.
	os.Setenv("GIT_CONFIG_COUNT", "1")
	os.Setenv("GIT_CONFIG_KEY_0", "url.file://"+testGitRoot+"/.insteadOf")
	os.Setenv("GIT_CONFIG_VALUE_0", testGitHub)
	flag.Parse()
	n := shards()
	if n < 2 {
		exit(m.Run())
	}
	names, err := testNames()
	if err != nil || len(names) < n {
		exit(m.Run())
	}
	// This process runs every 2n-th test, so that its own result, test log and
	// timeout are real ones; n-1 copies take the rest one test at a time,
	// because a few tests take a minute and most a second.
	var own []string
	queue := make(chan string, len(names))
	for i, name := range names {
		if i%(2*n) == 0 {
			own = append(own, name)
		} else {
			queue <- name
		}
	}
	close(queue)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()
	logFile := flag.Lookup("test.testlogfile").Value.String()
	var args []string
	for _, a := range os.Args[1:] {
		if !strings.HasPrefix(a, "-test.testlogfile") {
			args = append(args, a)
		}
	}
	var (
		mu     sync.Mutex
		failed []string
		read   bytes.Buffer
		wg     sync.WaitGroup
	)
	// The copies get the environment as it is now: this process's own tests
	// change theirs with t.Setenv while copies start (a TMPDIR that is then
	// removed, among others).
	env := os.Environ()
	for w := 1; w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			childLog := logFile + "." + strconv.Itoa(w)
			defer os.Remove(childLog)
			for name := range queue {
				a := append(append([]string{}, args...), "-test.run="+pattern([]string{name}))
				if logFile != "" {
					a = append(a, "-test.testlogfile="+childLog)
				}
				cmd := exec.CommandContext(ctx, os.Args[0], a...)
				cmd.Env = append(append([]string{}, env...), "LAB_TEST_SHARD="+strconv.Itoa(w))
				out, err := cmd.CombinedOutput()
				mu.Lock()
				if err != nil {
					failed = append(failed, name)
				}
				if err != nil || testing.Verbose() {
					os.Stdout.Write(out)
				}
				if b, err := os.ReadFile(childLog); err == nil {
					_, rest, _ := bytes.Cut(b, []byte("\n"))
					read.Write(rest)
				}
				mu.Unlock()
			}
		}()
	}

	flag.Set("test.run", pattern(own))
	code := m.Run()
	wg.Wait()
	// What the copies read joins this process's test log, which the go command
	// keys its cache of the result on. The log is complete once m.Run returns.
	if f, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0); err == nil {
		f.Write(read.Bytes())
		f.Close()
	}
	if len(failed) > 0 {
		code = 1
		fmt.Printf("FAIL in their own processes: %s\n", strings.Join(failed, " "))
	}
	exit(code)
}

func shards() int {
	if os.Getenv("LAB_TEST_SHARD") != "" {
		return 1
	}
	for name, off := range map[string]string{
		"test.run": "", "test.skip": "", "test.list": "", "test.bench": "", "test.fuzz": "", "test.shuffle": "off",
		"test.coverprofile": "", "test.gocoverdir": "", "test.cpuprofile": "", "test.memprofile": "",
		"test.blockprofile": "", "test.mutexprofile": "", "test.trace": "",
	} {
		if f := flag.Lookup(name); f != nil && f.Value.String() != off {
			return 1
		}
	}
	if v := os.Getenv("LAB_TEST_SHARDS"); v != "" {
		n, _ := strconv.Atoi(v)
		return n
	}
	return min(12, 2*runtime.NumCPU())
}

func testNames() ([]string, error) {
	cmd := exec.Command(os.Args[0], "-test.list=^Test")
	cmd.Env = append(os.Environ(), "LAB_TEST_SHARD=list")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Fields(string(out)) {
		if strings.HasPrefix(line, "Test") {
			names = append(names, line)
		}
	}
	return names, nil
}

func pattern(names []string) string { return "^(" + strings.Join(names, "|") + ")$" }

func needsLinux(t *testing.T, what string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("needs Linux: " + what)
	}
}

// testGitHub is where the tests' repositories are said to be, and
// testGitRoot the folder git is told to find them in (gitRepo makes them).
const testGitHub = "https://github.com/labtest/"

var testGitRoot = filepath.Join(os.TempDir(), "labgit-"+strconv.Itoa(os.Getpid()))
