package fronting

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

// newTestCertificate returns a self-signed certificate for the given names.
// IP literals become IP SANs, everything else a DNS SAN.
func newTestCertificate(t *testing.T, commonName string, names ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, name)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func newTestSelector(t *testing.T, certs ...tls.Certificate) *CertSelector {
	t.Helper()
	selector, err := NewCertSelector(certs)
	if err != nil {
		t.Fatalf("NewCertSelector: %v", err)
	}
	return selector
}

// localAddrConn reports a fixed local address, standing in for the socket a
// client connected to.
type localAddrConn struct {
	net.Conn
	local net.Addr
}

func (c localAddrConn) LocalAddr() net.Addr { return c.local }

func connOn(ip string) net.Conn {
	return localAddrConn{local: &net.TCPAddr{IP: net.ParseIP(ip), Port: 443}}
}

func TestCertSelectorGetCertificate(t *testing.T) {
	selector := newTestSelector(t,
		newTestCertificate(t, "", "site.example.test", "*.wild.example.test", "203.0.113.7"))

	cases := []struct {
		name       string
		serverName string
		conn       net.Conn
		wantCert   bool
	}{
		{"site name", "site.example.test", nil, true},
		{"site name in upper case", "SITE.Example.Test", nil, true},
		{"wildcard label", "a.wild.example.test", nil, true},
		{"wildcard apex", "wild.example.test", nil, false},
		{"two labels under a wildcard", "a.b.wild.example.test", nil, false},
		{"unknown name", "other.invalid", nil, false},
		{"site name as a suffix", "site.example.test.evil.test", nil, false},
		{"no SNI on an IP the certificate names", "", connOn("203.0.113.7"), true},
		{"no SNI on an IPv4-mapped address", "", connOn("::ffff:203.0.113.7"), true},
		{"no SNI on another IP", "", connOn("198.51.100.1"), false},
		{"no SNI and no connection", "", nil, false},
		// An SNI is never an IP literal, but a name still wins over the address.
		{"unknown name on a named IP", "other.invalid", connOn("203.0.113.7"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cert, err := selector.GetCertificate(&tls.ClientHelloInfo{ServerName: tc.serverName, Conn: tc.conn})
			if tc.wantCert {
				if err != nil || cert == nil {
					t.Fatalf("GetCertificate(%q) = %v, %v; want a certificate", tc.serverName, cert, err)
				}
				return
			}
			// crypto/tls only sends internal_error when the callback fails,
			// so "no certificate" must be an error and never (nil, nil).
			if err == nil {
				t.Fatalf("GetCertificate(%q) returned no error", tc.serverName)
			}
			if cert != nil {
				t.Fatalf("GetCertificate(%q) returned a certificate with error %v", tc.serverName, err)
			}
		})
	}
}

func TestCertSelectorPicksTheMatchingCertificate(t *testing.T) {
	first := newTestCertificate(t, "", "one.example.test")
	second := newTestCertificate(t, "", "two.example.test")
	selector := newTestSelector(t, first, second)

	cert, err := selector.GetCertificate(&tls.ClientHelloInfo{ServerName: "two.example.test"})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if !reflect.DeepEqual(cert.Certificate, second.Certificate) {
		t.Fatal("GetCertificate returned the certificate of another site")
	}
}

func TestCertSelectorMatchHost(t *testing.T) {
	selector := newTestSelector(t,
		newTestCertificate(t, "", "site.example.test", "*.wild.example.test", "203.0.113.7", "2001:db8::7"))

	cases := []struct {
		host string
		want bool
	}{
		{"site.example.test", true},
		{"site.example.test:443", true},
		{"SITE.EXAMPLE.TEST", true},
		{"a.wild.example.test:8443", true},
		{"203.0.113.7", true},
		{"203.0.113.7:443", true},
		{"[2001:db8::7]", true},
		{"[2001:db8::7]:443", true},
		{"", false},
		{"invalid.example", false},
		{"invalid.example:443", false},
		{"wild.example.test", false},
		{"site.example.test.evil.test", false},
		{"198.51.100.1", false},
		{"[::1]:443", false},
	}
	for _, tc := range cases {
		if got := selector.MatchHost(tc.host); got != tc.want {
			t.Errorf("MatchHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestCertSelectorNames(t *testing.T) {
	cases := []struct {
		name string
		cert tls.Certificate
		want []string
	}{
		{
			name: "SANs are listed and the common name is ignored",
			cert: newTestCertificate(t, "Example Org", "Site.Example.Test", "*.wild.example.test", "203.0.113.7"),
			want: []string{"*.wild.example.test", "203.0.113.7", "site.example.test"},
		},
		{
			name: "a certificate without SANs falls back to its common name",
			cert: newTestCertificate(t, "legacy.example.test"),
			want: []string{"legacy.example.test"},
		},
		{
			name: "a certificate that names nothing",
			cert: newTestCertificate(t, ""),
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newTestSelector(t, tc.cert).Names(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Names() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStrictSNIEnabledDefaultsToTrue(t *testing.T) {
	if !(Config{}).StrictSNIEnabled() {
		t.Fatal("strict SNI must be on when unset")
	}
	off, on := false, true
	if (Config{StrictSNI: &off}).StrictSNIEnabled() {
		t.Fatal("strict_sni=false was ignored")
	}
	if !(Config{StrictSNI: &on}).StrictSNIEnabled() {
		t.Fatal("strict_sni=true was ignored")
	}
}

// handshake dials a TLS server that uses cfg and returns the client's view of
// the handshake.
func handshake(t *testing.T, cfg *tls.Config, serverName string) (tls.ConnectionState, error) {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.(*tls.Conn).Handshake()
	}()

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", listener.Addr().String(), &tls.Config{
		ServerName:         serverName, // empty: the ClientHello carries no SNI
		InsecureSkipVerify: true,
	})
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer conn.Close()
	return conn.ConnectionState(), nil
}

func TestStrictTLSConfigHandshake(t *testing.T) {
	cert := newTestCertificate(t, "", "site.example.test")
	strict := CaddyLikeStrictTLSConfig(newTestSelector(t, cert))

	state, err := handshake(t, strict, "site.example.test")
	if err != nil {
		t.Fatalf("handshake with the site name failed: %v", err)
	}
	if got := state.PeerCertificates[0].DNSNames; !reflect.DeepEqual(got, []string{"site.example.test"}) {
		t.Fatalf("served certificate names = %v", got)
	}

	// Caddy answers both of these with the internal_error alert.
	for _, serverName := range []string{"", "wrong.invalid"} {
		_, err := handshake(t, strict, serverName)
		if err == nil {
			t.Fatalf("handshake with SNI %q completed", serverName)
		}
		if !strings.Contains(err.Error(), "internal error") {
			t.Fatalf("handshake with SNI %q failed with %v, want the internal_error alert", serverName, err)
		}
	}

	// The permissive configuration keeps serving the certificate to anyone.
	permissive := CaddyLikeTLSConfig([]tls.Certificate{cert})
	for _, serverName := range []string{"", "wrong.invalid"} {
		if _, err := handshake(t, permissive, serverName); err != nil {
			t.Fatalf("permissive handshake with SNI %q failed: %v", serverName, err)
		}
	}
}

func TestStrictTLSConfigServesIPCertificateWithoutSNI(t *testing.T) {
	strict := CaddyLikeStrictTLSConfig(newTestSelector(t, newTestCertificate(t, "", "127.0.0.1")))
	if _, err := handshake(t, strict, ""); err != nil {
		t.Fatalf("handshake without SNI on an IP the certificate names failed: %v", err)
	}
}
