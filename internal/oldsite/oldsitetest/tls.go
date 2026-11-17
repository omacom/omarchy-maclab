package oldsitetest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Authority is a throwaway CA and a server certificate it signed, for a
// host name that is not the one dialled: sslmode verify-ca checks the
// chain only.
type Authority struct {
	CA, Leaf *x509.Certificate
	// CAFile is the CA as PEM, in a temp folder.
	CAFile string
	server tls.Certificate
}

func NewAuthority(t testing.TB) *Authority {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "throwaway root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "not-the-host.example"}, DNSNames: []string{"not-the-host.example"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	ca, _ := x509.ParseCertificate(caDER)
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(leafDER)
	file := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Authority{CA: ca, Leaf: leaf, CAFile: file, server: tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: key}}
}

// TLSPostgres answers on loopback as a Postgres over TLS with the
// authority's chain. After the handshake it refuses the user (SQLSTATE
// 28P01), so a client that got that far passed TLS. Heard gets everything
// a connection sent, the request for TLS included, once it is done.
func (a *Authority) TLSPostgres(t testing.TB) (port string, heard chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	heard = make(chan []byte, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go a.answer(c, heard)
		}
	}()
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return port, heard
}

func (a *Authority) answer(c net.Conn, heard chan []byte) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	var read bytes.Buffer
	defer func() { heard <- read.Bytes() }()
	request := make([]byte, 8)
	if _, err := io.ReadFull(c, request); err != nil {
		return
	}
	read.Write(request)
	c.Write([]byte{'S'})
	server := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{a.server}})
	if server.Handshake() != nil {
		return
	}
	buf := make([]byte, 4096)
	n, _ := server.Read(buf)
	read.Write(buf[:n])
	if n == 0 {
		return
	}
	fields := []byte("SFATAL\x00C28P01\x00Mno\x00\x00")
	msg := append([]byte{'E'}, binary.BigEndian.AppendUint32(nil, uint32(len(fields)+4))...)
	server.Write(append(msg, fields...))
	io.Copy(io.Discard, server)
}
