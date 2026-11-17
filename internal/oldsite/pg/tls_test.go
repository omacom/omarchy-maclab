package pg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/omacom/omarchy-maclab/internal/oldsite/oldsitetest"
)

func newAuthority(t *testing.T) *oldsitetest.Authority { return oldsitetest.NewAuthority(t) }

func tlsPostgres(t *testing.T, a *oldsitetest.Authority) (string, chan []byte) {
	return a.TLSPostgres(t)
}

func ping(t *testing.T, p *Postgres) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db := stdlib.OpenDB(*p.cfg)
	defer db.Close()
	return plain(db.PingContext(ctx))
}

// Beyond this machine there is no read without the CA file: the address
// alone is refused before anything is dialled, and says nothing of itself.
func TestBeyondThisMachineNothingIsReadWithoutTheCAFile(t *testing.T) {
	for _, address := range []string{
		"postgresql://u:sekret@db.example.net:5432/railway", "postgres://u:sekret@db.example.net/railway?sslmode=require",
		"postgres://u:sekret@203.0.113.9:6543/railway?sslmode=verify-ca", "postgres://u:sekret@db.example.net/railway?sslmode=verify-full",
	} {
		p, err := Open(address, "")
		if err == nil {
			p.Close()
			t.Errorf("%s was taken with no CA file", address)
			continue
		}
		if !strings.Contains(err.Error(), "CA file") {
			t.Errorf("%s: the refusal reads %q", address, err)
		}
		for _, part := range []string{"sekret", "db.example.net", "203.0.113.9"} {
			if strings.Contains(err.Error(), part) {
				t.Errorf("the refusal repeats %q: %v", part, err)
			}
		}
	}
	// On this machine the CA file may be left out.
	for _, address := range []string{"postgres://u:sekret@127.0.0.1:5432/d?sslmode=disable", "postgres://u:sekret@localhost/d"} {
		p, err := Open(address, "")
		if err != nil {
			t.Errorf("%s: %v", address, err)
			continue
		}
		p.Close()
	}
}

func TestACAFileThatHoldsNoCertificateIsRefused(t *testing.T) {
	dir := t.TempDir()
	empty, notPEM := filepath.Join(dir, "empty.pem"), filepath.Join(dir, "key.txt")
	os.WriteFile(empty, nil, 0o644)
	os.WriteFile(notPEM, []byte("sekret-file-text\n"), 0o644)
	for _, file := range []string{empty, notPEM, filepath.Join(dir, "missing.pem")} {
		p, err := Open("postgres://u:sekret@db.example.net/railway", file)
		if err == nil {
			p.Close()
			t.Errorf("%s was taken as a CA file", filepath.Base(file))
			continue
		}
		if strings.Contains(err.Error(), "sekret") || strings.Contains(err.Error(), dir) {
			t.Errorf("the refusal repeats the file or the address: %v", err)
		}
	}
	a := newAuthority(t)
	if p, err := Open("postgres://u:sekret@127.0.0.1/d?sslmode=disable", a.CAFile); err == nil {
		p.Close()
		t.Error("a CA file with TLS turned off was taken")
	}
}

// With the CA file the certificate is checked against it alone (verify-ca):
// a server whose chain leads to it passes TLS whatever its host name, and
// one whose chain leads elsewhere gets nothing, the user included.
func TestTheCAFileIsWhatTheCertificateIsCheckedAgainst(t *testing.T) {
	a, other := newAuthority(t), newAuthority(t)
	port, got := tlsPostgres(t, a)
	address := "postgres://marker-user:marker-sekret@127.0.0.1:" + port + "/railway"

	p, err := Open(address, a.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	if p.cfg.TLSConfig == nil || len(p.cfg.Fallbacks) != 0 {
		t.Fatalf("TLS %v, %d fallbacks", p.cfg.TLSConfig != nil, len(p.cfg.Fallbacks))
	}
	if err := ping(t, p); err == nil || err.Error() != "the old site's Postgres did not accept that user and password" {
		t.Errorf("with its own CA file: %v", err)
	}
	p.Close()
	if read := <-got; !bytes.Contains(read, []byte("marker-user")) {
		t.Errorf("past TLS the server never heard the user: %q", read)
	}

	p, err = Open(address, other.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := ping(t, p); err == nil || err.Error() != "TLS with the old site's Postgres failed" {
		t.Errorf("with another CA file: %v", err)
	}
	p.Close()
	if read := <-got; bytes.Contains(read, []byte("marker")) {
		t.Errorf("a server the CA file does not vouch for heard %q", read)
	}

	// verify-full checks the host name too: 127.0.0.1 is not in the certificate.
	p, err = Open(address+"?sslmode=verify-full", a.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := ping(t, p); err == nil || err.Error() != "TLS with the old site's Postgres failed" {
		t.Errorf("verify-full with a certificate for another host: %v", err)
	}
	p.Close()
}

// Beyond this machine the CA file is what makes a read possible, and the
// password still goes only into SCRAM.
func TestBeyondThisMachineTheCAFileIsCheckedAndSCRAMStays(t *testing.T) {
	a := newAuthority(t)
	cfg := config(t, "postgres://u:sekret@db.example.net/railway?sslmode=prefer", a.CAFile)
	if cfg.TLSConfig == nil || cfg.TLSConfig.VerifyPeerCertificate == nil || len(cfg.Fallbacks) != 0 || cfg.RequireAuth != "scram-sha-256" {
		t.Errorf("TLS %v, checked %v, %d fallbacks, auth %q", cfg.TLSConfig != nil, cfg.TLSConfig != nil && cfg.TLSConfig.VerifyPeerCertificate != nil, len(cfg.Fallbacks), cfg.RequireAuth)
	}
}

// The chain is read to be looked at: leaf first, and nothing goes to the
// server past the request for TLS, neither the user nor the password.
func TestTheChainIsShownWithNothingSent(t *testing.T) {
	a := newAuthority(t)
	port, got := tlsPostgres(t, a)
	certs, err := Chain(context.Background(), "postgres://marker-user:marker-sekret@127.0.0.1:"+port+"/railway")
	if err != nil {
		t.Fatal(err)
	}
	if len(certs) != 2 || certs[0].Subject.CommonName != "not-the-host.example" || sha256.Sum256(certs[1].Raw) != sha256.Sum256(a.CA.Raw) {
		t.Errorf("the chain read: %d certificates", len(certs))
	}
	if read := <-got; len(read) != 8 || bytes.Contains(read, []byte("marker")) {
		t.Errorf("the server heard %q", read)
	}

	// A server that does not speak TLS, and one that is not there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			io.ReadFull(c, make([]byte, 8))
			c.Write([]byte{'N'})
			c.Close()
		}
	}()
	_, plainPort, _ := net.SplitHostPort(ln.Addr().String())
	if _, err := Chain(context.Background(), "postgres://u:sekret@127.0.0.1:"+plainPort+"/d"); err == nil || err.Error() != "the old site's Postgres does not speak TLS" {
		t.Errorf("a server with no TLS: %v", err)
	}
	if _, err := Chain(context.Background(), "postgres://u:sekret@db.example.net/d?sslmode=disable"); err == nil || strings.Contains(err.Error(), "sekret") {
		t.Errorf("an address that turns TLS off: %v", err)
	}
}
