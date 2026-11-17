// Package pg reads the old site's three tables from its Postgres, for the
// import command and nothing else: the Postgres client is linked into no
// other part of the lab.
//
// The address of that Postgres carries its password. It comes from the
// command's prompt, is kept in memory only, and no error from here repeats
// any part of it or anything the server said.
package pg

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/omacom/omarchy-maclab/internal/oldsite"
)

// tables is the old site's schema (site/db/schema.rb) as Postgres tells it:
// each table's columns and their types. A table with a column more or less
// is not read, so nothing the old site kept is left behind unnoticed.
var tables = map[string]map[string]string{
	"reports": {
		"id": "bigint", "public_id": "character varying", "deletion_token_digest": "character varying", "schema_version": "integer", "body": "jsonb",
		"created_at": "timestamp without time zone", "updated_at": "timestamp without time zone", "hidden_at": "timestamp without time zone",
		"machine_id": "character varying", "tester_login": "character varying",
	},
	"tester_bindings": {
		"id": "bigint", "machine_id": "character varying", "github_login": "character varying", "github_id": "bigint",
		"created_at": "timestamp without time zone", "updated_at": "timestamp without time zone",
	},
	"testers": {
		"id": "bigint", "login": "character varying", "github_id": "bigint",
		"created_at": "timestamp without time zone", "updated_at": "timestamp without time zone",
	},
}

// said is an error in this package's own words: it repeats nothing of the
// address and nothing the server sent.
type said string

func (s said) Error() string { return string(s) }

func sayf(format string, args ...any) error { return said(fmt.Sprintf(format, args...)) }

const shape = "postgresql://user:password@host:port/database"

// Postgres is the old site's database.
type Postgres struct {
	db  *sql.DB
	cfg *pgx.ConnConfig
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// address is a Postgres address as Open takes it, checked.
type address struct {
	u     *url.URL
	mode  string
	local bool
}

// parse checks an address, whole, as Railway shows it: no part of it is
// repeated in a refusal.
func parse(text string) (*address, error) {
	notOne := sayf("that is not a Postgres address: one reads %s", shape)
	u, err := url.Parse(strings.TrimSpace(text))
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.User == nil || u.Opaque != "" || u.Fragment != "" {
		return nil, notOne
	}
	password, _ := u.User.Password()
	database := strings.TrimPrefix(u.Path, "/")
	if u.User.Username() == "" || password == "" || u.Hostname() == "" || strings.Contains(u.Host, ",") || database == "" || strings.Contains(database, "/") {
		return nil, notOne
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return nil, notOne
		}
	}
	query := u.Query()
	for name := range query {
		if name != "sslmode" {
			return nil, sayf("the address has an option this command does not take (%q): only sslmode is", name)
		}
	}
	a := &address{u: u, mode: query.Get("sslmode"), local: loopback(u.Hostname())}
	switch a.mode {
	case "", "prefer", "require", "verify-ca", "verify-full":
	case "disable", "allow":
		if !a.local {
			return nil, sayf("the address turns TLS off (sslmode=%s): beyond this machine the old site's Postgres is read over TLS only", a.mode)
		}
	default:
		return nil, notOne
	}
	return a, nil
}

func (a *address) hostPort() string {
	port := a.u.Port()
	if port == "" {
		port = "5432"
	}
	return net.JoinHostPort(a.u.Hostname(), port)
}

// authorities reads the CA file: the certificates the old site's Postgres
// certificate must chain to. Nothing of the file is repeated.
func authorities(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, said("the CA file could not be read")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, said("the CA file holds no certificate (PEM)")
	}
	return pool, nil
}

// verifyCA checks the certificate the server shows against the CA file's
// authorities alone, as sslmode verify-ca does: the chain, not the host
// name, which verify-full adds.
func verifyCA(pool *x509.CertPool, host string, full bool) func([][]byte, [][]*x509.Certificate) error {
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return x509.CertificateInvalidError{Reason: x509.NotAuthorizedToSign, Detail: "no certificate"}
		}
		certs := make([]*x509.Certificate, len(raw))
		for i, der := range raw {
			cert, err := x509.ParseCertificate(der)
			if err != nil {
				return x509.CertificateInvalidError{Reason: x509.NotAuthorizedToSign, Detail: "a certificate that does not parse"}
			}
			certs[i] = cert
		}
		opts := x509.VerifyOptions{Roots: pool, Intermediates: x509.NewCertPool()}
		for _, cert := range certs[1:] {
			opts.Intermediates.AddCert(cert)
		}
		if full {
			opts.DNSName = host
		}
		_, err := certs[0].Verify(opts)
		return err
	}
}

// Open takes the address of the old site's Postgres, whole, as Railway
// shows it: postgresql://user:password@host:port/database, and the CA file
// its certificate must chain to. Nothing is read from the environment in
// their place. Beyond this machine the connection is made over TLS with
// the certificate checked against the CA file (sslmode verify-ca, or
// verify-full when the address asks), or not at all, and the password is
// answered only to SCRAM. On this machine the CA file may be left out.
// Open connects to nothing.
func Open(text, caFile string) (*Postgres, error) {
	a, err := parse(text)
	if err != nil {
		return nil, err
	}
	if !a.local && caFile == "" {
		return nil, said("beyond this machine the old site's Postgres is read only with its certificate checked: give the CA file its certificate chains to (labd import check certificate shows the chain)")
	}
	var pool *x509.CertPool
	if caFile != "" {
		if a.mode == "disable" || a.mode == "allow" {
			return nil, sayf("a CA file was given and the address turns TLS off (sslmode=%s)", a.mode)
		}
		if pool, err = authorities(caFile); err != nil {
			return nil, err
		}
	}
	for _, pair := range os.Environ() {
		if name, _, _ := strings.Cut(pair, "="); strings.HasPrefix(name, "PG") {
			os.Unsetenv(name)
		}
	}
	u := *a.u
	if pool != nil {
		// TLS that is not given up; the check of who answers is ours, below.
		query := u.Query()
		query.Set("sslmode", "require")
		u.RawQuery = query.Encode()
	}
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		return nil, sayf("that is not a Postgres address: one reads %s", shape)
	}
	if pool != nil {
		if cfg.TLSConfig == nil {
			return nil, said("the address turns TLS off: with a CA file the old site's Postgres is read over TLS only")
		}
		cfg.TLSConfig.InsecureSkipVerify = true
		cfg.TLSConfig.VerifyPeerCertificate = verifyCA(pool, a.u.Hostname(), a.mode == "verify-full")
		cfg.Fallbacks = nil
	}
	if !a.local {
		// The modes that give none were refused above; this holds whatever the client makes of the rest.
		if cfg.TLSConfig == nil {
			return nil, sayf("the address turns TLS off: beyond this machine the old site's Postgres is read over TLS only")
		}
		var overTLS []*pgconn.FallbackConfig
		for _, fallback := range cfg.Fallbacks {
			if fallback.TLSConfig != nil {
				overTLS = append(overTLS, fallback)
			}
		}
		cfg.Fallbacks = overTLS
		// The password goes only into SCRAM, which never shows it, even to
		// a server the CA file vouches for.
		cfg.RequireAuth = "scram-sha-256"
	}
	cfg.ConnectTimeout = 20 * time.Second
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(1)
	return &Postgres{db: db, cfg: cfg}, nil
}

// Chain is the certificate chain the old site's Postgres shows, leaf
// first, taken with no check of it and nothing sent past the request for
// TLS: neither the user nor the password. It is for looking at before the
// CA file is trusted.
func Chain(ctx context.Context, text string) ([]*x509.Certificate, error) {
	a, err := parse(text)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", a.hostPort())
	if err != nil {
		return nil, plain(err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	// SSLRequest: length 8, code 80877103.
	if _, err := conn.Write([]byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}); err != nil {
		return nil, said("no connection could be made to the old site's Postgres")
	}
	answer := make([]byte, 1)
	if _, err := io.ReadFull(conn, answer); err != nil {
		return nil, said("the old site's Postgres did not answer the request for TLS")
	}
	if answer[0] != 'S' {
		return nil, said("the old site's Postgres does not speak TLS")
	}
	client := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: a.u.Hostname(), MinVersion: tls.VersionTLS12})
	if err := client.HandshakeContext(ctx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
			return nil, said("the old site's Postgres did not answer in time")
		}
		return nil, said("TLS with the old site's Postgres failed")
	}
	certs := client.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, said("the old site's Postgres showed no certificate")
	}
	return certs, nil
}

func (p *Postgres) Close() error { return p.db.Close() }

// Read reads the three tables inside one read-only transaction, so that
// they are the tables of one moment, and writes nothing.
func (p *Postgres) Read(ctx context.Context, sink oldsite.Sink) (oldsite.Counts, error) {
	return read(ctx, p.db, sink)
}

// plain is a failure in words that are safe to print: a driver's own error
// names the host, the user and the database, and the server's may quote
// more.
func plain(err error) error {
	var own said
	var server *pgconn.PgError
	var dns *net.DNSError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	var connect *pgconn.ConnectError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &own):
		return own
	case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		return said("the old site's Postgres did not answer in time")
	case errors.Is(err, context.Canceled):
		return said("the read was stopped")
	case errors.As(err, &server):
		switch server.Code {
		case "28P01", "28000":
			return said("the old site's Postgres did not accept that user and password")
		case "3D000":
			return said("the old site's Postgres has no database of that name")
		case "42P01":
			return said("the old site's Postgres lacks one of the three tables")
		case "42501":
			return said("that user may not read the old site's tables")
		}
		return sayf("the old site's Postgres refused (SQLSTATE %s)", server.Code)
	case errors.As(err, &dns):
		return said("the address's host name was not found")
	case errors.Is(err, syscall.ECONNREFUSED):
		return said("the connection was refused at that host and port")
	case errors.As(err, &unknown), errors.As(err, &hostname), errors.As(err, &invalid), errors.As(err, &record), errors.As(err, &alert):
		return said("TLS with the old site's Postgres failed")
	case errors.As(err, &connect) && strings.Contains(connect.Error(), "require_auth"):
		return said("the old site's Postgres asked for the password in a way that shows it; it is answered only to SCRAM")
	case errors.As(err, &connect):
		return said("no connection could be made to the old site's Postgres")
	}
	return said("reading the old site's Postgres failed")
}

const stampFormat = `'YYYY-MM-DD"T"HH24:MI:SS.US'`

func stampOf(table, column, text string, id int64) (oldsite.Stamp, error) {
	at, err := oldsite.ParseStamp(text)
	if err != nil {
		return 0, sayf("%s row %d: its %s is not a time to the microsecond", table, id, column)
	}
	return at, nil
}

func read(ctx context.Context, db *sql.DB, sink oldsite.Sink) (oldsite.Counts, error) {
	var counts oldsite.Counts
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return counts, plain(err)
	}
	defer tx.Rollback()

	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, table := range names {
		if err := sameColumns(ctx, tx, table); err != nil {
			return counts, err
		}
	}
	for table, into := range map[string]*int64{"testers": &counts.Testers, "tester_bindings": &counts.TesterBindings, "reports": &counts.Reports} {
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(into); err != nil {
			return counts, plain(err)
		}
	}

	rows, err := tx.QueryContext(ctx, "SELECT id, login, github_id, to_char(created_at, "+stampFormat+"), to_char(updated_at, "+stampFormat+") FROM testers ORDER BY id")
	if err != nil {
		return counts, plain(err)
	}
	err = each(rows, "testers", func() (int64, error) {
		var row oldsite.Tester
		var githubID sql.NullInt64
		var created, updated string
		if err := rows.Scan(&row.ID, &row.Login, &githubID, &created, &updated); err != nil {
			return row.ID, err
		}
		if githubID.Valid {
			row.GitHubID = &githubID.Int64
		}
		if row.CreatedAt, err = stampOf("testers", "created_at", created, row.ID); err != nil {
			return row.ID, err
		}
		if row.UpdatedAt, err = stampOf("testers", "updated_at", updated, row.ID); err != nil {
			return row.ID, err
		}
		return row.ID, sink.Tester(&row)
	})
	if err != nil {
		return counts, err
	}

	rows, err = tx.QueryContext(ctx, "SELECT id, machine_id, github_login, github_id, to_char(created_at, "+stampFormat+"), to_char(updated_at, "+stampFormat+") FROM tester_bindings ORDER BY id")
	if err != nil {
		return counts, plain(err)
	}
	err = each(rows, "tester_bindings", func() (int64, error) {
		var row oldsite.TesterBinding
		var created, updated string
		if err := rows.Scan(&row.ID, &row.MachineID, &row.GitHubLogin, &row.GitHubID, &created, &updated); err != nil {
			return row.ID, err
		}
		if row.CreatedAt, err = stampOf("tester_bindings", "created_at", created, row.ID); err != nil {
			return row.ID, err
		}
		if row.UpdatedAt, err = stampOf("tester_bindings", "updated_at", updated, row.ID); err != nil {
			return row.ID, err
		}
		return row.ID, sink.TesterBinding(&row)
	})
	if err != nil {
		return counts, err
	}

	rows, err = tx.QueryContext(ctx, "SELECT id, public_id, deletion_token_digest, schema_version, body::text, to_char(created_at, "+stampFormat+"), to_char(updated_at, "+stampFormat+
		"), to_char(hidden_at, "+stampFormat+"), machine_id, tester_login FROM reports ORDER BY id")
	if err != nil {
		return counts, plain(err)
	}
	err = each(rows, "reports", func() (int64, error) {
		var row oldsite.Report
		var created, updated string
		var hidden, machine, login sql.NullString
		if err := rows.Scan(&row.ID, &row.PublicID, &row.DeletionTokenDigest, &row.SchemaVersion, &row.Body, &created, &updated, &hidden, &machine, &login); err != nil {
			return row.ID, err
		}
		if row.CreatedAt, err = stampOf("reports", "created_at", created, row.ID); err != nil {
			return row.ID, err
		}
		if row.UpdatedAt, err = stampOf("reports", "updated_at", updated, row.ID); err != nil {
			return row.ID, err
		}
		if hidden.Valid {
			at, err := stampOf("reports", "hidden_at", hidden.String, row.ID)
			if err != nil {
				return row.ID, err
			}
			row.HiddenAt = &at
		}
		if machine.Valid {
			row.MachineID = &machine.String
		}
		if login.Valid {
			row.TesterLogin = &login.String
		}
		return row.ID, sink.Report(&row)
	})
	return counts, err
}

// each takes the rows of one table. A row that does not read as the old
// site's is told of by its table and the id before it, never by its text.
func each(rows *sql.Rows, table string, row func() (int64, error)) error {
	defer rows.Close()
	var last int64
	for rows.Next() {
		id, err := row()
		var own said
		switch {
		case err == nil:
			last = id
		case errors.As(err, &own):
			return own
		case strings.HasPrefix(err.Error(), "sql: Scan error"):
			return sayf("the row of %s after id %d does not read as the old site's", table, last)
		default:
			// The sink's own failure: a file that cannot be written.
			return err
		}
	}
	return plain(rows.Err())
}

func sameColumns(ctx context.Context, tx *sql.Tx, table string) error {
	rows, err := tx.QueryContext(ctx, "SELECT column_name, data_type FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1 ORDER BY column_name", table)
	if err != nil {
		return plain(err)
	}
	defer rows.Close()
	want := tables[table]
	seen := 0
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			return plain(err)
		}
		switch expected, known := want[name]; {
		case !known:
			return sayf("the old site's %s has a column this command does not carry: %s. Nothing was read", table, name)
		case expected != kind:
			return sayf("the old site's %s.%s is %s, not %s. Nothing was read", table, name, kind, expected)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		return plain(err)
	}
	if seen == 0 {
		return sayf("the old site's Postgres has no table %s where that address looks", table)
	}
	if seen != len(want) {
		return sayf("the old site's %s lacks a column this command carries. Nothing was read", table)
	}
	return nil
}
