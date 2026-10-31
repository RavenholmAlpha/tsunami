// TSUNAMI Server — main entry point
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	configfile "github.com/tsunami-protocol/tsunami/pkg/config"
	"github.com/tsunami-protocol/tsunami/pkg/fronting"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"github.com/tsunami-protocol/tsunami/pkg/server"
	"github.com/tsunami-protocol/tsunami/pkg/shaping"
	"github.com/tsunami-protocol/tsunami/pkg/transport"
)

// Populated at build time via -ldflags
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	var (
		listen         = flag.String("listen", ":443", "Listen address")
		cert           = flag.String("cert", "", "TLS certificate file")
		key            = flag.String("key", "", "TLS private key file")
		password       = flag.String("password", "", "User password")
		configPath     = flag.String("config", "", "JSON config file")
		fallback       = flag.String("fallback", "", "Fallback backend address (e.g., 127.0.0.1:8080)")
		forceBBR       = flag.Bool("force-bbr", false, "Force per-connection TCP BBR on Linux")
		useFronting    = flag.Bool("fronting", true, "Serve a Caddy-like HTTPS/HTTP2/WebSocket front")
		frontPath      = flag.String("front-path", fronting.DefaultPath, "Fronting HTTP path")
		frontSecret    = flag.String("front-secret", "", "Fronting HTTP-layer secret (defaults to password)")
		frontSite      = flag.String("front-site-name", "Welcome", "Fronting decoy site name")
		frontDecoy     = flag.String("front-decoy-proxy", "", "Deprecated: use --front-site-upstream")
		frontSiteMode  = flag.String("front-site-mode", "", "Site camouflage mode: builtin, local, or remote (default: inferred from the upstream)")
		frontSiteURL   = flag.String("front-site-upstream", "", "Site camouflage upstream URL or host:port, for example 127.0.0.1:8080 or https://www.example.com")
		frontPreserve  = newTriStateBool("front-site-preserve-host", "Preserve the client Host header for the site camouflage upstream (default: local=true, remote=false)")
		frontInsecure  = newTriStateBool("front-site-insecure-skip-verify", "Skip TLS certificate verification for an HTTPS site camouflage upstream")
		allowAll       = flag.Bool("allow-all", false, "Allow proxying to private/reserved IP addresses")
		trafficShaping = flag.Bool("traffic-shaping", false, "Enable constant-rate traffic shaping")
		showVersion    = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("tsunami-server %s (commit=%s, built=%s)\n", version, commit, buildTime)
		os.Exit(0)
	}

	var config server.Config
	if *configPath != "" {
		fileConfig, err := configfile.LoadFile(*configPath)
		if err != nil {
			log.Fatalf("tsunami-server: load config: %v", err)
		}
		if fileConfig.XBoard.BaseURL != "" {
			log.Fatal("tsunami-server: xboard configuration requires tsunami-node --config")
		}
		config = fileConfig.ToServerConfig()
	} else {
		if (*cert == "") != (*key == "") {
			log.Fatal("tsunami-server: --cert and --key must both be provided, or both omitted")
		}
		if *password == "" {
			log.Fatal("tsunami-server: --password is required")
		}

		config = server.Config{
			Listen: *listen,
			TLS: transport.TLSConfig{
				CertFile:   *cert,
				KeyFile:    *key,
				ALPN:       []string{"h2"},
				MinVersion: 0x0304, // TLS 1.3
			},
			TCP: *transport.DefaultTCPConfig(),
			Users: []*protocol.UserInfo{
				{
					Name:     "default",
					Password: *password,
				},
			},
			FallbackAddr: *fallback,
			Fronting: fronting.Config{
				Enabled:  *useFronting,
				Path:     *frontPath,
				Secret:   *frontSecret,
				SiteName: *frontSite,
			},
		}
	}

	// Site camouflage flags are applied on top of the file (or flag-built)
	// configuration in both modes, so operators can override a config file on
	// the command line. Invalid combinations fail startup with a clear message.
	if err := applyFrontingSiteFlags(&config.Fronting, *frontSiteMode, *frontSiteURL, frontPreserve, frontInsecure, *frontDecoy); err != nil {
		log.Fatalf("tsunami-server: %v", err)
	}
	if config.Fronting.Enabled {
		log.Printf("tsunami-server: fronting site mode=%s upstream=%q preserve_host=%t",
			config.Fronting.Site.Mode,
			frontingUpstreamForLog(config.Fronting.Site),
			config.Fronting.PreserveHostEnabled())
	}

	config.TCP.ForceBBR = *forceBBR
	config.AllowAll = *allowAll
	if *trafficShaping {
		shapingCfg := shaping.DefaultConfig()
		config.Shaping = &shapingCfg
	}

	srv, err := server.New(config)
	if err != nil {
		log.Fatalf("tsunami-server: %v", err)
	}

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("tsunami-server: shutting down...")
		srv.Close()
		os.Exit(0)
	}()

	log.Printf("tsunami-server %s (commit=%s) starting...", version, commit)
	if err := srv.Start(); err != nil {
		log.Fatalf("tsunami-server: %v", err)
	}
}

// triStateBool is a boolean flag that remembers whether it was provided, so it
// can distinguish "not set" from an explicit false. Options whose default
// depends on another setting need this: site preserve_host is true for local
// upstreams and false for remote ones.
type triStateBool struct {
	value bool
	set   bool
}

func newTriStateBool(name, usage string) *triStateBool {
	value := &triStateBool{}
	flag.Var(value, name, usage)
	return value
}

// String implements flag.Value. An unset flag reports an empty default so
// --help does not claim a value the server will not necessarily use.
func (b *triStateBool) String() string {
	if b == nil || !b.set {
		return ""
	}
	return strconv.FormatBool(b.value)
}

// Set implements flag.Value.
func (b *triStateBool) Set(raw string) error {
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return fmt.Errorf("want true or false, got %q", raw)
	}
	b.value = parsed
	b.set = true
	return nil
}

// IsBoolFlag lets "--flag" without a value mean "--flag=true".
func (b *triStateBool) IsBoolFlag() bool { return true }

// applyFrontingSiteFlags overlays the site camouflage CLI flags on cfg,
// normalizes the result, and validates it. The deprecated --front-decoy-proxy
// alias is only used when no site upstream is configured, matching the way the
// deprecated JSON key is handled.
func applyFrontingSiteFlags(cfg *fronting.Config, mode, upstream string, preserveHost, insecureSkipVerify *triStateBool, decoyProxy string) error {
	if legacy := strings.TrimSpace(decoyProxy); legacy != "" {
		log.Printf("tsunami-server: --front-decoy-proxy is deprecated; use --front-site-upstream or fronting.site.upstream")
		switch {
		case strings.TrimSpace(upstream) != "":
			// The replacement flag wins; the alias is silently ignored.
		case strings.TrimSpace(cfg.Site.Upstream) != "":
			log.Printf("tsunami-server: ignoring --front-decoy-proxy because a site upstream is already configured")
		default:
			cfg.DecoyProxy = legacy
		}
	}

	if raw := strings.TrimSpace(mode); raw != "" {
		switch normalized := fronting.SiteMode(strings.ToLower(raw)); normalized {
		case fronting.SiteModeBuiltin, fronting.SiteModeLocal, fronting.SiteModeRemote:
			cfg.Site.Mode = normalized
		default:
			return fmt.Errorf("invalid --front-site-mode %q: want builtin, local, or remote", mode)
		}
	}
	if raw := strings.TrimSpace(upstream); raw != "" {
		cfg.Site.Upstream = raw
	}
	if preserveHost != nil && preserveHost.set {
		value := preserveHost.value
		cfg.Site.PreserveHost = &value
	}
	if insecureSkipVerify != nil && insecureSkipVerify.set {
		cfg.Site.InsecureSkipVerify = insecureSkipVerify.value
	}

	cfg.Normalize()
	if err := cfg.SiteError(); err != nil {
		return fmt.Errorf("invalid site camouflage configuration: %w", err)
	}
	return nil
}

// frontingUpstreamForLog returns a log-safe upstream description.
func frontingUpstreamForLog(site fronting.SiteConfig) string {
	if upstream := site.UpstreamURL(); upstream != nil {
		return upstream.Redacted()
	}
	return site.Upstream
}
