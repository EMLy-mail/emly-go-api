// Package tlsserver serves the API over HTTPS - and with it both WebSocket
// routes over WSS, since they share the one http.Server - from a certificate
// and key on disk (TLS_CERT_FILE / TLS_KEY_FILE).
//
// It exists for site mirrors with no public DNS name, where nothing like
// Let's Encrypt can issue a certificate: an internal CA issues one for the
// mirror's hostname/IP and the machines trust that CA through their Windows
// Root store, which is what the EMLy Updater's Go TLS stack verifies against.
//
// Two things beyond tls.LoadX509KeyPair:
//
//   - Validate refuses, at startup, a certificate no client would accept. The
//     obvious candidate lying around - the 3gIT code-signing certificate the
//     updater installs - is exactly such a one (EKU Code Signing, no SAN), and
//     it would otherwise start fine and fail every handshake on the client
//     side, where nobody reads the logs.
//   - Reloader picks up a renewed certificate without a restart: on a
//     handshake it re-stats the files at most once per checkEvery and reloads
//     when either changed. A renewal that fails to load or validate keeps the
//     previous certificate serving - same rule as BanList's snapshot: a bad
//     refresh must not take the service down.
package tlsserver

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// defaultCheckEvery bounds how often a handshake re-stats the files. A
// renewed certificate is picked up within this long; the cost is two stat
// calls per interval, not per handshake.
const defaultCheckEvery = time.Minute

// Reloader serves the current certificate through GetCertificate.
type Reloader struct {
	certFile, keyFile string
	checkEvery        time.Duration
	now               func() time.Time

	mu        sync.Mutex
	cert      *tls.Certificate
	certMod   time.Time
	keyMod    time.Time
	nextCheck time.Time
}

// New loads and validates the pair. An error here is a configuration error:
// the caller should refuse to start rather than serve plain HTTP on a port
// clients expect to speak TLS on.
func New(certFile, keyFile string) (*Reloader, error) {
	r := &Reloader{certFile: certFile, keyFile: keyFile, checkEvery: defaultCheckEvery, now: time.Now}
	certMod, keyMod, err := r.stat()
	if err != nil {
		return nil, err
	}
	cert, err := r.load()
	if err != nil {
		return nil, err
	}
	r.cert, r.certMod, r.keyMod = cert, certMod, keyMod
	r.nextCheck = r.now().Add(r.checkEvery)
	return r, nil
}

// Leaf is the certificate currently served.
func (r *Reloader) Leaf() *x509.Certificate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cert.Leaf
}

// GetCertificate is the tls.Config hook.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := r.now(); !now.Before(r.nextCheck) {
		r.nextCheck = now.Add(r.checkEvery)
		r.maybeReload()
	}
	return r.cert, nil
}

// maybeReload runs with r.mu held.
func (r *Reloader) maybeReload() {
	certMod, keyMod, err := r.stat()
	if err != nil {
		slog.Warn("tls certificate check failed, keeping the current one", "err", err)
		return
	}
	if certMod.Equal(r.certMod) && keyMod.Equal(r.keyMod) {
		return
	}
	cert, err := r.load()
	if err != nil {
		// Not recording the new mod times on purpose: a renewal written in
		// two steps (cert first, key a moment later) is retried on the next
		// check instead of being written off until the files change again.
		slog.Warn("tls certificate changed on disk but cannot be used, keeping the current one", "err", err)
		return
	}
	r.cert, r.certMod, r.keyMod = cert, certMod, keyMod
	LogCertificate("tls certificate reloaded", cert.Leaf, r.now())
}

func (r *Reloader) stat() (certMod, keyMod time.Time, err error) {
	ci, err := os.Stat(r.certFile)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("TLS_CERT_FILE: %w", err)
	}
	ki, err := os.Stat(r.keyFile)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("TLS_KEY_FILE: %w", err)
	}
	return ci.ModTime(), ki.ModTime(), nil
}

func (r *Reloader) load() (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading TLS_CERT_FILE/TLS_KEY_FILE: %w", err)
	}
	// LoadX509KeyPair fills Leaf since Go 1.23.
	if err := Validate(cert.Leaf, r.now()); err != nil {
		return nil, err
	}
	return &cert, nil
}

// Validate rejects a certificate that clients would refuse anyway, so the
// failure shows up once in the API's log instead of on every client.
func Validate(leaf *x509.Certificate, now time.Time) error {
	if leaf == nil {
		return errors.New("no leaf certificate")
	}
	subject := leaf.Subject.String()
	if now.Before(leaf.NotBefore) {
		return fmt.Errorf("certificate %q is not valid before %s", subject, leaf.NotBefore.UTC().Format(time.RFC3339))
	}
	if now.After(leaf.NotAfter) {
		return fmt.Errorf("certificate %q expired on %s", subject, leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	// No EKU at all means "any use" to Go and Windows alike; one that lists
	// usages has to list server authentication.
	if len(leaf.ExtKeyUsage) > 0 &&
		!slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) &&
		!slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageAny) {
		return fmt.Errorf("certificate %q is not valid for server authentication (extended key usage: %s) - a code-signing certificate cannot serve HTTPS, issue one with EKU serverAuth",
			subject, extKeyUsageNames(leaf.ExtKeyUsage))
	}
	// Go clients stopped falling back to the Common Name in 1.15, so a
	// certificate without SANs matches no host name at all.
	if len(leaf.DNSNames) == 0 && len(leaf.IPAddresses) == 0 {
		return fmt.Errorf("certificate %q has no Subject Alternative Name - add the server's DNS name and/or IP address as SAN", subject)
	}
	return nil
}

// expiryWarning is how close to NotAfter LogCertificate starts warning.
const expiryWarning = 30 * 24 * time.Hour

// LogCertificate logs what is being served, at warn level once expiry is
// near: a mirror's internal certificate has nobody renewing it automatically.
func LogCertificate(msg string, leaf *x509.Certificate, now time.Time) {
	names := append([]string(nil), leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		names = append(names, ip.String())
	}
	attrs := []any{
		"subject", leaf.Subject.String(),
		"issuer", leaf.Issuer.String(),
		"names", strings.Join(names, ","),
		"not_after", leaf.NotAfter.UTC().Format(time.RFC3339),
	}
	if left := leaf.NotAfter.Sub(now); left < expiryWarning {
		slog.Warn(msg+" - expiring soon", append(attrs, "days_left", int(left.Hours()/24))...)
		return
	}
	slog.Info(msg, attrs...)
}

// ServerConfig is the tls.Config for the API's http.Server.
func (r *Reloader) ServerConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: r.GetCertificate,
	}
}

// HTTP1Only is the protocol set for the TLS server. net/http would otherwise
// negotiate HTTP/2 via ALPN, and an HTTP/2 ResponseWriter implements no
// http.Hijacker, which coder/websocket's Accept needs. The updater's own
// client forces HTTP/1.1 for its upgrade and so would not notice, but a
// WebSocket carried inside an HTTP/2 connection (RFC 8441, which browsers
// use when a server offers it) would fail on GET /v2/client/ws and
// GET /v2/stats/stream. Pinning http/1.1 takes the question off the table;
// nothing the API serves gains enough from HTTP/2 to be worth it.
func HTTP1Only() *http.Protocols {
	var p http.Protocols
	p.SetHTTP1(true)
	return &p
}

func extKeyUsageNames(usages []x509.ExtKeyUsage) string {
	names := make([]string, 0, len(usages))
	for _, u := range usages {
		switch u {
		case x509.ExtKeyUsageServerAuth:
			names = append(names, "serverAuth")
		case x509.ExtKeyUsageClientAuth:
			names = append(names, "clientAuth")
		case x509.ExtKeyUsageCodeSigning:
			names = append(names, "codeSigning")
		case x509.ExtKeyUsageEmailProtection:
			names = append(names, "emailProtection")
		case x509.ExtKeyUsageTimeStamping:
			names = append(names, "timeStamping")
		default:
			names = append(names, fmt.Sprintf("eku(%d)", u))
		}
	}
	return strings.Join(names, ",")
}
