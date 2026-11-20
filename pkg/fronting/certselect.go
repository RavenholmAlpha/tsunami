package fronting

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
)

// CertSelector picks the certificate for a TLS handshake the way Caddy
// (certmagic) does for a site block, instead of crypto/tls's default of
// handing the first certificate to every client.
//
// A stock Caddy serving "example.com { ... }" only holds a certificate for
// that name, so:
//   - a ClientHello naming the site, or a name covered by a wildcard
//     certificate, gets the certificate;
//   - a ClientHello without SNI is matched against the IP address the client
//     connected to, which only succeeds for certificates with IP SANs;
//   - everything else fails the handshake with the internal_error alert.
//
// The same names decide which HTTP Host values belong to the site.
type CertSelector struct {
	entries []certEntry
}

type certEntry struct {
	cert *tls.Certificate
	// exact holds normalized DNS names and IP literals.
	exact map[string]struct{}
	// wildcard holds the suffix of each "*.example.com" name, dot included.
	wildcard map[string]struct{}
}

// NewCertSelector indexes certs by the names on their leaf certificates.
func NewCertSelector(certs []tls.Certificate) (*CertSelector, error) {
	selector := &CertSelector{}
	for i := range certs {
		cert := &certs[i]
		leaf := cert.Leaf
		if leaf == nil {
			if len(cert.Certificate) == 0 {
				return nil, fmt.Errorf("fronting: certificate %d is empty", i)
			}
			parsed, err := x509.ParseCertificate(cert.Certificate[0])
			if err != nil {
				return nil, fmt.Errorf("fronting: parse certificate %d: %w", i, err)
			}
			leaf = parsed
		}

		entry := certEntry{
			cert:     cert,
			exact:    make(map[string]struct{}),
			wildcard: make(map[string]struct{}),
		}
		for _, name := range leaf.DNSNames {
			entry.add(name)
		}
		for _, ip := range leaf.IPAddresses {
			entry.add(ip.String())
		}
		// A legacy certificate without SANs is still served under its
		// Common Name rather than locking every client out.
		if len(entry.exact)+len(entry.wildcard) == 0 {
			entry.add(leaf.Subject.CommonName)
		}
		selector.entries = append(selector.entries, entry)
	}
	return selector, nil
}

func (e *certEntry) add(name string) {
	name = normalizeHostName(name)
	if name == "" {
		return
	}
	if strings.HasPrefix(name, "*.") {
		e.wildcard[name[1:]] = struct{}{}
		return
	}
	e.exact[name] = struct{}{}
}

// matches reports whether a normalized name is covered. A wildcard stands for
// exactly one label, as in certificate verification.
func (e *certEntry) matches(name string) bool {
	if _, ok := e.exact[name]; ok {
		return true
	}
	if dot := strings.IndexByte(name, '.'); dot > 0 {
		_, ok := e.wildcard[name[dot:]]
		return ok
	}
	return false
}

// Names returns every name the certificates cover, sorted.
func (s *CertSelector) Names() []string {
	var names []string
	for _, entry := range s.entries {
		for name := range entry.exact {
			names = append(names, name)
		}
		for suffix := range entry.wildcard {
			names = append(names, "*"+suffix)
		}
	}
	sort.Strings(names)
	return names
}

func (s *CertSelector) find(name string) *tls.Certificate {
	if name == "" {
		return nil
	}
	for i := range s.entries {
		if s.entries[i].matches(name) {
			return s.entries[i].cert
		}
	}
	return nil
}

// GetCertificate implements tls.Config.GetCertificate. Returning an error
// makes crypto/tls abort the handshake with internal_error, which is the
// alert Caddy sends for a server name it holds no certificate for.
func (s *CertSelector) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := normalizeHostName(hello.ServerName)
	if name == "" {
		if cert := s.find(localIP(hello.Conn)); cert != nil {
			return cert, nil
		}
		return nil, errors.New("fronting: no certificate available: the client sent no server name")
	}
	if cert := s.find(name); cert != nil {
		return cert, nil
	}
	return nil, fmt.Errorf("fronting: no certificate available for %q", name)
}

// MatchHost reports whether an HTTP Host header (with or without a port)
// names a site the certificates cover.
func (s *CertSelector) MatchHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return s.find(normalizeHostName(host)) != nil
}

// normalizeHostName lowercases a server name or host, drops IPv6 brackets,
// and puts IP literals in canonical form.
func normalizeHostName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimSuffix(strings.TrimPrefix(name, "["), "]")
	if addr, err := netip.ParseAddr(name); err == nil {
		return addr.Unmap().WithZone("").String()
	}
	return name
}

// localIP returns the address the client connected to, or "" if unknown.
func localIP(conn net.Conn) string {
	if conn == nil || conn.LocalAddr() == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return ""
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return ""
	}
	return addr.Unmap().WithZone("").String()
}
