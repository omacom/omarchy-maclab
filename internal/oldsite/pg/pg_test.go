package pg

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/omacom/omarchy-maclab/internal/oldsite"
	"github.com/omacom/omarchy-maclab/internal/oldsite/oldsitetest"
)

var update = flag.Bool("update", false, "rewrite testdata/old-site.sql.gz from the corpus")

func corpus(t testing.TB) *oldsitetest.Corpus {
	return oldsitetest.Made(t, bytes.Repeat([]byte{7}, 64))
}

// fake is a database/sql driver that answers the reader's questions from a
// corpus, with the values a Postgres gives, and keeps what it was asked.
type fake struct {
	corpus *oldsitetest.Corpus
	// columns is each table's columns and their types; counts what
	// count(*) answers.
	columns map[string][][2]string
	counts  oldsite.Counts

	mu      sync.Mutex
	asked   []string
	begun   []driver.TxOptions
	ended   []string
	closed  int
	failing string
}

func newFake(c *oldsitetest.Corpus) *fake {
	columns := map[string][][2]string{}
	for table, want := range tables {
		for name, kind := range want {
			columns[table] = append(columns[table], [2]string{name, kind})
		}
	}
	return &fake{corpus: c, columns: columns, counts: c.Counts()}
}

func (f *fake) db(t testing.TB) *sql.DB {
	db := sql.OpenDB(f)
	t.Cleanup(func() { db.Close() })
	return db
}

func (f *fake) Connect(context.Context) (driver.Conn, error) { return f, nil }
func (f *fake) Driver() driver.Driver                        { return nil }
func (f *fake) Prepare(string) (driver.Stmt, error)          { return nil, errors.New("not prepared") }
func (f *fake) Begin() (driver.Tx, error)                    { return nil, errors.New("no options") }

func (f *fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fake) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.begun = append(f.begun, opts)
	return fakeTx{f}, nil
}

type fakeTx struct{ f *fake }

func (tx fakeTx) end(how string) error {
	tx.f.mu.Lock()
	defer tx.f.mu.Unlock()
	tx.f.ended = append(tx.f.ended, how)
	return nil
}
func (tx fakeTx) Commit() error   { return tx.end("commit") }
func (tx fakeTx) Rollback() error { return tx.end("rollback") }

type fakeRows struct {
	names []string
	rows  [][]driver.Value
}

func (r *fakeRows) Columns() []string { return r.names }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

func null[T any](p *T, text func(T) driver.Value) driver.Value {
	if p == nil {
		return nil
	}
	return text(*p)
}

func (f *fake) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, query)
	if f.failing != "" && strings.Contains(query, f.failing) {
		return nil, errors.New(`FATAL: terminating connection to "db.internal.example" as user "postgres" (SQLSTATE 57P01)`)
	}
	stamp := func(s oldsite.Stamp) driver.Value { return s.String() }
	str := func(s string) driver.Value { return s }
	out := &fakeRows{}
	switch {
	case strings.Contains(query, "information_schema.columns"):
		out.names = []string{"column_name", "data_type"}
		for _, column := range f.columns[args[0].Value.(string)] {
			out.rows = append(out.rows, []driver.Value{column[0], column[1]})
		}
	case strings.Contains(query, "count(*)"):
		out.names = []string{"count"}
		n := map[bool]int64{true: f.counts.Testers}[strings.Contains(query, "FROM testers")]
		if strings.Contains(query, "FROM tester_bindings") {
			n = f.counts.TesterBindings
		} else if strings.Contains(query, "FROM reports") {
			n = f.counts.Reports
		}
		out.rows = [][]driver.Value{{n}}
	case strings.Contains(query, "FROM testers"):
		for _, row := range f.corpus.Testers {
			out.rows = append(out.rows, []driver.Value{row.ID, row.Login, null(row.GitHubID, func(n int64) driver.Value { return n }), stamp(row.CreatedAt), stamp(row.UpdatedAt)})
		}
		out.names = make([]string, 5)
	case strings.Contains(query, "FROM tester_bindings"):
		for _, row := range f.corpus.Bindings {
			out.rows = append(out.rows, []driver.Value{row.ID, row.MachineID, row.GitHubLogin, row.GitHubID, stamp(row.CreatedAt), stamp(row.UpdatedAt)})
		}
		out.names = make([]string, 6)
	case strings.Contains(query, "FROM reports"):
		for _, row := range f.corpus.Reports {
			out.rows = append(out.rows, []driver.Value{row.ID, row.PublicID, row.DeletionTokenDigest, row.SchemaVersion, row.Body,
				stamp(row.CreatedAt), stamp(row.UpdatedAt), null(row.HiddenAt, stamp), null(row.MachineID, str), null(row.TesterLogin, str)})
		}
		out.names = make([]string, 10)
	default:
		return nil, fmt.Errorf("the fake was asked %q", query)
	}
	return out, nil
}

type kept struct {
	testers  []oldsite.Tester
	bindings []oldsite.TesterBinding
	reports  []oldsite.Report
}

func (k *kept) Tester(row *oldsite.Tester) error {
	k.testers = append(k.testers, *row)
	return nil
}
func (k *kept) TesterBinding(row *oldsite.TesterBinding) error {
	k.bindings = append(k.bindings, *row)
	return nil
}
func (k *kept) Report(row *oldsite.Report) error {
	k.reports = append(k.reports, *row)
	return nil
}

func TestTheReaderGivesTheThreeTablesAsTheyStoodAtOneMoment(t *testing.T) {
	c := corpus(t)
	f := newFake(c)
	var got kept
	counts, err := read(context.Background(), f.db(t), &got)
	if err != nil {
		t.Fatal(err)
	}
	if counts != c.Counts() {
		t.Errorf("counted %v, the corpus has %v", counts, c.Counts())
	}
	if !reflect.DeepEqual(got.testers, c.Testers) || !reflect.DeepEqual(got.bindings, c.Bindings) || !reflect.DeepEqual(got.reports, c.Reports) {
		t.Error("the rows read are not the corpus's")
	}
	if len(f.begun) != 1 || !f.begun[0].ReadOnly || sql.IsolationLevel(f.begun[0].Isolation) != sql.LevelRepeatableRead {
		t.Errorf("the transaction was begun %+v: one read-only repeatable-read transaction is one moment", f.begun)
	}
	if len(f.ended) != 1 {
		t.Errorf("the transaction ended %v", f.ended)
	}
	for _, query := range f.asked {
		if !strings.HasPrefix(query, "SELECT ") || strings.Contains(query, ";") {
			t.Errorf("the reader sent %q", query)
		}
	}
}

func TestTheReaderWritesTheDumpTheImportReads(t *testing.T) {
	c := corpus(t)
	path := filepath.Join(t.TempDir(), "old-site.dump")
	src := &Postgres{db: newFake(c).db(t)}
	if _, err := oldsite.WriteDump(context.Background(), path, src, time.Now()); err != nil {
		t.Fatal(err)
	}
	dump, err := oldsite.OpenDump(path)
	if err != nil {
		t.Fatal(err)
	}
	var got kept
	if _, err := dump.Read(context.Background(), &got); err != nil {
		t.Fatal(err)
	}
	if dump.Rows != c.Counts() || !reflect.DeepEqual(got.reports, c.Reports) || !reflect.DeepEqual(got.bindings, c.Bindings) || !reflect.DeepEqual(got.testers, c.Testers) {
		t.Errorf("the dump holds %v, not the corpus", dump.Rows)
	}
}

func TestATableThatIsNotTheOldSitesIsRefused(t *testing.T) {
	for name, change := range map[string]func(f *fake){
		"a column more":       func(f *fake) { f.columns["reports"] = append(f.columns["reports"], [2]string{"uploader_ip", "inet"}) },
		"a column less":       func(f *fake) { f.columns["testers"] = f.columns["testers"][1:] },
		"a column's type":     func(f *fake) { f.columns["tester_bindings"][0][1] = "timestamp with time zone" },
		"no such table":       func(f *fake) { delete(f.columns, "tester_bindings") },
		"a row came or went":  func(f *fake) { f.counts.Reports++ },
		"a sign-in came":      func(f *fake) { f.counts.TesterBindings++ },
		"the connection died": func(f *fake) { f.failing = "FROM reports ORDER" },
	} {
		f := newFake(corpus(t))
		change(f)
		path := filepath.Join(t.TempDir(), "old-site.dump")
		_, err := oldsite.WriteDump(context.Background(), path, &Postgres{db: f.db(t)}, time.Now())
		if err == nil {
			t.Errorf("%s: a dump was written", name)
			continue
		}
		if _, serr := os.Stat(path); !os.IsNotExist(serr) {
			t.Errorf("%s: the dump was kept", name)
		}
		if strings.Contains(err.Error(), "db.internal.example") || strings.Contains(err.Error(), "postgres") {
			t.Errorf("%s: the error repeats what the server said: %v", name, err)
		}
	}
}

func TestOnlyAWholePostgresAddressIsTaken(t *testing.T) {
	for _, address := range []string{
		"", "db.example.net", "host=db.example.net user=u password=sekret dbname=d", "https://u:sekret@db.example.net/d",
		"postgres://db.example.net/d", "postgres://u@db.example.net/d", "postgres://u:sekret@db.example.net", "postgres://u:sekret@/d",
		"postgres://u:sekret@db.example.net/d?sslmode=disable", "postgres://u:sekret@db.example.net/d?sslmode=allow",
		"postgres://u:sekret@db.example.net/d?options=-c%20x", "postgres://u:sekret@db.example.net/d?service=old",
		"postgres://u:sekret@db.example.net,other.example.net/d", "postgres://u:sekret@db.example.net:notaport/d",
	} {
		p, err := Open(address, "")
		if err == nil {
			p.Close()
			t.Errorf("%q was taken as an address", address)
			continue
		}
		for _, part := range []string{"sekret", "db.example.net", "other.example.net"} {
			if strings.Contains(err.Error(), part) {
				t.Errorf("the refusal of %q repeats %q: %v", address, part, err)
			}
		}
	}
}

func config(t *testing.T, address string, caFile ...string) *pgx.ConnConfig {
	t.Helper()
	ca := ""
	if len(caFile) > 0 {
		ca = caFile[0]
	} else if !strings.Contains(address, "@127.0.0.1") && !strings.Contains(address, "@localhost") && !strings.Contains(address, "@[::1]") {
		ca = newAuthority(t).CAFile
	}
	p, err := Open(address, ca)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p.cfg
}

func TestAnAddressBeyondThisMachineIsReadOverTLSOnly(t *testing.T) {
	for _, address := range []string{
		"postgresql://u:sekret@db.example.net:5432/railway", "postgres://u:sekret@db.example.net/railway?sslmode=prefer",
		"postgres://u:sekret@db.example.net/railway?sslmode=require", "postgres://u:sekret@203.0.113.9:6543/railway",
	} {
		cfg := config(t, address)
		if cfg.TLSConfig == nil {
			t.Errorf("%s: no TLS", address)
		}
		for _, fallback := range cfg.Fallbacks {
			if fallback.TLSConfig == nil {
				t.Errorf("%s: falls back to no TLS", address)
			}
		}
	}
	for _, address := range []string{"postgres://u:sekret@127.0.0.1:5432/d?sslmode=disable", "postgres://u:sekret@localhost/d", "postgres://u:sekret@[::1]:5432/d?sslmode=disable"} {
		config(t, address)
	}
}

// Beyond this machine the password is answered only to SCRAM, which never
// shows it: a server, or whoever stands in for one, that asks for it in
// clear or as an MD5 gets no answer.
func TestBeyondThisMachineThePasswordIsNeverShown(t *testing.T) {
	remote := config(t, "postgresql://u:sekret@db.example.net:5432/railway")
	if remote.RequireAuth != "scram-sha-256" {
		t.Errorf("beyond this machine the client answers %q, want only scram-sha-256", remote.RequireAuth)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	sent := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			sent <- nil
			return
		}
		defer c.Close()
		buf := make([]byte, 4096)
		c.SetDeadline(time.Now().Add(10 * time.Second))
		c.Read(buf)                                  // the startup message
		c.Write([]byte{'R', 0, 0, 0, 8, 0, 0, 0, 3}) // "send the password in clear"
		n, _ := c.Read(buf)
		sent <- append([]byte(nil), buf[:n]...)
	}()
	asked := config(t, "postgres://u:sekret@"+ln.Addr().String()+"/d?sslmode=disable")
	asked.RequireAuth = remote.RequireAuth
	db := stdlib.OpenDB(*asked)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = db.PingContext(ctx)
	if err == nil {
		t.Fatal("a server that asks for the password in clear was taken")
	}
	db.Close()
	if got := <-sent; bytes.Contains(got, []byte("sekret")) {
		t.Errorf("the password was sent in clear")
	}
	if got := plain(err).Error(); got != "the old site's Postgres asked for the password in a way that shows it; it is answered only to SCRAM" {
		t.Errorf("the failure reads %q", got)
	}
}

func TestNothingOfTheEnvironmentReachesTheAddress(t *testing.T) {
	t.Setenv("PGHOST", "env.example.net")
	t.Setenv("PGPORT", "1")
	t.Setenv("PGUSER", "envuser")
	t.Setenv("PGPASSWORD", "envpassword")
	t.Setenv("PGDATABASE", "envdb")
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGOPTIONS", "-c statement_timeout=1")
	t.Setenv("PGAPPNAME", "envapp")
	t.Setenv("PGSERVICE", "envservice")
	cfg := config(t, "postgres://u:sekret@db.example.net:6543/railway")
	if cfg.Host != "db.example.net" || cfg.Port != 6543 || cfg.User != "u" || cfg.Password != "sekret" || cfg.Database != "railway" {
		t.Errorf("the address read is %s:%d %s %s", cfg.Host, cfg.Port, cfg.User, cfg.Database)
	}
	if cfg.TLSConfig == nil {
		t.Error("PGSSLMODE turned TLS off")
	}
	if len(cfg.RuntimeParams) != 0 {
		t.Errorf("runtime parameters from the environment: %v", cfg.RuntimeParams)
	}
}

func TestAFailureSaysNothingOfTheAddress(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, closedPort, _ := net.SplitHostPort(closed.Addr().String())
	closed.Close()
	for _, p := range []string{port, closedPort} {
		pg, err := Open("postgres://marker-user:marker-sekret@127.0.0.1:"+p+"/marker-db?sslmode=disable", "")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, err = pg.Read(ctx, &kept{})
		cancel()
		pg.Close()
		if err == nil {
			t.Fatal("a read from nothing worked")
		}
		for _, part := range []string{"marker", "127.0.0.1", p} {
			if strings.Contains(err.Error(), part) {
				t.Errorf("the failure repeats %q: %v", part, err)
			}
		}
	}
}

const sqlFile = "testdata/old-site.sql.gz"

func corpusSQL(t testing.TB) string {
	return "-- The made-up old site of internal/oldsite/oldsitetest. Rewrite with: go test ./internal/oldsite/pg -run TestTheCommittedSQL -update\n" +
		oldsitetest.Schema + "-- rows\n" + corpus(t).Rows()
}

func committedSQL(t testing.TB) string {
	t.Helper()
	f, err := os.Open(sqlFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	text, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}

// The SQL file that fills CI's Postgres once site/ is gone is the corpus
// every other import test reads.
func TestTheCommittedSQLIsTheCorpus(t *testing.T) {
	want := corpusSQL(t)
	if *update {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		zw.Write([]byte(want))
		zw.Close()
		if err := os.WriteFile(sqlFile, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if committedSQL(t) != want {
		t.Fatalf("%s is not the corpus: run this test with -update", sqlFile)
	}
}

// Against a real Postgres, as CI has one: LAB_TEST_POSTGRES is the address
// of a throwaway database on this machine, which the test empties and
// fills. With LAB_TEST_POSTGRES_SCHEMA=rails the three tables are the ones
// Rails made from site/db/schema.rb and only the rows are loaded.
func TestTheReaderReadsARealPostgres(t *testing.T) {
	address := os.Getenv("LAB_TEST_POSTGRES")
	if address == "" {
		t.Skip("no Postgres: set LAB_TEST_POSTGRES to a throwaway database on this machine")
	}
	cfg, err := pgx.ParseConfig(address)
	if err != nil || !loopback(cfg.Host) {
		t.Fatal("LAB_TEST_POSTGRES must name a database on this machine")
	}
	db := stdlib.OpenDB(*cfg)
	defer db.Close()
	ddl, rows, _ := strings.Cut(committedSQL(t), "-- rows\n")
	if os.Getenv("LAB_TEST_POSTGRES_SCHEMA") == "rails" {
		ddl = "TRUNCATE reports, tester_bindings, testers;\n"
	} else {
		ddl = "DROP TABLE IF EXISTS reports, tester_bindings, testers;\n" + ddl
	}
	for _, statement := range strings.SplitAfter(ddl+rows, ";\n") {
		if strings.TrimSpace(statement) == "" || strings.HasPrefix(statement, "--") && !strings.Contains(statement, "\nCREATE") {
			continue
		}
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%.80s: %v", statement, err)
		}
	}
	c := corpus(t)
	p, err := Open(address, "")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	path := filepath.Join(t.TempDir(), "old-site.dump")
	counts, err := oldsite.WriteDump(context.Background(), path, p, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if counts != c.Counts() {
		t.Errorf("the dump holds %v, the corpus %v", counts, c.Counts())
	}
	dump, err := oldsite.OpenDump(path)
	if err != nil {
		t.Fatal(err)
	}
	var got kept
	if _, err := dump.Read(context.Background(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.testers, c.Testers) || !reflect.DeepEqual(got.bindings, c.Bindings) {
		t.Error("testers or tester_bindings differ from the corpus")
	}
	for i, row := range got.reports {
		if i >= len(c.Reports) || !reflect.DeepEqual(row, c.Reports[i]) {
			t.Errorf("report %s differs from the corpus's", row.PublicID)
		}
	}
}
