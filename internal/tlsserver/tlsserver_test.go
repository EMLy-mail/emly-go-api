package tlsserver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type certSpec struct {
	cn        string
	eku       []x509.ExtKeyUsage
	dnsNames  []string
	ips       []net.IP
	notBefore time.Time
	notAfter  time.Time
}

func goodSpec(cn string) certSpec {
	return certSpec{
		cn:        cn,
		eku:       []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		dnsNames:  []string{"localhost"},
		ips:       []net.IP{net.ParseIP("127.0.0.1")},
		notBefore: time.Now().Add(-time.Hour),
		notAfter:  time.Now().Add(365 * 24 * time.Hour),
	}
}

// writePair writes a self-signed cert/key to dir and returns their paths and
// the parsed certificate (usable as its own root).
func writePair(t *testing.T, dir string, s certSpec) (certFile, keyFile string, cert *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: s.cn},
		NotBefore:             s.notBefore,
		NotAfter:              s.notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           s.eku,
		DNSNames:              s.dnsNames,
		IPAddresses:           s.ips,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, cert
}

func TestNewLoadsAValidServerCertificate(t *testing.T) {
	certFile, keyFile, want := writePair(t, t.TempDir(), goodSpec("mirror"))
	r, err := New(certFile, keyFile)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !r.Leaf().Equal(want) {
		t.Fatal("Leaf is not the certificate on disk")
	}
}

func TestValidateRejectsUnusableCertificates(t *testing.T) {
	now := time.Now()
	cases := map[string]struct {
		mutate func(*certSpec)
		want   string
	}{
		// The 3gIT certificate the updater installs looks exactly like this.
		"code signing only": {func(s *certSpec) {
			s.eku = []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}
			s.dnsNames, s.ips = nil, nil
		}, "codeSigning"},
		"no SAN":        {func(s *certSpec) { s.dnsNames, s.ips = nil, nil }, "Subject Alternative Name"},
		"expired":       {func(s *certSpec) { s.notBefore, s.notAfter = now.Add(-48*time.Hour), now.Add(-24*time.Hour) }, "expired"},
		"not yet valid": {func(s *certSpec) { s.notBefore = now.Add(24 * time.Hour) }, "not valid before"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := goodSpec(name)
			tc.mutate(&s)
			certFile, keyFile, _ := writePair(t, t.TempDir(), s)
			_, err := New(certFile, keyFile)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

func TestValidateAcceptsACertificateWithoutEKU(t *testing.T) {
	s := goodSpec("any-use")
	s.eku = nil
	certFile, keyFile, _ := writePair(t, t.TempDir(), s)
	if _, err := New(certFile, keyFile); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestNewRejectsAMismatchedKey(t *testing.T) {
	certFile, _, _ := writePair(t, t.TempDir(), goodSpec("a"))
	_, keyFile, _ := writePair(t, t.TempDir(), goodSpec("b"))
	if _, err := New(certFile, keyFile); err == nil {
		t.Fatal("New accepted a key that does not match the certificate")
	}
}

// renew rewrites the pair in dir and pushes both mod times forward, so the
// change is visible even on a filesystem with coarse timestamps.
func renew(t *testing.T, dir string, s certSpec, at time.Time) *x509.Certificate {
	t.Helper()
	certFile, keyFile, cert := writePair(t, dir, s)
	for _, f := range []string{certFile, keyFile} {
		if err := os.Chtimes(f, at, at); err != nil {
			t.Fatal(err)
		}
	}
	return cert
}

func TestGetCertificatePicksUpARenewal(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, first := writePair(t, dir, goodSpec("first"))
	r, err := New(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	r.now = func() time.Time { return clock }
	r.nextCheck = clock.Add(r.checkEvery)

	second := renew(t, dir, goodSpec("second"), time.Now().Add(time.Hour))

	got, _ := r.GetCertificate(nil)
	if !got.Leaf.Equal(first) {
		t.Fatal("reloaded before checkEvery elapsed")
	}
	clock = clock.Add(r.checkEvery)
	got, _ = r.GetCertificate(nil)
	if !got.Leaf.Equal(second) {
		t.Fatal("renewed certificate not picked up after checkEvery")
	}
}

func TestGetCertificateKeepsServingWhenARenewalIsUnusable(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, first := writePair(t, dir, goodSpec("first"))
	r, err := New(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	r.now = func() time.Time { return clock }

	bad := goodSpec("bad")
	bad.eku = []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}
	renew(t, dir, bad, time.Now().Add(time.Hour))

	clock = clock.Add(r.checkEvery)
	got, err := r.GetCertificate(nil)
	if err != nil || !got.Leaf.Equal(first) {
		t.Fatalf("GetCertificate = %v, %v; want the previous certificate", got, err)
	}

	// Removing the files entirely is survived the same way.
	os.Remove(certFile)
	clock = clock.Add(r.checkEvery)
	if got, _ := r.GetCertificate(nil); !got.Leaf.Equal(first) {
		t.Fatal("missing files dropped the served certificate")
	}
}

// TestWebSocketUpgradeOverTLS runs a wss:// round trip through the server
// config main.go builds, and checks that a client offering h2 still lands on
// HTTP/1.1 (see HTTP1Only for why).
func TestWebSocketUpgradeOverTLS(t *testing.T) {
	certFile, keyFile, root := writePair(t, t.TempDir(), goodSpec("mirror"))
	r, err := New(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Upgrade") == "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			c, err := websocket.Accept(w, req, nil)
			if err != nil {
				t.Errorf("Accept: %v", err)
				return
			}
			defer c.CloseNow()
			typ, msg, err := c.Read(req.Context())
			if err == nil {
				_ = c.Write(req.Context(), typ, msg)
			}
		}),
		TLSConfig: r.ServerConfig(),
		Protocols: HTTP1Only(),
	}
	go srv.ServeTLS(ln, "", "")
	t.Cleanup(func() { srv.Close() })

	pool := x509.NewCertPool()
	pool.AddCert(root)
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool, NextProtos: []string{"h2", "http/1.1"}},
		ForceAttemptHTTP2: true,
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "wss://"+ln.Addr().String(), &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	_, msg, err := c.Read(ctx)
	if err != nil || string(msg) != "ping" {
		t.Fatalf("echo = %q, %v", msg, err)
	}

	// And a plain HTTPS request confirms h2 was really not negotiated.
	resp, err := client.Get("https://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.ProtoMajor != 1 {
		t.Fatalf("negotiated %s, want HTTP/1.1", resp.Proto)
	}
}
