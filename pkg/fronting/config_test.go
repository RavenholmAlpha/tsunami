package fronting

import (
	"strings"
	"testing"
)

func boolPtr(v bool) *bool { return &v }

func TestNormalizeInfersSiteMode(t *testing.T) {
	tests := []struct {
		name        string
		site        SiteConfig
		decoyProxy  string
		wantMode    SiteMode
		wantUpstrm  string
		wantPreserv bool
	}{
		{
			name:        "no upstream is builtin",
			wantMode:    SiteModeBuiltin,
			wantPreserv: false,
		},
		{
			name:        "builtin mode is kept",
			site:        SiteConfig{Mode: SiteModeBuiltin},
			wantMode:    SiteModeBuiltin,
			wantPreserv: false,
		},
		{
			name:        "loopback ipv4 upstream is local",
			site:        SiteConfig{Upstream: "http://127.0.0.1:8080"},
			wantMode:    SiteModeLocal,
			wantUpstrm:  "http://127.0.0.1:8080",
			wantPreserv: true,
		},
		{
			name:        "localhost name is local",
			site:        SiteConfig{Upstream: "localhost:8080"},
			wantMode:    SiteModeLocal,
			wantUpstrm:  "http://localhost:8080",
			wantPreserv: true,
		},
		{
			name:        "loopback ipv6 is local",
			site:        SiteConfig{Upstream: "[::1]:8080"},
			wantMode:    SiteModeLocal,
			wantUpstrm:  "http://[::1]:8080",
			wantPreserv: true,
		},
		{
			name:        "rfc1918 upstream is local",
			site:        SiteConfig{Upstream: "10.1.2.3:8080"},
			wantMode:    SiteModeLocal,
			wantUpstrm:  "http://10.1.2.3:8080",
			wantPreserv: true,
		},
		{
			name:        "ipv6 ula upstream is local",
			site:        SiteConfig{Upstream: "http://[fd00::1]:8080"},
			wantMode:    SiteModeLocal,
			wantUpstrm:  "http://[fd00::1]:8080",
			wantPreserv: true,
		},
		{
			name:        "link local upstream is local",
			site:        SiteConfig{Upstream: "169.254.10.20:80"},
			wantMode:    SiteModeLocal,
			wantUpstrm:  "http://169.254.10.20:80",
			wantPreserv: true,
		},
		{
			name:        "public host is remote and rewrites host",
			site:        SiteConfig{Upstream: "https://www.example.com"},
			wantMode:    SiteModeRemote,
			wantUpstrm:  "https://www.example.com",
			wantPreserv: false,
		},
		{
			name:        "explicit local mode is respected",
			site:        SiteConfig{Mode: SiteModeLocal, Upstream: "http://192.168.1.5:8080"},
			wantMode:    SiteModeLocal,
			wantUpstrm:  "http://192.168.1.5:8080",
			wantPreserv: true,
		},
		{
			name:        "explicit remote mode is respected",
			site:        SiteConfig{Mode: SiteModeRemote, Upstream: "http://127.0.0.1:8080"},
			wantMode:    SiteModeRemote,
			wantUpstrm:  "http://127.0.0.1:8080",
			wantPreserv: false,
		},
		{
			name:        "explicit preserve_host wins in local mode",
			site:        SiteConfig{Mode: SiteModeLocal, Upstream: "http://127.0.0.1:8080", PreserveHost: boolPtr(false)},
			wantMode:    SiteModeLocal,
			wantUpstrm:  "http://127.0.0.1:8080",
			wantPreserv: false,
		},
		{
			name:        "explicit preserve_host wins in remote mode",
			site:        SiteConfig{Mode: SiteModeRemote, Upstream: "https://www.example.com", PreserveHost: boolPtr(true)},
			wantMode:    SiteModeRemote,
			wantUpstrm:  "https://www.example.com",
			wantPreserv: true,
		},
		{
			name:        "deprecated decoy proxy infers local",
			decoyProxy:  "http://127.0.0.1:9090",
			wantMode:    SiteModeLocal,
			wantUpstrm:  "http://127.0.0.1:9090",
			wantPreserv: true,
		},
		{
			name:        "deprecated decoy proxy infers remote",
			decoyProxy:  "https://origin.example.net",
			wantMode:    SiteModeRemote,
			wantUpstrm:  "https://origin.example.net",
			wantPreserv: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Site: tt.site, DecoyProxy: tt.decoyProxy}
			cfg.Normalize()

			if err := cfg.SiteError(); err != nil {
				t.Fatalf("site config error = %v", err)
			}
			if cfg.Site.Mode != tt.wantMode {
				t.Fatalf("site mode = %q, want %q", cfg.Site.Mode, tt.wantMode)
			}
			if cfg.Site.Upstream != tt.wantUpstrm {
				t.Fatalf("site upstream = %q, want %q", cfg.Site.Upstream, tt.wantUpstrm)
			}
			if got := cfg.PreserveHostEnabled(); got != tt.wantPreserv {
				t.Fatalf("preserve host = %t, want %t", got, tt.wantPreserv)
			}
			if tt.wantMode == SiteModeBuiltin && cfg.Site.UpstreamURL() != nil {
				t.Fatal("builtin mode parsed an upstream URL")
			}
			if tt.wantMode != SiteModeBuiltin && cfg.Site.UpstreamURL() == nil {
				t.Fatal("local/remote mode did not parse an upstream URL")
			}
		})
	}
}

func TestNormalizeInfersUpstreamScheme(t *testing.T) {
	tests := []struct {
		name       string
		upstream   string
		wantScheme string
		wantHost   string
	}{
		{name: "bare host port defaults to http", upstream: "127.0.0.1:8080", wantScheme: "http", wantHost: "127.0.0.1:8080"},
		{name: "bare host port 443 is https", upstream: "site.example.com:443", wantScheme: "https", wantHost: "site.example.com:443"},
		{name: "bare host defaults to http", upstream: "origin.example.com", wantScheme: "http", wantHost: "origin.example.com"},
		{name: "explicit https kept", upstream: "https://site.example.com:8443", wantScheme: "https", wantHost: "site.example.com:8443"},
		{name: "explicit http kept", upstream: "http://site.example.com:8080", wantScheme: "http", wantHost: "site.example.com:8080"},
		{name: "bare ipv6 443 is https", upstream: "[::1]:443", wantScheme: "https", wantHost: "[::1]:443"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Site: SiteConfig{Upstream: tt.upstream}}
			cfg.Normalize()
			if err := cfg.SiteError(); err != nil {
				t.Fatalf("site config error = %v", err)
			}
			parsed := cfg.Site.UpstreamURL()
			if parsed == nil {
				t.Fatal("upstream URL was not parsed")
			}
			if parsed.Scheme != tt.wantScheme {
				t.Fatalf("scheme = %q, want %q", parsed.Scheme, tt.wantScheme)
			}
			if parsed.Host != tt.wantHost {
				t.Fatalf("host = %q, want %q", parsed.Host, tt.wantHost)
			}
		})
	}
}

func TestNormalizeTrimsUpstreamTrailingSlash(t *testing.T) {
	cfg := Config{Site: SiteConfig{Upstream: "http://127.0.0.1:8080/"}}
	cfg.Normalize()
	if err := cfg.SiteError(); err != nil {
		t.Fatalf("site config error = %v", err)
	}
	if cfg.Site.Upstream != "http://127.0.0.1:8080" {
		t.Fatalf("upstream = %q, want trailing slash trimmed", cfg.Site.Upstream)
	}

	withPath := Config{Site: SiteConfig{Mode: SiteModeLocal, Upstream: "http://127.0.0.1:8080/base/"}}
	withPath.Normalize()
	if err := withPath.SiteError(); err != nil {
		t.Fatalf("site config error = %v", err)
	}
	if withPath.Site.Upstream != "http://127.0.0.1:8080/base" {
		t.Fatalf("upstream with path = %q, want trailing slash trimmed", withPath.Site.Upstream)
	}
}

func TestNormalizeSiteRejectsInvalidUpstreams(t *testing.T) {
	tests := []struct {
		name     string
		site     SiteConfig
		wantPart string
	}{
		{
			name:     "unsupported scheme",
			site:     SiteConfig{Upstream: "ftp://127.0.0.1:21"},
			wantPart: "must use http or https",
		},
		{
			name:     "missing host",
			site:     SiteConfig{Upstream: "https://"},
			wantPart: "has no host",
		},
		{
			name:     "unparsable url",
			site:     SiteConfig{Upstream: "http://exa mple.com"},
			wantPart: "parse site upstream",
		},
		{
			name:     "credentials not supported",
			site:     SiteConfig{Upstream: "http://user:pass@127.0.0.1:8080"},
			wantPart: "must not contain credentials",
		},
		{
			name:     "invalid port",
			site:     SiteConfig{Upstream: "http://127.0.0.1:99999"},
			wantPart: "invalid port",
		},
		{
			name:     "non numeric port",
			site:     SiteConfig{Upstream: "http://127.0.0.1:http"},
			wantPart: "parse site upstream",
		},
		{
			name:     "local mode without upstream",
			site:     SiteConfig{Mode: SiteModeLocal},
			wantPart: "requires a site upstream",
		},
		{
			name:     "remote mode without upstream",
			site:     SiteConfig{Mode: SiteModeRemote},
			wantPart: "requires a site upstream",
		},
		{
			name:     "unknown mode",
			site:     SiteConfig{Mode: SiteMode("neighbour"), Upstream: "https://example.com"},
			wantPart: "unsupported site mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Site: tt.site}
			cfg.Normalize()
			err := cfg.SiteError()
			if err == nil {
				t.Fatal("expected a site configuration error")
			}
			if !strings.Contains(err.Error(), tt.wantPart) {
				t.Fatalf("error = %q, want it to contain %q", err, tt.wantPart)
			}
			if validateErr := cfg.ValidateSite(); validateErr == nil {
				t.Fatal("ValidateSite accepted an invalid configuration")
			}
		})
	}
}

func TestValidateSiteAcceptsDecoyProxyOnly(t *testing.T) {
	cfg := Config{DecoyProxy: "127.0.0.1:8080"}
	if err := cfg.ValidateSite(); err != nil {
		t.Fatalf("ValidateSite(decoy_proxy) = %v", err)
	}
	// ValidateSite must not mutate the receiver.
	if cfg.Site.Upstream != "" || cfg.Site.Mode != "" {
		t.Fatalf("ValidateSite mutated config: %+v", cfg.Site)
	}
}

func TestNormalizeSiteExplicitConfigWinsOverDecoyProxy(t *testing.T) {
	cfg := Config{
		DecoyProxy: "http://127.0.0.1:9090",
		Site: SiteConfig{
			Mode:     SiteModeRemote,
			Upstream: "https://www.example.com",
		},
	}
	cfg.Normalize()
	if err := cfg.SiteError(); err != nil {
		t.Fatalf("site config error = %v", err)
	}
	if cfg.Site.Mode != SiteModeRemote || cfg.Site.Upstream != "https://www.example.com" {
		t.Fatalf("explicit site config lost: %+v", cfg.Site)
	}
	if cfg.PreserveHostEnabled() {
		t.Fatal("remote mode should rewrite the Host header by default")
	}
}

func TestNormalizeSiteBuiltinIgnoresUpstream(t *testing.T) {
	cfg := Config{Site: SiteConfig{Mode: SiteModeBuiltin, Upstream: "not a url"}}
	cfg.Normalize()
	if err := cfg.SiteError(); err != nil {
		t.Fatalf("builtin mode should ignore the upstream: %v", err)
	}
	if cfg.Site.UpstreamURL() != nil {
		t.Fatal("builtin mode must not expose an upstream URL")
	}
	if cfg.PreserveHostEnabled() {
		t.Fatal("builtin mode has no upstream to preserve Host for")
	}
}

func TestNormalizeSiteIsIdempotent(t *testing.T) {
	cfg := Config{Site: SiteConfig{Upstream: "127.0.0.1:8080/"}}
	cfg.Normalize()
	first := cfg.Site
	cfg.Normalize()
	if cfg.Site.Mode != first.Mode || cfg.Site.Upstream != first.Upstream {
		t.Fatalf("second Normalize changed site config: %+v -> %+v", first, cfg.Site)
	}
	if cfg.Site.UpstreamURL() == nil || cfg.Site.UpstreamURL().String() != "http://127.0.0.1:8080" {
		t.Fatalf("upstream URL after second Normalize = %v", cfg.Site.UpstreamURL())
	}
}
