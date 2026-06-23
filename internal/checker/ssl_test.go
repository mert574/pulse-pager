package checker

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"pulse/internal/domain"
)

// makeCert builds a self-signed leaf good for the given validity window and SANs.
// It returns the tls.Certificate to serve and an x509 pool that trusts it, so the
// checker can verify the chain in tests without touching the system roots.
func makeCert(t *testing.T, notBefore, notAfter time.Time, dnsNames []string, ips []net.IP) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(42),
		Subject:               pkix.Name{CommonName: "test-leaf"},
		Issuer:                pkix.Name{CommonName: "test-ca"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true, // self-signed: it is its own issuer
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// startTLS starts an httptest TLS server using the given certificate.
func startTLS(t *testing.T, cert tls.Certificate) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func sslMonitor(url string) *domain.Monitor {
	return &domain.Monitor{ID: 1, Type: domain.MonitorSSL, URL: url, TimeoutSeconds: 5}
}

func reasonOf(r *domain.CheckResult) domain.FailureReason {
	if r.FailureReason == nil {
		return ""
	}
	return *r.FailureReason
}

func TestCheckSSL(t *testing.T) {
	now := time.Now()
	loopback := []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback}

	tests := []struct {
		name        string
		notBefore   time.Time
		notAfter    time.Time
		ips         []net.IP
		wantHealthy bool
		wantReason  domain.FailureReason
	}{
		{
			name:        "healthy far from expiry",
			notBefore:   now.Add(-24 * time.Hour),
			notAfter:    now.Add(60 * 24 * time.Hour),
			ips:         loopback,
			wantHealthy: true,
		},
		{
			name:       "expiring within the widest threshold",
			notBefore:  now.Add(-24 * time.Hour),
			notAfter:   now.Add(5 * 24 * time.Hour),
			ips:        loopback,
			wantReason: domain.ReasonCertExpiringSoon,
		},
		{
			name:       "already expired",
			notBefore:  now.Add(-10 * 24 * time.Hour),
			notAfter:   now.Add(-1 * time.Hour),
			ips:        loopback,
			wantReason: domain.ReasonCertExpired,
		},
		{
			name:       "hostname mismatch is invalid",
			notBefore:  now.Add(-24 * time.Hour),
			notAfter:   now.Add(60 * 24 * time.Hour),
			ips:        []net.IP{net.ParseIP("10.1.2.3")}, // not the loopback we dial
			wantReason: domain.ReasonCertInvalid,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cert, pool := makeCert(t, tc.notBefore, tc.notAfter, nil, tc.ips)
			srv := startTLS(t, cert)
			c := New(Config{RootCAs: pool})

			res := c.Check(t.Context(), sslMonitor(srv.URL), false)

			if res.Healthy != tc.wantHealthy {
				t.Fatalf("healthy = %v, want %v (reason %q)", res.Healthy, tc.wantHealthy, reasonOf(res))
			}
			if got := reasonOf(res); got != tc.wantReason {
				t.Fatalf("reason = %q, want %q", got, tc.wantReason)
			}
			// We always learn the expiry once a leaf is presented, even when invalid.
			if res.CertExpiresAt == nil {
				t.Fatal("CertExpiresAt is nil, want the leaf NotAfter")
			}
			// ASN.1 stores the time to whole seconds, so compare with a tolerance.
			if d := res.CertExpiresAt.Sub(tc.notAfter); d > time.Second || d < -time.Second {
				t.Fatalf("CertExpiresAt = %v, want ~%v", res.CertExpiresAt, tc.notAfter)
			}
			// Self-signed, so issuer == subject.
			if res.CertInfo == nil || res.CertInfo.Subject != "test-leaf" {
				t.Fatalf("CertInfo = %+v, want subject test-leaf", res.CertInfo)
			}
		})
	}
}

func TestCheckSSLConnectionRefused(t *testing.T) {
	// A closed port is a connection error, and no cert is learned.
	c := New(Config{})
	res := c.Check(t.Context(), sslMonitor("https://127.0.0.1:1"), false)
	if res.Healthy {
		t.Fatal("expected unhealthy")
	}
	if r := reasonOf(res); r != domain.ReasonConnectionError && r != domain.ReasonTimeout {
		t.Fatalf("reason = %q, want connection_error/timeout", r)
	}
	if res.CertExpiresAt != nil {
		t.Fatalf("CertExpiresAt = %v, want nil on a failed dial", res.CertExpiresAt)
	}
}
